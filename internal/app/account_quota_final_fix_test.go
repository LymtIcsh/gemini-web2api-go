package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 故障目标：额度查询期间人工禁用或登录失效后，固定号返回 typed-nil 并在 HTTP 路径 panic。
func TestQuotaPinnedHTTPAccountBecomesUnavailableDuringQuery(t *testing.T) {
	for _, endpoint := range []string{"chat", "responses"} {
		for _, state := range []string{"disabled", "needs_login"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", endpoint, state, stream), func(t *testing.T) {
					quotaGenerationConfig(t)
					id, other := readyQuotaAccount(t), readyQuotaAccount(t)
					if err := setSelectedAccount(id); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = setSelectedAccount(0) })
					requests, send := controlledQuotaProtocol(t)
					w := httptest.NewRecorder()
					body := fmt.Sprintf(`{"model":"gemini-3.6-flash","messages":[{"role":"user","content":"quota concurrency"}],"input":"quota concurrency","stream":%t}`, stream)
					r := httptest.NewRequest("POST", "/v1/"+endpoint, strings.NewReader(body))
					done := make(chan any, 1)
					go func() {
						defer func() { done <- recover() }()
						if endpoint == "chat" {
							handleChatCompletions(w, r)
						} else {
							handleResponses(w, r)
						}
					}()
					req := nextQuotaRequest(t, requests)
					if req.Op != "quota" || req.AccountID != id {
						t.Fatalf("固定请求未查询原账号额度: %+v", req)
					}
					if state == "disabled" {
						if err := accountSetStatus(id, state); err != nil {
							t.Fatal(err)
						}
					} else {
						markBrowserLoginRequired(id, "测试并发登录失效")
					}
					send(req, quotaProtocol(req, fmt.Sprintf(`{"used_percent":100,"weekly_used_percent":0,"checked_at":%d}`, time.Now().Unix()))[0])
					select {
					case panicValue := <-done:
						if panicValue != nil {
							t.Fatalf("固定账号额度并发状态变更导致 handler panic: %v", panicValue)
						}
					case unexpected := <-requests:
						t.Fatalf("不可用或阻塞账号仍生成/跨号: %+v", unexpected)
					case <-time.After(2 * time.Second):
						t.Fatal("固定账号错误响应超时")
					}
					if endpoint == "responses" && stream {
						if w.Code != 200 || !strings.Contains(w.Body.String(), "event: response.failed") || !strings.Contains(w.Body.String(), `"type":"upstream_error"`) || !strings.Contains(w.Body.String(), fmt.Sprintf("#%d", id)) || strings.Contains(w.Body.String(), "account_quota_exhausted") {
							t.Fatalf("已开流未发明确普通错误事件: %d %s", w.Code, w.Body.String())
						}
					} else {
						var response struct {
							Error map[string]string `json:"error"`
						}
						if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
							t.Fatalf("没有明确错误 JSON: %d %s: %v", w.Code, w.Body.String(), err)
						}
						if w.Code != 502 || response.Error["message"] == "" || !strings.Contains(response.Error["message"], fmt.Sprintf("#%d", id)) || strings.Contains(w.Body.String(), "account_quota_exhausted") {
							t.Fatalf("人工不可用状态未明确归属为普通错误: %d %s", w.Code, w.Body.String())
						}
					}
					if w.Header().Get("Retry-After") != "" {
						t.Fatal("人工不可用状态生成了虚假额度重试截止")
					}
					if selectedAccountID() != id || accountReady(accountByID(id)) || !accountReady(accountByID(other)) || quotaEligible(id) {
						t.Fatal("固定选择、人工不可用状态或已确认额度阻塞被改写")
					}
					if state == "disabled" && accountByID(id).Status != "disabled" {
						t.Fatal("额度查询重新启用了人工禁用账号")
					}
					var status int
					var detail string
					waitQuotaCondition(t, func() bool {
						return getDB().QueryRow(`SELECT status,error FROM requests WHERE account_id=? ORDER BY id DESC LIMIT 1`, id).Scan(&status, &detail) == nil
					})
					if status != 502 || !strings.Contains(detail, fmt.Sprintf("#%d", id)) {
						t.Fatalf("统计丢失真实失败类别和账号归属: %d %s", status, detail)
					}
				})
			}
		}
	}
}

// 故障目标：错误接口内的 nil 指针被 errors.As 匹配后解引用，或被误报为额度 429。
func TestQuotaTypedNilDoesNotBecomeQuotaHTTPError(t *testing.T) {
	var absent *AccountQuotaError
	for _, err := range []error{absent, fmt.Errorf("wrapped unavailable: %w", absent)} {
		t.Run(fmt.Sprintf("%T", err), func(t *testing.T) {
			defer func() {
				if v := recover(); v != nil {
					t.Fatalf("nil 额度错误导致 HTTP helper panic: %v", v)
				}
			}()
			w := httptest.NewRecorder()
			if quotaHTTPError(w, err) || generationErrorStatus(err) != 502 || generationErrorBody(err)["type"] != "upstream_error" || strings.Contains(generationErrorBody(err)["message"], "account_quota_exhausted") {
				t.Fatalf("nil 额度错误被错误分类: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

// 边界保护：提交前新限额与人工状态变更必须阻止生成，旧会话不能切换到正常号。
func TestQuotaPinnedLateAdmissionUnavailableKeepsOriginalAccount(t *testing.T) {
	quotaGenerationConfig(t)
	id, other := readyQuotaAccount(t), readyQuotaAccount(t)
	withBrowserProtocol(t, func(req browserRequest) []browserEvent {
		if req.AccountID != id {
			t.Errorf("固定请求跨号: %d，正常号 %d", req.AccountID, other)
		}
		if req.Op == "quota" {
			return protocolAvailableQuota(t, req)
		}
		if req.URL == "https://gemini.google.com/app" {
			b, _ := browserAccountByID(id)
			p := float64(100)
			if err := persistQuota(id, *b, nil, quotaObservation{UsedPercent: &p, CheckedAt: time.Now().Unix()}, time.Now().UnixNano()); err != nil {
				t.Error(err)
			}
			if err := accountSetStatus(id, "disabled"); err != nil {
				t.Error(err)
			}
			return protocolBody(`{"SNlM0e":"signed-in-token:12345"}`)
		}
		t.Errorf("阻塞或禁用后仍提交生成: %+v", req)
		return protocolBody(okRaw)
	})
	mc := Models["gemini-3.6-flash"]
	mc.PinAccountID = id
	for _, existing := range []bool{false, true} {
		var res *StreamResult
		var err error
		if existing {
			res, err = streamGenerateConv("continue", mc, &convState{accountID: id, isLogin: true, turn: 1}, nil, nil)
		} else {
			res, err = streamGenerate("quota", "quota", mc, nil, nil)
		}
		var q *AccountQuotaError
		if err == nil || errors.As(err, &q) || res == nil || res.AccountID != id {
			t.Fatalf("不可用固定账号没有有效普通错误与原归属 existing=%t: %+v %v", existing, res, err)
		}
	}
}
