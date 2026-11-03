package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// 真实两轮生成 payload 应沿用上一轮全部续接字段，面板换号不能影响原会话。
func TestBrowserConversationTwoTurnsRetainTokenAndHistory(t *testing.T) {
	id, other := newBrowserTestAccount(t), newBrowserTestAccount(t)
	for _, entry := range []struct {
		id    int64
		email string
	}{{id, "owner@example.com"}, {other, "other@example.com"}} {
		meta, _ := browserAccountByID(entry.id)
		if err := storeBrowserSession(entry.id, browserSession{ProfileID: meta.ProfileID, Email: entry.email, Cookie: "SAPISID=" + entry.email, LoginState: "ready"}); err != nil {
			t.Fatal(err)
		}
	}
	previous := rtCfg()
	rtMu.Lock()
	rtVal.MultiTurn = true
	rtVal.AutoDeleteConversation = true
	rtVal.RetryAttempts = 1
	rtVal.RequestTimeout = 0
	rtVal.AnonFirst = false
	rtMu.Unlock()
	t.Cleanup(func() { rtMu.Lock(); rtVal = previous; rtMu.Unlock(); _ = setSelectedAccount(0) })
	var mu sync.Mutex
	var payloads [][]interface{}
	var deletions int
	deleteCall := make(chan struct{}, 1)
	withBrowserProtocol(t, func(req browserRequest) []browserEvent {
		if req.AccountID != id {
			t.Errorf("面板切换影响原会话账号: %d", req.AccountID)
		}
		if req.Op == "quota" {
			return protocolAvailableQuota(t, req)
		}
		if req.URL == "https://gemini.google.com/app" {
			return protocolBody(`{"SNlM0e":"signed-in-token:12345"}`)
		}
		if !strings.Contains(req.URL, "StreamGenerate") {
			mu.Lock()
			deletions++
			mu.Unlock()
			select {
			case deleteCall <- struct{}{}:
			default:
			}
			return protocolBody("")
		}
		body, _ := base64.StdEncoding.DecodeString(req.BodyBase64)
		form, _ := url.ParseQuery(string(body))
		var outer []interface{}
		if err := json.Unmarshal([]byte(form.Get("f.req")), &outer); err != nil {
			t.Error(err)
			return nil
		}
		var inner []interface{}
		if err := json.Unmarshal([]byte(outer[1].(string)), &inner); err != nil {
			t.Error(err)
			return nil
		}
		mu.Lock()
		payloads = append(payloads, inner)
		turn := len(payloads)
		mu.Unlock()
		var rows []interface{}
		line := strings.Split(okRaw, "\n")[3]
		_ = json.Unmarshal([]byte(line), &rows)
		row := rows[0].([]interface{})
		var frame []interface{}
		_ = json.Unmarshal([]byte(row[2].(string)), &frame)
		frame[4].([]interface{})[0].([]interface{})[0] = "rc_abcdef"
		if turn == 1 {
			frame[26] = "opaque-token-one"
		} else {
			frame[1] = []interface{}{"c_x", "r_second"}
			frame[26] = "opaque-token-two"
		}
		encoded, _ := json.Marshal(frame)
		row[2] = string(encoded)
		encoded, _ = json.Marshal(rows)
		return protocolBody(string(encoded) + "\n")
	})
	conv := &convState{accountID: id, isLogin: true}
	mc := Models["gemini-3.6-flash"]
	mc.Context = context.Background()
	if _, err := streamGenerateConv("first", mc, conv, nil, nil); err != nil {
		t.Fatal(err)
	}
	if conv.tok26 != "opaque-token-one" {
		t.Fatalf("第一轮没有保存 token: %q", conv.tok26)
	}
	_ = setSelectedAccount(other)
	if _, err := streamGenerateConv("second", mc, conv, nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-deleteCall:
		t.Fatal("多轮会话发送了删除 RPC")
	case <-time.After(100 * time.Millisecond):
	}
	mu.Lock()
	defer mu.Unlock()
	if len(payloads) != 2 {
		t.Fatalf("生成次数: %d", len(payloads))
	}
	first := payloads[0][2].([]interface{})
	next := payloads[1][2].([]interface{})
	if first[0] != "" || first[9] != "" || next[0] != "c_x" || next[1] != "r_y" || next[2] != "rc_abcdef" || next[9] != "opaque-token-one" {
		t.Fatalf("续接字段丢失: %#v", next)
	}
	if payloads[1][17].([]interface{})[0].([]interface{})[0] != float64(1) || conv.turn != 2 || conv.tok26 != "opaque-token-two" {
		t.Fatalf("轮次/token未更新: %#v %+v", payloads[1][17], conv)
	}
	if deletions != 0 {
		t.Fatalf("多轮会话发送了删除 RPC: %d", deletions)
	}
}

