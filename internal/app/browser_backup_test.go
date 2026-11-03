package app

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// 给现有数据库增加账号档案前，备份必须包含真实旧记录，后续启动不重复生成备份。
func TestBrowserSchemaBackupPreservesExistingAccounts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gemini.db")
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.Exec(`CREATE TABLE accounts(id INTEGER PRIMARY KEY, cookie TEXT); INSERT INTO accounts VALUES(9,'old-cookie')`); err != nil {
		t.Fatal(err)
	}
	backup, err := backupBeforeBrowserSchema(conn, path)
	if err != nil || backup == "" {
		t.Fatalf("没有升级前备份: %s %v", backup, err)
	}
	copyDB, err := sql.Open("sqlite", backup)
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	var got string
	if err := copyDB.QueryRow(`SELECT cookie FROM accounts WHERE id=9`).Scan(&got); err != nil || got != "old-cookie" {
		t.Fatalf("旧记录备份损坏: %q %v", got, err)
	}
	if _, err := conn.Exec(`CREATE TABLE browser_accounts(account_id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if next, err := backupBeforeBrowserSchema(conn, path); err != nil || next != "" {
		t.Fatalf("重复备份: %s %v", next, err)
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatal(err)
	}
}
