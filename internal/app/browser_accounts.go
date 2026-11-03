package app

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// BrowserAccount 保存账号的浏览器归属；Cookie 和人工启用状态仍由 accounts 管理。
type BrowserAccount struct {
	AccountID  int64  `json:"account_id"`
	Email      string `json:"email"`
	ProfileID  string `json:"profile_id"`
	LoginState string `json:"login_state"`
	LastSyncAt int64  `json:"last_sync_at"`
}

type browserSession struct {
	Email      string `json:"email"`
	Cookie     string `json:"cookie"`
	LoginState string `json:"login_state"`
	ProfileID  string `json:"profile_id"`
}

func browserAccountByID(id int64) (*BrowserAccount, error) {
	var b BrowserAccount
	err := getDB().QueryRow(`SELECT account_id,email,profile_id,login_state,last_sync_at FROM browser_accounts WHERE account_id=?`, id).
		Scan(&b.AccountID, &b.Email, &b.ProfileID, &b.LoginState, &b.LastSyncAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &b, err
}

func accountCreateBrowser(label, note string) (int64, error) {
	id, err := insertID(`INSERT INTO accounts(label,cookie,status,note,created_at) VALUES (?,?,?,?,?)`,
		strings.TrimSpace(label), "", "enabled", strings.TrimSpace(note), time.Now().Unix())
	if err != nil {
		return 0, err
	}
	if _, err = attachBrowserProfile(id); err != nil {
		_ = accountDelete(id)
		return 0, err
	}
	return id, nil
}

// 既有 Cookie 行可通过网页登录绑定档案，原账号 ID 和请求记录不变。
func attachBrowserProfile(id int64) (*BrowserAccount, error) {
	if b, err := browserAccountByID(id); b != nil || err != nil {
		return b, err
	}
	if accountByID(id) == nil {
		return nil, fmt.Errorf("账号不存在")
	}
	b := &BrowserAccount{AccountID: id, ProfileID: uuid.NewString(), LoginState: "needs_login"}
	_, err := getDB().Exec(`INSERT INTO browser_accounts(account_id,email,profile_id,login_state,last_sync_at) VALUES (?,?,?,?,?)`,
		id, "", b.ProfileID, b.LoginState, 0)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func accountReady(a *CookieAccount) bool {
	if a == nil {
		return false
	}
	_, ready, _, err := accountBrowserState(a.ID)
	return err == nil && ready
}

// 一次查询取得账号存在性、登录状态与档案归属，避免删除跨两次查询造成旧传输回落。
func accountBrowserState(id int64) (bool, bool, *BrowserAccount, error) {
	var status, cookie string
	var b BrowserAccount
	err := getDB().QueryRow(`SELECT a.status,a.cookie,COALESCE(b.account_id,0),COALESCE(b.email,''),
		COALESCE(b.profile_id,''),COALESCE(b.login_state,''),COALESCE(b.last_sync_at,0)
		FROM accounts a LEFT JOIN browser_accounts b ON b.account_id=a.id WHERE a.id=?`, id).
		Scan(&status, &cookie, &b.AccountID, &b.Email, &b.ProfileID, &b.LoginState, &b.LastSyncAt)
	if err == sql.ErrNoRows {
		return false, false, nil, nil
	}
	if err != nil {
		return false, false, nil, err
	}
	ready := status == "enabled" && cookie != ""
	if b.AccountID == 0 {
		return true, ready, nil, nil
	}
	return true, ready && b.LoginState == "ready", &b, nil
}

// 实际页面邮箱和档案确认身份，允许同邮箱重新登录时更换整套会话 Cookie。
func storeBrowserSession(id int64, s browserSession) error {
	b, err := browserAccountByID(id)
	if err != nil || b == nil {
		return fmt.Errorf("账号浏览器档案不存在")
	}
	if s.ProfileID != b.ProfileID {
		return fmt.Errorf("账号浏览器档案不一致")
	}
	if s.LoginState != "ready" && s.LoginState != "needs_login" && s.LoginState != "logging_in" {
		return fmt.Errorf("浏览器登录状态无效")
	}
	email := strings.ToLower(strings.TrimSpace(s.Email))
	if b.Email != "" && email != "" && !strings.EqualFold(b.Email, email) {
		markBrowserLoginRequired(id, "浏览器登录邮箱与原账号不一致")
		return fmt.Errorf("浏览器登录邮箱与原账号不一致，请在原账号档案中重新登录")
	}
	if s.LoginState == "ready" && (email == "" || extractSAPISID(s.Cookie) == "") {
		markBrowserLoginRequired(id, "尚未确认账号邮箱或完整登录态")
		return fmt.Errorf("尚未确认账号邮箱或完整登录态")
	}
	if email == "" {
		email = b.Email
	}
	tx, err := getDB().Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	if _, err = tx.Exec(`UPDATE browser_accounts SET email=?,login_state=?,last_sync_at=? WHERE account_id=?`, email, s.LoginState, now, id); err != nil {
		return err
	}
	if s.LoginState == "ready" {
		if _, err = tx.Exec(`UPDATE accounts SET cookie=?,fail_count=0,last_error='',last_ok_at=? WHERE id=?`, s.Cookie, now, id); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

const selectedAccountKey = "selected_account_id"

func selectedAccountID() int64 {
	id, _ := strconv.ParseInt(kvGet(selectedAccountKey), 10, 64)
	return id
}

func setSelectedAccount(id int64) error {
	if id < 0 || (id > 0 && !accountReady(accountByID(id))) {
		return fmt.Errorf("请选择已启用且登录有效的账号")
	}
	return kvSet(selectedAccountKey, strconv.FormatInt(id, 10))
}

// HTTP 页面和后台刷新共用登录错误分类，网络错误不会把账号标成需要重新登录。
func markBrowserLoginRequired(id int64, detail string) {
	_, _ = getDB().Exec(`UPDATE browser_accounts SET login_state='needs_login' WHERE account_id=?`, id)
	_, _ = getDB().Exec(`UPDATE accounts SET last_error=? WHERE id=?`, truncate(detail, 200), id)
}

func browserAccountsExist() bool {
	var count int
	return getDB().QueryRow(`SELECT COUNT(*) FROM browser_accounts`).Scan(&count) == nil && count > 0
}
