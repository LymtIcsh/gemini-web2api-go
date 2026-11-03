package app

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// 无运行中的工作进程时，禁用/失效/删除的出口都不能阻止档案删除或启动依赖。
func TestBrowserDeleteIdleNeedsNoNetworkOrWorker(t *testing.T) {
	previous := browserManager.rpc
	browserManager.rpc = nil
	t.Cleanup(func() { browserManager.rpc = previous })
	t.Setenv("BROWSER_NODE", "nonexistent-node")
	t.Setenv("BROWSER_WORKER_PATH", "nonexistent-worker.mjs")
	for _, mode := range []string{"disabled", "failed", "deleted"} {
		t.Run(mode, func(t *testing.T) {
			id := newBrowserTestAccount(t)
			proxyID, err := proxyCreate("不可用出口", "http://127.0.0.1:1", 1)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = proxyDelete(proxyID) })
			bindAccountProxy(id, proxyID)
			switch mode {
			case "disabled":
				no := false
				err = proxyUpdate(proxyID, "不可用出口", "http://127.0.0.1:1", &no, nil)
			case "failed":
				_, err = getDB().Exec(`UPDATE proxies SET fail_count=5,last_used=9999999999 WHERE id=?`, proxyID)
				loadProxies()
			case "deleted":
				err = proxyDelete(proxyID)
			}
			if err != nil {
				t.Fatal(err)
			}
			meta, _ := browserAccountByID(id)
			_ = storeBrowserSession(id, browserSession{ProfileID: meta.ProfileID, Email: "owner@example.com", Cookie: "SAPISID=owner", LoginState: "ready"})
			_ = setSelectedAccount(id)
			w := httptest.NewRecorder()
			handleAdminAccountItem(w, httptest.NewRequest(http.MethodDelete, "/admin/api/accounts/"+strconv.FormatInt(id, 10), nil))
			if w.Code != 200 || accountByID(id) != nil || selectedAccountID() != 0 {
				t.Fatalf("空闲档案删除失败: %d %s", w.Code, w.Body)
			}
			if browserManager.rpc != nil {
				t.Fatal("删除启动了工作进程")
			}
		})
	}
}

// 活动档案由现有工作进程判断忙碌状态，关闭失败必须保留账号和选择。
func TestBrowserDeleteActiveCloseFailureKeepsAccount(t *testing.T) {
	id := newBrowserTestAccount(t)
	meta, _ := browserAccountByID(id)
	_ = storeBrowserSession(id, browserSession{ProfileID: meta.ProfileID, Email: "owner@example.com", Cookie: "SAPISID=owner", LoginState: "ready"})
	_ = setSelectedAccount(id)
	t.Cleanup(func() { _ = setSelectedAccount(0) })
	bindAccountProxy(id, 999999)
	withBrowserProtocol(t, func(req browserRequest) []browserEvent {
		if req.Op != "close" || req.ProxyURL != "" || req.ProfileID != meta.ProfileID {
			t.Errorf("关闭依赖网络参数: %+v", req)
		}
		return []browserEvent{{Type: "error", Code: "profile_busy", Error: "档案有活动请求"}}
	})
	for _, entry := range []struct {
		path    string
		handler http.HandlerFunc
	}{{"accounts", handleAdminAccountItem}, {"cookies", handleAdminCookieItem}} {
		w := httptest.NewRecorder()
		entry.handler(w, httptest.NewRequest(http.MethodDelete, "/admin/api/"+entry.path+"/"+strconv.FormatInt(id, 10), nil))
		if w.Code != 409 || accountByID(id) == nil || selectedAccountID() != id {
			t.Fatalf("%s关闭失败没有保留账号: %d %s", entry.path, w.Code, w.Body)
		}
		if !strings.Contains(w.Body.String(), "profile_busy") {
			t.Fatalf("未实际请求关闭: %s", w.Body)
		}
	}
}

func TestBrowserLegacyDeleteIdleNeedsNoWorker(t *testing.T) {
	id := newBrowserTestAccount(t)
	previous := browserManager.rpc
	browserManager.rpc = nil
	t.Cleanup(func() { browserManager.rpc = previous })
	t.Setenv("BROWSER_WORKER_PATH", "nonexistent-worker.mjs")
	bindAccountProxy(id, 999999)
	w := httptest.NewRecorder()
	handleAdminCookieItem(w, httptest.NewRequest(http.MethodDelete, "/admin/api/cookies/"+strconv.FormatInt(id, 10), nil))
	if w.Code != 200 || accountByID(id) != nil || browserManager.rpc != nil {
		t.Fatalf("旧删除入口需要依赖: %d %s", w.Code, w.Body)
	}
}

func TestBrowserDeleteStoppedWorkerDoesNotRestart(t *testing.T) {
	id := newBrowserTestAccount(t)
	withBrowserProtocol(t, func(req browserRequest) []browserEvent { t.Errorf("已退出进程仍发请求: %+v", req); return nil })
	browserManager.rpc.fail(errors.New("已退出"))
	t.Setenv("BROWSER_WORKER_PATH", "nonexistent-worker.mjs")
	w := httptest.NewRecorder()
	handleAdminAccountItem(w, httptest.NewRequest(http.MethodDelete, "/admin/api/accounts/"+strconv.FormatInt(id, 10), nil))
	if w.Code != 200 || accountByID(id) != nil {
		t.Fatalf("已退出进程阻止删除: %d %s", w.Code, w.Body)
	}
}
