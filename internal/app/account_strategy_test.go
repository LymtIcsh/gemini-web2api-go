package app

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestPickCookieStickyStrategy 验证单号粘性策略：
// 优先使用当前健康账号，额度未用完且未异常时连续请求不换号；当账号异常或限流时自动切换到下一个健康账号。
func TestPickCookieStickyStrategy(t *testing.T) {
	origStrategy := rtCfg().AccountStrategy
	rtMu.Lock()
	rtVal.AccountStrategy = "sticky"
	rtMu.Unlock()
	defer func() {
		rtMu.Lock()
		rtVal.AccountStrategy = origStrategy
		rtMu.Unlock()
	}()

	id1, err := accountAdd("sticky-1", "SAPISID=s1; SID=x1", "")
	if err != nil {
		t.Fatalf("添加账号 1 失败: %v", err)
	}
	t.Cleanup(func() { _ = accountDelete(id1) })

	id2, err := accountAdd("sticky-2", "SAPISID=s2; SID=x2", "")
	if err != nil {
		t.Fatalf("添加账号 2 失败: %v", err)
	}
	t.Cleanup(func() { _ = accountDelete(id2) })

	// 重置活跃账号指针
	pickMu.Lock()
	activeAccountID = 0
	pickMu.Unlock()

	// 第一次挑选
	a1, ok := pickCookieAccount()
	if !ok || a1 == nil {
		t.Fatal("未能挑到账号")
	}
	firstID := a1.ID

	// 连续 5 次挑选，在粘性模式下必须始终是同一个账号
	for i := 0; i < 5; i++ {
		a, ok := pickCookieAccount()
		if !ok || a.ID != firstID {
			t.Fatalf("第 %d 次挑选未保持粘性: got %v, want %d", i+1, a.ID, firstID)
		}
	}

	// 模拟该账号遇到异常（例如 401 鉴权失败）
	markAccountResult(firstID, false, "upstream 401 unauthorized")

	// 再次挑选，此时应当自动切换到另一个可用账号
	a2, ok := pickCookieAccount()
	if !ok || a2 == nil || a2.ID == firstID {
		t.Fatalf("账号失败后未切换新账号: got %v, want another", a2)
	}
	secondID := a2.ID

	// 新账号应当继续保持粘性
	for i := 0; i < 3; i++ {
		a, ok := pickCookieAccount()
		if !ok || a.ID != secondID {
			t.Fatalf("切换后未在新账号保持粘性: got %v, want %d", a.ID, secondID)
		}
	}

	// 模拟新账号额度耗尽（限流）
	qKey := quotaKey(secondID)
	_ = kvSet(qKey, `{"five_blocked":true}`)
	t.Cleanup(func() { _ = kvSet(qKey, "") })

	// 修复账号 1
	markAccountResult(firstID, true, "")

	// 挑选时应跳过限额的 secondID，切回已恢复的 firstID
	a3, ok := pickCookieAccount()
	if !ok || a3 == nil || a3.ID != firstID {
		t.Fatalf("限流后未切换至可用账号: got %v, want %d", a3, firstID)
	}
}

// TestConversationFailoverOnQuotaOrError 验证多轮会话在遇到额度用尽或异常时的无感切换（Failover）：
// 会话一开始绑定账号 A，第二轮请求时账号 A 发生错误或限流，系统无感切换到账号 B 并全量重建会话。
func TestConversationFailoverOnQuotaOrError(t *testing.T) {
	origStrategy := rtCfg().AccountStrategy
	origMulti := rtCfg().MultiTurn
	rtMu.Lock()
	rtVal.AccountStrategy = "sticky"
	rtVal.MultiTurn = true
	rtMu.Unlock()
	defer func() {
		rtMu.Lock()
		rtVal.AccountStrategy = origStrategy
		rtVal.MultiTurn = origMulti
		rtMu.Unlock()
	}()

	quotaGenerationConfig(t)
	acctA, acctB := readyQuotaAccount(t), readyQuotaAccount(t)

	// mock 上游响应：如果请求来自 acctA 则返回额度耗尽，如果来自 acctB 则成功返回
	withBrowserProtocol(t, func(req browserRequest) []browserEvent {
		if req.Op == "quota" {
			p := 0
			if req.AccountID == acctA {
				p = 100 // 账号 A 限流 100%
			}
			return quotaProtocol(req, fmt.Sprintf(`{"used_percent":%d,"checked_at":%d}`, p, time.Now().Unix()))
		}
		if req.URL == "https://gemini.google.com/app" {
			return protocolBody(`{"SNlM0e":"signed-in-token:12345"}`)
		}
		return protocolBody(okRaw)
	})

	// 模拟已存在一路绑定了 acctA 的会话
	msgs := []map[string]interface{}{
		{"role": "user", "content": "turn 1 user message"},
		{"role": "assistant", "content": "turn 1 assistant reply"},
		{"role": "user", "content": "turn 2 user message"},
	}

	parentKey := convParentKey(msgs)
	// 在 convStore 中放入绑定 acctA 的会话
	convPut(parentKey, &convState{
		accountID: acctA,
		isLogin:   true,
		turn:      1,
		cid:       "c_dummy",
		rid:       "r_dummy",
		rcid:      "rc_dummy",
	})

	mc := Models["gemini-3.6-flash"]
	mc.PinAccountID = 0 // 未固定账号，允许故障转移

	var emitted string
	onDelta := func(s string) { emitted += s }

	text, _, res, err := callGeminiConv(msgs, mc, nil, nil, onDelta, nil)
	if err != nil {
		t.Fatalf("callGeminiConv 故障转移失败: %v", err)
	}

	if res.AccountID != acctB {
		t.Fatalf("会话未正确切换到健康账号 acctB (#%d): 实际为 #%d", acctB, res.AccountID)
	}

	if !strings.Contains(text, "banana") && !strings.Contains(emitted, "banana") {
		t.Fatalf("未获得正确回复内容: text=%q, emitted=%q", text, emitted)
	}

	// 验证旧会话已从 store 中清除，且新会话已经以 acctB 存入
	if oldConv := convGet(parentKey); oldConv != nil && oldConv.accountID == acctA {
		t.Errorf("旧会话未被清理: %+v", oldConv)
	}

	childKey := convChildKey(msgs, text)
	newConv := convGet(childKey)
	if newConv == nil || newConv.accountID != acctB {
		t.Fatalf("新会话未正确存入或绑定的不是 acctB: %+v", newConv)
	}
}
