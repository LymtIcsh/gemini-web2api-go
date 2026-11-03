package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// 已确认的视频不能因尾部响应流中断被重发，必须取回同一会话的 MP4。
func TestBrowserVideoStreamFailureAfterAcknowledgementRecoversExistingVideo(t *testing.T) {
	testBrowserVideoStreamFailureRecoversExistingVideo(t, "testdata/video_ack_raw.txt")
}

// 实际浏览器先收到本轮 ID，确认帧前断流；历史轮询仍能取回已经提交的视频。
func TestBrowserVideoStreamFailureBeforeAcknowledgementRecoversExistingVideo(t *testing.T) {
	testBrowserVideoStreamFailureRecoversExistingVideo(t, "testdata/video_stream_partial_raw.txt")
}

func testBrowserVideoStreamFailureRecoversExistingVideo(t *testing.T, fixture string) {
	t.Helper()
	id := newBrowserTestAccount(t)
	meta, _ := browserAccountByID(id)
	if err := storeBrowserSession(id, browserSession{ProfileID: meta.ProfileID, Email: "owner@example.com", Cookie: "SAPISID=owner", LoginState: "ready"}); err != nil {
		t.Fatal(err)
	}
	previous := rtCfg()
	rtMu.Lock()
	rtVal.RetryAttempts = 2
	rtVal.RetryDelaySec = 0
	rtVal.RequestTimeout = 0
	rtVal.PerIPRPM = 0
	rtVal.PerIPRPH = 0
	rtMu.Unlock()
	t.Cleanup(func() { rtMu.Lock(); rtVal = previous; rtMu.Unlock() })
	ack, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	// 字节和会话归属按既有真实确认 fixture 固定，避免从被测解析函数计算预期。
	mp4 := append([]byte{0, 0, 0, 32, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, bytes.Repeat([]byte{7}, 4096)...)
	turn := []any{[]any{"c_0a840dff0f2e13b1", "r_b1e9ff3bc71d448b"}, nil, nil, nil, "video_generation", "rc_1234567890", "https://contribution.usercontent.google.com/download?c=cmVzcG9uc2VfZGF0YQ"}
	inner, _ := json.Marshal([]any{[]any{turn}})
	history, _ := json.Marshal([]any{[]any{"wrb.fr", "hNvQHb", string(inner)}})
	generateCalls, historyCalls, downloadCalls := 0, 0, 0
	withBrowserProtocol(t, func(req browserRequest) []browserEvent {
		if req.AccountID != id || req.ProfileID != meta.ProfileID {
			t.Errorf("取回现有视频时账号档案改变")
		}
		if req.Op == "quota" {
			return protocolAvailableQuota(t, req)
		}
		switch {
		case req.URL == "https://gemini.google.com/app":
			return protocolBody(`{"SNlM0e":"signed-in-token:12345"}`)
		case strings.Contains(req.URL, "/StreamGenerate?"):
			generateCalls++
			events := protocolBody(string(ack))
			events[2] = browserEvent{Type: "error", Code: "browser_error", Error: "page.evaluate: Error in input stream"}
			return events
		case strings.Contains(req.URL, "rpcids=hNvQHb"):
			historyCalls++
			return protocolBody(string(history))
		case req.Op == "download":
			downloadCalls++
			events := protocolBody(string(mp4))
			events[0].Headers = map[string]string{"content-type": "video/mp4"}
			return events
		default:
			t.Errorf("意外请求: %s", req.URL)
			return []browserEvent{{Type: "error", Code: "unexpected_request"}}
		}
	})
	j := &videoJob{ID: "video_stream_recovery", Model: "gemini-video", Prompt: "已有视频确认", Size: "1280x720"}
	putVideoJob(j)
	t.Cleanup(func() { videoJobsMu.Lock(); delete(videoJobs, j.ID); videoJobsMu.Unlock() })
	runVideoJobWith(j, nil, id, nil)
	w := httptest.NewRecorder()
	handleVideoItem(w, httptest.NewRequest(http.MethodGet, "/v1/videos/"+j.ID+"/content", nil))
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "video/mp4" || !bytes.Equal(w.Body.Bytes(), mp4) {
		t.Fatalf("已生成视频未返回: status=%d job=%s error=%s", w.Code, j.Status, j.Error)
	}
	if generateCalls != 1 || historyCalls != 1 || downloadCalls != 1 {
		t.Fatalf("错误重发生成或未继续取回: generate=%d history=%d download=%d", generateCalls, historyCalls, downloadCalls)
	}
	if j.accountID != id || j.cid != "c_0a840dff0f2e13b1" || j.rid != "r_b1e9ff3bc71d448b" {
		t.Fatalf("返回的产物失去本次会话归属: account=%d cid=%s rid=%s", j.accountID, j.cid, j.rid)
	}
}

// 缺少本轮 ID、鉴权或取消错误不能误取旧视频，也不能吞掉文字流错误。
func TestBrowserStreamFailureWithoutRecoverableVideoStaysFailure(t *testing.T) {
	ack, err := os.ReadFile("testdata/video_ack_raw.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, model, raw, code string
		status                 int
	}{
		{"video_without_ids", "gemini-video", `[["wrb.fr",null,"[null,null,{\"7\":[\"video_generation\"]}]"]]`, "browser_error", 200},
		{"video_without_rid", "gemini-video", `[["wrb.fr",null,"[null,[\"c_0a840dff0f2e13b1\",\"\"],{\"7\":[\"video_generation\"]}]"]]`, "browser_error", 200},
		{"video_cancelled", "gemini-video", string(ack), "cancelled", 200},
		{"video_timed_out", "gemini-video", string(ack), "timeout", 200},
		{"video_login_required", "gemini-video", string(ack), "login_required", 200},
		{"video_http_error", "gemini-video", string(ack), "browser_error", 403},
		{"text_stream_failure", "gemini-3.6-flash", okRaw, "browser_error", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := newBrowserTestAccount(t)
			meta, _ := browserAccountByID(id)
			if err := storeBrowserSession(id, browserSession{ProfileID: meta.ProfileID, Email: "owner@example.com", Cookie: "SAPISID=owner", LoginState: "ready"}); err != nil {
				t.Fatal(err)
			}
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
					return protocolBody(`{"SNlM0e":"signed-in-token:12345"}`)
				}
				if !strings.Contains(req.URL, "/StreamGenerate?") {
					t.Errorf("失败任务不应取回其他视频: %s", req.URL)
				}
				events := protocolBody(tc.raw)
				events[0].Status = tc.status
				events[2] = browserEvent{Type: "error", Code: tc.code, Error: "page.evaluate: Error in input stream"}
				return events
			})
			mc := Models[tc.model]
			mc.PinAccountID, mc.Context = id, context.Background()
			if _, err := streamGenerate("test", "test", mc, nil, nil); err == nil {
				t.Fatal("未确认、取消或文字错误被当作视频成功")
			}
		})
	}
}

