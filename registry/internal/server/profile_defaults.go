package server

// Личные значения по умолчанию для профиля: API и общий merge для всех
// путей установки. Приоритет: явные значения формы > личные defaults >
// непустые значения профиля > значения регистратора.

import (
	"errors"
	"net/http"
	"strings"

	"sharedd/registry/internal/control"
)

func (r *Registry) mountProfileDefaults(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/me/profile-defaults", r.requireSession(r.handleProfileDefaultsGet))
	mux.HandleFunc("PUT /api/me/profile-defaults", r.requireSession(r.requireCSRF(r.handleProfileDefaultsPut)))
}

func (r *Registry) handleProfileDefaultsGet(w http.ResponseWriter, req *http.Request) {
	u := sessionFrom(req).user
	profileID, ok := parseID(req.URL.Query().Get("profile_id"))
	if !ok {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "profile_id is required")
		return
	}
	if _, err := r.selectableProfileVersion(u, profileID); err != nil {
		writeProfileSelectErr(w, err)
		return
	}
	d, err := r.control.GetUserProfileDefaults(u.ID, profileID)
	if errors.Is(err, control.ErrNotFound) {
		writeCabinetJSON(w, http.StatusOK, control.UserProfileDefaults{
			ProfileID: profileID, Vars: map[string]string{}, Revision: 0,
		})
		return
	}
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	writeCabinetJSON(w, http.StatusOK, d)
}

func (r *Registry) handleProfileDefaultsPut(w http.ResponseWriter, req *http.Request) {
	u := sessionFrom(req).user
	var body struct {
		ProfileID        int64             `json:"profile_id"`
		Vars             map[string]string `json:"vars"`
		ExpectedRevision int64             `json:"expected_revision"`
	}
	if err := decodeCabinetJSON(w, req, &body); err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "bad json")
		return
	}
	if body.ProfileID <= 0 {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "profile_id is required")
		return
	}
	pv, err := r.selectableProfileVersion(u, body.ProfileID)
	if err != nil {
		writeProfileSelectErr(w, err)
		return
	}
	if pv.Format != "" && pv.Format != control.FormatText {
		controlAPIError(w, http.StatusUnprocessableEntity, "profile_incompatible", "defaults are supported for text profiles only")
		return
	}
	secret := map[string]bool{}
	for _, name := range control.SecretVarNames(pv.VarsSchema) {
		secret[name] = true
	}
	for name := range body.Vars {
		n := strings.TrimSpace(name)
		if secret[n] || isConventionalSecretVar(n) {
			controlAPIError(w, http.StatusBadRequest, "validation_failed",
				"секретные значения нельзя сохранять как значения по умолчанию")
			return
		}
	}
	d, err := r.control.SetUserProfileDefaults(u.ID, body.ProfileID, body.Vars, body.ExpectedRevision)
	if errors.Is(err, control.ErrRevisionConflict) {
		controlAPIError(w, http.StatusConflict, "revision_conflict", "значения уже изменились; обновите и повторите")
		return
	}
	if errors.Is(err, control.ErrValidation) {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	writeCabinetJSON(w, http.StatusOK, d)
}

func isConventionalSecretVar(name string) bool {
	switch name {
	case "proxy_secret", "secret", "password", "authkey", "node_secret", "ssh_key", "private_key", "token":
		return true
	}
	return false
}

// mergeInstallVars — единая точка приоритета для всех установок.
func (r *Registry) mergeInstallVars(userID int64, pv control.ProfileVersion, userVars map[string]any) (map[string]any, error) {
	merged := make(map[string]any, len(userVars)+8)
	if r.control != nil && userID > 0 {
		d, err := r.control.GetUserProfileDefaults(userID, pv.ProfileID)
		switch {
		case err == nil:
			for k, v := range d.Vars {
				merged[k] = v
			}
		case errors.Is(err, control.ErrNotFound):
			// нет личных значений — обычный путь
		default:
			return nil, err
		}
	}
	for k, v := range userVars {
		merged[k] = v
	}
	return control.MergeProfileVars(pv.VarsSchema, merged)
}
