package server

// OWNER DECISION: [provisioning] user_profiles_enabled дефолт true. Шаги
// профиля — shell-команды, исполняемые на СВОЁМ VPS пользователя (мок-исполнитель
// в тестах, сети нет). Проверяем: дефолт включён; create+use end-to-end; при
// выключенном флаге и создание, и выбор отвергаются; лимит max_user_profiles
// по-прежнему срабатывает.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Дефолт (флаг не задан) = включено; профиль создаётся и применяется на VPS
// end-to-end мок-исполнителем.
func TestUserProfilesEnabledByDefaultEndToEnd(t *testing.T) {
	r, _ := newCabinetRegistry(t) // UserProfilesEnabled оставлен nil → дефолт true
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	linkBeget(t, r, sess, csrf)

	rec := cabinetCall(t, r, http.MethodGet, "/api/me/profiles", cookieMap(sess, csrf), "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list profiles: %d %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Enabled bool `json:"enabled"`
		Max     int  `json:"max"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if !list.Enabled {
		t.Fatalf("user profiles must be enabled by default, got %s", rec.Body.String())
	}

	body := jsonBody(t, map[string]any{"name": "alice-e2e", "steps": "# e2e\necho E2E_STEP\n"})
	rec = cabinetCall(t, r, http.MethodPost, "/api/me/profiles", cookieMap(sess, csrf), csrf, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create profile: %d %s", rec.Code, rec.Body.String())
	}
	var created profileAdminJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	rec = cabinetCall(t, r, http.MethodPost, "/api/me/vps", cookieMap(sess, csrf), csrf,
		fmt.Sprintf(`{"region":"ru1","idem_key":"e2e-prof","profile_id":%d,"confirm":true}`, created.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("use own profile: %d %s", rec.Code, rec.Body.String())
	}
	var op operationJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &op)
	waitOpTerminal(t, r, op.ID)

	if exec := r.sshExec.(*fakeExec); !strings.Contains(exec.allCommands(), "E2E_STEP") {
		t.Fatalf("profile steps must run on VPS, got %q", exec.allCommands())
	}
}

// Выключенный флаг отвергает и создание, и выбор своего профиля.
func TestUserProfilesDisabledRejectsCreateAndUse(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	linkBeget(t, r, sess, csrf)

	// Пока включено — создаём профиль, чтобы проверить именно выбор.
	body := jsonBody(t, map[string]any{"name": "alice-off", "steps": "# s\necho X\n"})
	rec := cabinetCall(t, r, http.MethodPost, "/api/me/profiles", cookieMap(sess, csrf), csrf, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create profile: %d %s", rec.Code, rec.Body.String())
	}
	var created profileAdminJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	r.cfg.Provisioning.UserProfilesEnabled = boolPtr(false)

	rec = cabinetCall(t, r, http.MethodPost, "/api/me/profiles", cookieMap(sess, csrf), csrf, body)
	if rec.Code != http.StatusForbidden || errCode(t, rec) != "user_profiles_disabled" {
		t.Fatalf("disabled create: want 403 user_profiles_disabled, got %d %s", rec.Code, rec.Body.String())
	}
	rec = cabinetCall(t, r, http.MethodPost, "/api/me/vps", cookieMap(sess, csrf), csrf,
		fmt.Sprintf(`{"region":"ru1","idem_key":"off-use","profile_id":%d,"confirm":true}`, created.ID))
	if rec.Code != http.StatusForbidden || errCode(t, rec) != "user_profiles_disabled" {
		t.Fatalf("disabled use: want 403 user_profiles_disabled, got %d %s", rec.Code, rec.Body.String())
	}
}

// Лимит max_user_profiles по-прежнему отдаёт профильную ошибку.
func TestUserProfilesLimitEnforced(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	r.cfg.Provisioning.MaxUserProfiles = 1
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")

	first := jsonBody(t, map[string]any{"name": "p1", "steps": "# s\necho 1\n"})
	rec := cabinetCall(t, r, http.MethodPost, "/api/me/profiles", cookieMap(sess, csrf), csrf, first)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first profile: %d %s", rec.Code, rec.Body.String())
	}
	second := jsonBody(t, map[string]any{"name": "p2", "steps": "# s\necho 2\n"})
	rec = cabinetCall(t, r, http.MethodPost, "/api/me/profiles", cookieMap(sess, csrf), csrf, second)
	if rec.Code != http.StatusForbidden || errCode(t, rec) != "profile_limit_reached" {
		t.Fatalf("limit: want 403 profile_limit_reached, got %d %s", rec.Code, rec.Body.String())
	}
}
