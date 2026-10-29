package server

// Серверные тесты M1 (docs/api-contract.md §2-§5, docs/specs/accounts-quota.md
// «Тесты обязательны»). Проверяют HTTP-поведение: authkey-авторизацию
// /register, транзакционную квоту, admin user-API, fail-closed и
// users.enabled=false как точное прежнее поведение.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"sharedd/registry/internal/control"
)

// newUsersTestRegistry — Registry в user-режиме с реальной control-БД
// (тот же путь открытия, что и в проде: openControl).
func newUsersTestRegistry(t *testing.T) *Registry {
	t.Helper()
	r := newTestRegistry(t)
	r.cfg.Users.Enabled = boolPtr(true)
	r.cfg.Users.LegacyMaxNodes = 100000
	r.cfg.Database.ControlDB = filepath.Join(t.TempDir(), "control.db")
	r.openControl()
	if r.control == nil {
		t.Fatal("control db must open in test env")
	}
	return r
}

func boolPtr(b bool) *bool { return &b }

// adminCall — запрос к панельному API с Bearer (или без).
func adminCall(t *testing.T, r *Registry, method, target, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	r.buildMux().ServeHTTP(rec, req)
	return rec
}

// registerCall — POST /register с телом и опциональным authkey.
func registerCall(t *testing.T, r *Registry, nodeID, ip, authkey string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"node_id":%q,"ip":%q`, nodeID, ip)
	if authkey != "" {
		body += fmt.Sprintf(`,"authkey":%q`, authkey)
	}
	body += `}`
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.buildMux().ServeHTTP(rec, req)
	return rec
}

// userKey — создать пользователя и ротировать ключ (админский путь).
func userKey(t *testing.T, r *Registry, name string, maxNodes int) (int64, string) {
	t.Helper()
	u, err := r.control.CreateUser(name, "password-123", "user", maxNodes)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	k, err := r.control.RotateAuthkey(u.ID)
	if err != nil {
		t.Fatalf("rotate %s: %v", name, err)
	}
	return u.ID, k
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("bad error body %q: %v", rec.Body.String(), err)
	}
	return m["error"]
}

// ── POST /register: authkey-авторизация ──────────────────────────

func TestRegisterAuthkeyRequiredWhenUsersEnabled(t *testing.T) {
	r := newUsersTestRegistry(t)

	// без authkey, нода неизвестна → 403 bad_authkey
	rec := registerCall(t, r, "node-a1b2c", "1.2.3.4", "")
	if rec.Code != http.StatusForbidden || errCode(t, rec) != "bad_authkey" {
		t.Fatalf("want 403 bad_authkey, got %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := r.state.Candidates["node-a1b2c"]; ok {
		t.Fatal("node must not be registered without authkey")
	}

	// с валидным authkey → 200 + владелец привязан
	_, key := userKey(t, r, "alice", 5)
	rec = registerCall(t, r, "node-a1b2c", "1.2.3.4", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := r.state.Candidates["node-a1b2c"]; !ok {
		t.Fatal("node must be registered with valid authkey")
	}
	owner, legacy, ok, err := r.control.NodeOwner("node-a1b2c")
	if err != nil || !ok || legacy || owner == r.control.LegacyUserID() {
		t.Fatalf("owner binding missing: %d %t %t %v", owner, legacy, ok, err)
	}
}

func TestRegisterUnknownAndRevokedAuthkey(t *testing.T) {
	r := newUsersTestRegistry(t)
	if rec := registerCall(t, r, "node-a1b2c", "1.2.3.4", "sda_ffffffffffffffffffffffffffffffff"); rec.Code != http.StatusForbidden || errCode(t, rec) != "bad_authkey" {
		t.Fatalf("unknown key: want 403 bad_authkey, got %d %s", rec.Code, rec.Body.String())
	}
	id, key := userKey(t, r, "alice", 5)
	if err := r.control.RevokeAuthkeys(id); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if rec := registerCall(t, r, "node-a1b2c", "1.2.3.4", key); rec.Code != http.StatusForbidden || errCode(t, rec) != "bad_authkey" {
		t.Fatalf("revoked key: want 403 bad_authkey, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestRegisterForeignAuthkeyOwnerMismatch(t *testing.T) {
	r := newUsersTestRegistry(t)
	_, keyA := userKey(t, r, "alice", 5)
	_, keyB := userKey(t, r, "bob", 5)
	if rec := registerCall(t, r, "node-a1b2c", "1.2.3.4", keyA); rec.Code != http.StatusOK {
		t.Fatalf("alice bind: %d %s", rec.Code, rec.Body.String())
	}
	// чужой authkey на чужой ноде → 403 owner_mismatch
	rec := registerCall(t, r, "node-a1b2c", "1.2.3.4", keyB)
	if rec.Code != http.StatusForbidden || errCode(t, rec) != "owner_mismatch" {
		t.Fatalf("want 403 owner_mismatch, got %d %s", rec.Code, rec.Body.String())
	}
	// bob регистрирует СВОЮ ноду — нормально
	if rec := registerCall(t, r, "node-b1b2c", "5.6.7.8", keyB); rec.Code != http.StatusOK {
		t.Fatalf("bob own node: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRegisterOwnNodeAgainNoSlot(t *testing.T) {
	r := newUsersTestRegistry(t)
	id, key := userKey(t, r, "alice", 5)
	for i := 0; i < 3; i++ {
		if rec := registerCall(t, r, "node-a1b2c", "1.2.3.4", key); rec.Code != http.StatusOK {
			t.Fatalf("re-register %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	used, err := r.control.UsedNodes(id)
	if err != nil || used != 1 {
		t.Fatalf("used = %d err=%v, want 1", used, err)
	}
}

// Два конкурентных POST /register при одном свободном месте: ровно один 200.
func TestRegisterConcurrentOneSlotExactlyOneWinner(t *testing.T) {
	r := newUsersTestRegistry(t)
	_, key := userKey(t, r, "alice", 1)
	srv := httptest.NewServer(r.buildMux())
	defer srv.Close()

	const n = 8
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"node_id":"node-w%02da1b2c","ip":"9.9.9.9","authkey":%q}`, i, key)
			resp, err := http.Post(srv.URL+"/register", "application/json", strings.NewReader(body))
			if err != nil {
				t.Errorf("post: %v", err)
				return
			}
			defer func() { _ = resp.Body.Close() }()
			codes[i] = resp.StatusCode
		}(i)
	}
	wg.Wait()
	ok, denied := 0, 0
	for i, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusForbidden:
			denied++
		default:
			t.Fatalf("goroutine %d: unexpected status %d", i, c)
		}
	}
	if ok != 1 || denied != n-1 {
		t.Fatalf("want exactly one 200 and %d 403, got %d 200 / %d 403", n-1, ok, denied)
	}
}

