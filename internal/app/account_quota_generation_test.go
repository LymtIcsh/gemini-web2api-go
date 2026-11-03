package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

// 超长正文转成附件后再换号，必须为新账号重传原文并仅引用新账号的附件。
func TestQuotaLateSwitchReuploadsCompleteLongContext(t *testing.T) {
	quotaGenerationConfig(t)
	first, normal := readyQuotaAccount(t), readyQuotaAccount(t)
	rtMu.Lock()
	rtVal.MaxPromptBytes = 512
	rtMu.Unlock()
	original := strings.Repeat("HISTORY_MARKER_7f03: original conversation facts\n", 80) + "Latest user: summarize these facts"
	uploaded := map[int64]string{}
	generated := 0
	withBrowserProtocol(t, func(req browserRequest) []browserEvent {
		if req.Op == "quota" {
			return protocolAvailableQuota(t, req)
		}
		if req.URL == "https://gemini.google.com/app" {
			return protocolBody(`{"SNlM0e":"signed-in-token:12345","qKIAYe":"push-1234"}`)
		}
		if req.URL == uploadHost {
			body, _ := base64.StdEncoding.DecodeString(req.BodyBase64)
			if string(body) != "File name: message.txt" {
				t.Errorf("unexpected context upload name: %s", body)
			}
			events := protocolBody("")
			events[0].Headers = map[string]string{"x-goog-upload-url": "https://push.clients6.google.com/context/final"}
			return events
		}
		if req.URL == "https://push.clients6.google.com/context/final" {
			body, _ := base64.StdEncoding.DecodeString(req.BodyBase64)
			uploaded[req.AccountID] = string(body)
			if req.AccountID == first {
				b, _ := browserAccountByID(first)
				p := float64(100)
				if err := persistQuota(first, *b, nil, quotaObservation{UsedPercent: &p, CheckedAt: time.Now().Unix()}, time.Now().UnixNano()); err != nil {
					t.Error(err)
				}
				return protocolBody("/contrib_service/ttl_1d/first-message")
			}
			return protocolBody("/contrib_service/ttl_1d/normal-message")
		}
		if req.AccountID != normal || !strings.Contains(req.URL, "StreamGenerate") {
			t.Errorf("unexpected submitted generation: %+v", req)
		}
		generated++
		body, _ := base64.StdEncoding.DecodeString(req.BodyBase64)
		form, _ := url.ParseQuery(string(body))
		var outer []any
		if err := json.Unmarshal([]byte(form.Get("f.req")), &outer); err != nil {
			t.Error(err)
			return protocolBody(okRaw)
		}
		var inner []any
		if err := json.Unmarshal([]byte(outer[1].(string)), &inner); err != nil {
			t.Error(err)
			return protocolBody(okRaw)
		}
		input := inner[0].([]any)
		refs, ok := input[3].([]any)
		if !ok || len(refs) != 1 {
			t.Errorf("new account generation lost context attachment: %#v", input[3])
		} else {
			file := refs[0].([]any)
			if file[0].([]any)[0] != "/contrib_service/ttl_1d/normal-message" || file[1] != "message.txt" {
				t.Errorf("new account reused wrong context reference: %#v", file)
			}
		}
		return protocolBody(okRaw)
	})
	res, err := streamGenerate(original, "summarize these facts", Models["gemini-3.6-flash"], nil, nil)
	if err != nil || res.AccountID != normal || generated != 1 {
		t.Fatalf("late long-context switch failed: %+v %v generated=%d", res, err, generated)
	}
	if uploaded[first] != original || uploaded[normal] != original {
		t.Fatalf("complete original context not reuploaded: first=%d normal=%d expected=%d", len(uploaded[first]), len(uploaded[normal]), len(original))
	}
}

// 连续四次提交前出现限额仍须找到第五号，候选各试一次且不耗鉴权预算。
func TestQuotaFourLateAdmissionLimitsKeepAuthBudget(t *testing.T) {
	quotaGenerationConfig(t)
	ids := []int64{readyQuotaAccount(t), readyQuotaAccount(t), readyQuotaAccount(t), readyQuotaAccount(t), readyQuotaAccount(t)}
	quotaCalls := map[int64]int{}
	withBrowserProtocol(t, func(req browserRequest) []browserEvent {
		if req.Op == "quota" {
			quotaCalls[req.AccountID]++
			return protocolAvailableQuota(t, req)
		}
		if req.URL == "https://gemini.google.com/app" {
			if req.AccountID != ids[4] {
				b, _ := browserAccountByID(req.AccountID)
				p := float64(100)
				if err := persistQuota(req.AccountID, *b, nil, quotaObservation{UsedPercent: &p, CheckedAt: time.Now().Unix()}, time.Now().UnixNano()); err != nil {
					t.Error(err)
				}
			}
			return protocolBody(`{"SNlM0e":"signed-in-token:12345"}`)
		}
		if req.AccountID != ids[4] || !strings.Contains(req.URL, "StreamGenerate") {
			t.Errorf("late limited account submitted: %+v", req)
		}
		return protocolBody(okRaw)
	})
	res, err := streamGenerate("quota", "quota", Models["gemini-3.6-flash"], nil, nil)
	if err != nil || res.AccountID != ids[4] {
		t.Fatalf("late quota candidates exhausted auth budget: %+v %v", res, err)
	}
	for _, id := range ids {
		if quotaCalls[id] != 1 || accountByID(id).FailCount != 0 {
			t.Fatalf("late quota repeated candidate or changed auth health: %d %+v", id, quotaCalls)
		}
	}
}

