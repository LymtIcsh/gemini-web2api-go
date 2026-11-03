package app

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"sync"
	"testing"

	"modernc.org/sqlite"
)

// 测试侧仅模拟 MySQL 默认的未修改行数，所有 SQL 和事务仍执行于真实 SQLite。
type quotaNoopRowsDriver struct{ sqlite.Driver }

func (d *quotaNoopRowsDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.Driver.Open(name)
	if err != nil {
		return nil, err
	}
	return &quotaNoopRowsConn{Conn: conn}, nil
}

type quotaNoopRowsConn struct{ driver.Conn }

func (c *quotaNoopRowsConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	execer, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, fmt.Errorf("测试 SQLite 驱动不支持 ExecContext")
	}
	result, err := execer.ExecContext(ctx, query, args)
	if err == nil && query == "UPDATE accounts SET id=id WHERE id=?" {
		return driver.RowsAffected(0), nil
	}
	return result, err
}

var registerQuotaNoopRows sync.Once

func withQuotaNoopAffectedRows(t *testing.T) {
	t.Helper()
	previous := getDB()
	previousDialect := curDialect
	registerQuotaNoopRows.Do(func() { sql.Register("quota_noop_changed_rows", &quotaNoopRowsDriver{}) })
	conn, err := sql.Open("quota_noop_changed_rows", fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", cfg.DBPath))
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(8)
	dbw = &dbx{raw: conn}
	curDialect = dialectMySQL
	t.Cleanup(func() {
		dbw = previous
		curDialect = previousDialect
		conn.Close()
	})
}

// RowsAffected 为零但账号真实存在时必须保存额度，且删除后的状态不得重新建立。
func TestQuotaMySQLNoopAffectedRowsStillPersistsExistingAccount(t *testing.T) {
	id := readyQuotaAccount(t)
	b, _ := browserAccountByID(id)
	withQuotaNoopAffectedRows(t)
	p := float64(100)
	session := &browserSession{ProfileID: b.ProfileID, Email: b.Email, Cookie: "SAPISID=updated", LoginState: "ready"}
	if err := persistQuota(id, *b, session, quotaObservation{UsedPercent: &p, CheckedAt: 2000}, 2000); err != nil {
		t.Fatalf("existing account rejected with no-op changed rows=0: %v", err)
	}
	if q := readAccountQuota(id); q.State != "limited" || q.UsedPercent == nil || *q.UsedPercent != 100 || accountByID(id).Cookie != "SAPISID=updated" {
		t.Fatalf("quota or session was not persisted: %+v", q)
	}
	if err := accountDelete(id); err != nil {
		t.Fatal(err)
	}
	if err := persistQuota(id, *b, session, quotaObservation{UsedPercent: &p, CheckedAt: 2001}, 2001); err == nil {
		t.Fatal("deleted account persisted after no-op lock")
	}
	if kvGet(quotaKey(id)) != "" || accountByID(id) != nil {
		t.Fatal("no-op changed rows boundary recreated deleted state")
	}
}
