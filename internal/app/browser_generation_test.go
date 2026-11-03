package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func withBrowserProtocol(t *testing.T, respond func(browserRequest) []browserEvent) {
	t.Helper()
	inputR, inputW := io.Pipe()
	outputR, outputW := io.Pipe()
	rpc := newBrowserRPC(inputW, outputR, nil)
	browserManager.Lock()
	prev := browserManager.rpc
	browserManager.rpc = rpc
	browserManager.Unlock()
	t.Cleanup(func() {
		inputR.Close()
		outputW.Close()
		rpc.Close()
		browserManager.Lock()
		browserManager.rpc = prev
		browserManager.Unlock()
	})
	go func() {
		dec, enc := json.NewDecoder(inputR), json.NewEncoder(outputW)
		for {
			var req browserRequest
			if dec.Decode(&req) != nil {
				return
			}
			for _, e := range respond(req) {
				e.ID, e.AccountID = req.ID, req.AccountID
				if enc.Encode(e) != nil {
					return
				}
			}
		}
	}()
}

func protocolBody(body string) []browserEvent {
	return []browserEvent{{Type: "start", Status: 200}, {Type: "chunk", DataBase64: base64.StdEncoding.EncodeToString([]byte(body))}, {Type: "end"}}
}

// 旧业务回归显式应答 quota，并验证实际账号档案与当前 Cookie。
func protocolAvailableQuota(t *testing.T, req browserRequest) []browserEvent {
	t.Helper()
	b, err := browserAccountByID(req.AccountID)
	a := accountByID(req.AccountID)
	if err != nil || b == nil || a == nil || req.Op != "quota" || req.ProfileID != b.ProfileID || req.ExpectedEmail != b.Email || req.TimeoutMs <= 0 {
		t.Errorf("quota 子请求归属或超时无效: %+v", req)
		return []browserEvent{{Type: "error", Code: "invalid_quota_request"}}
	}
	p, available := float64(0), false
	return []browserEvent{{Type: "end", Session: &browserSession{ProfileID: b.ProfileID, Email: b.Email, Cookie: a.Cookie, LoginState: "ready"}, Quota: &quotaObservation{UsedPercent: &p, FiveHourLimited: &available, WeeklyUsedPercent: &p, WeeklyLimited: &available, CheckedAt: time.Now().Unix()}}}
}

// 集成整个现有生成函数，页面参数和生成请求都必须落在同一个账号档案的工作进程。
func TestBrowserGenerationUsesProfileForAllRequests(t *testing.T) {
	id := newBrowserTestAccount(t)
	meta, _ := browserAccountByID(id)
	_ = storeBrowserSession(id, browserSession{ProfileID: meta.ProfileID, Email: "owner@example.com", Cookie: "SAPISID=owner", LoginState: "ready"})
	_ = setSelectedAccount(id)
	t.Cleanup(func() { _ = setSelectedAccount(0) })
	previous := rtCfg()
	rtMu.Lock()
	rtVal.RetryAttempts = 1
	rtVal.RequestTimeout = 0
	rtVal.AnonFirst = false
	rtVal.GeminiBLAuto = true
	rtVal.GeminiBL = "boq_gemini-web-uiserver_20261007.01_p0"
	rtMu.Unlock()
	t.Cleanup(func() { rtMu.Lock(); rtVal = previous; rtMu.Unlock() })
	calls := make(chan browserRequest, 3)
	withBrowserProtocol(t, func(req browserRequest) []browserEvent {
		if req.Op == "quota" {
			return protocolAvailableQuota(t, req)
		}
		calls <- req
		if req.URL == "https://gemini.google.com/app" {
			return protocolBody(`{"SNlM0e":"signed-in-token:12345","cfb2h":"boq_gemini-web-uiserver_20261021.01_p0"}`)
		}
		return protocolBody(okRaw)
	})
	delta := ""
	mc := Models["gemini-3.6-flash"]
	mc.Context = context.Background()
	res, err := streamGenerate("test browser", "test browser", mc, func(s string) { delta += s }, nil)
	if err != nil || res.AccountID != id || delta != "banana" {
		t.Fatalf("生成未使用浏览器: %+v delta=%q %v", res, delta, err)
	}
	for i := 0; i < 2; i++ {
		r := <-calls
		if r.AccountID != id || r.ProfileID != meta.ProfileID || r.Op != "request" {
			t.Fatalf("子请求账号归属丢失: %+v", r)
		}
		if i == 1 && !strings.Contains(r.URL, "bl=boq_gemini-web-uiserver_20261021.01_p0") {
			t.Fatalf("未使用浏览器页面构建参数: %s", r.URL)
		}
	}
}

// 固定账号需要登录时，生成必须直接失败；不能退回匿名模式消耗错误模型。
func TestBrowserPinnedLoginRequiredDoesNotGoAnonymous(t *testing.T) {
	id := newBrowserTestAccount(t)
	mc := Models["gemini-3.6-flash"]
	mc.PinAccountID = id
	res, err := streamGenerate("test", "test", mc, nil, nil)
	if err == nil || res.AccountID != id {
		t.Fatalf("固定账号未登录却继续生成: %+v %v", res, err)
	}
}

