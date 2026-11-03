package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func readyQuotaAccount(t *testing.T) int64 {
	t.Helper()
	id := newBrowserTestAccount(t)
	b, _ := browserAccountByID(id)
	if err := storeBrowserSession(id, browserSession{ProfileID: b.ProfileID, Email: fmt.Sprintf("quota%d@example.com", id), Cookie: "SAPISID=quota", LoginState: "ready"}); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestQuotaTransportFailureCannotRecover(t *testing.T) {
	id := readyQuotaAccount(t)
	observation := `{"used_percent":100,"weekly_used_percent":100,"checked_at":400}`
	failing := false
	withBrowserProtocol(t, func(req browserRequest) []browserEvent {
		events := quotaProtocol(req, observation)
		if failing {
			events[0].Type = "error"
			events[0].Code = "browser_error"
			events[0].Error = "network credentials"
		}
		return events
	})
	quotaAdmin(t, id)
	failing = true
	observation = `{"used_percent":0,"weekly_used_percent":0,"checked_at":401}`
	quotaAdmin(t, id)
	if quotaView(t, id)["state"] != "limited" {
		t.Fatal("failed operation cleared confirmed blocking")
	}
}

// 成功 end 的局部已确认窗口可恢复，未识别截止和另一窗口不得否定网页事实。
func TestQuotaPartialSuccessfulObservationRecoversUnknownDeadline(t *testing.T) {
	id := readyQuotaAccount(t)
	observation := `{"used_percent":100,"weekly_used_percent":100,"checked_at":500}`
	withBrowserProtocol(t, func(req browserRequest) []browserEvent { return quotaProtocol(req, observation) })
	quotaAdmin(t, id)
	observation = `{"used_percent":0,"five_hour_limited":false,"reset_at":null,"weekly_used_percent":null,"weekly_limited":null,"checked_at":501,"error":"reset time missing"}`
	code, _ := quotaAdmin(t, id)
	q := quotaView(t, id)
	if code != 409 || q["limited_by"] != "weekly" || q["used_percent"] != float64(0) || q["reset_at"] != nil {
		t.Fatalf("successful partial five-hour recovery lost: %d %+v", code, q)
	}
	observation = `{"weekly_used_percent":0,"weekly_limited":false,"weekly_reset_at":null,"checked_at":502,"error":"reset time missing"}`
	quotaAdmin(t, id)
	if q = quotaView(t, id); q["state"] == "limited" || q["eligible"] != true {
		t.Fatalf("successful partial weekly recovery lost: %+v", q)
	}
}

func TestQuotaLateAdmissionLimitSwitchesBeforeSubmission(t *testing.T) {
	quotaGenerationConfig(t)
	first, normal := readyQuotaAccount(t), readyQuotaAccount(t)
	withBrowserProtocol(t, func(req browserRequest) []browserEvent {
		if req.Op == "quota" {
			return quotaProtocol(req, fmt.Sprintf(`{"used_percent":0,"weekly_used_percent":0,"checked_at":%d}`, time.Now().Unix()))
		}
		if req.URL == "https://gemini.google.com/app" {
			if req.AccountID == first {
				b, _ := browserAccountByID(first)
				p := float64(100)
				if err := persistQuota(first, *b, nil, quotaObservation{UsedPercent: &p, CheckedAt: time.Now().Unix()}, time.Now().UnixNano()); err != nil {
					t.Error(err)
				}
			}
			return protocolBody(`{"SNlM0e":"signed-in-token:12345"}`)
		}
		if req.AccountID != normal {
			t.Error("submitted exhausted account")
		}
		return protocolBody(okRaw)
	})
	res, err := streamGenerate("quota", "quota", Models["gemini-3.6-flash"], nil, nil)
	if err != nil || res.AccountID != normal {
		t.Fatalf("final admission did not switch: %+v %v", res, err)
	}
}

func TestQuotaPinnedAndExistingConversationStayOnAccount(t *testing.T) {
	quotaGenerationConfig(t)
	limited, normal := readyQuotaAccount(t), readyQuotaAccount(t)
	withBrowserProtocol(t, func(req browserRequest) []browserEvent {
		if req.AccountID == normal {
			t.Error("pinned quota switched account")
		}
		return quotaProtocol(req, fmt.Sprintf(`{"used_percent":100,"checked_at":%d}`, time.Now().Unix()))
	})
	mc := Models["gemini-3.6-flash"]
	mc.PinAccountID = limited
	for _, existing := range []bool{false, true} {
		var res *StreamResult
		var err error
		if existing {
			res, err = streamGenerateConv("continue", mc, &convState{accountID: limited, isLogin: true, turn: 1}, nil, nil)
		} else {
			res, err = streamGenerate("quota", "quota", mc, nil, nil)
		}
		var qe *AccountQuotaError
		if !errors.As(err, &qe) || res.AccountID != limited {
			t.Fatalf("pinned quota attribution: %+v %v", res, err)
		}
	}
}

func TestQuotaFreshConversationSkipsExhaustedCandidate(t *testing.T) {
	quotaGenerationConfig(t)
	first, normal := readyQuotaAccount(t), readyQuotaAccount(t)
	withBrowserProtocol(t, func(req browserRequest) []browserEvent {
		if req.Op == "quota" {
			p := 0
			if req.AccountID == first {
				p = 100
			}
			return quotaProtocol(req, fmt.Sprintf(`{"used_percent":%d,"checked_at":%d}`, p, time.Now().Unix()))
		}
		if req.URL == "https://gemini.google.com/app" {
			return protocolBody(`{"SNlM0e":"signed-in-token:12345"}`)
		}
		return protocolBody(okRaw)
	})
	text, _, res, err := callGeminiConv([]map[string]any{{"role": "user", "content": "fresh quota conversation"}}, Models["gemini-3.6-flash"], nil, nil, nil, nil)
	if err != nil || res.AccountID != normal || text != "banana" {
		t.Fatalf("new conversation incorrectly pinned: %+v %s %v", res, text, err)
	}
}

func TestQuotaSemanticErrorClassificationAndStartedChat(t *testing.T) {
	err := &AccountQuotaError{}
	if classifyError(err.Error()) != "account_quota_exhausted" {
		t.Fatal("quota misclassified as network/IP/auth")
	}
	w := httptest.NewRecorder()
	s := newSSEWriter(w, "test", 1, "model")
	s.SendContent("before")
	s.Fail(fmt.Errorf("wrapped: %w", err))
	if !strings.Contains(w.Body.String(), `"type":"account_quota_exhausted"`) {
		t.Fatalf("started SSE missing quota type: %s", w.Body.String())
	}
}

func quotaProtocol(req browserRequest, observation string) []browserEvent {
	var e browserEvent
	raw := fmt.Sprintf(`{"type":"end","session":{"profile_id":%q,"email":%q,"cookie":"SAPISID=quota","login_state":"ready"},"quota":%s}`, req.ProfileID, req.ExpectedEmail, observation)
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		panic(err)
	}
	return []browserEvent{e}
}

