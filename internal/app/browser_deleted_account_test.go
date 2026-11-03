package app

import (
	"context"
	"strings"
	"testing"
)

// 多步骤生成在 app 后被删除时必须带账号归属失败，不能空指针或继续生成。
func TestBrowserDeletedAfterAppFailsWithoutGenerate(t *testing.T) {
	id := newBrowserTestAccount(t)
	meta, _ := browserAccountByID(id)
	_ = storeBrowserSession(id, browserSession{ProfileID: meta.ProfileID, Email: "owner@example.com", Cookie: "SAPISID=owner", LoginState: "ready"})
	previous := rtCfg()
	rtMu.Lock()
	rtVal.RetryAttempts = 1
	rtVal.RequestTimeout = 0
	rtMu.Unlock()
	t.Cleanup(func() { rtMu.Lock(); rtVal = previous; rtMu.Unlock() })
	withBrowserProtocol(t, func(req browserRequest) []browserEvent {
		if req.Op == "quota" {
			return protocolAvailableQuota(t, req)
		}
		if req.URL != "https://gemini.google.com/app" {
			t.Error("删除后仍发生成请求")
		}
		_ = accountDelete(id)
		return protocolBody(`{"SNlM0e":"signed-in-token:12345"}`)
	})
	mc := Models["gemini-3.6-flash"]
	mc.PinAccountID = id
	res, err := streamGenerate("test", "test", mc, nil, nil)
	if err == nil || res.AccountID != id || !strings.Contains(err.Error(), "账号") {
		t.Fatalf("删除后未归属失败: %+v %v", res, err)
	}
}

func TestBrowserDeletedTransportCannotContinue(t *testing.T) {
	id := newBrowserTestAccount(t)
	meta, _ := browserAccountByID(id)
	_ = storeBrowserSession(id, browserSession{ProfileID: meta.ProfileID, Email: "owner@example.com", Cookie: "SAPISID=owner", LoginState: "ready"})
	tr, err := newBrowserTransport(context.Background(), accountByID(id), "")
	if err != nil {
		t.Fatal(err)
	}
	_ = accountDelete(id)
	withBrowserProtocol(t, func(req browserRequest) []browserEvent {
		t.Error("删除后仍使用浏览器档案")
		return protocolBody("unexpected")
	})
	if _, _, _, _, err := tr.exchange("request", "GET", "https://local.invalid", nil, nil, nil); err == nil {
		t.Fatal("删除后的传输仍可发送请求")
	}
	conv := &convState{accountID: id, isLogin: true}
	res, err := streamGenerateConv("continue", Models["gemini-3.6-flash"], conv, nil, nil)
	if err == nil || res.AccountID != id {
		t.Fatalf("删除旧会话未固定失败: %+v %v", res, err)
	}
}