// 普通 HTTP 429 和生成 IP slot 满不能变成账号额度证据。
func TestQuotaOrdinary429AndIPCapacityKeepQuotaEligibility(t *testing.T) {
	quotaGenerationConfig(t)
	id := readyQuotaAccount(t)
	withBrowserProtocol(t, func(req browserRequest) []browserEvent {
		if req.Op == "quota" {
			return protocolAvailableQuota(t, req)
		}
		if req.URL == "https://gemini.google.com/app" {
			return protocolBody(`{"SNlM0e":"signed-in-token:12345"}`)
		}
		events := protocolBody("upstream temporarily busy")
		events[0].Status = 429
		return events
	})
	mc := Models["gemini-3.6-flash"]
	mc.PinAccountID, mc.Context = id, context.Background()
	_, err := streamGenerate("quota", "quota", mc, nil, nil)
	var qe *AccountQuotaError
	if err == nil || errors.As(err, &qe) || !quotaEligible(id) || accountByID(id).FailCount != 0 {
		t.Fatalf("ordinary 429 poisoned quota/auth: %v %+v", err, quotaView(t, id))
	}
	slot, ok, slotErr := acquireSlot(0)
	if !ok {
		t.Fatal(slotErr)
	}
	defer releaseSlot(slot.ID)
	_, err = streamGenerate("quota", "quota", mc, nil, nil)
	var capacity *RateLimitError
	if !errors.As(err, &capacity) || errors.As(err, &qe) || !quotaEligible(id) || accountByID(id).FailCount != 0 {
		t.Fatalf("IP capacity poisoned quota/auth: %v", err)
	}
}

// 生成提交后再出现限额不得向第二号重放请求。
func TestQuotaSubmittedGenerationNeverReplaysAcrossAccounts(t *testing.T) {
	quotaGenerationConfig(t)
	first, normal := readyQuotaAccount(t), readyQuotaAccount(t)
	rtMu.Lock()
	rtVal.RetryAttempts = 2
	rtVal.RetryDelaySec = 0
	rtMu.Unlock()
	generated := 0
	withBrowserProtocol(t, func(req browserRequest) []browserEvent {
		if req.AccountID != first {
			t.Errorf("submitted generation switched to account %d instead of %d", req.AccountID, first)
		}
		if req.Op == "quota" {
			return protocolAvailableQuota(t, req)
		}
		if req.URL == "https://gemini.google.com/app" {
			return protocolBody(`{"SNlM0e":"signed-in-token:12345"}`)
		}
		generated++
		b, _ := browserAccountByID(first)
		p := float64(100)
		if err := persistQuota(first, *b, nil, quotaObservation{UsedPercent: &p, CheckedAt: time.Now().Unix()}, time.Now().UnixNano()); err != nil {
			t.Error(err)
		}
		return []browserEvent{{Type: "error", Code: "browser_error", Error: "temporary network failure"}}
	})
	res, err := streamGenerate("quota", "quota", Models["gemini-3.6-flash"], nil, nil)
	var qe *AccountQuotaError
	if !errors.As(err, &qe) || res.AccountID != first || generated != 1 || !quotaEligible(normal) {
		t.Fatalf("submitted generation replayed: %+v %v calls=%d", res, err, generated)
	}
}

// 视频共用准入，异步任务失败仍记录语义 429；已经存在的任务读取不受阻塞。
func TestQuotaVideoGenerationReportsSemanticFailure(t *testing.T) {
	quotaGenerationConfig(t)
	id := readyQuotaAccount(t)
	withBrowserProtocol(t, func(req browserRequest) []browserEvent {
		if req.Op != "quota" {
			t.Errorf("limited video submitted request: %+v", req)
		}
		return quotaProtocol(req, fmt.Sprintf(`{"used_percent":100,"checked_at":%d}`, time.Now().Unix()))
	})
	j := &videoJob{ID: "quota_video", Model: "gemini-video", Prompt: "quota video", Size: "1280x720"}
	runVideoJobWith(j, nil, id, nil)
	if j.Status != "failed" || !strings.Contains(j.Error, "account_quota_exhausted") || j.accountID != id {
		t.Fatalf("video quota failure lost: %+v", j)
	}
	var status int
	var detail string
	waitQuotaCondition(t, func() bool {
		return getDB().QueryRow(`SELECT status,error FROM requests WHERE endpoint='videos' AND account_id=? ORDER BY id DESC LIMIT 1`, id).Scan(&status, &detail) == nil
	})
	if status != 429 || classifyError(detail) != "account_quota_exhausted" {
		t.Fatalf("video quota incorrectly recorded: %d %s", status, detail)
	}
}
