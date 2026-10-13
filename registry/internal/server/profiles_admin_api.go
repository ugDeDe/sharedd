package server

// Профили установки: admin CRUD + версии (addendum-profiles-vps.md §3).
// Правка шагов создаёт НОВУЮ версию (история не перезаписывается) и сбрасывает
// published — оператор публикует явно. Переименование (PUT с непустым name) —
// метаданные: версию НЕ создаёт и published не сбрасывает. Профиль — текст
// шагов (shell-команды), выполняемых по SSH на VPS. Все под panelAuthorized +
// adminUsersGuard.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"sharedd/registry/internal/control"
)

// maxStepsBytes — потолок размера текста шагов (addendum §3: ≤ 64 KiB).
const maxStepsBytes = 64 << 10

// profileAdminJSON — профиль с последней версией (steps/vars_schema).
type profileAdminJSON struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Published   bool   `json:"published"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
	Steps       string `json:"steps,omitempty"`
	VarsSchema  string `json:"vars_schema,omitempty"`
	// Versions — число версий профиля (addendum §7.5).
	Versions int `json:"versions"`
}

func (r *Registry) profileAdminView(p control.Profile) (profileAdminJSON, error) {
	out := profileAdminJSON{
		ID: p.ID, Name: p.Name, Description: p.Description, Published: p.Published,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
	if v, err := r.control.ProfileVersion(p.ID, 0); err == nil {
		out.Steps, out.VarsSchema = v.Steps, v.VarsSchema
	} else if !errors.Is(err, control.ErrNotFound) {
		return out, fmt.Errorf("read latest profile version: %w", err)
	}
	if n, err := r.control.CountProfileVersions(p.ID); err == nil {
		out.Versions = n
	} else {
		return out, fmt.Errorf("count profile versions: %w", err)
	}
	return out, nil
}

func (r *Registry) writeProfileView(w http.ResponseWriter, p control.Profile, status int, operation string) {
	out, err := r.profileAdminView(p)
	if err != nil {
		writeControlErr(w, err, operation+".view")
		return
	}
	mountJSON(w)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(out)
}

type profilePayload struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	Published   *bool   `json:"published"`
	Steps       *string `json:"steps"`
	VarsSchema  *string `json:"vars_schema"`
}

// validateStepsInput — размер (общая для admin и user). Собственного
// шаблонизатора/YAML больше нет: шаги не парсятся на сохранении.
func validateStepsInput(steps string) error {
	if steps == "" {
		return errors.New("steps are required")
	}
	if len(steps) > maxStepsBytes {
		return errors.New("steps exceed 64 KiB")
	}
	return nil
}

func (r *Registry) mountAdminProfiles(mux *http.ServeMux) {
	mux.HandleFunc("GET /panel/api/admin/profiles", r.handleAdminListProfiles)
	mux.HandleFunc("POST /panel/api/admin/profiles", r.handleAdminCreateProfile)
	mux.HandleFunc("PUT /panel/api/admin/profiles/{id}", r.handleAdminUpdateProfile)
	mux.HandleFunc("DELETE /panel/api/admin/profiles/{id}", r.handleAdminDeleteProfile)
}

func (r *Registry) handleAdminListProfiles(w http.ResponseWriter, req *http.Request) {
	if !r.adminUsersGuard(w, req) {
		return
	}
	profiles, err := r.control.AllProfiles()
	if err != nil {
		writeControlErr(w, err, "admin.profiles.list")
		return
	}
	out := make([]profileAdminJSON, 0, len(profiles))
	for _, p := range profiles {
		// Админка показывает только системные/опубликованные профили
		// (created_by=0). Личные копии пользователей (created_by≠0) видны
		// только их владельцу через /api/me/profiles.
		if p.CreatedBy != 0 {
			continue
		}
		view, err := r.profileAdminView(p)
		if err != nil {
			writeControlErr(w, err, "admin.profiles.list.view")
			return
		}
		out = append(out, view)
	}
	mountJSON(w)
	_ = json.NewEncoder(w).Encode(map[string]any{"profiles": out})
}

func (r *Registry) handleAdminCreateProfile(w http.ResponseWriter, req *http.Request) {
	if !r.adminUsersGuard(w, req) {
		return
	}
	var body profilePayload
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 128<<10)).Decode(&body); err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "bad json")
		return
	}
	if body.Name == nil || *body.Name == "" {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "name is required")
		return
	}
	if body.Steps == nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "steps are required")
		return
	}
	if err := validateStepsInput(*body.Steps); err != nil {
		controlAPIError(w, http.StatusBadRequest, "steps_rejected", err.Error())
		return
	}
	varsSchema := ""
	if body.VarsSchema != nil {
		varsSchema = *body.VarsSchema
	}
	if err := control.ValidateProfileVarsSchema(varsSchema); err != nil {
		writeControlErr(w, err, "admin.profiles.create.validate_vars")
		return
	}
	description := ""
	if body.Description != nil {
		description = *body.Description
	}
	p, err := r.control.CreateProfile(*body.Name, description, 0)
	if err != nil {
		writeControlErr(w, err, "admin.profiles.create.metadata")
		return
	}
	if _, err := r.control.AddProfileVersion(p.ID, *body.Steps, varsSchema); err != nil {
		if cleanupErr := r.control.DeleteProfile(p.ID); cleanupErr != nil {
			logProfileControlErr("admin.profiles.create.cleanup", cleanupErr)
		}
		writeControlErr(w, err, "admin.profiles.create.version")
		return
	}
	if body.Published != nil && *body.Published {
		if p, err = r.control.PublishProfile(p.ID); err != nil {
			writeControlErr(w, err, "admin.profiles.create.publish")
			return
		}
	}
	r.writeProfileView(w, p, http.StatusCreated, "admin.profiles.create")
}

func (r *Registry) handleAdminUpdateProfile(w http.ResponseWriter, req *http.Request) {
	if !r.adminUsersGuard(w, req) {
		return
	}
	id, ok := parseID(req.PathValue("id"))
	if !ok {
		controlAPIError(w, http.StatusNotFound, "not_found", "bad profile id")
		return
	}
	var body profilePayload
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 128<<10)).Decode(&body); err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "bad json")
		return
	}
	if body.Steps != nil {
		if err := validateStepsInput(*body.Steps); err != nil {
			controlAPIError(w, http.StatusBadRequest, "steps_rejected", err.Error())
			return
		}
	}
	if body.VarsSchema != nil {
		if err := control.ValidateProfileVarsSchema(*body.VarsSchema); err != nil {
			writeControlErr(w, err, "admin.profiles.update.validate_vars")
			return
		}
	}
	if _, err := r.control.Profile(id); err != nil {
		writeControlErr(w, err, "admin.profiles.update.lookup")
		return
	}
	// Переименование — метаданные: НЕ создаёт новую версию (в отличие от
	// steps/vars_schema ниже). Коллизия → 409 duplicate_name.
	if body.Name != nil {
		if _, err := r.control.RenameProfile(id, *body.Name); err != nil {
			writeControlErr(w, err, "admin.profiles.update.rename")
			return
		}
	}
	if body.Steps != nil || body.VarsSchema != nil {
		v, err := r.control.ProfileVersion(id, 0)
		if err != nil && !errors.Is(err, control.ErrNotFound) {
			writeControlErr(w, err, "admin.profiles.update.latest_version")
			return
		}
		if body.Steps != nil {
			v.Steps = *body.Steps
		}
		if body.VarsSchema != nil {
			v.VarsSchema = *body.VarsSchema
		}
		if _, err := r.control.AddProfileVersion(id, v.Steps, v.VarsSchema); err != nil {
			writeControlErr(w, err, "admin.profiles.update.version")
			return
		}
	}
	if body.Description != nil {
		if _, err := r.control.UpdateProfileDescription(id, *body.Description); err != nil {
			writeControlErr(w, err, "admin.profiles.update.description")
			return
		}
	}
	if body.Published != nil {
		if _, err := r.control.SetProfilePublished(id, *body.Published); err != nil {
			writeControlErr(w, err, "admin.profiles.update.publish")
			return
		}
	}
	p, err := r.control.Profile(id)
	if err != nil {
		writeControlErr(w, err, "admin.profiles.update.result")
		return
	}
	r.writeProfileView(w, p, http.StatusOK, "admin.profiles.update")
}

func (r *Registry) handleAdminDeleteProfile(w http.ResponseWriter, req *http.Request) {
	if !r.adminUsersGuard(w, req) {
		return
	}
	id, ok := parseID(req.PathValue("id"))
	if !ok {
		controlAPIError(w, http.StatusNotFound, "not_found", "bad profile id")
		return
	}
	if err := r.control.DeleteProfile(id); err != nil {
		writeControlErr(w, err, "admin.profiles.delete")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
