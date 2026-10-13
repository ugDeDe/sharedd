package server

// Пользовательские профили установки (addendum-profiles-vps.md §3):
// GET/POST /api/me/profiles, PUT/DELETE /api/me/profiles/{id} — только свои,
// published=false (локальные), лимит [provisioning] max_user_profiles.
// POST /api/me/profiles/{id}/copy — копия видимого профиля (опубликованного
// ИЛИ своего) в свой черновик.
// Отключаемо флагом user_profiles_enabled.

import (
	"errors"
	"fmt"
	"net/http"

	"sharedd/registry/internal/control"
)

// defaultMaxUserProfiles — если конфиг не задан (0) — контрактный дефолт 5.
const defaultMaxUserProfiles = 5

func (r *Registry) userProfilesEnabled() bool {
	return resolveUserProfilesEnabled(r.cfg.Provisioning.UserProfilesEnabled)
}

func (r *Registry) maxUserProfiles() int {
	if m := r.cfg.Provisioning.MaxUserProfiles; m > 0 {
		return m
	}
	return defaultMaxUserProfiles
}

func (r *Registry) mountMeProfiles(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/me/profiles", r.requireSession(r.handleMeProfiles))
	mux.HandleFunc("POST /api/me/profiles", r.requireSession(r.requireCSRF(r.handleMeCreateProfile)))
	mux.HandleFunc("PUT /api/me/profiles/{id}", r.requireSession(r.requireCSRF(r.handleMeUpdateProfile)))
	mux.HandleFunc("DELETE /api/me/profiles/{id}", r.requireSession(r.requireCSRF(r.handleMeDeleteProfile)))
	mux.HandleFunc("POST /api/me/profiles/{id}/copy", r.requireSession(r.requireCSRF(r.handleMeCopyProfile)))
}

func (r *Registry) handleMeCreateProfile(w http.ResponseWriter, req *http.Request) {
	if !r.userProfilesEnabled() {
		controlAPIError(w, http.StatusForbidden, "user_profiles_disabled", "user profiles are disabled")
		return
	}
	u := sessionFrom(req).user
	var body profilePayload
	if err := decodeCabinetJSON(w, req, &body); err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "bad json")
		return
	}
	if body.Name == nil || *body.Name == "" || body.Steps == nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "name and steps are required")
		return
	}
	if err := validateStepsInput(*body.Steps); err != nil {
		controlAPIError(w, http.StatusBadRequest, "steps_rejected", err.Error())
		return
	}
	n, err := r.control.CountUserProfiles(u.ID)
	if err != nil {
		writeControlErr(w, err, "user.profiles.create.count")
		return
	}
	if n >= r.maxUserProfiles() {
		controlAPIError(w, http.StatusForbidden, "profile_limit_reached", "max_user_profiles reached")
		return
	}
	varsSchema := ""
	if body.VarsSchema != nil {
		varsSchema = *body.VarsSchema
	}
	if err := control.ValidateProfileVarsSchema(varsSchema); err != nil {
		writeControlErr(w, err, "user.profiles.create.validate_vars")
		return
	}
	description := ""
	if body.Description != nil {
		description = *body.Description
	}
	p, err := r.control.CreateProfile(*body.Name, description, u.ID)
	if err != nil {
		writeControlErr(w, err, "user.profiles.create.metadata")
		return
	}
	if _, err := r.control.AddProfileVersion(p.ID, *body.Steps, varsSchema); err != nil {
		if cleanupErr := r.control.DeleteProfile(p.ID); cleanupErr != nil {
			logProfileControlErr("user.profiles.create.cleanup", cleanupErr)
		}
		writeControlErr(w, err, "user.profiles.create.version")
		return
	}
	r.writeProfileView(w, p, http.StatusCreated, "user.profiles.create")
}

// copyProfileSuffix — подпись копии профиля.
const copyProfileSuffix = " (моя копия)"

// maxCopyNameAttempts — предел подбора уникального имени («X (моя копия) -2»,
// -3…); после него — 409 duplicate_name (гонки с параллельным созданием).
const maxCopyNameAttempts = 100

// handleMeCopyProfile — POST /api/me/profiles/{id}/copy: создаёт у себя копию
// видимого профиля (опубликованного ИЛИ своего; чужой неопубликованный/несуществующий
// → 404). Шаги и vars_schema — дословно из последней видимой версии (как в
// selectableProfileVersion); секретных ЗНАЧЕНИЙ в копии нет — vars_schema
// хранит только объявления (флаги/дефолты), которые профиль и так виден
// своим зрителям. Лимит max_user_profiles; published=false, owner=пользователь.
func (r *Registry) handleMeCopyProfile(w http.ResponseWriter, req *http.Request) {
	if !r.userProfilesEnabled() {
		controlAPIError(w, http.StatusForbidden, "user_profiles_disabled", "Пользовательские профили отключены администратором.")
		return
	}
	u := sessionFrom(req).user
	id, ok := parseID(req.PathValue("id"))
	if !ok {
		controlAPIError(w, http.StatusNotFound, "not_found", "bad profile id")
		return
	}
	src, err := r.control.Profile(id)
	if err != nil {
		writeControlErr(w, err, "user.profiles.copy.lookup")
		return
	}
	if !src.Published && src.CreatedBy != u.ID {
		writeControlErr(w, control.ErrNotFound, "user.profiles.copy.visible") // чужой → 404
		return
	}
	pv, err := r.control.ProfileVersion(id, 0)
	if err != nil {
		writeControlErr(w, err, "user.profiles.copy.version") // без версий → 404
		return
	}
	n, err := r.control.CountUserProfiles(u.ID)
	if err != nil {
		writeControlErr(w, err, "user.profiles.copy.count")
		return
	}
	if n >= r.maxUserProfiles() {
		controlAPIError(w, http.StatusForbidden, "profile_limit_reached", "max_user_profiles reached")
		return
	}
	// Имя: «X (моя копия)», при коллизии «X (моя копия) -2», -3… — до предела.
	p := src
	p.Name = src.Name + copyProfileSuffix
	for attempt := 2; ; attempt++ {
		created, err := r.control.CreateProfile(p.Name, src.Description, u.ID)
		if err == nil {
			p = created
			break
		}
		if !errors.Is(err, control.ErrDuplicateName) {
			writeControlErr(w, err, "user.profiles.copy.metadata")
			return
		}
		if attempt > maxCopyNameAttempts {
			writeControlErr(w, err, "user.profiles.copy.name") // 409 duplicate_name
			return
		}
		p.Name = fmt.Sprintf("%s%s -%d", src.Name, copyProfileSuffix, attempt)
	}
	if _, err := r.control.AddProfileVersion(p.ID, pv.Steps, pv.VarsSchema); err != nil {
		if cleanupErr := r.control.DeleteProfile(p.ID); cleanupErr != nil {
			logProfileControlErr("user.profiles.copy.cleanup", cleanupErr)
		}
		writeControlErr(w, err, "user.profiles.copy.new_version")
		return
	}
	r.writeProfileView(w, p, http.StatusCreated, "user.profiles.copy")
}

