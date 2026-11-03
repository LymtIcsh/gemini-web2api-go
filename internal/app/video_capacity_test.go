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
	"time"
)

// 保留实际会话的容量失败结构，证明工具标记不能掩盖明确的上游拒绝。
func TestVideoCapacityStopsArtifactPolling(t *testing.T) {
	raw, err := os.ReadFile("testdata/video_capacity_hist.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, history, want string
	}{
		{"real_chinese", string(raw), "系统已达到容量上限"},
		{"english", videoCapacityTestHistory("Video generation is at capacity right now. Please try again later.", "", ""), "at capacity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, transport := videoCapacityTestTransport(t, context.Background())
			historyCalls, downloadCalls := 0, 0
			withBrowserProtocol(t, func(req browserRequest) []browserEvent {
				if req.AccountID != id {
					t.Errorf("容量错误改变了所属账号: %d", req.AccountID)
				}
				if req.Op == "download" {
					downloadCalls++
					return protocolBody("unexpected download")
				}
				historyCalls++
				return protocolBody(tc.history)
			})
			arts, _, err := fetchDownloadArtifacts("c_0a840dff0f2e13b1", "r_b1e9ff3bc71d448b", "SAPISID=owner", "owner", "token", "", "video/mp4", toolVideo, 3, 0, transport)
			if err == nil || !strings.Contains(err.Error(), "video_capacity_exhausted") || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("容量失败未保留上游原因: artifacts=%d err=%v", len(arts), err)
			}
			if historyCalls != 1 || downloadCalls != 0 {
				t.Errorf("明确容量失败后仍请求产物: history=%d download=%d", historyCalls, downloadCalls)
			}
		})
	}
}

// 容量字样只出现在提示词时，正常确认必须继续到本轮可播放的产物。
func TestVideoCapacityInPromptDoesNotStopPendingVideo(t *testing.T) {
	_, transport := videoCapacityTestTransport(t, context.Background())
	historyCalls := 0
	mp4 := append([]byte{0, 0, 0, 32, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, bytes.Repeat([]byte{7}, 40)...)
	withBrowserProtocol(t, func(req browserRequest) []browserEvent {
		if req.Op == "download" {
			events := protocolBody(string(mp4))
			events[0].Headers = map[string]string{"content-type": "video/mp4"}
			return events
		}
		historyCalls++
		link := ""
		if historyCalls > 1 {
			link = "https://contribution.usercontent.google.com/download?c=cmVzcG9uc2VfZGF0YQ"
		}
		return protocolBody(videoCapacityTestHistory("I'm generating your video. This may take a few minutes due to high demand.", "系统已达到容量上限", link))
	})
	arts, _, err := fetchDownloadArtifacts("c_0a840dff0f2e13b1", "r_b1e9ff3bc71d448b", "SAPISID=owner", "owner", "token", "", "video/mp4", toolVideo, 3, 0, transport)
	if err != nil || len(arts) != 1 || !bytes.Equal(arts[0].Data, mp4) || historyCalls != 2 {
		t.Fatalf("正常待完成视频被容量字样误伤: history=%d artifacts=%d err=%v", historyCalls, len(arts), err)
	}
}

// 实际HTTP错误与持久化记录必须包含容量原因，不能声称视频生成成功。
func TestBrowserVideoCapacityReturnsUpstreamReason(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	id, _ := videoCapacityTestTransport(t, ctx)
	previousSelected := selectedAccountID()
	if err := setSelectedAccount(id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = setSelectedAccount(previousSelected) })
	ack, _ := os.ReadFile("testdata/video_ack_raw.txt")
	history, _ := os.ReadFile("testdata/video_capacity_hist.txt")
	generateCalls, historyCalls := 0, 0
	withBrowserProtocol(t, func(req browserRequest) []browserEvent {
		switch {
		case req.Op == "quota":
			return protocolAvailableQuota(t, req)
		case req.URL == "https://gemini.google.com/app":
			return protocolBody(`{"SNlM0e":"signed-in-token:12345"}`)
		case strings.Contains(req.URL, "/StreamGenerate?"):
			generateCalls++
			return protocolBody(string(ack))
		case strings.Contains(req.URL, "rpcids=hNvQHb"):
			historyCalls++
			return protocolBody(string(history))
		default:
			t.Errorf("容量拒绝产生了意外请求: %s", req.URL)
			return protocolBody("")
		}
	})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gemini-video","messages":[{"role":"user","content":"capacity replay"}]}`)).WithContext(ctx)
	handleChatCompletions(w, r)
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != 502 || !strings.Contains(body.Error.Message, "video_capacity_exhausted") || !strings.Contains(body.Error.Message, "系统已达到容量上限") || strings.Contains(body.Error.Message, "succeeded") {
		t.Errorf("HTTP错误未准确说明上游容量拒绝: status=%d message=%q", w.Code, body.Error.Message)
	}
	if generateCalls != 1 || historyCalls != 1 {
		t.Errorf("容量拒绝后重复生成或轮询: generate=%d history=%d", generateCalls, historyCalls)
	}
	if got := classifyError(body.Error.Message); got != "upstream_rejected" {
		t.Errorf("容量拒绝被误分到其他故障: %s", got)
	}
	var status int
	var detail string
	waitQuotaCondition(t, func() bool {
		return getDB().QueryRow(`SELECT status,error FROM requests WHERE endpoint='chat.completions' AND account_id=? ORDER BY id DESC LIMIT 1`, id).Scan(&status, &detail) == nil
	})
	if status != 502 || !strings.Contains(detail, "系统已达到容量上限") || strings.Contains(detail, "succeeded") {
		t.Errorf("请求统计丢失上游容量原因: status=%d error=%s", status, detail)
	}
}

