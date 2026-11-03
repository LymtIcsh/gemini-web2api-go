package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ProxyProbeResult 是单条代理测速与连通性检测结果。
type ProxyProbeResult struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	Enabled   bool   `json:"enabled"`
	Ok        bool   `json:"ok"`
	LatencyMs int64  `json:"latency_ms"`
	Status    int    `json:"status"`
	Error     string `json:"error"`
}

// AutoBindResult 是智能最优绑定的执行汇总。
type AutoBindResult struct {
	TotalAccounts  int              `json:"total_accounts"`
	BoundAccounts  int              `json:"bound_accounts"`
	HealthyProxies int              `json:"healthy_proxies"`
	Bindings       []AccountBinding `json:"bindings"`
}

// AccountBinding 记录账号与最优代理的绑定对应关系。
type AccountBinding struct {
	AccountID    int64  `json:"account_id"`
	AccountLabel string `json:"account_label"`
	ProxyID      int64  `json:"proxy_id"`
	ProxyName    string `json:"proxy_name"`
	LatencyMs    int64  `json:"latency_ms"`
	IsHome       bool   `json:"is_home"`
}

// probeProxyURL 实际向 Google Gemini (/app) 发起探测，检测连通性、地区合规与往返延迟。
//
// 1. 如果 Google 返回 302 且重定向包含 sorry/index，说明命中地区限制或 IP 风控拦截。
// 2. 如果返回 200 或正常跳转（如未登录时跳转 accounts.google.com），代表 Gemini 在该出口可达。
// 3. 统计往返耗时 LatencyMs，方便评测优劣。
func probeProxyURL(proxyURL string, timeout time.Duration) (ok bool, latencyMs int64, status int, errStr string) {
	t0 := time.Now()

	tr := &http.Transport{
		DisableCompression: false,
		ForceAttemptHTTP2:  false,
	}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			tr.Proxy = http.ProxyURL(u)
		} else {
			return false, 0, 0, fmt.Sprintf("invalid proxy URL: %v", err)
		}
	}

	client := &http.Client{
		Transport: tr,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	req, err := http.NewRequest("GET", "https://gemini.google.com/app", nil)
	if err != nil {
		return false, time.Since(t0).Milliseconds(), 0, err.Error()
	}
	req.Header.Set("User-Agent", ChromeUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := client.Do(req)
	latencyMs = time.Since(t0).Milliseconds()
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "Client.Timeout") || strings.Contains(msg, "context deadline exceeded") {
			msg = "连接超时 (Timeout)"
		} else if strings.Contains(msg, "connection refused") || strings.Contains(msg, "actively refused") {
			msg = "连接被拒绝 (Connection Refused)"
		} else if strings.Contains(msg, "EOF") {
			msg = "代理连接异常中断 (EOF)"
		} else if strings.Contains(msg, "wsarecv") || strings.Contains(msg, "connection was aborted") {
			msg = "代理连接被重置 (Connection Aborted)"
		}
		return false, latencyMs, 0, msg
	}
	defer resp.Body.Close()

	status = resp.StatusCode
	loc := resp.Header.Get("Location")

	if status == 200 {
		return true, latencyMs, 200, ""
	}
	if status >= 300 && status < 400 {
		if strings.Contains(loc, "sorry") {
			return false, latencyMs, status, "Gemini地区限制/Google拦截 (sorry/index)"
		}
		// 正常 Google 登录或服务重定向
		return true, latencyMs, status, ""
	}
	if status == 403 {
		return false, latencyMs, 403, "HTTP 403 访问被拒绝"
	}
	if status == 429 {
		return false, latencyMs, 429, "HTTP 429 请求过于频繁"
	}
	return false, latencyMs, status, fmt.Sprintf("HTTP %d", status)
}

// probeAllProxies 并发测试所有启用的代理节点（可选择同步更新 DB 失败计数与最后错误）。
func probeAllProxies(updateDB bool) []ProxyProbeResult {
	proxies := listProxies()
	results := make([]ProxyProbeResult, len(proxies))

	var wg sync.WaitGroup
	// 最多 10 并发探测，避免瞬间打满本地连接
	sem := make(chan struct{}, 10)

	for i, p := range proxies {
		wg.Add(1)
		go func(idx int, proxy Proxy) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			res := ProxyProbeResult{
				ID:      proxy.ID,
				Name:    proxy.Name,
				URL:     proxy.URL,
				Enabled: proxy.Enabled,
			}

			if !proxy.Enabled {
				res.Ok = false
				res.Status = 0
				res.Error = "代理已禁用"
				results[idx] = res
				return
			}

			ok, ms, code, errMsg := probeProxyURL(proxy.URL, 8*time.Second)
			res.Ok = ok
			res.LatencyMs = ms
			res.Status = code
			res.Error = errMsg
			results[idx] = res

			if updateDB {
				if ok {
					recordProxyResult(proxy.ID, true, "")
				} else {
					recordProxyResult(proxy.ID, false, errMsg)
				}
			}
		}(i, p)
	}

	wg.Wait()
	return results
}

