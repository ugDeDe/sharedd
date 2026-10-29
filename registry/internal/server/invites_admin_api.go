package server

// Панельные admin-API приглашений и «Разрешить общий секрет»
// (invitations-migration-contract.md §2-§3). Все под panelAuthorized +
// adminUsersGuard ([users] enabled=false / control нет → 503).

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
)

// inviteTTLDefaults — contract §2: default 72h, допустимо 1..720.
// invite max_uses (addendum §2): 1..1000, дефолт 1.
const (
	inviteTTLDefaultHours = 72
	inviteTTLMinHours     = 1
	inviteTTLMaxHours     = 720
	inviteMaxUsesDefault  = 1
	inviteMaxUsesMin      = 1
	inviteMaxUsesMax      = 1000
)

func (r *Registry) mountInvites(mux *http.ServeMux) {
	mux.HandleFunc("POST /panel/api/admin/invites", r.handleAdminCreateInvite)
	mux.HandleFunc("GET /panel/api/admin/invites", r.handleAdminListInvites)
	mux.HandleFunc("DELETE /panel/api/admin/invites/{id}", r.handleAdminRevokeInvite)
	mux.HandleFunc("GET /panel/api/admin/legacy-secret", r.handleAdminGetLegacySecret)
	mux.HandleFunc("POST /panel/api/admin/legacy-secret", r.handleAdminSetLegacySecret)
}

// handleAdminCreateInvite — POST /panel/api/admin/invites {ttl_hours} →
// 201 {id, invite_url, expires_at, prefix}; полный токен в invite_url
// показывается ОДИН раз.
func (r *Registry) handleAdminCreateInvite(w http.ResponseWriter, req *http.Request) {
	if !r.adminUsersGuard(w, req) {
		return
	}
	var body struct {
		TTLHours int `json:"ttl_hours"`
		MaxUses  int `json:"max_uses"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&body); err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "bad json")
		return
	}
	if body.TTLHours == 0 {
		body.TTLHours = inviteTTLDefaultHours
	}
	if body.TTLHours < inviteTTLMinHours || body.TTLHours > inviteTTLMaxHours {
		controlAPIError(w, http.StatusBadRequest, "validation_failed",
			"ttl_hours must be 1..720")
		return
	}
	if body.MaxUses == 0 {
		body.MaxUses = inviteMaxUsesDefault
	}
	if body.MaxUses < inviteMaxUsesMin || body.MaxUses > inviteMaxUsesMax {
		controlAPIError(w, http.StatusBadRequest, "validation_failed",
			"max_uses must be 1..1000")
		return
	}
	id, token, prefix, expiresAt, err := r.control.CreateInvite(body.TTLHours, body.MaxUses, time.Now())
	if err != nil {
		writeControlErr(w, err)
		return
	}
	mountJSON(w)
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":         id,
		"invite_url": r.inviteURL(token),
		"expires_at": expiresAt,
		"prefix":     prefix,
		"max_uses":   body.MaxUses,
	})
}

// inviteURL — panel.public_url + /signin?invite=<token>; без public_url —
// относительный путь (панель домена всё равно одна).
func (r *Registry) inviteURL(token string) string {
	r.cfgMu.RLock()
	base := r.cfg.Panel.PublicURL
	r.cfgMu.RUnlock()
	return base + "/signin?invite=" + token
}

func (r *Registry) handleAdminListInvites(w http.ResponseWriter, req *http.Request) {
	if !r.adminUsersGuard(w, req) {
		return
	}
	now := time.Now()
	invites, err := r.control.Invites(now)
	if err != nil {
		writeControlErr(w, err)
		return
	}
	out := make([]map[string]any, 0, len(invites))
	for _, iv := range invites {
		out = append(out, map[string]any{
			"id":               iv.ID,
			"prefix":           iv.Prefix,
			"created_at":       iv.CreatedAt,
			"expires_at":       iv.ExpiresAt,
			"status":           iv.Status(now),
			"used_by":          nullInt64(iv.UsedBy),
			"used_by_username": nullString(iv.UsedByName),
			"max_uses":         iv.MaxUses,
			"used_count":       iv.UsedCount,
		})
	}
	mountJSON(w)
	_ = json.NewEncoder(w).Encode(map[string]any{"invites": out})
}

func (r *Registry) handleAdminRevokeInvite(w http.ResponseWriter, req *http.Request) {
	if !r.adminUsersGuard(w, req) {
		return
	}
	id, err := strconv.ParseInt(req.PathValue("id"), 10, 64)
	if err != nil {
		controlAPIError(w, http.StatusNotFound, "not_found", "bad invite id")
		return
	}
	if _, err := r.control.RevokeInvite(id, time.Now()); err != nil {
		writeControlErr(w, err)
		return
	}
	// Идемпотентно: уже отозванное/использованное — тот же 204.
	w.WriteHeader(http.StatusNoContent)
}

// ── «Разрешить общий секрет» ──────────────────────────────────────

// handleAdminGetLegacySecret — GET → {enabled, affected_nodes, as_of};
// ошибка счётчика → affected_nodes: null (contract §3).
func (r *Registry) handleAdminGetLegacySecret(w http.ResponseWriter, req *http.Request) {
	if !r.adminUsersGuard(w, req) {
		return
	}
	resp := map[string]any{
		"enabled": r.legacySharedSecretEnabled(),
		"as_of":   time.Now().UTC().Format(time.RFC3339),
	}
	if n, err := r.affectedNodesWithoutSecret(); err != nil {
		resp["affected_nodes"] = nil
	} else {
		resp["affected_nodes"] = n
	}
	mountJSON(w)
	_ = json.NewEncoder(w).Encode(resp)
}

// handleAdminSetLegacySecret — POST {enabled} → пишет settings, затем
// зеркало в памяти. Любая неудача записи → 5xx, память НЕ меняем
// (fail-closed, частичный успех невозможен — один UPDATE).
func (r *Registry) handleAdminSetLegacySecret(w http.ResponseWriter, req *http.Request) {
	if !r.adminUsersGuard(w, req) {
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&body); err != nil || body.Enabled == nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "enabled (bool) required")
		return
	}
	if err := r.control.SetLegacySecret(*body.Enabled); err != nil {
		controlAPIError(w, http.StatusInternalServerError, "internal", "failed to persist legacy secret setting")
		return
	}
	r.legacyShared.Store(*body.Enabled)
	mountJSON(w)
	_ = json.NewEncoder(w).Encode(map[string]any{"enabled": *body.Enabled, "persisted": true})
}

// affectedNodesWithoutSecret — живые кандидаты без персонального секрета
// (contract §1: счётчик миграции). nil при ошибке — контракт требует null.
func (r *Registry) affectedNodesWithoutSecret() (int, error) {
	if r.control == nil {
		return 0, errors.New("control unavailable")
	}
	r.mu.RLock()
	ids := make([]string, 0, len(r.state.Candidates))
	for id := range r.state.Candidates {
		ids = append(ids, id)
	}
	r.mu.RUnlock()
	return r.control.CountNodesWithoutSecret(ids)
}

func nullInt64(v sql.NullInt64) any {
	if v.Valid {
		return v.Int64
	}
	return nil
}

func nullString(v sql.NullString) any {
	if v.Valid {
		return v.String
	}
	return nil
}