// 断流后只允许取本轮结果；历史中的旧视频不能覆盖本轮明确的拒绝。
func TestBrowserVideoStreamFailureDoesNotRecoverAnotherTurn(t *testing.T) {
	id := newBrowserTestAccount(t)
	meta, _ := browserAccountByID(id)
	if err := storeBrowserSession(id, browserSession{ProfileID: meta.ProfileID, Email: "owner@example.com", Cookie: "SAPISID=owner", LoginState: "ready"}); err != nil {
		t.Fatal(err)
	}
	previous := rtCfg()
	rtMu.Lock()
	rtVal.RetryAttempts = 2
	rtVal.RequestTimeout = 0
	rtMu.Unlock()
	t.Cleanup(func() { rtMu.Lock(); rtVal = previous; rtMu.Unlock() })
	partial, err := os.ReadFile("testdata/video_stream_partial_raw.txt")
	if err != nil {
		t.Fatal(err)
	}
	current := []any{[]any{"c_0a840dff0f2e13b1", "r_b1e9ff3bc71d448b"}, nil, nil, []any{[]any{[]any{"rc_1234567890", []any{"I can't generate that video."}}}}}
	old := []any{[]any{"c_0a840dff0f2e13b1", "r_ffffffffffffffff"}, nil, nil, nil, "video_generation", "https://contribution.usercontent.google.com/download?c=cmVzcG9uc2VfZGF0YQ"}
	inner, _ := json.Marshal([]any{[]any{old, current}})
	history, _ := json.Marshal([]any{[]any{"wrb.fr", "hNvQHb", string(inner)}})
	generateCalls, historyCalls, downloadCalls := 0, 0, 0
	withBrowserProtocol(t, func(req browserRequest) []browserEvent {
		if req.AccountID != id || req.ProfileID != meta.ProfileID {
			t.Error("断流恢复改变了账号归属")
		}
		if req.Op == "quota" {
			return protocolAvailableQuota(t, req)
		}
		switch {
		case req.URL == "https://gemini.google.com/app":
			return protocolBody(`{"SNlM0e":"signed-in-token:12345"}`)
		case strings.Contains(req.URL, "/StreamGenerate?"):
			generateCalls++
			events := protocolBody(string(partial))
			events[2] = browserEvent{Type: "error", Code: "browser_error", Error: "page.evaluate: Error in input stream"}
			return events
		case strings.Contains(req.URL, "rpcids=hNvQHb"):
			historyCalls++
			return protocolBody(string(history))
		case req.Op == "download":
			downloadCalls++
			return protocolBody("old video")
		default:
			t.Errorf("意外请求: %s", req.URL)
			return []browserEvent{{Type: "error", Code: "unexpected_request"}}
		}
	})
	mc := Models["gemini-video"]
	mc.PinAccountID, mc.Context = id, context.Background()
	_, _, result, err := callGemini("test", "test", mc, nil, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), errVideoRefused) {
		t.Fatalf("本轮拒绝未保留: result=%+v err=%v", result, err)
	}
	if generateCalls != 1 || historyCalls != 1 || downloadCalls != 0 {
		t.Fatalf("重复生成或误取旧轮产物: generate=%d history=%d download=%d", generateCalls, historyCalls, downloadCalls)
	}
}
