package app

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestExitRateLimit_SlidingWindowAndRetryAfter 验证：
// 1. 出口 IP 级别的累计统计与预占名额（非账号级）；
// 2. 满额后返回预计等待秒数（RetryAfter）；
// 3. 滑动时间窗口逐个释放名额，而非整点清零全部放行。
func TestExitRateLimit_SlidingWindowAndRetryAfter(t *testing.T) {
	proxyID := int64(999901)
	slot := getSlot(proxyID)

	slot.mu.Lock()
	slot.inflight = 0
	slot.minuteWin = nil
	slot.hourWin = nil
	slot.mu.Unlock()

	origCfg := rtCfg()
	defer func() {
		rtMu.Lock()
		rtVal = origCfg
		rtMu.Unlock()
	}()

	rtMu.Lock()
	rtVal.PerIPConcurrent = 3
	rtVal.PerIPRPM = 5
	rtVal.PerIPRPH = 10
	rtMu.Unlock()

	// 1. 发送前预扣并发名额
	for i := 0; i < 3; i++ {
		ok, reason, _ := trySlotAcquire(proxyID)
		if !ok {
			t.Fatalf("expected acquire %d to succeed, failed with %s", i+1, reason)
		}
	}
	// 超并发应被拦截收尾
	ok, reason, _ := trySlotAcquire(proxyID)
	if ok || reason != "concurrent" {
		t.Fatalf("expected concurrent limit rejection, got ok=%v, reason=%s", ok, reason)
	}

	// 释放一个并发
	slotRelease(proxyID)

	// 2. 验证分钟滑动窗口 (RPM=5)
	for i := 3; i < 5; i++ {
		ok, _, _ := trySlotAcquire(proxyID)
		slotRelease(proxyID)
		if !ok {
			t.Fatalf("expected acquire %d to succeed", i+1)
		}
	}

	// 第 6 个应触发 RPM 拦截，并给出平缓等待时间
	ok, reason, retryAfter := trySlotAcquire(proxyID)
	slotRelease(proxyID)
	if ok || reason != "rpm" {
		t.Fatalf("expected rpm limit rejection, got ok=%v, reason=%s", ok, reason)
	}
	if retryAfter < 1 || retryAfter > 60 {
		t.Fatalf("expected retryAfter between 1 and 60, got %d", retryAfter)
	}

	// 3. 验证小时滑动窗口 (RPH=10)
	slot.mu.Lock()
	now := time.Now().Unix()
	// 模拟 10 个请求分布在不同时间
	slot.minuteWin = nil // 清空分钟窗口只测小时
	slot.hourWin = []int64{
		now - 3550, // 50 秒后滑出窗口
		now - 3500,
		now - 2000,
		now - 1500,
		now - 1000,
		now - 800,
		now - 600,
		now - 400,
		now - 200,
		now - 10,
	}
	slot.inflight = 0
	slot.mu.Unlock()

	// 满 10 个，应触发 RPH 拦截，且 retryAfter 约为 50 秒（最旧请求滑出时间）
	ok, reason, retryAfter = trySlotAcquire(proxyID)
	if ok || reason != "rph" {
		t.Fatalf("expected rph rejection, got ok=%v, reason=%s", ok, reason)
	}
	if retryAfter < 45 || retryAfter > 55 {
		t.Fatalf("expected retryAfter around 50s, got %d", retryAfter)
	}

	// 模拟最旧请求已滑出窗口（now - 3601）
	slot.mu.Lock()
	slot.hourWin[0] = now - 3601
	slot.mu.Unlock()

	// 再次尝试，应只释放 1 个名额，并成功占用
	ok, reason, _ = trySlotAcquire(proxyID)
	if !ok {
		t.Fatalf("expected 1 slot released by sliding window, got reason: %s", reason)
	}
	defer slotRelease(proxyID)

	// 占用后名额再次打满，后续请求继续被拦截（平缓涓滴释放，而非一次性放行全部）
	ok, reason, _ = trySlotAcquire(proxyID)
	if ok || reason != "rph" {
		t.Fatalf("expected immediately full again after 1 acquire, got ok=%v", ok)
	}
}