// max_nodes вниз: существующие ноды живут, новые регистрации отклонены.
func TestMaxNodesDownBlocksNewRegistrations(t *testing.T) {
	r := newUsersTestRegistry(t)
	id, key := userKey(t, r, "alice", 2)
	if rec := registerCall(t, r, "node-a1b2c", "1.2.3.4", key); rec.Code != http.StatusOK {
		t.Fatalf("bind a: %d", rec.Code)
	}
	if rec := registerCall(t, r, "node-b1b2c", "5.6.7.8", key); rec.Code != http.StatusOK {
		t.Fatalf("bind b: %d", rec.Code)
	}
	if _, err := r.control.UpdateUser(id, control.UserPatch{MaxNodes: intPtr(1)}); err != nil {
		t.Fatalf("patch max_nodes: %v", err)
	}
	rec := registerCall(t, r, "node-c1b2c", "8.8.8.8", key)
	if rec.Code != http.StatusForbidden || errCode(t, rec) != "quota_exceeded" {
		t.Fatalf("want 403 quota_exceeded, got %d %s", rec.Code, rec.Body.String())
	}
	// существующая нода продолжает работать (heartbeat не отклоняем)
	hb := httptest.NewRequest(http.MethodPost, "/heartbeat", strings.NewReader(`{"node_id":"node-a1b2c"}`))
	hrec := httptest.NewRecorder()
	r.buildMux().ServeHTTP(hrec, hb)
	if hrec.Code != http.StatusOK {
		t.Fatalf("heartbeat of quota-limited user's node must stay 200, got %d", hrec.Code)
	}
}