func quotaAdmin(t *testing.T, id int64) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	handleAdminAccountItem(w, httptest.NewRequest(http.MethodPost, fmt.Sprintf("/admin/api/accounts/%d/quota", id), nil))
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return w.Code, result
}

func quotaView(t *testing.T, id int64) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(accountAdminView(*accountByID(id)))
	var view map[string]any
	_ = json.Unmarshal(raw, &view)
	q, ok := view["quota"].(map[string]any)
	if !ok {
		t.Fatalf("account quota missing: %s", raw)
	}
	return q
}

// 删除持久化或将零误判限额会改变真正的管理 API 和选号行为。
func TestQuotaAdminPersistsZeroAndUnknown(t *testing.T) {
	id := readyQuotaAccount(t)
	observation := `{"used_percent":0,"five_hour_limited":false,"weekly_used_percent":0,"weekly_limited":false,"checked_at":100,"reset_at":500,"weekly_reset_at":900,"error":""}`
	withBrowserProtocol(t, func(req browserRequest) []browserEvent {
		if req.Op != "quota" {
			t.Errorf("unexpected op %s", req.Op)
		}
		return quotaProtocol(req, observation)
	})
	code, result := quotaAdmin(t, id)
	if code != 200 || result["ok"] != true {
		t.Fatalf("quota endpoint: %d %+v", code, result)
	}
	q := quotaView(t, id)
	if q["state"] != "available" || q["used_percent"] != float64(0) || q["eligible"] != true || !strings.Contains(kvGet(fmt.Sprintf("account_quota:%d", id)), `"used_percent":0`) {
		t.Fatalf("zero observation not persisted: %+v", q)
	}
	observation = `{"used_percent":null,"weekly_used_percent":null,"checked_at":101,"error":"panel missing"}`
	code, result = quotaAdmin(t, id)
	q = quotaView(t, id)
	if code != 409 || result["ok"] != false || q["state"] != "unknown" || q["used_percent"] != nil || q["eligible"] != true {
		t.Fatalf("failed read changed eligibility: %d %+v %+v", code, result, q)
	}
}

