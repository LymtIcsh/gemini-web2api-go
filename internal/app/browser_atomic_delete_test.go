package app

import (
	"context"
	"strconv"
	"testing"
)

// 第二步删除失败时，第一步档案删除必须回滚，外部不能看到只有 Cookie 的浏览器账号。
func TestBrowserDeleteRollsBackProfileOnAccountFailure(t *testing.T) {
	id := newBrowserTestAccount(t)
	meta, _ := browserAccountByID(id)
	_ = storeBrowserSession(id, browserSession{ProfileID: meta.ProfileID, Email: "owner@example.com", Cookie: "SAPISID=owner", LoginState: "ready"})
	_ = setSelectedAccount(id)
	trigger := "fail_account_delete_" + strconv.FormatInt(id, 10)
	_, err := getDB().Exec(`CREATE TRIGGER ` + trigger + ` BEFORE DELETE ON accounts WHEN OLD.id=` + strconv.FormatInt(id, 10) + ` BEGIN SELECT RAISE(ABORT,'expected delete failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = getDB().Exec(`DROP TRIGGER ` + trigger); _ = setSelectedAccount(0) })
	if err := accountDelete(id); err == nil {
		t.Fatal("测试触发器未阻止删除")
	}
	b, err := browserAccountByID(id)
	if err != nil || b == nil || b.ProfileID != meta.ProfileID || accountByID(id) == nil || selectedAccountID() != id {
		t.Fatalf("失败删除遗留半个账号: %+v %v", b, err)
	}
}

// 已经取得的浏览器账号快照在删除后不能被重新解释成旧 Cookie 账号。
func TestBrowserDeletedSnapshotCannotBecomeLegacy(t *testing.T) {
	id := newBrowserTestAccount(t)
	meta, _ := browserAccountByID(id)
	_ = storeBrowserSession(id, browserSession{ProfileID: meta.ProfileID, Email: "owner@example.com", Cookie: "SAPISID=owner", LoginState: "ready"})
	a := accountByID(id)
	_ = accountDelete(id)
	if accountReady(a) {
		t.Fatal("已删账号的快照被当成有效旧 Cookie")
	}
	if tr, err := newBrowserTransport(context.Background(), a, ""); err == nil || tr != nil {
		t.Fatalf("已删浏览器快照回落旧传输: %+v %v", tr, err)
	}
	conv := &convState{accountID: id, isLogin: true, cookie: a.Cookie}
	if res, err := streamGenerateConv("continue", Models["gemini-3.6-flash"], conv, nil, nil); err == nil || res.AccountID != id {
		t.Fatalf("已删会话回落旧传输: %+v %v", res, err)
	}
}

// 真正旧 Cookie 账号仍走旧传输；只有账号已不存在才拒绝，不能把所有无档案账号一律禁用。
func TestBrowserSnapshotKeepsLegacyCookieTransport(t *testing.T) {
	id, err := accountAdd("旧账号", "SAPISID=legacy", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = accountDelete(id) })
	a := accountByID(id)
	if !accountReady(a) {
		t.Fatal("旧 Cookie 账号无法使用")
	}
	if tr, err := newBrowserTransport(context.Background(), a, ""); err != nil || tr != nil {
		t.Fatalf("旧 Cookie 被改成浏览器传输: %+v %v", tr, err)
	}
	_ = accountSetStatus(id, "disabled")
	if accountReady(a) {
		t.Fatal("旧快照忽略了当前禁用状态")
	}
}
