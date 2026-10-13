package server

// addendum-profiles-vps.md §5.3, §5.6: admin/user профили, каталог тарифов с
// ценой (null когда нет). Профиль — текст шагов (shell-команды), без YAML.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"sharedd/registry/internal/beget"
)

func jsonBody(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// §5.3 — админ CRUD + версии; правка шагов создаёт новую версию.
func TestAdminProfilesCRUDAndVersions(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	r.cfg.Panel.Token = testPanelToken

	createBody := jsonBody(t, map[string]any{
		"name": "p1", "description": "d1",
		"steps": "# s\necho hi\n", "published": true,
	})
	rec := adminCall(t, r, http.MethodPost, "/panel/api/admin/profiles", testPanelToken, createBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create profile: %d %s", rec.Code, rec.Body.String())
	}
	var created profileAdminJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID == 0 || !created.Published {
		t.Fatalf("created = %+v err=%v", created, err)
	}

	// список содержит профиль.
	rec = adminCall(t, r, http.MethodGet, "/panel/api/admin/profiles", testPanelToken, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"p1"`) {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}

	// правка шагов → НОВАЯ версия 2 (история не перезаписана).
	updBody := jsonBody(t, map[string]any{"steps": "# two\necho 2\n"})
	rec = adminCall(t, r, http.MethodPut, fmt.Sprintf("/panel/api/admin/profiles/%d", created.ID), testPanelToken, updBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("update profile: %d %s", rec.Code, rec.Body.String())
	}
	v1, err := r.control.ProfileVersion(created.ID, 1)
	if err != nil || v1.Steps == "" {
		t.Fatalf("version 1 must remain: %+v err=%v", v1, err)
	}
	v2, err := r.control.ProfileVersion(created.ID, 2)
	if err != nil || !strings.Contains(v2.Steps, "echo 2") {
		t.Fatalf("version 2 = %+v err=%v", v2, err)
	}
	// правка сбрасывает published (оператор публикует явно).
	p, _ := r.control.Profile(created.ID)
	if p.Published {
		t.Fatal("editing a profile must unpublish it")
	}

	// пустой текст шагов отклоняется.
	badBody := jsonBody(t, map[string]any{"steps": ""})
	rec = adminCall(t, r, http.MethodPut, fmt.Sprintf("/panel/api/admin/profiles/%d", created.ID), testPanelToken, badBody)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty steps: want 400, got %d %s", rec.Code, rec.Body.String())
	}

	// удаление.
	rec = adminCall(t, r, http.MethodDelete, fmt.Sprintf("/panel/api/admin/profiles/%d", created.ID), testPanelToken, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
}

// §5.3 — пользователь правит только свои; чужой id → 404.
func TestUserProfilesScopedToOwn(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	cabinetUser(t, r, "bob", 5)
	sessA, csrfA := cabinetLogin(t, r, "alice", "password-123")
	sessB, csrfB := cabinetLogin(t, r, "bob", "password-123")

	body := jsonBody(t, map[string]any{"name": "alice-local", "steps": "# s\necho hi\n"})
	rec := cabinetCall(t, r, http.MethodPost, "/api/me/profiles", cookieMap(sessA, csrfA), csrfA, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("alice create profile: %d %s", rec.Code, rec.Body.String())
	}
	var created profileAdminJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.ID == 0 || created.Published {
		t.Fatalf("user profile must be unpublished: %+v", created)
	}

	// bob правит чужой → 404.
	upd := jsonBody(t, map[string]any{"description": "hijack"})
	rec = cabinetCall(t, r, http.MethodPut, fmt.Sprintf("/api/me/profiles/%d", created.ID), cookieMap(sessB, csrfB), csrfB, upd)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bob update alice profile: want 404, got %d %s", rec.Code, rec.Body.String())
	}
	rec = cabinetCall(t, r, http.MethodDelete, fmt.Sprintf("/api/me/profiles/%d", created.ID), cookieMap(sessB, csrfB), csrfB, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bob delete alice profile: want 404, got %d %s", rec.Code, rec.Body.String())
	}

	// bob видит только свои (пусто), alice — свой.
	rec = cabinetCall(t, r, http.MethodGet, "/api/me/profiles", cookieMap(sessB, csrfB), "", "")
	if !strings.Contains(rec.Body.String(), `"profiles":[]`) {
		t.Fatalf("bob must see no own profiles: %s", rec.Body.String())
	}
	rec = cabinetCall(t, r, http.MethodGet, "/api/me/profiles", cookieMap(sessA, csrfA), "", "")
	if !strings.Contains(rec.Body.String(), "alice-local") {
		t.Fatalf("alice must see her profile: %s", rec.Body.String())
	}
}

// §5.6 — каталог: регионы и тарифы; отсутствие цены → null (не 0).
func TestBegetCatalogPricingNull(t *testing.T) {
	r, fake := newCabinetRegistry(t)
	price := 250.0
	fake.configs = []beget.ConfigurationInfo{
		{ID: "cfg-priced", Name: "Priced", CPU: 2, RAM: 2048, Disk: 20480, Price: &price, Available: true},
		{ID: "cfg-noprice", Name: "NoPrice", CPU: 1, RAM: 1024, Disk: 10240, Available: true},
	}
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	if rec := cabinetCall(t, r, http.MethodPut, "/api/me/beget/link", cookieMap(sess, csrf), csrf,
		`{"login":"a@beget","password":"s3cret"}`); rec.Code != http.StatusOK {
		t.Fatalf("link: %d %s", rec.Code, rec.Body.String())
	}
	rec := cabinetCall(t, r, http.MethodGet, "/api/me/beget/regions", cookieMap(sess, csrf), "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("regions: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"regions"`) || !strings.Contains(body, `"configurations"`) {
		t.Fatalf("catalog must include regions and configurations: %s", body)
	}
	if !strings.Contains(body, `"price":250`) || !strings.Contains(body, `"price":null`) {
		t.Fatalf("price must be 250 and null (not 0): %s", body)
	}
}
