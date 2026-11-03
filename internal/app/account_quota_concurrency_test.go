package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// 受控真实管道允许测试跨账号交错、迟到响应及针对请求的取消。
func controlledQuotaProtocol(t *testing.T) (<-chan browserRequest, func(browserRequest, browserEvent)) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	rpc := newBrowserRPC(inW, outR, nil)
	browserManager.Lock()
	prev := browserManager.rpc
	browserManager.rpc = rpc
	browserManager.Unlock()
	requests := make(chan browserRequest, 32)
	var writeMu sync.Mutex
	t.Cleanup(func() {
		inR.Close()
		outW.Close()
		rpc.Close()
		waitQuotaCondition(t, func() bool {
			quotaFlights.Lock()
			defer quotaFlights.Unlock()
			return len(quotaFlights.items) == 0
		})
		browserManager.Lock()
		browserManager.rpc = prev
		browserManager.Unlock()
	})
	go func() {
		dec := json.NewDecoder(inR)
		defer close(requests)
		for {
			var req browserRequest
			if dec.Decode(&req) != nil {
				return
			}
			requests <- req
		}
	}()
	return requests, func(req browserRequest, e browserEvent) {
		t.Helper()
		e.ID, e.AccountID = req.ID, req.AccountID
		writeMu.Lock()
		defer writeMu.Unlock()
		if err := json.NewEncoder(outW).Encode(e); err != nil {
			t.Fatal(err)
		}
	}
}

func waitQuotaCondition(t *testing.T, condition func() bool) {
	t.Helper()
	timer, tick := time.NewTimer(2*time.Second), time.NewTicker(time.Millisecond)
	defer timer.Stop()
	defer tick.Stop()
	for !condition() {
		select {
		case <-tick.C:
		case <-timer.C:
			t.Fatal("quota 状态未在两秒内完成")
		}
	}
}

func nextQuotaRequest(t *testing.T, requests <-chan browserRequest) browserRequest {
	t.Helper()
	select {
	case req, ok := <-requests:
		if !ok {
			t.Fatal("quota 管道已关闭")
		}
		return req
	case <-time.After(2 * time.Second):
		t.Fatal("quota 请求未在两秒内发出")
		return browserRequest{}
	}
}

// 删除去重或在一个等待者取消时取消共享查询，会使第二个真实调用失败。
func TestQuotaDeduplicatesAndKeepsRemainingWaiter(t *testing.T) {
	id := readyQuotaAccount(t)
	requests, send := controlledQuotaProtocol(t)
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- refreshAccountQuota(firstCtx, id, nil) }()
	req := nextQuotaRequest(t, requests)
	go func() { second <- refreshAccountQuota(context.Background(), id, nil) }()
	waitQuotaCondition(t, func() bool {
		quotaFlights.Lock()
		defer quotaFlights.Unlock()
		return quotaFlights.items[id] != nil && quotaFlights.items[id].waiters == 2
	})
	cancelFirst()
	if err := awaitBrowserResult(t, first); !errors.Is(err, context.Canceled) {
		t.Fatalf("first waiter: %v", err)
	}
	select {
	case duplicate := <-requests:
		t.Fatalf("shared quota was duplicated or cancelled: %+v", duplicate)
	case <-time.After(50 * time.Millisecond):
	}
	send(req, quotaProtocol(req, `{"used_percent":0,"weekly_used_percent":0,"checked_at":1000}`)[0])
	if err := awaitBrowserResult(t, second); err != nil {
		t.Fatal(err)
	}
	if q := quotaView(t, id); q["state"] != "available" || q["used_percent"] != float64(0) {
		t.Fatalf("shared result not persisted: %+v", q)
	}
}

