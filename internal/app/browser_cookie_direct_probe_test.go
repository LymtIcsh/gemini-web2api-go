package app

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 可选真实探针只读指定库，用既有客户端查登录页面；不打印凭证、不回写账号。
func TestBrowserCookieDirectProbe(t *testing.T) {
	databasePath := os.Getenv("BROWSER_COOKIE_DIRECT_PROBE_DB")
	if databasePath == "" {
		t.Skip("真实 Cookie 直连探针未启用")
	}
	id, err := strconv.ParseInt(os.Getenv("BROWSER_COOKIE_DIRECT_PROBE_ACCOUNT"), 10, 64)
	if err != nil || id <= 0 {
		t.Fatal("探针必须显式指定账号ID")
	}
	production, err := sql.Open("sqlite", "file:"+filepath.ToSlash(databasePath)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer production.Close()
	var cookie, proxyURL, email string
	err = production.QueryRow(`SELECT a.cookie,COALESCE(p.url,''),COALESCE(b.email,'')
		FROM accounts a LEFT JOIN proxies p ON p.id=a.proxy_id
		LEFT JOIN browser_accounts b ON b.account_id=a.id WHERE a.id=?`, id).Scan(&cookie, &proxyURL, &email)
	if err != nil || cookie == "" {
		t.Fatalf("无法读取指定账号登录快照：%v", err)
	}
	cfg.RequestTimeout = 20
	cfg.Impersonate = "chrome_146"
	started := time.Now()
	token, err := getXSRF(cookie, proxyURL)
	t.Logf("账号=%d 代理=%t 已绑定邮箱=%t 耗时=%s token存在=%t 错误=%v", id, proxyURL != "", email != "", time.Since(started).Round(time.Millisecond), token != "", err)
	if err != nil || token == "" {
		t.Fatal("现有 Cookie 直连客户端未获得已登录 token")
	}
	if os.Getenv("BROWSER_COOKIE_DIRECT_PROBE_GENERATE") != "1" {
		return
	}
	// 生成仅在TestMain的临时库使用Cookie副本，原账号和原库不回写。
	testDB := cfg.DBPath
	cfg = defaultConfig()
	cfg.DBPath = testDB
	cfg.RequestTimeout = 45
	cfg.RetryAttempts = 1
	cfg.GeminiBLAuto = false
	cfg.PerIPRPM = 1000
	cfg.PerIPRPH = 2000
	initRuntimeConfig()
	idCopy, err := accountAdd("Cookie直连临时探针", cookie, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = accountDelete(idCopy) })
	model := os.Getenv("BROWSER_COOKIE_DIRECT_PROBE_MODEL")
	if model == "" {
		model = "gemini-4-argon"
	}
	_, mc, err := resolveModel(model)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	mc.Context, mc.PinAccountID = ctx, idCopy
	answer := ""
	started = time.Now()
	result, err := streamGenerate("只回复 MEMORY_OPT_OK", "只回复 MEMORY_OPT_OK", mc, func(delta string) { answer += delta }, nil)
	t.Logf("真实直连模型=%s 耗时=%s 回答=%q 错误=%v", model, time.Since(started).Round(time.Millisecond), answer, err)
	if err != nil || result == nil || result.AccountID != idCopy || !strings.Contains(answer, "MEMORY_OPT_OK") {
		t.Fatal("Cookie直连未获得真实文字回复")
	}
}
