package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// 新接口必须直接创建浏览器档案，列表返回可操作状态而不泄露 Cookie。
func TestAdminBrowserAccountCreateAndList(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/admin/api/accounts", strings.NewReader(`{"label":"工作账号"}`))
	handleAdminAccounts(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("创建失败 %d: %s", w.Code, w.Body)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	t.Cleanup(func() { _ = accountDelete(created.ID) })
	w = httptest.NewRecorder()
	handleAdminAccounts(w, httptest.NewRequest(http.MethodGet, "/admin/api/accounts", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "profile_id") || !strings.Contains(w.Body.String(), "needs_login") {
		t.Fatalf("列表未包含登录信息: %s", w.Body)
	}
	var result struct {
		Items []map[string]interface{} `json:"items"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	for _, item := range result.Items {
		if _, ok := item["cookie"]; ok {
			t.Fatal("账号列表回传了 Cookie 凭据")
		}
	}
}

func TestAdminBrowserSelectionRejectsUnknown(t *testing.T) {
	w := httptest.NewRecorder()
	handleAdminAccountSelection(w, httptest.NewRequest(http.MethodPut, "/admin/api/accounts/selection", strings.NewReader(`{"account_id":-1}`)))
	if w.Code != 400 {
		t.Fatalf("无效选择未拒绝: %d", w.Code)
	}
	w = httptest.NewRecorder()
	handleAdminAccountSelection(w, httptest.NewRequest(http.MethodPut, "/admin/api/accounts/selection", strings.NewReader(`{"account_id":0}`)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "account_id") {
		t.Fatalf("不能切换为自动选号: %d %s", w.Code, w.Body)
	}
}

// 工作进程确认的实际邮箱才可以成为账号身份，账号标签不能替代身份校验。
func TestAdminBrowserConfirmLoginUsesWorkerSession(t *testing.T) {
	id := newBrowserTestAccount(t)
	meta, _ := browserAccountByID(id)
	withBrowserProtocol(t, func(req browserRequest) []browserEvent {
		if req.Op != "confirm-login" {
			t.Errorf("错误控制操作: %s", req.Op)
		}
		return []browserEvent{{Type: "end", Session: &browserSession{ProfileID: meta.ProfileID, Email: "real@example.com", Cookie: "SAPISID=real", LoginState: "ready"}}}
	})
	w := httptest.NewRecorder()
	handleAdminAccountItem(w, httptest.NewRequest(http.MethodPost, "/admin/api/accounts/"+strconv.FormatInt(id, 10)+"/confirm-login", nil))
	if w.Code != 200 || !accountReady(accountByID(id)) || !strings.Contains(w.Body.String(), "real@example.com") {
		t.Fatalf("登录确认未持久化: %d %s", w.Code, w.Body)
	}
}