// 用户在面板换号以后，原会话和视频续写必须继续使用任务原账号。
func TestBrowserExistingConversationIgnoresSelectedAccount(t *testing.T) {
	id := newBrowserTestAccount(t)
	other := newBrowserTestAccount(t)
	for _, entry := range []struct {
		id    int64
		email string
	}{{id, "owner@example.com"}, {other, "other@example.com"}} {
		meta, _ := browserAccountByID(entry.id)
		_ = storeBrowserSession(entry.id, browserSession{ProfileID: meta.ProfileID, Email: entry.email, Cookie: "SAPISID=" + entry.email, LoginState: "ready"})
	}
	_ = setSelectedAccount(other)
	t.Cleanup(func() { _ = setSelectedAccount(0) })
	previous := rtCfg()
	rtMu.Lock()
	rtVal.RetryAttempts = 1
	rtVal.RequestTimeout = 0
	rtVal.AnonFirst = false
	rtMu.Unlock()
	t.Cleanup(func() { rtMu.Lock(); rtVal = previous; rtMu.Unlock() })
	withBrowserProtocol(t, func(req browserRequest) []browserEvent {
		if req.AccountID != id {
			t.Errorf("原任务被切换到账号 %d", req.AccountID)
		}
		if req.Op == "quota" {
			return protocolAvailableQuota(t, req)
		}
		if req.URL == "https://gemini.google.com/app" {
			return protocolBody(`{"SNlM0e":"signed-in-token:12345"}`)
		}
		return protocolBody(okRaw)
	})
	conv := &convState{accountID: id, isLogin: true}
	res, err := streamGenerateConv("continue", Models["gemini-3.6-flash"], conv, nil, nil)
	if err != nil || res.AccountID != id || conv.turn != 1 {
		t.Fatalf("会话切号: %+v %v", res, err)
	}
}

// 页面更新 Cookie 后，生成必须使用最新 Cookie 快照而不是旧账号快照。
func TestBrowserGenerationSyncsPageCookie(t *testing.T) {
	id := newBrowserTestAccount(t)
	meta, _ := browserAccountByID(id)
	_ = storeBrowserSession(id, browserSession{ProfileID: meta.ProfileID, Email: "owner@example.com", Cookie: "SAPISID=old", LoginState: "ready"})
	previous := rtCfg()
	rtMu.Lock()
	rtVal.RetryAttempts = 1
	rtVal.RequestTimeout = 0
	rtMu.Unlock()
	t.Cleanup(func() { rtMu.Lock(); rtVal = previous; rtMu.Unlock() })
	withBrowserProtocol(t, func(req browserRequest) []browserEvent {
		if req.Op == "quota" {
			return protocolAvailableQuota(t, req)
		}
		if req.URL == "https://gemini.google.com/app" {
			events := protocolBody(`{"SNlM0e":"signed-in-token:12345"}`)
			events[2].Session = &browserSession{ProfileID: meta.ProfileID, Email: "owner@example.com", Cookie: "SAPISID=new", LoginState: "ready"}
			return events
		}
		if req.Headers["Cookie"] != "SAPISID=new" {
			t.Errorf("生成使用了过期 Cookie: %s", req.Headers["Cookie"])
		}
		return protocolBody(okRaw)
	})
	mc := Models["gemini-3.6-flash"]
	mc.PinAccountID = id
	if _, err := streamGenerate("test", "test", mc, nil, nil); err != nil {
		t.Fatal(err)
	}
}

// 浏览器上传、历史轮询和下载都要使用相同账号档案，媒体不能绕回 Go 网络。
func TestBrowserUploadPollAndDownloadUseProfile(t *testing.T) {
	id := newBrowserTestAccount(t)
	meta, _ := browserAccountByID(id)
	_ = storeBrowserSession(id, browserSession{ProfileID: meta.ProfileID, Email: "owner@example.com", Cookie: "SAPISID=owner", LoginState: "ready"})
	limit := float64(100)
	meta, _ = browserAccountByID(id)
	if err := persistQuota(id, *meta, nil, quotaObservation{UsedPercent: &limit, CheckedAt: time.Now().Unix()}, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	var operations []string
	withBrowserProtocol(t, func(req browserRequest) []browserEvent {
		if req.AccountID != id || req.ProfileID != meta.ProfileID {
			t.Errorf("媒体串号: %+v", req)
		}
		operations = append(operations, req.Op+":"+req.Method)
		switch {
		case req.URL == "https://gemini.google.com/app":
			return protocolBody(`{"SNlM0e":"signed-in-token:12345","qKIAYe":"push-1234"}`)
		case req.URL == uploadHost:
			events := protocolBody("")
			events[0].Headers = map[string]string{"x-goog-upload-url": "https://push.clients6.google.com/upload/final"}
			return events
		case strings.HasSuffix(req.URL, "/final"):
			return protocolBody("/contrib_service/ttl_1d/ref")
		case req.Op == "download":
			events := protocolBody("binary-image")
			events[0].Headers = map[string]string{"content-type": "image/png"}
			return events
		default:
			return protocolBody("history-payload")
		}
	})
	tr, _ := newBrowserTransport(context.Background(), accountByID(id), "")
	ref, err := uploadBytes("SAPISID=owner", "", []byte{0, 1, 2}, "图.png", tr)
	if err != nil || ref != "/contrib_service/ttl_1d/ref" {
		t.Fatalf("上传 %q %v", ref, err)
	}
	if raw, err := pollHistoryRaw("c_id", "SAPISID=owner", "owner", "token", "", tr); err != nil || raw != "history-payload" {
		t.Fatalf("轮询 %q %v", raw, err)
	}
	mime, data, err := downloadBytes("https://exampleusercontent.invalid/media", "SAPISID=owner", "", "image/png", tr)
	if err != nil || mime != "image/png" || string(data) != "binary-image" || len(operations) != 5 {
		t.Fatalf("媒体 %s %q %v %v", mime, data, operations, err)
	}
}
