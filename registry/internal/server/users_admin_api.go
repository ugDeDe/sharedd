package server

// Панельные admin user-API (docs/api-contract.md §2): пользователи,
// authkey-ротация, освобождение мест. Все под существующим panelAuthorized
// (Bearer panel.token). [users] enabled=false → 503 users_disabled;
// control-БД не открылась → 503 control_unavailable (fail-closed).

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"modernc.org/sqlite"
	"sharedd/registry/internal/control"
)

// userJSON — формат пользователя в ответах admin-API. used_nodes/disabled
// БЕЗ omitempty: контракт §2 требует их присутствия даже нулевыми в list/patch
// (ранее omitempty молча выкидывал нулевые значения — расхождение с контрактом).
type userJSON struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	Role      string `json:"role"`
	MaxNodes  int    `json:"max_nodes"`
	UsedNodes int    `json:"used_nodes"`
	Disabled  bool   `json:"disabled"`
}

func toUserJSON(u control.UserWithUsage) userJSON {
	return userJSON{
		ID:        u.ID,
		Username:  u.Username,
		Role:      u.Role,
		MaxNodes:  u.MaxNodes,
		UsedNodes: u.UsedNodes,
		Disabled:  u.Disabled,
	}
}

// writeControlErr — маппинг sentinel-ошибок control на статусы контракта §5.
func writeControlErr(w http.ResponseWriter, err error, operation ...string) {
	if len(operation) > 0 {
		logProfileControlErr(operation[0], err)
	}
	switch {
	case errors.Is(err, control.ErrValidation):
		controlAPIError(w, http.StatusBadRequest, "validation_failed", err.Error())
	case errors.Is(err, control.ErrDuplicate):
		controlAPIError(w, http.StatusConflict, "duplicate", "username already exists")
	case errors.Is(err, control.ErrDuplicateName):
		controlAPIError(w, http.StatusConflict, "duplicate_name", "Профиль с таким именем уже существует")
	case errors.Is(err, control.ErrNotFound):
		controlAPIError(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, control.ErrLastAdmin):
		controlAPIError(w, http.StatusBadRequest, "last_admin", "cannot disable the last active admin")
	case errors.Is(err, control.ErrLegacyProtected):
		controlAPIError(w, http.StatusBadRequest, "legacy_protected", "legacy user cannot be modified")
	case errors.Is(err, control.ErrInviteRequired):
		controlAPIError(w, http.StatusForbidden, "invite_required", "invite required")
	case errors.Is(err, control.ErrInviteInvalid):
		controlAPIError(w, http.StatusForbidden, "invite_invalid", "unknown or malformed invite")
	case errors.Is(err, control.ErrInviteExpired):
		controlAPIError(w, http.StatusForbidden, "invite_expired", "invite has expired")
	case errors.Is(err, control.ErrInviteUsed):
		controlAPIError(w, http.StatusForbidden, "invite_used", "invite has already been used")
	case errors.Is(err, control.ErrInviteRevoked):
		controlAPIError(w, http.StatusForbidden, "invite_revoked", "invite has been revoked")
	default:
		var dbErr *sqlite.Error
		if len(operation) > 0 && errors.As(err, &dbErr) && dbErr.Code()&0xff == 19 { // SQLITE_CONSTRAINT
			controlAPIError(w, http.StatusConflict, "constraint_conflict", "Изменение профиля нарушает ограничение хранилища")
			return
		}
		controlAPIError(w, http.StatusInternalServerError, "internal", "control db error")
	}
}

// SQLite diagnostics contain schema names/codes, not bound values. Redact
// quoted text too: validation errors can quote user-supplied variable names.
var controlErrorQuoted = regexp.MustCompile(`"(?:\\.|[^"\\])*"|'(?:''|[^'])*'`)