// 面板固定账号失效时，模型执行预检仍允许原有已登录任务固定其账号；新任务继续拒绝失效选号。
func TestBrowserPinnedModelSurvivesInvalidSelectedAccount(t *testing.T) {
	id, other := newBrowserTestAccount(t), newBrowserTestAccount(t)
	meta, _ := browserAccountByID(id)
	_ = storeBrowserSession(id, browserSession{ProfileID: meta.ProfileID, Email: "owner@example.com", Cookie: "SAPISID=owner", LoginState: "ready"})
	previous := rtCfg()
	rtMu.Lock()
	rtVal.RetryAttempts = 1
	rtVal.RequestTimeout = 0
	rtVal.AnonFirst = false
	rtMu.Unlock()
	t.Cleanup(func() { rtMu.Lock(); rtVal = previous; rtMu.Unlock(); _ = setSelectedAccount(0) })
	withBrowserProtocol(t, func(req browserRequest) []browserEvent {
		if req.AccountID != id {
			t.Errorf("固定任务错误切号: %d", req.AccountID)
		}
		if req.Op == "quota" {
			return protocolAvailableQuota(t, req)
		}
		if req.URL == "https://gemini.google.com/app" {
			return protocolBody(`{"SNlM0e":"signed-in-token:12345"}`)
		}
		if req.Op == "download" {
			events := protocolBody("local-video-bytes")
			events[0].Headers = map[string]string{"content-type": "video/mp4"}
			return events
		}
		if strings.Contains(req.URL, "hNvQHb") {
			downloadURL := "https://contribution.usercontent.google.com/download?c=" + base64.RawURLEncoding.EncodeToString([]byte("response_data"))
			turn := []interface{}{[]interface{}{"c_x", "r_y"}, nil, []interface{}{[]interface{}{"video_generation"}}, []interface{}{[]interface{}{"rc_abcdef", []interface{}{downloadURL}}}}
			inner, _ := json.Marshal([]interface{}{[]interface{}{turn}})
			frame, _ := json.Marshal([]interface{}{[]interface{}{"wrb.fr", "hNvQHb", string(inner)}})
			return protocolBody(string(frame) + "\n")
		}
		return protocolBody(okRaw)
	})
	for _, status := range []string{"needs_login", "disabled"} {
		t.Run(status, func(t *testing.T) {
			otherMeta, _ := browserAccountByID(other)
			_ = storeBrowserSession(other, browserSession{ProfileID: otherMeta.ProfileID, Email: "other@example.com", Cookie: "SAPISID=other", LoginState: "ready"})
			_ = accountSetStatus(other, "enabled")
			_ = setSelectedAccount(other)
			if status == "disabled" {
				_ = accountSetStatus(other, "disabled")
			} else {
				markBrowserLoginRequired(other, "expired")
			}
			for _, name := range []string{"gemini-3.1-pro", "gemini-video"} {
				_, mc, err := resolveModel(name)
				if err != nil {
					t.Errorf("原任务模型预检被选号挡住: %s %v", name, err)
					continue
				}
				mc.PinAccountID = id
				if name == "gemini-3.1-pro" {
					conv := &convState{accountID: id, isLogin: true}
					res, err := streamGenerateConv("continue", mc, conv, nil, nil)
					if err != nil || res.AccountID != id {
						t.Errorf("旧登录会话失败: %+v %v", res, err)
					}
				} else {
					mc.Context = context.Background()
					mc.Cont = &ConvRef{CID: "c_x", RID: "r_y", RCID: "rc_abcdef", Turn: 1}
					res, err := streamGenerate("extend", "extend", mc, nil, nil)
					if err != nil || res.AccountID != id || len(res.Artifacts) != 1 || string(res.Artifacts[0].Data) != "local-video-bytes" {
						t.Errorf("视频固定任务丢号或未走本地下载: %+v %v", res, err)
					}
				}
			}
			mc := Models["gemini-3.1-pro"]
			if _, err := streamGenerate("new", "new", mc, nil, nil); err == nil {
				t.Error("新任务未拒绝失效的面板账号")
			}
		})
	}
}