// 截止时间只安排复查；每个窗口必须分别得到新可用证据。
func TestQuotaTwoWindowsRecoveryAndStaleObservation(t *testing.T) {
	id := readyQuotaAccount(t)
	observation := `{"used_percent":125,"five_hour_limited":true,"reset_at":1,"weekly_used_percent":100,"weekly_limited":true,"weekly_reset_at":2,"checked_at":200}`
	withBrowserProtocol(t, func(req browserRequest) []browserEvent { return quotaProtocol(req, observation) })
	if code, _ := quotaAdmin(t, id); code != 200 {
		t.Fatalf("quota endpoint %d", code)
	}
	q := quotaView(t, id)
	if q["state"] != "limited" || q["used_percent"] != float64(125) || q["blocked_until"] != float64(2) || q["eligible"] != false {
		t.Fatalf("window limit lost: %+v", q)
	}
	if !accountReady(accountByID(id)) || !hasCookie() {
		t.Fatal("quota changed identity readiness/model availability")
	}
	if a, ok := pickCookieAccount(); ok && a.ID == id {
		t.Fatal("expired limit admitted without evidence")
	}
	observation = `{"used_percent":0,"five_hour_limited":false,"reset_at":9999999999,"checked_at":201}`
	quotaAdmin(t, id)
	q = quotaView(t, id)
	if q["state"] != "limited" || q["limited_by"] != "weekly" {
		t.Fatalf("partial read cleared weekly: %+v", q)
	}
	observation = `{"weekly_used_percent":0,"weekly_limited":false,"weekly_reset_at":2,"checked_at":199}`
	quotaAdmin(t, id)
	if quotaView(t, id)["state"] != "limited" {
		t.Fatal("stale recovery accepted")
	}
	observation = `{"checked_at":202,"error":"read failed"}`
	quotaAdmin(t, id)
	if quotaView(t, id)["state"] != "limited" {
		t.Fatal("failed read cleared limit")
	}
	_ = accountSetStatus(id, "disabled")
	observation = `{"weekly_used_percent":0,"weekly_limited":false,"weekly_reset_at":2,"checked_at":203}`
	quotaAdmin(t, id)
	q = quotaView(t, id)
	if q["state"] != "available" || q["eligible"] != false || accountByID(id).Status != "disabled" {
		t.Fatalf("recovery enabled manual status: %+v", q)
	}
	if err := accountDelete(id); err != nil {
		t.Fatal(err)
	}
	if kvGet(fmt.Sprintf("account_quota:%d", id)) != "" {
		t.Fatal("delete retained quota KV")
	}
}

func quotaGenerationConfig(t *testing.T) {
	t.Helper()
	prev := rtCfg()
	rtMu.Lock()
	rtVal.AnonFirst = false
	rtVal.FallbackAnon = true
	rtVal.RequestTimeout = 3
	rtVal.RetryAttempts = 1
	rtVal.PerIPConcurrent = 1
	rtMu.Unlock()
	t.Cleanup(func() { rtMu.Lock(); rtVal = prev; rtMu.Unlock() })
	_ = setSelectedAccount(0)
}

// 配额跳过不能消耗鉴权重试次数，也不能重复占用同一个 IP slot。
func TestQuotaGenerationSkipsFourLimitedAccounts(t *testing.T) {
	quotaGenerationConfig(t)
	ids := []int64{readyQuotaAccount(t), readyQuotaAccount(t), readyQuotaAccount(t), readyQuotaAccount(t), readyQuotaAccount(t)}
	calls := map[int64]int{}
	withBrowserProtocol(t, func(req browserRequest) []browserEvent {
		if req.Op == "quota" {
			calls[req.AccountID]++
			percent := 0
			if req.AccountID != ids[4] {
				percent = 100
			}
			return quotaProtocol(req, fmt.Sprintf(`{"used_percent":%d,"weekly_used_percent":0,"checked_at":%d}`, percent, time.Now().Unix()))
		}
		if req.AccountID != ids[4] {
			t.Errorf("submitted generation for exhausted account %d", req.AccountID)
		}
		if req.URL == "https://gemini.google.com/app" {
			return protocolBody(`{"SNlM0e":"signed-in-token:12345"}`)
		}
		return protocolBody(okRaw)
	})
	mc := Models["gemini-3.6-flash"]
	mc.Context = context.Background()
	res, err := streamGenerate("quota", "quota", mc, nil, nil)
	if err != nil || res.AccountID != ids[4] {
		t.Fatalf("quota fallback failed: %+v %v", res, err)
	}
	for _, id := range ids {
		if calls[id] != 1 {
			t.Fatalf("account %d checked %d times", id, calls[id])
		}
		if accountByID(id).FailCount != 0 {
			t.Fatal("quota affected auth health")
		}
	}
}

