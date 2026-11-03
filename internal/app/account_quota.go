package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 两窗口均记录累计已用百分比；空指针表示网页未确认。
type quotaObservation struct {
	UsageText         string   `json:"usage_text"`
	UsedPercent       *float64 `json:"used_percent"`
	ResetAt           *int64   `json:"reset_at"`
	FiveHourLimited   *bool    `json:"five_hour_limited"`
	WeeklyUsedPercent *float64 `json:"weekly_used_percent"`
	WeeklyResetAt     *int64   `json:"weekly_reset_at"`
	WeeklyLimited     *bool    `json:"weekly_limited"`
	CheckedAt         int64    `json:"checked_at"`
	Error             string   `json:"error"`
}

type accountQuota struct {
	quotaObservation
	State         string `json:"state"`
	LimitedBy     string `json:"limited_by"`
	BlockedUntil  *int64 `json:"blocked_until"`
	Eligible      bool   `json:"eligible"`
	FiveBlocked   bool   `json:"five_blocked"`
	WeekBlocked   bool   `json:"week_blocked"`
	FiveDeadline  *int64 `json:"five_deadline"`
	WeekDeadline  *int64 `json:"week_deadline"`
	AcceptedOrder int64  `json:"accepted_order"`
}

func quotaKey(id int64) string { return "account_quota:" + strconv.FormatInt(id, 10) }

func readAccountQuota(id int64) accountQuota {
	q := accountQuota{State: "unknown"}
	_ = json.Unmarshal([]byte(kvGet(quotaKey(id))), &q)
	q.describe()
	q.Eligible = accountReady(accountByID(id)) && !q.FiveBlocked && !q.WeekBlocked
	return q
}

func (q *accountQuota) describe() {
	q.LimitedBy = ""
	q.BlockedUntil = nil
	if q.FiveBlocked {
		q.LimitedBy = "five_hour"
		q.BlockedUntil = q.FiveDeadline
	}
	if q.WeekBlocked {
		if q.FiveBlocked {
			q.LimitedBy = "both"
		} else {
			q.LimitedBy = "weekly"
		}
		if !q.FiveBlocked {
			q.BlockedUntil = q.WeekDeadline
		} else if q.FiveDeadline == nil || q.WeekDeadline == nil {
			q.BlockedUntil = nil
		} else if *q.WeekDeadline > *q.FiveDeadline {
			q.BlockedUntil = q.WeekDeadline
		}
	}
	if q.FiveBlocked || q.WeekBlocked {
		q.State = "limited"
	} else if q.Error != "" || (q.UsedPercent == nil && q.WeeklyUsedPercent == nil && q.FiveHourLimited == nil && q.WeeklyLimited == nil) {
		q.State = "unknown"
	} else {
		q.State = "available"
	}
}

func quotaEligible(id int64) bool { q := readAccountQuota(id); return !q.FiveBlocked && !q.WeekBlocked }

func validUsage(p *float64) *float64 {
	if p != nil && (*p < 0 || math.IsNaN(*p) || math.IsInf(*p, 0)) {
		return nil
	}
	return p
}

func quotaWindow(p *float64, limited *bool) *bool {
	if p != nil {
		b := *p >= 100
		if limited != nil && *limited {
			b = true
		}
		return &b
	}
	return limited
}

// 错误只保留类别，避免网页/网络原文携带 Cookie、邮箱或代理凭据。
func quotaErrorSummary(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "用量读取已取消"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "用量读取超时"
	}
	if loginRequired(err) {
		return "账号登录失效，请重新登录"
	}
	return "无法确认网页用量，请稍后重试"
}

