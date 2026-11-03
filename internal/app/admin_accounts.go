package app

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

func accountAdminView(a CookieAccount) map[string]interface{} {
	view := cookieAcctView(a)
	view["email"], view["profile_id"], view["login_state"], view["last_sync_at"] = "", "", "cookie_only", int64(0)
	if b, err := browserAccountByID(a.ID); err == nil && b != nil {
		view["email"], view["profile_id"], view["login_state"], view["last_sync_at"] = b.Email, b.ProfileID, b.LoginState, b.LastSyncAt
	}
	view["selected"] = selectedAccountID() == a.ID
	view["quota"] = readAccountQuota(a.ID)
	return view
}

func handleAdminAccounts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		items := make([]map[string]interface{}, 0)
		ready, poolReady, quotaLimited := 0, 0, 0
		for _, a := range accountList() {
			items = append(items, accountAdminView(a))
			if accountReady(&a) {
				ready++
				if quotaEligible(a.ID) {
					poolReady++
				}
			}
			if readAccountQuota(a.ID).State == "limited" {
				quotaLimited++
			}
		}
		total, enabled := accountCount()
		writeJSON(w, 200, map[string]interface{}{"items": items, "total": total, "enabled": enabled, "ready": ready, "pool_ready": poolReady, "quota_limited": quotaLimited, "account_id": selectedAccountID()})
	case http.MethodPost:
		var p struct{ Label, Note string }
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			writeJSON(w, 400, map[string]string{"error": "请求格式无效"})
			return
		}
		id, err := accountCreateBrowser(p.Label, p.Note)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]interface{}{"id": id, "account": accountAdminView(*accountByID(id))})
	case http.MethodPatch:
		var p struct {
			IDs     []int64 `json:"ids"`
			Status  *string `json:"status"`
			ProxyID *int64  `json:"proxy_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			writeJSON(w, 400, map[string]string{"error": "请求格式无效"})
			return
		}
		if p.Status != nil {
			if err := accountSetStatusBatch(p.IDs, *p.Status); err != nil {
				writeJSON(w, 400, map[string]string{"error": err.Error()})
				return
			}
		}
		if p.ProxyID != nil {
			if *p.ProxyID < 0 || (*p.ProxyID > 0 && !proxyUsableByID(*p.ProxyID)) {
				writeJSON(w, 400, map[string]string{"error": "所选出口不可用"})
				return
			}
			if err := accountSetProxyBatch(p.IDs, *p.ProxyID); err != nil {
				writeJSON(w, 500, map[string]string{"error": err.Error()})
				return
			}
		}
		total, enabled := accountCount()
		writeJSON(w, 200, map[string]interface{}{"ok": true, "total": total, "enabled": enabled})
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

func handleAdminAccountSelection(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut {
		var p struct {
			AccountID *int64 `json:"account_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil || p.AccountID == nil {
			writeJSON(w, 400, map[string]string{"error": "请选择账号或自动模式"})
			return
		}
		if err := setSelectedAccount(*p.AccountID); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
	} else if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	writeJSON(w, 200, map[string]interface{}{"account_id": selectedAccountID()})
}

// 新旧管理入口共用关闭流程，活动档案关闭失败时保留账号记录。
func deleteAdminAccount(w http.ResponseWriter, r *http.Request, id int64) {
	a := accountByID(id)
	if a == nil {
		writeJSON(w, 404, map[string]string{"error": "账号不存在"})
		return
	}
	if b, err := browserAccountByID(id); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	} else if b != nil {
		if _, err := browserAccountAction(r.Context(), a, "close"); err != nil {
			writeJSON(w, 409, map[string]string{"error": err.Error()})
			return
		}
	}
	if err := accountDelete(id); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func handleAdminAccountItem(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/admin/api/accounts/"), "/")
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 || len(parts) > 2 {
		writeJSON(w, 400, map[string]string{"error": "账号编号无效"})
		return
	}
	a := accountByID(id)
	if a == nil {
		writeJSON(w, 404, map[string]string{"error": "账号不存在"})
		return
	}
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, 200, accountAdminView(*a))
		case http.MethodDelete:
			deleteAdminAccount(w, r, id)
		case http.MethodPatch:
			var p struct {
				Label, Note, Status *string
				ProxyID             *int64 `json:"proxy_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				writeJSON(w, 400, map[string]string{"error": "请求格式无效"})
				return
			}
			if p.Status != nil {
				if err := accountSetStatus(id, *p.Status); err != nil {
					writeJSON(w, 400, map[string]string{"error": err.Error()})
					return
				}
			}
			if p.ProxyID != nil {
				if *p.ProxyID < 0 || (*p.ProxyID > 0 && !proxyUsableByID(*p.ProxyID)) {
					writeJSON(w, 400, map[string]string{"error": "所选出口不可用"})
					return
				}
				bindAccountProxy(id, *p.ProxyID)
			}
			if p.Label != nil {
				a.Label = *p.Label
			}
			if p.Note != nil {
				a.Note = *p.Note
			}
			if err := accountUpdateMeta(id, a.Label, a.Note); err != nil {
				writeJSON(w, 500, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, 200, accountAdminView(*accountByID(id)))
		default:
			writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		}
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	op := parts[1]
	if op == "quota" {
		err := refreshAccountQuota(r.Context(), id, nil)
		current := accountByID(id)
		if current == nil {
			writeJSON(w, 404, map[string]any{"ok": false, "error": "账号不存在"})
			return
		}
		result := map[string]any{"ok": err == nil, "account": accountAdminView(*current)}
		if err != nil {
			result["error"] = quotaErrorSummary(err)
			writeJSON(w, 409, result)
		} else {
			writeJSON(w, 200, result)
		}
		return
	}
	if op != "login" && op != "confirm-login" && op != "refresh" && op != "open" {
		writeJSON(w, 404, map[string]string{"error": "账号操作不存在"})
		return
	}
	if _, err := browserAccountAction(r.Context(), a, op); err != nil {
		writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]interface{}{"ok": true, "account": accountAdminView(*accountByID(id))})
}