// 全池额度耗尽时两个 HTTP 协议必须返回语义 429，不能 fallback_anon。
func TestQuotaHTTPProtocolsReturn429(t *testing.T) {
	quotaGenerationConfig(t)
	id := readyQuotaAccount(t)
	withBrowserProtocol(t, func(req browserRequest) []browserEvent {
		return quotaProtocol(req, fmt.Sprintf(`{"used_percent":100,"reset_at":%d,"weekly_used_percent":0,"checked_at":%d}`, time.Now().Unix()+3600, time.Now().Unix()))
	})
	for _, endpoint := range []string{"chat", "responses"} {
		for _, stream := range []bool{false, true} {
			body := fmt.Sprintf(`{"model":"gemini-3.6-flash","messages":[{"role":"user","content":"quota"}],"input":"quota","stream":%t}`, stream)
			w := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/v1/"+endpoint, strings.NewReader(body))
			if endpoint == "chat" {
				handleChatCompletions(w, r)
			} else {
				handleResponses(w, r)
			}
			if !strings.Contains(w.Body.String(), "account_quota_exhausted") {
				t.Fatalf("%s stream=%v missing quota type: %d %s", endpoint, stream, w.Code, w.Body.String())
			}
			if !stream && (w.Code != 429 || w.Header().Get("Retry-After") == "") {
				t.Fatalf("%s missing 429/retry: %d %+v", endpoint, w.Code, w.Header())
			}
		}
	}
	_ = id
}

// 账号池轮转选号时必须自动跳过已知额度耗尽（limited）的账号，直接选用有额度的正常账号。
func TestQuotaPoolSkipsPreExhaustedAccounts(t *testing.T) {
	quotaGenerationConfig(t)
	exhausted1 := readyQuotaAccount(t)
	exhausted2 := readyQuotaAccount(t)
	available := readyQuotaAccount(t)

	// 将 exhausted1 和 exhausted2 的 last_used_at 设得更久远，确保按时间排序它们排在前面
	_, _ = getDB().Exec("UPDATE accounts SET last_used_at=100 WHERE id IN (?, ?)", exhausted1, exhausted2)
	_, _ = getDB().Exec("UPDATE accounts SET last_used_at=200 WHERE id=?", available)

	// 标记 exhausted1 和 exhausted2 额度耗尽
	b1, _ := browserAccountByID(exhausted1)
	b2, _ := browserAccountByID(exhausted2)
	p100 := float64(100)
	now := time.Now().Unix()
	if err := persistQuota(exhausted1, *b1, nil, quotaObservation{UsedPercent: &p100, CheckedAt: now}, 1); err != nil {
		t.Fatal(err)
	}
	if err := persistQuota(exhausted2, *b2, nil, quotaObservation{UsedPercent: &p100, CheckedAt: now}, 1); err != nil {
		t.Fatal(err)
	}

	withBrowserProtocol(t, func(req browserRequest) []browserEvent {
		if req.AccountID != available {
			t.Errorf("使用了额度耗尽账号 %d，期望使用正常账号 %d", req.AccountID, available)
		}
		if req.Op == "quota" {
			return protocolAvailableQuota(t, req)
		}
		if req.URL == "https://gemini.google.com/app" {
			return protocolBody(`{"SNlM0e":"signed-in-token:12345"}`)
		}
		return protocolBody(okRaw)
	})

	mc := Models["gemini-3.6-flash"]
	mc.Context = context.Background()
	res, err := streamGenerate("test prompt", "test prompt", mc, nil, nil)
	if err != nil {
		t.Fatalf("streamGenerate failed: %v", err)
	}
	if res.AccountID != available {
		t.Fatalf("expected account %d, got %d", available, res.AccountID)
	}
}
