package app

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// SQLite 原生快照包含 WAL 中已经提交的数据，新增档案表前保存一次旧库。
func backupBeforeBrowserSchema(conn *sql.DB, databasePath string) (string, error) {
	var count int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='accounts'`).Scan(&count); err != nil || count == 0 {
		return "", err
	}
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='browser_accounts'`).Scan(&count); err != nil || count > 0 {
		return "", err
	}
	path, err := filepath.Abs(databasePath)
	if err != nil {
		return "", err
	}
	backup := path + ".before-browser-" + time.Now().Format("20060102-150405.000000000") + ".db"
	if _, err := conn.Exec(`VACUUM INTO '` + strings.ReplaceAll(backup, "'", "''") + `'`); err != nil {
		return "", fmt.Errorf("新增浏览器档案前备份数据库失败: %w", err)
	}
	return backup, nil
}