// proxyResidentialScore 判断节点名称是否带有家宽、住宅等优质标记。
func proxyResidentialScore(name string) int {
	low := strings.ToLower(name)
	if strings.Contains(low, "家宽") || strings.Contains(low, "住宅") ||
		strings.Contains(low, "residential") || strings.Contains(low, "home") {
		return 1
	}
	return 0
}

// autoBindProxiesToAccounts 自动测试并筛选全部满足 Gemini 地区限制的可用节点，
// 优先家宽、住宅，并按延迟由低到高排序，将账号 1:1（或均衡循环）绑定到最优节点。
func autoBindProxiesToAccounts(accountIDs []int64) (*AutoBindResult, error) {
	// 1. 全量并发探测节点
	probes := probeAllProxies(true)

	// 2. 筛选可用且启用的健康节点
	var healthy []ProxyProbeResult
	for _, p := range probes {
		if p.Ok && p.Enabled {
			healthy = append(healthy, p)
		}
	}
	if len(healthy) == 0 {
		return nil, errors.New("未检测到可用且满足 Gemini 地区限制的代理节点，请先在代理池检查或启动节点")
	}

	// 3. 排序：优先家宽 (Score 降序)，其次延迟升序 (LatencyMs 升序)
	sort.Slice(healthy, func(i, j int) bool {
		si := proxyResidentialScore(healthy[i].Name)
		sj := proxyResidentialScore(healthy[j].Name)
		if si != sj {
			return si > sj
		}
		return healthy[i].LatencyMs < healthy[j].LatencyMs
	})

	// 4. 获取待绑定的账号列表
	var query string
	var args []interface{}
	if len(accountIDs) > 0 {
		placeholders := make([]string, len(accountIDs))
		for i, id := range accountIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query = fmt.Sprintf("SELECT id, label FROM accounts WHERE id IN (%s) ORDER BY id ASC", strings.Join(placeholders, ","))
	} else {
		query = "SELECT id, label FROM accounts ORDER BY id ASC"
	}

	rows, err := getDB().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("读取账号列表失败: %v", err)
	}
	defer rows.Close()

	type targetAccount struct {
		id    int64
		label string
	}
	var targets []targetAccount
	for rows.Next() {
		var a targetAccount
		if err := rows.Scan(&a.id, &a.label); err == nil {
			targets = append(targets, a)
		}
	}
	if len(targets) == 0 {
		return nil, errors.New("账号池中暂无待绑定账号")
	}

	// 5. 1:1 分配绑定（账号数多于节点数时循环复用低延迟节点）
	var bindings []AccountBinding
	for i, acct := range targets {
		assignedProxy := healthy[i%len(healthy)]
		_, err := getDB().Exec("UPDATE accounts SET proxy_id=? WHERE id=?", assignedProxy.ID, acct.id)
		if err != nil {
			return nil, fmt.Errorf("更新账号 #%d 代理绑定失败: %v", acct.id, err)
		}
		bindings = append(bindings, AccountBinding{
			AccountID:    acct.id,
			AccountLabel: acct.label,
			ProxyID:      assignedProxy.ID,
			ProxyName:    assignedProxy.Name,
			LatencyMs:    assignedProxy.LatencyMs,
			IsHome:       proxyResidentialScore(assignedProxy.Name) > 0,
		})
	}

	return &AutoBindResult{
		TotalAccounts:  len(targets),
		BoundAccounts:  len(bindings),
		HealthyProxies: len(healthy),
		Bindings:       bindings,
	}, nil
}