func logProfileControlErr(operation string, err error) {
	message := controlErrorQuoted.ReplaceAllString(err.Error(), "[redacted]")
	message = strings.Map(func(c rune) rune {
		if c < 32 || c == 127 {
			return ' '
		}
		return c
	}, message)
	if len(message) > 1024 {
		message = message[:1024] + "…"
	}
	var dbErr *sqlite.Error
	if errors.As(err, &dbErr) {
		log.Printf("control operation=%s sqlite_code=%d error=%s", operation, dbErr.Code(), message)
		return
	}
	log.Printf("control operation=%s error_type=%T error=%s", operation, err, message)
}

func (r *Registry) adminUsersGuard(w http.ResponseWriter, req *http.Request) bool {
	if !r.panelAuthorized(req) {
		controlAPIError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return false
	}
	if !r.usersEnabled() {
		controlAPIError(w, http.StatusServiceUnavailable, "users_disabled", "user mode is disabled")
		return false
	}
	if r.control == nil {
		log.Printf("control operation=%s %s guard: store is nil", req.Method, req.Pattern)
		r.writeControlUnavailable(w)
		return false
	}
	return true
}

func mountJSON(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
}

// mountAdminUsers подключает /panel/api/admin/*. Регистрируется всегда:
// гвард решает 401/503/далее. Точные паттерны побеждают GET /panel/ сабтри.
func (r *Registry) mountAdminUsers(mux *http.ServeMux) {
	mux.HandleFunc("POST /panel/api/admin/users", r.handleAdminCreateUser)
	mux.HandleFunc("GET /panel/api/admin/users", r.handleAdminListUsers)
	mux.HandleFunc("PATCH /panel/api/admin/users/{id}", r.handleAdminPatchUser)
	mux.HandleFunc("DELETE /panel/api/admin/users/{id}", r.handleAdminDeleteUser)
	mux.HandleFunc("POST /panel/api/admin/users/{id}/authkey/rotate", r.handleAdminRotateAuthkey)
	mux.HandleFunc("DELETE /panel/api/admin/users/{id}/authkey", r.handleAdminRevokeAuthkey)
	mux.HandleFunc("POST /panel/api/admin/users/{id}/release-node", r.handleAdminReleaseNode)
}

func (r *Registry) handleAdminCreateUser(w http.ResponseWriter, req *http.Request) {
	if !r.adminUsersGuard(w, req) {
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
		MaxNodes int    `json:"max_nodes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&body); err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "bad json")
		return
	}
	if body.Role == "" {
		body.Role = "user" // дефолт роли: username+password достаточно
	}
	// [users] default_max_nodes: админ может не задавать max_nodes — тогда
	// новый пользователь получает настроенную квоту (0 = прежнее поведение).
	if body.MaxNodes == 0 {
		body.MaxNodes = r.cfg.Users.DefaultMaxNodes
	}
	u, err := r.control.CreateUser(body.Username, body.Password, body.Role, body.MaxNodes)
	if err != nil {
		writeControlErr(w, err)
		return
	}
	mountJSON(w)
	_ = json.NewEncoder(w).Encode(toUserJSON(control.UserWithUsage{User: u}))
}

func (r *Registry) handleAdminListUsers(w http.ResponseWriter, req *http.Request) {
	if !r.adminUsersGuard(w, req) {
		return
	}
	users, err := r.control.Users()
	if err != nil {
		writeControlErr(w, err)
		return
	}
	out := make([]userJSON, 0, len(users))
	for _, u := range users {
		out = append(out, toUserJSON(u))
	}
	mountJSON(w)
	_ = json.NewEncoder(w).Encode(map[string]any{"users": out})
}

func (r *Registry) handleAdminPatchUser(w http.ResponseWriter, req *http.Request) {
	if !r.adminUsersGuard(w, req) {
		return
	}
	id, err := strconv.ParseInt(req.PathValue("id"), 10, 64)
	if err != nil {
		controlAPIError(w, http.StatusNotFound, "not_found", "bad user id")
		return
	}
	var body struct {
		MaxNodes *int   `json:"max_nodes"`
		Disabled *bool  `json:"disabled"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&body); err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "bad json")
		return
	}
	patch := control.UserPatch{MaxNodes: body.MaxNodes, Disabled: body.Disabled}
	if body.Password != "" {
		patch.Password = &body.Password
	}
	u, err := r.control.UpdateUser(id, patch)
	if err != nil {
		writeControlErr(w, err)
		return
	}
	mountJSON(w)
	_ = json.NewEncoder(w).Encode(toUserJSON(u))
}

