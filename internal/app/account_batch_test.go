package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAccountBatchStatus(t *testing.T) {
	id1, err := accountCreateBrowser("batch-test-1", "")
	if err != nil {
		t.Fatalf("accountCreateBrowser 1 failed: %v", err)
	}
	t.Cleanup(func() { _ = accountDelete(id1) })

	id2, err := accountCreateBrowser("batch-test-2", "")
	if err != nil {
		t.Fatalf("accountCreateBrowser 2 failed: %v", err)
	}
	t.Cleanup(func() { _ = accountDelete(id2) })

	// 1. Batch disable id1 only
	if err := accountSetStatusBatch([]int64{id1}, "disabled"); err != nil {
		t.Fatalf("accountSetStatusBatch failed: %v", err)
	}
	a1 := accountByID(id1)
	if a1.Status != "disabled" {
		t.Errorf("expected disabled, got %s", a1.Status)
	}
	a2 := accountByID(id2)
	if a2.Status != "enabled" {
		t.Errorf("expected enabled, got %s", a2.Status)
	}

	// 2. Batch disable specific list
	if err := accountSetStatusBatch([]int64{id1, id2}, "disabled"); err != nil {
		t.Fatalf("accountSetStatusBatch all failed: %v", err)
	}
	a2 = accountByID(id2)
	if a2.Status != "disabled" {
		t.Errorf("expected disabled, got %s", a2.Status)
	}

	// 3. Batch enable via HTTP PATCH
	body, _ := json.Marshal(map[string]interface{}{
		"ids":    []int64{id1, id2},
		"status": "enabled",
	})
	req := httptest.NewRequest(http.MethodPatch, "/admin/api/accounts", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handleAdminAccounts(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	a1 = accountByID(id1)
	if a1.Status != "enabled" {
		t.Errorf("expected enabled via HTTP, got %s", a1.Status)
	}

	// 4. Batch proxy binding via HTTP PATCH
	if err := accountSetProxyBatch([]int64{id1, id2}, 0); err != nil {
		t.Fatalf("accountSetProxyBatch failed: %v", err)
	}
	a1 = accountByID(id1)
	if a1.ProxyID != 0 {
		t.Errorf("expected proxy 0, got %d", a1.ProxyID)
	}

	bodyProxy, _ := json.Marshal(map[string]interface{}{
		"ids":      []int64{id1, id2},
		"proxy_id": 0,
	})
	reqProxy := httptest.NewRequest(http.MethodPatch, "/admin/api/accounts", bytes.NewReader(bodyProxy))
	wProxy := httptest.NewRecorder()
	handleAdminAccounts(wProxy, reqProxy)
	if wProxy.Code != 200 {
		t.Fatalf("expected 200 for batch proxy, got %d: %s", wProxy.Code, wProxy.Body.String())
	}
}