// handleAdminProxiesProbe 处理代理连通性与测速探针请求。
// POST/GET /admin/api/proxies/probe?id=N（id 可选，不传测全部）
func handleAdminProxiesProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}

	idStr := r.URL.Query().Get("id")
	if idStr != "" {
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad proxy id"})
			return
		}
		var found *Proxy
		for _, p := range listProxies() {
			if p.ID == id {
				found = &p
				break
			}
		}
		if found == nil {
			writeJSON(w, 404, map[string]string{"error": "proxy not found"})
			return
		}

		ok, ms, code, errMsg := probeProxyURL(found.URL, 8*time.Second)
		if ok {
			recordProxyResult(found.ID, true, "")
		} else {
			recordProxyResult(found.ID, false, errMsg)
		}

		writeJSON(w, 200, map[string]interface{}{
			"items": []ProxyProbeResult{{
				ID:        found.ID,
				Name:      found.Name,
				URL:       found.URL,
				Enabled:   found.Enabled,
				Ok:        ok,
				LatencyMs: ms,
				Status:    code,
				Error:     errMsg,
			}},
		})
		return
	}

	// 探测全部
	results := probeAllProxies(true)
	writeJSON(w, 200, map[string]interface{}{"items": results})
}

// handleAdminProxiesAutoBind 处理最优代理自动绑定请求。
// POST /admin/api/proxies/auto-bind
// Body (可选): {"account_ids": [1, 2, 3]}
func handleAdminProxiesAutoBind(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}

	var req struct {
		AccountIDs []int64 `json:"account_ids"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	res, err := autoBindProxiesToAccounts(req.AccountIDs)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, 200, map[string]interface{}{
		"ok":     true,
		"result": res,
	})
}

// POST /admin/api/proxies/clean-unavailable
func handleAdminProxiesCleanUnavailable(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}

	var req struct {
		IDs []int64 `json:"ids"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	deleted := 0
	if len(req.IDs) > 0 {
		for _, id := range req.IDs {
			if err := proxyDelete(id); err == nil {
				deleted++
			}
		}
	} else {
		for _, p := range listProxies() {
			if p.FailCount > 0 {
				if err := proxyDelete(p.ID); err == nil {
					deleted++
				}
			}
		}
	}

	writeJSON(w, 200, map[string]interface{}{
		"ok":      true,
		"deleted": deleted,
	})
}

// POST /admin/api/proxies/import-clash 一键从本地 Clash Verge 配置文件导入全部可用节点监听端口
func handleAdminProxiesImportClash(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}

	appData := os.Getenv("APPDATA")
	if appData == "" {
		writeJSON(w, 500, map[string]string{"error": "APPDATA environment variable not found"})
		return
	}
	cfgPath := filepath.Join(appData, "io.github.clash-verge-rev.clash-verge-rev", "clash-verge.yaml")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "无法读取 clash-verge.yaml: " + err.Error()})
		return
	}

	lines := strings.Split(string(data), "\n")
	type item struct {
		name string
		port int
	}
	var listeners []item
	var curName string
	var curPort int
	inListeners := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "listeners:") {
			inListeners = true
			continue
		}
		if inListeners {
			if len(line) > 0 && line[0] != ' ' && line[0] != '-' && line[0] != '\t' {
				break
			}
			if strings.HasPrefix(trimmed, "- name:") {
				curName = ""
				curPort = 0
			}
			if strings.HasPrefix(trimmed, "port:") {
				pStr := strings.TrimSpace(strings.TrimPrefix(trimmed, "port:"))
				curPort, _ = strconv.Atoi(pStr)
			}
			if strings.HasPrefix(trimmed, "proxy:") {
				curName = strings.TrimSpace(strings.TrimPrefix(trimmed, "proxy:"))
				curName = strings.Trim(curName, "\"'")
				if curPort > 0 && curName != "" {
					listeners = append(listeners, item{name: curName, port: curPort})
				}
			}
		}
	}

	existing := make(map[string]bool)
	for _, p := range listProxies() {
		existing[p.URL] = true
	}

	imported := 0
	mainURL := "http://127.0.0.1:7897"
	if !existing[mainURL] {
		_, _ = proxyCreate("Clash-7897 (默认主出口)", mainURL, 1)
		imported++
	}

	for _, l := range listeners {
		u := fmt.Sprintf("http://127.0.0.1:%d", l.port)
		if !existing[u] {
			_, err := proxyCreate(l.name, u, 1)
			if err == nil {
				imported++
			}
		}
	}

	writeJSON(w, 200, map[string]interface{}{
		"ok":       true,
		"imported": imported,
		"total":    len(listeners) + 1,
	})
}