// intPtr — переиспользуем intPtr из config_editor.go.

// release-node освобождает место для новых регистраций.
func TestReleaseNodeFreesSlotViaAPI(t *testing.T) {
	r := newUsersTestRegistry(t)
	id, key := userKey(t, r, "alice", 1)
	if rec := registerCall(t, r, "node-a1b2c", "1.2.3.4", key); rec.Code != http.StatusOK {
		t.Fatalf("bind: %d", rec.Code)
	}
	if rec := registerCall(t, r, "node-b1b2c", "5.6.7.8", key); rec.Code != http.StatusForbidden {
		t.Fatalf("second bind must hit quota, got %d", rec.Code)
	}
	rec := adminCall(t, r, http.MethodPost, fmt.Sprintf("/panel/api/admin/users/%d/release-node", id), "", `{"node_id":"node-a1b2c"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("release: %d %s", rec.Code, rec.Body.String())
	}
	if rec := registerCall(t, r, "node-b1b2c", "5.6.7.8", key); rec.Code != http.StatusOK {
		t.Fatalf("bind after release: %d %s", rec.Code, rec.Body.String())
	}
	// неизвестная нода → 404
	rec = adminCall(t, r, http.MethodPost, fmt.Sprintf("/panel/api/admin/users/%d/release-node", id), "", `{"node_id":"node-missing0"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("release unknown: %d %s", rec.Code, rec.Body.String())
	}
}

// Блокировка пользователя: ноды работают, новые регистрации 403.
func TestDisabledUserKeepsNodesButCannotRegister(t *testing.T) {
	r := newUsersTestRegistry(t)
	id, key := userKey(t, r, "alice", 5)
	if rec := registerCall(t, r, "node-a1b2c", "1.2.3.4", key); rec.Code != http.StatusOK {
		t.Fatalf("bind: %d", rec.Code)
	}
	if _, err := r.control.UpdateUser(id, control.UserPatch{Disabled: boolPtr(true)}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	rec := registerCall(t, r, "node-b1b2c", "5.6.7.8", key)
	if rec.Code != http.StatusForbidden || errCode(t, rec) != "user_disabled" {
		t.Fatalf("want 403 user_disabled, got %d %s", rec.Code, rec.Body.String())
	}
	hb := httptest.NewRequest(http.MethodPost, "/heartbeat", strings.NewReader(`{"node_id":"node-a1b2c"}`))
	hrec := httptest.NewRecorder()
	r.buildMux().ServeHTTP(hrec, hb)
	if hrec.Code != http.StatusOK {
		t.Fatalf("disabled user's node heartbeat must stay 200, got %d", hrec.Code)
	}
}

// Legacy-миграция: ноды, существовавшие до включения user-режима, привязаны
// к legacy-пользователю и перерегистрируются старым агентом без authkey.
func TestLegacyMigrationBindsExistingCandidates(t *testing.T) {
	r := newTestRegistry(t) // сначала legacy-режим: candidate уже в state
	r.register(registerRequest{NodeID: "node-olda1", IP: "4.4.4.4"})

	// «включили» user-режим: openControl + миграция (как в Run)
	r.cfg.Users.Enabled = boolPtr(true)
	r.cfg.Users.LegacyMaxNodes = 100000
	r.cfg.Database.ControlDB = filepath.Join(t.TempDir(), "control.db")
	r.openControl()
	r.migrateLegacyOwners()

	owner, legacy, ok, err := r.control.NodeOwner("node-olda1")
	if err != nil || !ok || !legacy || owner != r.control.LegacyUserID() {
		t.Fatalf("legacy binding: %d %t %t %v", owner, legacy, ok, err)
	}
	used, err := r.control.UsedNodes(r.control.LegacyUserID())
	if err != nil || used != 0 {
		t.Fatalf("legacy rows must not count toward quota: %d %v", used, err)
	}
	// старый агент без authkey перерегистрируется
	if rec := registerCall(t, r, "node-olda1", "4.4.4.4", ""); rec.Code != http.StatusOK {
		t.Fatalf("legacy re-register: %d %s", rec.Code, rec.Body.String())
	}
	// а НОВАЯ нода без authkey — нет
	if rec := registerCall(t, r, "node-newb2", "5.5.5.5", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("new node without authkey must 403, got %d", rec.Code)
	}
}

// Рестарт регистратора: контрольная БД персистентна, владельцы сохраняются.
func TestOwnersSurviveRegistryRestart(t *testing.T) {
	dir := t.TempDir()
	r1 := newTestRegistry(t)
	r1.cfg.Users.Enabled = boolPtr(true)
	r1.cfg.Users.LegacyMaxNodes = 100000
	r1.cfg.Database.ControlDB = filepath.Join(dir, "control.db")
	r1.openControl()
	_, key := userKey(t, r1, "alice", 5)
	if rec := registerCall(t, r1, "node-a1b2c", "1.2.3.4", key); rec.Code != http.StatusOK {
		t.Fatalf("bind: %d", rec.Code)
	}

	// «рестарт»: новый Registry на том же control.db
	r2 := newTestRegistry(t)
	r2.cfg.Users.Enabled = boolPtr(true)
	r2.cfg.Users.LegacyMaxNodes = 100000
	r2.cfg.Database.ControlDB = filepath.Join(dir, "control.db")
	r2.openControl()
	if rec := registerCall(t, r2, "node-a1b2c", "1.2.3.4", key); rec.Code != http.StatusOK {
		t.Fatalf("re-register after restart: %d %s", rec.Code, rec.Body.String())
	}
	// чужой пользователь по-прежнему не может захватить ноду
	_, keyB := userKey(t, r2, "bob", 5)
	if rec := registerCall(t, r2, "node-a1b2c", "1.2.3.4", keyB); rec.Code != http.StatusForbidden {
		t.Fatalf("owner_mismatch must survive restart, got %d", rec.Code)
	}
}

// Cross-tenant IP-eviction: чужую ноду по тому же IP не вытесняем
// (tenant isolation; раньше любой арендатор мог занять чужой IP).
func TestRegisterSameIPDoesNotEvictOtherTenant(t *testing.T) {
	r := newUsersTestRegistry(t)
	_, keyA := userKey(t, r, "alice", 1)
	_, keyB := userKey(t, r, "bob", 1)
	if rec := registerCall(t, r, "node-aa1b2", "1.2.3.4", keyA); rec.Code != http.StatusOK {
		t.Fatalf("alice bind: %d %s", rec.Code, rec.Body.String())
	}
	// bob регистрирует СВОЮ ноду с тем же IP — нода alice должна остаться
	if rec := registerCall(t, r, "node-bb1b2", "1.2.3.4", keyB); rec.Code != http.StatusOK {
		t.Fatalf("bob bind: %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := r.state.Candidates["node-aa1b2"]; !ok {
		t.Fatal("victim node must not be evicted by a cross-tenant same-IP register")
	}
	if _, ok := r.state.Candidates["node-bb1b2"]; !ok {
		t.Fatal("bob's own node must register")
	}
}

// ── fail-closed ──────────────────────────────────────────────────
// control.db открыть не удалось → /register 503 при users.enabled=true;
// при users.enabled=false регистрация работает как раньше.
func TestFailClosedRegisterWhenControlUnavailable(t *testing.T) {
	r := newTestRegistry(t)
	r.cfg.Users.Enabled = boolPtr(true)
	r.cfg.Users.LegacyMaxNodes = 100000
	r.cfg.Database.ControlDB = filepath.Join(t.TempDir(), "no-such-dir", "control.db")
	r.openControl() // должен упасть тихо, store = nil
	if r.control != nil {
		t.Fatal("control must be nil after failed open")
	}
	if rec := registerCall(t, r, "node-a1b2c", "1.2.3.4", "sda_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("enabled+broken db: want 503, got %d %s", rec.Code, rec.Body.String())
	}
	if code := errCode(t, registerCall(t, r, "node-a1b2c", "1.2.3.4", "")); code != "control_unavailable" {
		t.Fatalf("want control_unavailable, got %s", code)
	}

	// users.enabled=false — прежнее поведение, регистрация без authkey работает
	r2 := newTestRegistry(t)
	r2.cfg.Database.ControlDB = filepath.Join(t.TempDir(), "no-such-dir", "control.db")
	// openControl при enabled=false ничего не делает (проверяем это явно)
	r2.openControl()
	if r2.control != nil {
		t.Fatal("disabled users mode must not open control db")
	}
	if rec := registerCall(t, r2, "node-a1b2c", "1.2.3.4", ""); rec.Code != http.StatusOK {
		t.Fatalf("disabled mode must behave as before: want 200, got %d %s", rec.Code, rec.Body.String())
	}
}

// ── панельные admin user-API ─────────────────────────────────────

const testPanelToken = "panel-secret-token"

func newPanelUsersRegistry(t *testing.T) *Registry {
	t.Helper()
	r := newUsersTestRegistry(t)
	r.cfg.Panel.Token = testPanelToken
	return r
}

func TestAdminAPIsRequireBearer(t *testing.T) {
	r := newPanelUsersRegistry(t)
	for _, c := range []struct{ m, p string }{
		{http.MethodPost, "/panel/api/admin/users"},
		{http.MethodGet, "/panel/api/admin/users"},
		{http.MethodPatch, "/panel/api/admin/users/2"},
		{http.MethodPost, "/panel/api/admin/users/2/authkey/rotate"},
		{http.MethodDelete, "/panel/api/admin/users/2/authkey"},
		{http.MethodPost, "/panel/api/admin/users/2/release-node"},
	} {
		rec := adminCall(t, r, c.m, c.p, "", "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without token: want 401, got %d", c.m, c.p, rec.Code)
		}
	}
}

func TestAdminUserCRUD(t *testing.T) {
	r := newPanelUsersRegistry(t)

	// create
	rec := adminCall(t, r, http.MethodPost, "/panel/api/admin/users", testPanelToken,
		`{"username":"alice","password":"password-123","role":"user","max_nodes":5}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
		Role     string `json:"role"`
		MaxNodes int    `json:"max_nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.Username != "alice" || created.Role != "user" || created.MaxNodes != 5 {
		t.Fatalf("create body: %+v err=%v", created, err)
	}

	// дубликат → 409
	if rec := adminCall(t, r, http.MethodPost, "/panel/api/admin/users", testPanelToken,
		`{"username":"alice","password":"password-123"}`); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate: want 409, got %d %s", rec.Code, rec.Body.String())
	}
	// кривой username → 400
	if rec := adminCall(t, r, http.MethodPost, "/panel/api/admin/users", testPanelToken,
		`{"username":"ALICE","password":"password-123"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad username: want 400, got %d", rec.Code)
	}
	// короткий пароль → 400
	if rec := adminCall(t, r, http.MethodPost, "/panel/api/admin/users", testPanelToken,
		`{"username":"bob","password":"short"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("short password: want 400, got %d", rec.Code)
	}

	// rotate: полный ключ один раз
	rec = adminCall(t, r, http.MethodPost, fmt.Sprintf("/panel/api/admin/users/%d/authkey/rotate", created.ID), testPanelToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body.String())
	}
	var rot struct {
		Authkey string `json:"authkey"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rot); err != nil || !strings.HasPrefix(rot.Authkey, "sda_") {
		t.Fatalf("rotate body %q err=%v", rec.Body.String(), err)
	}

	// revoke → 204
	rec = adminCall(t, r, http.MethodDelete, fmt.Sprintf("/panel/api/admin/users/%d/authkey", created.ID), testPanelToken, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d", rec.Code)
	}
	// отозванный ключ больше не авторизует регистрацию
	if rec := registerCall(t, r, "node-a1b2c", "1.2.3.4", rot.Authkey); rec.Code != http.StatusForbidden {
		t.Fatalf("revoked key must not register, got %d", rec.Code)
	}

	// patch: max_nodes вниз + список с used_nodes
	if rec := adminCall(t, r, http.MethodPatch, fmt.Sprintf("/panel/api/admin/users/%d", created.ID), testPanelToken, `{"max_nodes":9}`); rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	rec = adminCall(t, r, http.MethodGet, "/panel/api/admin/users", testPanelToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Users []struct {
			ID        int64  `json:"id"`
			Username  string `json:"username"`
			Role      string `json:"role"`
			MaxNodes  int    `json:"max_nodes"`
			UsedNodes int    `json:"used_nodes"`
			Disabled  bool   `json:"disabled"`
		} `json:"users"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list body: %v", err)
	}
	if len(list.Users) != 2 { // alice + legacy
		t.Fatalf("want 2 users (alice+legacy), got %d", len(list.Users))
	}
	// контракт §2: used_nodes/disabled присутствуют даже нулевыми (omitempty убран)
	body := rec.Body.String()
	if !strings.Contains(body, `"used_nodes":0`) || !strings.Contains(body, `"disabled":false`) {
		t.Fatalf("zero used_nodes/disabled must still be present, got %s", body)
	}

	// последний активный админ не блокируется
	rec = adminCall(t, r, http.MethodPost, "/panel/api/admin/users", testPanelToken, `{"username":"root","password":"password-123","role":"admin"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create admin: %d %s", rec.Code, rec.Body.String())
	}
	var admin struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &admin)
	if rec := adminCall(t, r, http.MethodPatch, fmt.Sprintf("/panel/api/admin/users/%d", admin.ID), testPanelToken, `{"disabled":true}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("disable last admin: want 400, got %d %s", rec.Code, rec.Body.String())
	}
	// 404 для отсутствующего пользователя
	if rec := adminCall(t, r, http.MethodPatch, "/panel/api/admin/users/9999", testPanelToken, `{"max_nodes":1}`); rec.Code != http.StatusNotFound {
		t.Fatalf("patch unknown: want 404, got %d", rec.Code)
	}
}

// users.enabled=false: admin user-API отвечают 503 users_disabled.
func TestAdminAPIs503WhenUsersDisabled(t *testing.T) {
	r := newTestRegistry(t) // enabled=false
	r.cfg.Panel.Token = testPanelToken
	for _, c := range []struct{ m, p, b string }{
		{http.MethodPost, "/panel/api/admin/users", `{"username":"x","password":"password-123"}`},
		{http.MethodGet, "/panel/api/admin/users", ""},
		{http.MethodPost, "/panel/api/admin/users/2/authkey/rotate", ""},
	} {
		rec := adminCall(t, r, c.m, c.p, testPanelToken, c.b)
		if rec.Code != http.StatusServiceUnavailable || errCode(t, rec) != "users_disabled" {
			t.Fatalf("%s %s: want 503 users_disabled, got %d %s", c.m, c.p, rec.Code, rec.Body.String())
		}
	}
}