// 用户看到的总用时必须覆盖前置准入、历史获取和成功或失败的下载。
func TestBrowserMediaTotalIncludesArtifactRetrieval(t *testing.T) {
	for _, downloadFails := range []bool{false, true} {
		name := "success"
		if downloadFails {
			name = "download_failure"
		}
		t.Run(name, func(t *testing.T) {
			id, _ := videoCapacityTestTransport(t, context.Background())
			ack, _ := os.ReadFile("testdata/video_ack_raw.txt")
			link := "https://contribution.usercontent.google.com/download?c=cmVzcG9uc2VfZGF0YQ"
			withBrowserProtocol(t, func(req browserRequest) []browserEvent {
				switch {
				case req.Op == "quota":
					time.Sleep(40 * time.Millisecond)
					return protocolAvailableQuota(t, req)
				case req.URL == "https://gemini.google.com/app":
					return protocolBody(`{"SNlM0e":"signed-in-token:12345"}`)
				case strings.Contains(req.URL, "/StreamGenerate?"):
					return protocolBody(string(ack))
				case strings.Contains(req.URL, "rpcids=hNvQHb"):
					time.Sleep(60 * time.Millisecond)
					return protocolBody(videoCapacityTestHistory("Your video is ready.", "", link))
				case req.Op == "download":
					time.Sleep(80 * time.Millisecond)
					events := protocolBody(string([]byte{0, 0, 0, 32, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}))
					events[0].Headers = map[string]string{"content-type": "video/mp4"}
					if downloadFails {
						events[0].Status = 403
					}
					return events
				default:
					t.Errorf("意外请求: %s", req.URL)
					return protocolBody("")
				}
			})
			mc := Models["gemini-video"]
			mc.PinAccountID, mc.Context = id, context.Background()
			_, _, res, err := callGemini("timed video", "timed video", mc, nil, nil, nil, nil)
			if (err != nil) != downloadFails {
				t.Fatalf("下载结果不符: %v", err)
			}
			if res == nil || res.TotalMs < 178 {
				t.Errorf("总用时漏算媒体取回: result=%+v", res)
			}
			if downloadFails && strings.Contains(err.Error(), "succeeded") {
				t.Errorf("下载失败却宣称生成成功: %v", err)
			}
			status, detail := 200, ""
			if err != nil {
				status, detail = generationErrorStatus(err), err.Error()
			}
			recordRequest("videos", "gemini-video", "timed video", "", res, status, detail, false)
			var recordedMs int64
			waitQuotaCondition(t, func() bool {
				return getDB().QueryRow(`SELECT total_ms FROM requests WHERE endpoint='videos' AND account_id=? ORDER BY id DESC LIMIT 1`, id).Scan(&recordedMs) == nil
			})
			if recordedMs < 178 || recordedMs != res.TotalMs {
				t.Errorf("持久化耗时没有包含完整媒体取回: stored=%d result=%d", recordedMs, res.TotalMs)
			}
		})
	}
}

// 只建立测试账号和传输归属，外部Google网络由既有io.Pipe协议夹具回放。
func videoCapacityTestTransport(t *testing.T, ctx context.Context) (int64, *browserTransport) {
	t.Helper()
	id := newBrowserTestAccount(t)
	meta, err := browserAccountByID(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := storeBrowserSession(id, browserSession{ProfileID: meta.ProfileID, Email: "owner@example.com", Cookie: "SAPISID=owner", LoginState: "ready"}); err != nil {
		t.Fatal(err)
	}
	previous := rtCfg()
	rtMu.Lock()
	rtVal.RequestTimeout = 1
	rtVal.RetryAttempts = 1
	rtVal.PerIPRPM = 0
	rtVal.PerIPRPH = 0
	rtMu.Unlock()
	t.Cleanup(func() { rtMu.Lock(); rtVal = previous; rtMu.Unlock() })
	return id, &browserTransport{ctx: ctx, accountID: id, profileID: meta.ProfileID, email: "owner@example.com"}
}

// 字面历史结构用于待完成、英文容量回复和完成产物，预期不依赖生产解析器。
func videoCapacityTestHistory(reply, prompt, download string) string {
	turn := []any{[]any{"c_0a840dff0f2e13b1", "r_b1e9ff3bc71d448b"}, nil, []any{[]any{prompt}}, []any{[]any{[]any{"rc_1234567890", []any{reply}}}}, "video_generation"}
	if download != "" {
		turn = append(turn, download)
	}
	inner, _ := json.Marshal([]any{[]any{turn}})
	env, _ := json.Marshal([]any{[]any{"wrb.fr", "hNvQHb", string(inner)}})
	return string(env)
}