func (r *Registry) handleAdminRotateAuthkey(w http.ResponseWriter, req *http.Request) {
	if !r.adminUsersGuard(w, req) {
		return
	}
	id, err := strconv.ParseInt(req.PathValue("id"), 10, 64)
	if err != nil {
		controlAPIError(w, http.StatusNotFound, "not_found", "bad user id")
		return
	}
	key, err := r.control.RotateAuthkey(id)
	if err != nil {
		writeControlErr(w, err)
		return
	}
	mountJSON(w)
	_ = json.NewEncoder(w).Encode(map[string]string{"authkey": key}) // полный ключ — один раз
}

// handleAdminDeleteUser — DELETE /panel/api/admin/users/{id}: полное удаление
// пользователя. Отказ для системного legacy и последнего активного админа —
// с русским сообщением. Успех — 204.
func (r *Registry) handleAdminDeleteUser(w http.ResponseWriter, req *http.Request) {
	if !r.adminUsersGuard(w, req) {
		return
	}
	id, err := strconv.ParseInt(req.PathValue("id"), 10, 64)
	if err != nil {
		controlAPIError(w, http.StatusNotFound, "not_found", "bad user id")
		return
	}
	if err := r.control.DeleteUser(id); err != nil {
		switch {
		case errors.Is(err, control.ErrLegacyProtected):
			controlAPIError(w, http.StatusBadRequest, "legacy_protected", "Системного пользователя legacy удалить нельзя.")
		case errors.Is(err, control.ErrLastAdmin):
			controlAPIError(w, http.StatusBadRequest, "last_admin", "Нельзя удалить последнего активного администратора.")
		default:
			writeControlErr(w, err, "admin.users.delete")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (r *Registry) handleAdminRevokeAuthkey(w http.ResponseWriter, req *http.Request) {
	if !r.adminUsersGuard(w, req) {
		return
	}
	id, err := strconv.ParseInt(req.PathValue("id"), 10, 64)
	if err != nil {
		controlAPIError(w, http.StatusNotFound, "not_found", "bad user id")
		return
	}
	if err := r.control.RevokeAuthkeys(id); err != nil {
		writeControlErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (r *Registry) handleAdminReleaseNode(w http.ResponseWriter, req *http.Request) {
	if !r.adminUsersGuard(w, req) {
		return
	}
	id, err := strconv.ParseInt(req.PathValue("id"), 10, 64)
	if err != nil {
		controlAPIError(w, http.StatusNotFound, "not_found", "bad user id")
		return
	}
	var body struct {
		NodeID string `json:"node_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&body); err != nil || body.NodeID == "" {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "node_id required")
		return
	}
	// Освободить можно только ноду, привязанную к УКАЗАННОМУ пользователю.
	ownerID, _, ok, err := r.control.NodeOwner(body.NodeID)
	if err != nil {
		writeControlErr(w, err)
		return
	}
	if !ok || ownerID != id {
		controlAPIError(w, http.StatusNotFound, "not_found", "node is not bound to this user")
		return
	}
	if _, err := r.control.ReleaseNode(body.NodeID); err != nil {
		writeControlErr(w, err)
		return
	}
	mountJSON(w)
	_ = json.NewEncoder(w).Encode(map[string]any{"released": true, "node_id": body.NodeID})
}
