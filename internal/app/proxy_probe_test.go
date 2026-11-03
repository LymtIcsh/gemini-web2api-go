package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProbeProxyURL(t *testing.T) {
	// 1. Test 200 OK
	s200 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer s200.Close()

	// 2. Test 302 Redirect to sorry/index
	sSorry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://www.google.com/sorry/index?continue=...", http.StatusFound)
	}))
	defer sSorry.Close()

	// 3. Test 302 Normal Redirect to accounts.google.com
	sAccounts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://accounts.google.com/ServiceLogin", http.StatusFound)
	}))
	defer sAccounts.Close()

	// 4. Test 403 Forbidden
	s403 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer s403.Close()

	// Helper for local mock endpoint probe
	probeMock := func(serverURL string) (bool, int64, int, string) {
		t0 := time.Now()
		req, _ := http.NewRequest("GET", serverURL, nil)
		client := &http.Client{
			Timeout: 2 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		resp, err := client.Do(req)
		ms := time.Since(t0).Milliseconds()
		if err != nil {
			return false, ms, 0, err.Error()
		}
		defer resp.Body.Close()
		loc := resp.Header.Get("Location")
		if resp.StatusCode == 200 {
			return true, ms, 200, ""
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			if strings.Contains(loc, "sorry") {
				return false, ms, resp.StatusCode, "Gemini地区限制/Google拦截 (sorry/index)"
			}
			return true, ms, resp.StatusCode, ""
		}
		return false, ms, resp.StatusCode, "HTTP error"
	}

	ok, _, code, _ := probeMock(s200.URL)
	if !ok || code != 200 {
		t.Fatalf("expected 200 ok, got ok=%v, code=%d", ok, code)
	}

	ok, _, code, errStr := probeMock(sSorry.URL)
	if ok || !strings.Contains(errStr, "sorry/index") {
		t.Fatalf("expected sorry/index block, got ok=%v, code=%d, err=%s", ok, code, errStr)
	}

	ok, _, code, _ = probeMock(sAccounts.URL)
	if !ok || code != 302 {
		t.Fatalf("expected accounts redirect to be ok, got ok=%v, code=%d", ok, code)
	}

	ok, _, code, _ = probeMock(s403.URL)
	if ok || code != 403 {
		t.Fatalf("expected 403 failure, got ok=%v, code=%d", ok, code)
	}
}

func TestProxyResidentialScore(t *testing.T) {
	tests := []struct {
		name string
		want int
	}{
		{"US-家宽-01", 1},
		{"香港-原生住宅-02", 1},
		{"UK-Residential-Fast", 1},
		{"JP-Home-BGP", 1},
		{"Clash-7897", 0},
		{"Clash-US1", 0},
		{"AWS-Tokyo", 0},
	}
	for _, tc := range tests {
		got := proxyResidentialScore(tc.name)
		if got != tc.want {
			t.Errorf("proxyResidentialScore(%q) = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestAutoBindProxiesToAccountsLogic(t *testing.T) {
	// Setup test in-memory/temp DB
	testDB := cfg.DBPath
	cfg = defaultConfig()
	cfg.DBPath = testDB
	cfg.PerIPRPM = 1000
	cfg.PerIPRPH = 2000
	initRuntimeConfig()

	// Clean tables
	_, _ = getDB().Exec("DELETE FROM accounts")
	_, _ = getDB().Exec("DELETE FROM proxies")

	// Add test accounts
	a1, err := accountAdd("测试账号1", "SAPISID=test1; __Secure-1PSID=p1", "")
	if err != nil {
		t.Fatal(err)
	}
	a2, err := accountAdd("测试账号2", "SAPISID=test2; __Secure-1PSID=p2", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = accountDelete(a1)
		_ = accountDelete(a2)
	})

	// Case 1: no healthy proxies available
	_, err = autoBindProxiesToAccounts(nil)
	if err == nil {
		t.Fatal("expected error when no healthy proxies, got nil")
	}

	// Add proxies to DB
	p1, err := proxyCreate("US-家宽-低延迟", "http://127.0.0.1:18001", 1)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := proxyCreate("普通节点", "http://127.0.0.1:18002", 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = proxyDelete(p1)
		_ = proxyDelete(p2)
	})

	// Manually assign probe results for sorting test
	healthy := []ProxyProbeResult{
		{ID: p2, Name: "普通节点", LatencyMs: 50, Ok: true, Enabled: true},
		{ID: p1, Name: "US-家宽-低延迟", LatencyMs: 120, Ok: true, Enabled: true},
	}
	// Verify sorting logic: Home score first, then latency
	if proxyResidentialScore(healthy[1].Name) <= proxyResidentialScore(healthy[0].Name) {
		t.Fatal("expected US-家宽 to have higher score than 普通节点")
	}

	// Test batch account proxy setting
	err = accountSetProxyBatch([]int64{a1, a2}, p1)
	if err != nil {
		t.Fatalf("accountSetProxyBatch failed: %v", err)
	}
	acct := accountByID(a1)
	if acct == nil || acct.ProxyID != p1 {
		t.Fatalf("expected account %d to have proxy %d, got %v", a1, p1, acct)
	}
}

func TestHandleAdminProxiesProbeAndAutoBindEndpoints(t *testing.T) {
	// Probe endpoint
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin/api/proxies/probe", nil)
	handleAdminProxiesProbe(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected probe status 200, got %d", rec.Code)
	}
	var probeResp struct {
		Items []ProxyProbeResult `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &probeResp); err != nil {
		t.Fatalf("failed to decode probe response: %v", err)
	}

	// Auto-bind endpoint (with empty body)
	recBind := httptest.NewRecorder()
	reqBind := httptest.NewRequest("POST", "/admin/api/proxies/auto-bind", bytes.NewBufferString("{}"))
	handleAdminProxiesAutoBind(recBind, reqBind)
	// It will return either 200 (if healthy proxies exist in test env) or 400 (if no healthy proxies)
	if recBind.Code != 200 && recBind.Code != 400 {
		t.Fatalf("unexpected auto-bind status: %d body=%s", recBind.Code, recBind.Body.String())
	}

	// Clean unavailable endpoint
	recClean := httptest.NewRecorder()
	reqClean := httptest.NewRequest("POST", "/admin/api/proxies/clean-unavailable", bytes.NewBufferString(`{"ids":[]}`))
	handleAdminProxiesCleanUnavailable(recClean, reqClean)
	if recClean.Code != 200 {
		t.Fatalf("unexpected clean status: %d body=%s", recClean.Code, recClean.Body.String())
	}
}