// 与删除事务竞争时先锁住 accounts 行，禁止检查存在后重新创建已删除账号的 KV。
func persistQuota(id int64, b BrowserAccount, s *browserSession, o quotaObservation, order int64) error {
	tx, err := getDB().Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE accounts SET id=id WHERE id=?`, id); err != nil {
		return err
	}
	// MySQL 默认返回实际修改行数；取得事务锁后查询真实行，不将零修改误判删除。
	var currentID int64
	err = tx.QueryRow(`SELECT id FROM accounts WHERE id=?`, id).Scan(&currentID)
	if err == sql.ErrNoRows {
		return fmt.Errorf("账号不存在")
	}
	if err != nil {
		return err
	}
	var profile, email string
	if err = tx.QueryRow(`SELECT profile_id,email FROM browser_accounts WHERE account_id=?`, id).Scan(&profile, &email); err != nil {
		return err
	}
	if profile != b.ProfileID || !strings.EqualFold(email, b.Email) {
		return fmt.Errorf("账号浏览器归属已改变")
	}
	if s != nil && (s.ProfileID != profile || !strings.EqualFold(strings.TrimSpace(s.Email), email) || s.LoginState != "ready" || email == "" || extractSAPISID(s.Cookie) == "") {
		return fmt.Errorf("用量结果账号归属无效")
	}
	var raw string
	err = tx.QueryRow(`SELECT v FROM kv WHERE k=?`, quotaKey(id)).Scan(&raw)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	q := accountQuota{}
	_ = json.Unmarshal([]byte(raw), &q)
	if order < q.AcceptedOrder || o.CheckedAt < q.CheckedAt {
		return nil
	}
	o.UsedPercent = validUsage(o.UsedPercent)
	o.WeeklyUsedPercent = validUsage(o.WeeklyUsedPercent)
	if o.CheckedAt <= 0 {
		o.CheckedAt = time.Now().Unix()
	}
	if o.ResetAt != nil && *o.ResetAt <= 0 {
		o.ResetAt = nil
	}
	if o.WeeklyResetAt != nil && *o.WeeklyResetAt <= 0 {
		o.WeeklyResetAt = nil
	}
	o.FiveHourLimited = quotaWindow(o.UsedPercent, o.FiveHourLimited)
	o.WeeklyLimited = quotaWindow(o.WeeklyUsedPercent, o.WeeklyLimited)
	if o.Error != "" {
		o.Error = quotaErrorSummary(errors.New(o.Error))
	}
	if o.FiveHourLimited != nil {
		q.FiveBlocked = *o.FiveHourLimited
		q.FiveDeadline = o.ResetAt
	}
	if o.WeeklyLimited != nil {
		q.WeekBlocked = *o.WeeklyLimited
		q.WeekDeadline = o.WeeklyResetAt
	}
	if o.Error != "" {
		q.quotaObservation = o
	} else {
		q.UsageText = o.UsageText
		q.Error = ""
		q.CheckedAt = o.CheckedAt
		if o.FiveHourLimited != nil {
			q.UsedPercent = o.UsedPercent
			q.ResetAt = o.ResetAt
			q.FiveHourLimited = o.FiveHourLimited
		}
		if o.WeeklyLimited != nil {
			q.WeeklyUsedPercent = o.WeeklyUsedPercent
			q.WeeklyResetAt = o.WeeklyResetAt
			q.WeeklyLimited = o.WeeklyLimited
		}
	}
	q.AcceptedOrder = order
	q.describe()
	rawBytes, err := json.Marshal(q)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(upsert("kv", []string{"k", "v"}, []string{"k"}), quotaKey(id), string(rawBytes)); err != nil {
		return err
	}
	if s != nil {
		if _, err = tx.Exec(`UPDATE accounts SET cookie=? WHERE id=?`, s.Cookie, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type quotaFlight struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	err     error
}

var quotaFlights = struct {
	sync.Mutex
	items map[int64]*quotaFlight
}{items: map[int64]*quotaFlight{}}

// 同账号共享有界查询；最后一个等待者离开时取消，其他账号继续消费 IPC。
func refreshAccountQuota(ctx context.Context, id int64, proxyURL *string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	quotaFlights.Lock()
	f := quotaFlights.items[id]
	if f == nil {
		taskCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		f = &quotaFlight{done: make(chan struct{}), cancel: cancel}
		quotaFlights.items[id] = f
		var proxyCopy *string
		if proxyURL != nil {
			v := *proxyURL
			proxyCopy = &v
		}
		go func() {
			err := queryAccountQuota(taskCtx, id, proxyCopy, time.Now().UnixNano())
			quotaFlights.Lock()
			f.err = err
			delete(quotaFlights.items, id)
			close(f.done)
			quotaFlights.Unlock()
			cancel()
		}()
	}
	f.waiters++
	quotaFlights.Unlock()
	defer func() {
		quotaFlights.Lock()
		f.waiters--
		if f.waiters == 0 {
			f.cancel()
		}
		quotaFlights.Unlock()
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-f.done:
		return f.err
	}
}

func queryAccountQuota(ctx context.Context, id int64, proxyURL *string, order int64) error {
	a := accountByID(id)
	b, err := browserAccountByID(id)
	if err != nil || a == nil || b == nil {
		return fmt.Errorf("账号没有浏览器档案，无法读取用量")
	}
	if b.LoginState != "ready" || b.Email == "" || a.Cookie == "" {
		return fmt.Errorf("账号尚未确认登录，无法读取用量")
	}
	var observation *quotaObservation
	var session *browserSession
	if proxyURL == nil {
		url, e := accountProxyURL(a)
		err = e
		proxyURL = &url
	}
	if err == nil {
		var rpc *browserRPC
		rpc, err = managedBrowserRPC()
		if err == nil {
			requestCtx, cancel := browserRequestContext(ctx)
			defer cancel()
			err = rpc.Call(requestCtx, browserRequest{AccountID: id, Op: "quota", ProfileID: b.ProfileID, ExpectedEmail: b.Email, ProxyURL: *proxyURL, TimeoutMs: 45000}, func(e browserEvent) error {
				if e.Type == "end" || e.Type == "error" {
					session = e.Session
					observation = e.Quota
				}
				return nil
			})
		}
	}
	if loginRequired(err) {
		markBrowserLoginRequired(id, "账号登录失效，请重新登录")
	}
	if err == nil && (session == nil || observation == nil) {
		err = fmt.Errorf("浏览器没有返回已验证用量")
	}
	if observation == nil {
		observation = &quotaObservation{CheckedAt: time.Now().Unix(), Error: quotaErrorSummary(err)}
	}
	if err != nil {
		observation.Error = quotaErrorSummary(err)
	}
	// RPC 真正失败时仅记录读取失败；成功 end 中的局部字段仍是已验证网页事实。
	if err != nil {
		observation = &quotaObservation{CheckedAt: time.Now().Unix(), Error: quotaErrorSummary(err)}
	}
	if e := persistQuota(id, *b, session, *observation, order); e != nil {
		return e
	}
	if err == nil && observation.Error != "" {
		err = errors.New(observation.Error)
	}
	return err
}

// AccountQuotaError 与 IP 并发限流分开；截止时间只表示下一次恢复确认机会。
type AccountQuotaError struct {
	AccountID    int64
	BlockedUntil *int64
}

func (e *AccountQuotaError) Error() string {
	if e == nil {
		return "账号额度状态无效，请重试"
	}
	return "account_quota_exhausted: 账号额度已耗尽，等待网页确认恢复"
}

// 固定号与已选账号必须返回有效错误；人工状态变化优先按真实账号归属报告。
func quotaAccountError(id int64) error {
	if !accountReady(accountByID(id)) {
		return fmt.Errorf("账号 #%d 已删除、禁用或登录失效，无法继续生成", id)
	}
	q := readAccountQuota(id)
	if q.State == "limited" {
		return &AccountQuotaError{AccountID: id, BlockedUntil: q.BlockedUntil}
	}
	return fmt.Errorf("账号 #%d 额度状态已改变，请重试", id)
}

func quotaPoolError(pin int64) *AccountQuotaError {
	var blocked *AccountQuotaError
	for _, a := range accountList() {
		if pin > 0 && a.ID != pin || !accountReady(&a) {
			continue
		}
		q := readAccountQuota(a.ID)
		if q.State != "limited" {
			continue
		}
		if blocked == nil {
			blocked = &AccountQuotaError{AccountID: pin}
		}
		if q.BlockedUntil != nil && (blocked.BlockedUntil == nil || *q.BlockedUntil < *blocked.BlockedUntil) {
			v := *q.BlockedUntil
			blocked.BlockedUntil = &v
		}
	}
	return blocked
}

func quotaHTTPError(w http.ResponseWriter, err error) bool {
	var q *AccountQuotaError
	if !errors.As(err, &q) || q == nil {
		return false
	}
	if q.BlockedUntil != nil && *q.BlockedUntil > time.Now().Unix() {
		w.Header().Set("Retry-After", strconv.FormatInt(*q.BlockedUntil-time.Now().Unix(), 10))
	}
	writeJSON(w, 429, map[string]any{"error": generationErrorBody(err)})
	return true
}

func generationErrorBody(err error) map[string]string {
	typ := "upstream_error"
	var q *AccountQuotaError
	if errors.As(err, &q) && q != nil {
		typ = "account_quota_exhausted"
	}
	return map[string]string{"message": err.Error(), "type": typ}
}
func generationErrorStatus(err error) int {
	var q *AccountQuotaError
	if errors.As(err, &q) && q != nil {
		return 429
	}
	return 502
}

// 后台只启动两个固定执行者，不按账号堆积 goroutine；到期失败至少隔三十秒复查。
func quotaSweep(ctx context.Context, all bool) {
	jobs := make(chan int64)
	var workers sync.WaitGroup
	for i := 0; i < 2; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for id := range jobs {
				_ = refreshAccountQuota(ctx, id, nil)
			}
		}()
	}
	now := time.Now().Unix()
	for _, a := range accountList() {
		if !accountReady(&a) {
			continue
		}
		b, _ := browserAccountByID(a.ID)
		if b == nil {
			continue
		}
		q := readAccountQuota(a.ID)
		due := q.State == "limited" && (q.BlockedUntil == nil || *q.BlockedUntil <= now) && q.CheckedAt <= now-30
		if !all && !due {
			continue
		}
		select {
		case jobs <- a.ID:
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return
		}
	}
	close(jobs)
	workers.Wait()
}

func runQuotaScheduler(ctx context.Context) {
	syncTick := time.NewTicker(5 * time.Minute)
	dueTick := time.NewTicker(30 * time.Second)
	defer syncTick.Stop()
	defer dueTick.Stop()
	quotaSweep(ctx, true)
	for {
		select {
		case <-ctx.Done():
			return
		case <-syncTick.C:
			quotaSweep(ctx, true)
		case <-dueTick.C:
			quotaSweep(ctx, false)
		}
	}
}
