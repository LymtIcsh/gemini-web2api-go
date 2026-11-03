package app

import (
	"strings"
	"testing"
)

func newBrowserTestAccount(t *testing.T) int64 {
	t.Helper()
	id, err := accountCreateBrowser("浏览器账号", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = accountDelete(id) })
	return id
}

// 未登录档案不能被自动调度，两个账号必须得到不同的持久档案。
func TestBrowserAccountsStartUnreadyAndIsolated(t *testing.T) {
	id := newBrowserTestAccount(t)
	other := newBrowserTestAccount(t)
	a, err := browserAccountByID(id)
	if err != nil || a == nil || a.LoginState != "needs_login" || a.ProfileID == "" {
		t.Fatalf("未登录档案: %+v %v", a, err)
	}
	b, _ := browserAccountByID(other)
	if a.ProfileID == b.ProfileID {
		t.Fatal("账号共用了档案")
	}
	if accountReady(accountByID(id)) {
		t.Fatal("未登录账号被认为可生成")
	}
}

// 登录身份保持不变时，重新登录换发的会话主键必须能正常写回。
func TestBrowserReloginReplacesCookiesForSameEmail(t *testing.T) {
	id := newBrowserTestAccount(t)
	meta, _ := browserAccountByID(id)
	err := storeBrowserSession(id, browserSession{ProfileID: meta.ProfileID, Email: "owner@example.com", Cookie: "SAPISID=old; __Secure-1PSID=old", LoginState: "ready"})
	if err != nil {
		t.Fatal(err)
	}
	err = storeBrowserSession(id, browserSession{ProfileID: meta.ProfileID, Email: "OWNER@example.com", Cookie: "SAPISID=new; __Secure-1PSID=new", LoginState: "ready"})
	if err != nil {
		t.Fatal(err)
	}
	if a := accountByID(id); a.Cookie != "SAPISID=new; __Secure-1PSID=new" || !accountReady(a) {
		t.Fatalf("合法重新登录未更新: %+v", a)
	}
}

// 别的邮箱或档案不能覆盖原账号，即使 Cookie 字符串格式有效。
func TestBrowserWrongIdentityDoesNotOverwrite(t *testing.T) {
	id := newBrowserTestAccount(t)
	meta, _ := browserAccountByID(id)
	good := browserSession{ProfileID: meta.ProfileID, Email: "owner@example.com", Cookie: "SAPISID=owner; __Secure-1PSID=owner", LoginState: "ready"}
	if err := storeBrowserSession(id, good); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []browserSession{
		{ProfileID: meta.ProfileID, Email: "other@example.com", Cookie: "SAPISID=other", LoginState: "ready"},
		{ProfileID: "another-profile", Email: "owner@example.com", Cookie: "SAPISID=other", LoginState: "ready"},
		{ProfileID: meta.ProfileID, Email: "", Cookie: "SAPISID=other", LoginState: "ready"},
	} {
		if err := storeBrowserSession(id, bad); err == nil {
			t.Fatal("错误身份被写入")
		}
	}
	if a := accountByID(id); a.Cookie != good.Cookie {
		t.Fatal("原会话被覆盖")
	}
}

// 登录状态与人工启用状态独立，登录成功不能重新启用用户禁用的账号。
func TestBrowserLoginDoesNotEnableDisabledAccount(t *testing.T) {
	id := newBrowserTestAccount(t)
	_ = accountSetStatus(id, "disabled")
	meta, _ := browserAccountByID(id)
	if err := storeBrowserSession(id, browserSession{ProfileID: meta.ProfileID, Email: "owner@example.com", Cookie: "SAPISID=owner", LoginState: "ready"}); err != nil {
		t.Fatal(err)
	}
	if a := accountByID(id); a.Status != "disabled" || accountReady(a) {
		t.Fatalf("手动禁用被覆盖: %+v", a)
	}
}

// 固定账号失效时不得静默改用另一个账号。
func TestBrowserSelectionNeverFallsBack(t *testing.T) {
	id := newBrowserTestAccount(t)
	meta, _ := browserAccountByID(id)
	_ = storeBrowserSession(id, browserSession{ProfileID: meta.ProfileID, Email: "owner@example.com", Cookie: "SAPISID=owner", LoginState: "ready"})
	if err := setSelectedAccount(id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = setSelectedAccount(0) })
	if a, ok := pickCookieAccount(); !ok || a.ID != id {
		t.Fatal("没有使用固定账号")
	}
	_ = storeBrowserSession(id, browserSession{ProfileID: meta.ProfileID, Email: "owner@example.com", LoginState: "needs_login"})
	if a, ok := pickCookieAccount(); ok || a != nil {
		t.Fatal("未登录的固定账号被悄悄替换")
	}
	if err := setSelectedAccount(-1); err == nil || !strings.Contains(err.Error(), "账号") {
		t.Fatal("非法选择未返回明确错误")
	}
}