func (r *Registry) handleMeUpdateProfile(w http.ResponseWriter, req *http.Request) {
	if !r.userProfilesEnabled() {
		controlAPIError(w, http.StatusForbidden, "user_profiles_disabled", "user profiles are disabled")
		return
	}
	u := sessionFrom(req).user
	id, ok := parseID(req.PathValue("id"))
	if !ok {
		controlAPIError(w, http.StatusNotFound, "not_found", "bad profile id")
		return
	}
	if _, err := r.control.OwnedProfile(id, u.ID); err != nil {
		writeControlErr(w, err, "user.profiles.update.owner") // чужой → 404
		return
	}
	var body profilePayload
	if err := decodeCabinetJSON(w, req, &body); err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "bad json")
		return
	}
	// Переименование СВОЕЙ копии — метаданные: версию НЕ создаёт, published не
	// сбрасывает (в отличие от steps ниже). Имя тримится, непустое, уникально
	// без учёта регистра среди видимых профилей; коллизия → 409 duplicate_name.
	if body.Name != nil {
		if _, err := r.control.RenameProfile(id, *body.Name); err != nil {
			writeControlErr(w, err, "user.profiles.update.rename")
			return
		}
	}
	if body.Steps != nil {
		if err := validateStepsInput(*body.Steps); err != nil {
			controlAPIError(w, http.StatusBadRequest, "steps_rejected", err.Error())
			return
		}
		varsSchema := ""
		if body.VarsSchema != nil {
			varsSchema = *body.VarsSchema
		}
		if _, err := r.control.AddProfileVersion(id, *body.Steps, varsSchema); err != nil {
			writeControlErr(w, err, "user.profiles.update.version")
			return
		}
	}
	if body.Description != nil {
		if _, err := r.control.UpdateProfileDescription(id, *body.Description); err != nil {
			writeControlErr(w, err, "user.profiles.update.description")
			return
		}
	}
	p, err := r.control.Profile(id)
	if err != nil {
		writeControlErr(w, err, "user.profiles.update.result")
		return
	}
	r.writeProfileView(w, p, http.StatusOK, "user.profiles.update")
}

func (r *Registry) handleMeDeleteProfile(w http.ResponseWriter, req *http.Request) {
	u := sessionFrom(req).user
	id, ok := parseID(req.PathValue("id"))
	if !ok {
		controlAPIError(w, http.StatusNotFound, "not_found", "bad profile id")
		return
	}
	if _, err := r.control.OwnedProfile(id, u.ID); err != nil {
		writeControlErr(w, err, "user.profiles.delete.owner")
		return
	}
	if err := r.control.DeleteProfile(id); err != nil {
		writeControlErr(w, err, "user.profiles.delete")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleMeProfiles — GET /api/me/profiles: свои профили (полные, для правки)
// и published-профили (для выбора при создании VPS).
func (r *Registry) handleMeProfiles(w http.ResponseWriter, req *http.Request) {
	u := sessionFrom(req).user
	own, err := r.control.ProfilesByOwner(u.ID)
	if err != nil {
		writeControlErr(w, err, "user.profiles.list.own")
		return
	}
	published, err := r.control.PublishedProfiles()
	if err != nil {
		writeControlErr(w, err, "user.profiles.list.published")
		return
	}
	ownViews := make([]profileAdminJSON, 0, len(own))
	for _, p := range own {
		view, err := r.profileAdminView(p)
		if err != nil {
			writeControlErr(w, err, "user.profiles.list.view")
			return
		}
		ownViews = append(ownViews, view)
	}
	pubViews := make([]profileJSON, 0, len(published))
	for _, p := range published {
		pj := profileJSON{ID: p.ID, Name: p.Name, Description: p.Description}
		v, err := r.control.ProfileVersion(p.ID, 0)
		if err != nil {
			writeControlErr(w, err, "user.profiles.list.published_version")
			return
		}
		pj.VarsSchema = v.VarsSchema
		pubViews = append(pubViews, pj)
	}
	writeCabinetJSON(w, http.StatusOK, map[string]any{
		"profiles":  ownViews,
		"published": pubViews,
		"enabled":   r.userProfilesEnabled(),
		"max":       r.maxUserProfiles(),
	})
}