// 最后等待者离开必须取消原请求，迟到响应不能阻塞或污染其他账号。
func TestQuotaCancellationReleasesOtherAccountAndIgnoresLateResult(t *testing.T) {
	id, other := readyQuotaAccount(t), readyQuotaAccount(t)
	requests, send := controlledQuotaProtocol(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- refreshAccountQuota(ctx, id, nil) }()
	first := nextQuotaRequest(t, requests)
	cancel()
	if err := awaitBrowserResult(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	cancellation := nextQuotaRequest(t, requests)
	if cancellation.Op != "cancel" || cancellation.TargetID != first.ID || cancellation.AccountID != id {
		t.Fatalf("wrong quota cancellation: %+v", cancellation)
	}
	waitQuotaCondition(t, func() bool {
		quotaFlights.Lock()
		defer quotaFlights.Unlock()
		return quotaFlights.items[id] == nil
	})
	send(first, quotaProtocol(first, `{"used_percent":100,"checked_at":1001}`)[0])
	go func() { done <- refreshAccountQuota(context.Background(), other, nil) }()
	next := nextQuotaRequest(t, requests)
	if next.Op != "quota" || next.AccountID != other {
		t.Fatalf("other quota stalled: %+v", next)
	}
	send(next, quotaProtocol(next, `{"used_percent":0,"checked_at":1002}`)[0])
	if err := awaitBrowserResult(t, done); err != nil {
		t.Fatal(err)
	}
	if quotaView(t, id)["state"] != "unknown" || quotaView(t, other)["state"] != "available" {
		t.Fatal("cancelled late result changed account quota")
	}
}

// 查询在删除前发出、删除后完成时，事务必须拒绝重新建立 quota KV。
func TestQuotaDeletedAccountRejectsLateObservation(t *testing.T) {
	id := readyQuotaAccount(t)
	requests, send := controlledQuotaProtocol(t)
	done := make(chan error, 1)
	go func() { done <- refreshAccountQuota(context.Background(), id, nil) }()
	req := nextQuotaRequest(t, requests)
	if err := accountDelete(id); err != nil {
		t.Fatal(err)
	}
	send(req, quotaProtocol(req, `{"used_percent":100,"checked_at":1003}`)[0])
	if err := awaitBrowserResult(t, done); err == nil {
		t.Fatal("deleted account quota accepted")
	}
	if kvGet(quotaKey(id)) != "" || accountByID(id) != nil {
		t.Fatal("late quota recreated deleted state")
	}
}

// 并发保存与删除不论先后，删除结束后均不得残留 quota。
func TestQuotaConcurrentDeleteAndPersistenceRemainAtomic(t *testing.T) {
	for i := 0; i < 12; i++ {
		id := readyQuotaAccount(t)
		b, _ := browserAccountByID(id)
		start := make(chan struct{})
		deleted, persisted := make(chan error, 1), make(chan error, 1)
		go func() { <-start; deleted <- accountDelete(id) }()
		go func() {
			<-start
			p := float64(100)
			persisted <- persistQuota(id, *b, nil, quotaObservation{UsedPercent: &p, CheckedAt: 1004}, 1004)
		}()
		close(start)
		if err := awaitBrowserResult(t, deleted); err != nil {
			t.Fatal(err)
		}
		_ = awaitBrowserResult(t, persisted)
		if kvGet(quotaKey(id)) != "" || accountByID(id) != nil {
			t.Fatalf("concurrent delete leaked quota on iteration %d", i)
		}
	}
}

// 同一秒更早开始的查询不能覆盖更晚接受的限额，错误身份也不能写入。
func TestQuotaObservationOrderAndSessionOwnership(t *testing.T) {
	id := readyQuotaAccount(t)
	b, _ := browserAccountByID(id)
	limited, available := float64(100), float64(0)
	if err := persistQuota(id, *b, nil, quotaObservation{UsedPercent: &limited, CheckedAt: 1005}, 20); err != nil {
		t.Fatal(err)
	}
	if err := persistQuota(id, *b, nil, quotaObservation{UsedPercent: &available, CheckedAt: 1005}, 10); err != nil {
		t.Fatal(err)
	}
	withBrowserProtocol(t, func(req browserRequest) []browserEvent {
		events := quotaProtocol(req, `{"used_percent":0,"checked_at":1006}`)
		events[0].Session.Email = "wrong@example.com"
		return events
	})
	if code, _ := quotaAdmin(t, id); code != 409 {
		t.Fatalf("invalid identity accepted: %d", code)
	}
	if q := quotaView(t, id); q["state"] != "limited" || q["checked_at"] != float64(1005) {
		t.Fatalf("late order or wrong identity cleared limit: %+v", q)
	}
}

// 超过两个账号时后台不得继续提交；生命周期取消必须让扫描和 IPC 都退出。
func TestQuotaSchedulerBoundsWorkAndStopsOnCancellation(t *testing.T) {
	for i := 0; i < 7; i++ {
		readyQuotaAccount(t)
	}
	requests, _ := controlledQuotaProtocol(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { runQuotaScheduler(ctx); done <- nil }()
	first, second := nextQuotaRequest(t, requests), nextQuotaRequest(t, requests)
	if first.Op != "quota" || second.Op != "quota" || first.AccountID == second.AccountID {
		t.Fatalf("scheduler work attribution: %+v %+v", first, second)
	}
	select {
	case overflow := <-requests:
		t.Fatalf("more than two active background quotas: %+v", overflow)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	if err := awaitBrowserResult(t, done); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for len(seen) < 2 {
		req := nextQuotaRequest(t, requests)
		if req.Op != "cancel" || (req.TargetID != first.ID && req.TargetID != second.ID) {
			t.Fatalf("shutdown submitted queued work: %+v", req)
		}
		seen[req.TargetID] = true
	}
}

// 到期失败保存检查时间，紧接的到期扫描不能密集重试；人工刷新仍可立即执行。
func TestQuotaDueSweepThrottlesFailureAndAllowsManualRefresh(t *testing.T) {
	id := readyQuotaAccount(t)
	requests, send := controlledQuotaProtocol(t)
	b, _ := browserAccountByID(id)
	p, deadline := float64(100), time.Now().Unix()-90
	if err := persistQuota(id, *b, nil, quotaObservation{UsedPercent: &p, ResetAt: &deadline, CheckedAt: time.Now().Unix() - 60}, 1); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { quotaSweep(context.Background(), false); done <- nil }()
	req := nextQuotaRequest(t, requests)
	send(req, browserEvent{Type: "error", Code: "browser_error", Error: "credential detail"})
	if err := awaitBrowserResult(t, done); err != nil {
		t.Fatal(err)
	}
	quotaSweep(context.Background(), false)
	select {
	case retry := <-requests:
		t.Fatalf("due failure retried immediately: %+v", retry)
	default:
	}
	go func() { done <- refreshAccountQuota(context.Background(), id, nil) }()
	req = nextQuotaRequest(t, requests)
	send(req, quotaProtocol(req, fmt.Sprintf(`{"used_percent":0,"checked_at":%d}`, time.Now().Unix()))[0])
	if err := awaitBrowserResult(t, done); err != nil || quotaView(t, id)["state"] != "available" {
		t.Fatalf("manual refresh did not recover: %v", err)
	}
}

// 只有明确恢复截止才返回 Retry-After，池错误使用最早已知候选时刻。
func TestQuotaPoolRetryDeadlineUsesEarliestKnownCandidate(t *testing.T) {
	first, second, unknown := readyQuotaAccount(t), readyQuotaAccount(t), readyQuotaAccount(t)
	now := time.Now().Unix()
	for _, tc := range []struct {
		id       int64
		deadline *int64
	}{{first, &[]int64{now + 900}[0]}, {second, &[]int64{now + 300}[0]}, {unknown, nil}} {
		b, _ := browserAccountByID(tc.id)
		p := float64(100)
		if err := persistQuota(tc.id, *b, nil, quotaObservation{UsedPercent: &p, ResetAt: tc.deadline, CheckedAt: now}, now); err != nil {
			t.Fatal(err)
		}
	}
	pool := quotaPoolError(0)
	if pool == nil || pool.BlockedUntil == nil || *pool.BlockedUntil != now+300 {
		t.Fatalf("wrong pool retry: %+v", pool)
	}
	w := httptest.NewRecorder()
	if !quotaHTTPError(w, quotaPoolError(unknown)) || w.Code != 429 || w.Header().Get("Retry-After") != "" {
		t.Fatalf("unknown deadline synthesized retry: %d %+v", w.Code, w.Header())
	}
}

// 独立 SQLite 连接必须读到 nullable 字段及确认阻塞，状态不能只存在进程内存。
func TestQuotaPersistentStateReadableFromSeparateConnection(t *testing.T) {
	id := readyQuotaAccount(t)
	b, _ := browserAccountByID(id)
	p := float64(100)
	if err := persistQuota(id, *b, nil, quotaObservation{UsedPercent: &p, CheckedAt: 1007}, 1007); err != nil {
		t.Fatal(err)
	}
	conn, err := sql.Open("sqlite", cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var raw string
	if err := conn.QueryRow(`SELECT v FROM kv WHERE k=?`, quotaKey(id)).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var q map[string]any
	if err := json.Unmarshal([]byte(raw), &q); err != nil {
		t.Fatal(err)
	}
	if q["state"] != "limited" || q["five_blocked"] != true || q["used_percent"] != float64(100) || q["weekly_used_percent"] != nil || q["blocked_until"] != nil {
		t.Fatalf("persistent nullable state lost: %+v", q)
	}
}

// quota 在 stdin 背压时取消，也必须保留共享工作进程并让下一账号继续查询。
func TestQuotaBlockedStdinCancellationKeepsWorkerUsable(t *testing.T) {
	id, other := readyQuotaAccount(t), readyQuotaAccount(t)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	input := &signalledBrowserInput{WriteCloser: inW, started: make(chan struct{})}
	rpc := newBrowserRPC(input, outR, nil)
	browserManager.Lock()
	prev := browserManager.rpc
	browserManager.rpc = rpc
	browserManager.Unlock()
	t.Cleanup(func() {
		inR.Close()
		outW.Close()
		rpc.Close()
		waitQuotaCondition(t, func() bool {
			quotaFlights.Lock()
			defer quotaFlights.Unlock()
			return len(quotaFlights.items) == 0
		})
		browserManager.Lock()
		browserManager.rpc = prev
		browserManager.Unlock()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- refreshAccountQuota(ctx, id, nil) }()
	select {
	case <-input.started:
	case <-time.After(2 * time.Second):
		t.Fatal("quota stdin 未产生背压")
	}
	cancel()
	if err := awaitBrowserResult(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("blocked quota cancellation: %v", err)
	}
	cancelled := make(chan browserRequest, 1)
	go func() {
		dec, enc := json.NewDecoder(inR), json.NewEncoder(outW)
		for {
			var req browserRequest
			if dec.Decode(&req) != nil {
				return
			}
			if req.Op == "cancel" {
				cancelled <- req
			} else if req.AccountID == other {
				e := quotaProtocol(req, `{"used_percent":0,"checked_at":1008}`)[0]
				e.ID, e.AccountID = req.ID, req.AccountID
				_ = enc.Encode(e)
			}
		}
	}()
	go func() { done <- refreshAccountQuota(context.Background(), other, nil) }()
	if err := awaitBrowserResult(t, done); err != nil || quotaView(t, other)["state"] != "available" {
		t.Fatalf("other account failed after blocked cancellation: %v", err)
	}
	if req := nextQuotaRequest(t, cancelled); req.Op != "cancel" || req.AccountID != id || req.TargetID == "" {
		t.Fatalf("cancelled quota lost attribution: %+v", req)
	}
}