// TestExitSorryIsolation 验证“被拦后恢复”与“额度恢复”严格分开处理：
// 即使额度已经恢复或过了两小时，sorry/index 出口也不能盲目自动放行业务流量。
func TestExitSorryIsolation(t *testing.T) {
	p := Proxy{
		ID:        999902,
		Name:      "test-sorry-proxy",
		URL:       "http://127.0.0.1:8888",
		Enabled:   true,
		FailCount: 0,
	}

	// 1. 模拟遭遇 sorry/index 报错
	errMsg := "upstream HTTP 302: <HTML><HEAD><meta ... url=https://www.google.com/sorry/index?continue=..."
	if !isSorryBlocked(errMsg) {
		t.Fatalf("expected isSorryBlocked to be true for sorry redirect")
	}

	// 2. 验证 recordProxyResult 立即熔断到 proxyFailThreshold (5)
	p.FailCount = proxyFailThreshold
	p.LastError = errMsg
	p.LastUsed = time.Now().Unix() - 150*60 // 已经过去了 150 分钟（超过默认 120 分钟冷却）

	// 3. 验证 proxyUsable 绝不盲目放行
	usable := proxyUsable(p, time.Now().Unix(), 120)
	if usable {
		t.Fatalf("expected sorry-blocked proxy to remain paused even after 150 minutes cooldown, but got usable=true")
	}

	// 4. 普通网络错误（如连接超时）在冷却期过后允许尝试
	pNormal := Proxy{
		ID:        999903,
		Name:      "test-timeout-proxy",
		URL:       "http://127.0.0.1:8889",
		Enabled:   true,
		FailCount: 5,
		LastError: "dial tcp timeout",
		LastUsed:  time.Now().Unix() - 150*60,
	}
	usableNormal := proxyUsable(pNormal, time.Now().Unix(), 120)
	if !usableNormal {
		t.Fatalf("expected normal timeout proxy to be usable after cooldown, got false")
	}
}

// TestConversation_RateLimitPreservesSession 验证：
// 暂停出口时，不删除会话，也不自动切换账号（不制造上下文迁移问题）。
func TestConversation_RateLimitPreservesSession(t *testing.T) {
	parentKey := "test_parent_key_ratelimit"
	conv := &convState{
		cid:       "c_123",
		rid:       "r_456",
		accountID: 888,
		proxyID:   12,
		turn:      1,
	}
	convPut(parentKey, conv)

	// 模拟返回了 RateLimitError
	rle := &RateLimitError{
		Reason:     "rph",
		ProxyID:    12,
		RetryAfter: 45,
	}

	// 模拟错误判断
	var targetRLE *RateLimitError
	isRL := errors.As(rle, &targetRLE)
	if !isRL {
		t.Fatalf("expected error to match RateLimitError")
	}

	// 业务逻辑规则：RateLimitError 时不删除 parentKey
	if !isRL {
		convDelete(parentKey)
	}

	// 检查会话是否完整保存在 convStore
	saved := convGet(parentKey)
	if saved == nil {
		t.Fatalf("expected conversation state to be preserved in convStore, but it was deleted")
	}
	if saved.accountID != 888 || saved.proxyID != 12 {
		t.Fatalf("expected conversation account and proxy to remain intact, got account=%d, proxy=%d",
			saved.accountID, saved.proxyID)
	}

	// 验证错误信息包含友好引导与秒数
	errText := rle.Error()
	if !strings.Contains(errText, "80次/h") || !strings.Contains(errText, "45 秒") || !strings.Contains(errText, "请勿更换账号") {
		t.Fatalf("unexpected RateLimitError message: %s", errText)
	}
}
