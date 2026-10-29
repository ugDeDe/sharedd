package server

// Обязательные тесты миграции (invitations-migration-contract.md §5 пп.3-4, 6):
// матрица «Разрешить общий секрет» на всех 5 machine-маршрутах, node_secret
// (выдача/принятие/чужой node_id/перевыпуск), affected_nodes, и admin-API
// приглашений + саморегистрация по приглашению.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newMigrationRegistry — user-режим + общий токен machine API + панельный токен.
func newMigrationRegistry(t *testing.T) *Registry {
	t.Helper()
	r := newPanelUsersRegistry(t)
	r.cfg.Security.NodeToken = "shared-secret"
	return r
}

// registerNode — POST /register с Bearer и authkey; возвращает тело ответа.
func registerNode(t *testing.T, r *Registry, bearer, nodeID, ip, authkey string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"node_id":%q,"ip":%q,"node_type":"classic"`, nodeID, ip)
	if authkey != "" {
		body += fmt.Sprintf(`,"authkey":%q`, authkey)
	}
	body += `}`
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	r.buildMux().ServeHTTP(rec, req)
	return rec
}

// machineCall — запрос к machine API с Bearer.
func machineCall(t *testing.T, r *Registry, method, target, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	r.buildMux().ServeHTTP(rec, req)
	return rec
}

// reportBody — валидный /report (свежий checked_at; окно скользящее, не
// календарное — детерминизм тестов не страдает).
func reportBody(nodeID, ip string) string {
	return fmt.Sprintf(`{"node_id":%q,"ip":%q,"port":8443,"checked_at":%q,"healthy":true}`,
		nodeID, ip, time.Now().UTC().Format(time.RFC3339Nano))
}

// issuedSecret — вытащить node_secret из тела 200 /register.
func issuedSecret(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var resp struct {
		NodeSecret string `json:"node_secret"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad register response %q: %v", rec.Body.String(), err)
	}
	if resp.NodeSecret == "" {
		t.Fatalf("node_secret must be issued, got %q", rec.Body.String())
	}
	return resp.NodeSecret
}

// seedNode — зарегистрировать ноду по authkey, вернуть (node_id, секрет,
// authkey владельца).
func seedNode(t *testing.T, r *Registry, name string) (string, string, string) {
	t.Helper()
	_, key := userKey(t, r, name, 10)
	nodeID := name + "-abcde"
	rec := registerNode(t, r, "shared-secret", nodeID, "1.2.3.4", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("seed register %s: status=%d body=%s", nodeID, rec.Code, rec.Body.String())
	}
	return nodeID, issuedSecret(t, rec), key
}

// §5.3 — OFF: общий токен отклонён на всех 5 маршрутах; node_secret
// принимается; чужой node_id → 403; перевыпуск по authkey.
func TestLegacyOffAuthMatrix(t *testing.T) {
	r := newMigrationRegistry(t)
	nodeA, secretA, keyA := seedNode(t, r, "nodea")
	nodeB, secretB, _ := seedNode(t, r, "nodeb")

	// OFF через панельный API (как это делает оператор).
	adminCall(t, r, http.MethodPost, "/panel/api/admin/legacy-secret", testPanelToken,
		`{"enabled":false}`)

	const shared = "shared-secret"
	// Общий токен отклонён везде (401 shared_token_disabled).
	cases := []struct {
		name   string
		method string
		target string
		body   string
	}{
		{"register", http.MethodPost, "/register", `{"node_id":"fresh-xxxx1","ip":"1.2.3.4"}`},
		{"heartbeat", http.MethodPost, "/heartbeat", fmt.Sprintf(`{"node_id":%q}`, nodeA)},
		{"report", http.MethodPost, "/report", reportBody(nodeA, "1.2.3.4")},
		{"retire", http.MethodPost, "/retire", fmt.Sprintf(`{"node_id":%q,"ip":"1.2.3.4"}`, nodeA)},
		{"config", http.MethodGet, "/config", ""},
	}
	for _, tc := range cases {
		t.Run("shared-"+tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+shared)
			if tc.target == "/config" {
				req.Header.Set("X-ShareDD-Node-ID", nodeA)
			}
			rec := httptest.NewRecorder()
			r.buildMux().ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized || errCode(t, rec) != "shared_token_disabled" {
				t.Fatalf("status=%d code=%q, want 401 shared_token_disabled", rec.Code, errCode(t, rec))
			}
		})
	}

	// node_secret принимается на всех 5 маршрутах.
	for _, tc := range []struct {
		name   string
		method string
		target string
		body   string
		want   int
	}{
		{"heartbeat", http.MethodPost, "/heartbeat", fmt.Sprintf(`{"node_id":%q}`, nodeA), http.StatusOK},
		{"report", http.MethodPost, "/report", reportBody(nodeA, "1.2.3.4"), http.StatusOK},
		{"config", http.MethodGet, "/config", "", http.StatusOK},
		// /retire: гейт пропускает (свой секрет), дальше 403 ip-mismatch —
		// httptest RemoteAddr не равен заявленному ip.
		{"retire", http.MethodPost, "/retire", fmt.Sprintf(`{"node_id":%q,"ip":"1.2.3.4"}`, nodeA), http.StatusForbidden},
	} {
		t.Run("secret-"+tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+secretA)
			if tc.target == "/config" {
				req.Header.Set("X-ShareDD-Node-ID", nodeA)
			}
			rec := httptest.NewRecorder()
			r.buildMux().ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status=%d (code=%q), want %d", rec.Code, errCode(t, rec), tc.want)
			}
		})
	}

	// Чужой node_id с чужим секретом → 403 node_id_mismatch.
	rec := machineCall(t, r, http.MethodPost, "/heartbeat", secretB, fmt.Sprintf(`{"node_id":%q}`, nodeA))
	if rec.Code != http.StatusForbidden || errCode(t, rec) != "node_id_mismatch" {
		t.Fatalf("foreign node_id: status=%d code=%q, want 403 node_id_mismatch", rec.Code, errCode(t, rec))
	}

	// Перерегистрация СВОЕЙ ноды с authkey владельца при OFF → 200 и НОВЫЙ
	// секрет; старый секрет инвалидируется.
	rec = registerNode(t, r, shared, nodeA, "1.2.3.4", keyA)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-register with authkey: status=%d body=%s", rec.Code, rec.Body.String())
	}
	secretA2 := issuedSecret(t, rec)
	if secretA2 == secretA {
		t.Fatal("re-issue must produce a new secret")
	}
	rec = machineCall(t, r, http.MethodPost, "/heartbeat", secretA, fmt.Sprintf(`{"node_id":%q}`, nodeA))
	if rec.Code == http.StatusOK {
		t.Fatal("old secret must be invalidated after re-issue")
	}

	// /register новой ноды по node_secret её владельца — тоже возможен
	// (перевыпуск), но чужой секрет новой ноде секрет НЕ выдаёт.
	rec = machineCall(t, r, http.MethodPost, "/register", secretB,
		fmt.Sprintf(`{"node_id":%q,"ip":"1.2.3.4","node_type":"classic"}`, nodeB))
	if rec.Code != http.StatusOK {
		t.Fatalf("own-secret re-register: status=%d", rec.Code)
	}
}

// §5.4 — ON: общий токен работает везде (legacy-поведение).
func TestLegacyOnSharedTokenWorksEverywhere(t *testing.T) {
	r := newMigrationRegistry(t)
	nodeA, _, keyA := seedNode(t, r, "nodea")
	const shared = "shared-secret"

	for _, tc := range []struct {
		name   string
		method string
		target string
		body   string
		want   int
	}{
		// Перерегистрация своей ноды (authkey владельца поверх общего
		// токена — прежнее user-режим-поведение).
		{"register", http.MethodPost, "/register", fmt.Sprintf(`{"node_id":%q,"ip":"1.2.3.4","node_type":"classic","authkey":%q}`, nodeA, keyA), http.StatusOK},
		{"heartbeat", http.MethodPost, "/heartbeat", fmt.Sprintf(`{"node_id":%q}`, nodeA), http.StatusOK},
		{"report", http.MethodPost, "/report", reportBody(nodeA, "1.2.3.4"), http.StatusOK},
		{"config", http.MethodGet, "/config", "", http.StatusOK},
		// /retire: гейт пропускает общий токен, дальше штатный ip-mismatch.
		{"retire", http.MethodPost, "/retire", fmt.Sprintf(`{"node_id":%q,"ip":"1.2.3.4"}`, nodeA), http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+shared)
			if tc.target == "/config" {
				req.Header.Set("X-ShareDD-Node-ID", nodeA)
			}
			rec := httptest.NewRecorder()
			r.buildMux().ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status=%d (code=%q), want %d", rec.Code, errCode(t, rec), tc.want)
			}
		})
	}
}

// §5.6 — affected_nodes: нода с секретом не считается, без секрета считается.
func TestAffectedNodesCount(t *testing.T) {
	r := newMigrationRegistry(t)
	// Две ноды в живых кандидатах, у обеих секреты выданы при регистрации.
	seedNode(t, r, "nodea")
	seedNode(t, r, "nodeb")

	// Сбросим секрет одной ноды — эмулируем непереведённого агента.
	if _, _, err := r.control.SetNodeSecret("nodeb-abcde", ""); err != nil {
		t.Fatal(err)
	}
	rec := adminCall(t, r, http.MethodGet, "/panel/api/admin/legacy-secret", testPanelToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var resp struct {
		Enabled       bool   `json:"enabled"`
		AffectedNodes *int   `json:"affected_nodes"`
		AsOf          string `json:"as_of"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Enabled {
		t.Fatal("default must be ON")
	}
	if resp.AffectedNodes == nil || *resp.AffectedNodes != 1 {
		t.Fatalf("affected_nodes=%v, want 1", resp.AffectedNodes)
	}

	// OFF-запись возвращает persisted:true и мгновенно применяется.
	rec = adminCall(t, r, http.MethodPost, "/panel/api/admin/legacy-secret", testPanelToken, `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("post status=%d body=%s", rec.Code, rec.Body.String())
	}
	var setResp struct {
		Enabled   bool `json:"enabled"`
		Persisted bool `json:"persisted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &setResp); err != nil || !setResp.Persisted || setResp.Enabled {
		t.Fatalf("set response=%s", rec.Body.String())
	}
	rec = machineCall(t, r, http.MethodPost, "/heartbeat", "shared-secret", `{"node_id":"nodea-abcde"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("shared token must be rejected immediately after OFF, status=%d", rec.Code)
	}
}

// Приглашения: admin-API create/list/revoke + саморегистрация по
// приглашению (contract §2): одноразовость, различимые коды,
// invite_url из panel.public_url.
func TestInvitesAdminAPIAndRegistration(t *testing.T) {
	r := newMigrationRegistry(t)
	r.cfg.Panel.PublicURL = "https://panel.example.com"

	// create: ttl_hours вне диапазона → 400; дефолт (пустое тело) → 201.
	rec := adminCall(t, r, http.MethodPost, "/panel/api/admin/invites", testPanelToken, `{"ttl_hours":9999}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("ttl 9999: want 400, got %d", rec.Code)
	}
	rec = adminCall(t, r, http.MethodPost, "/panel/api/admin/invites", testPanelToken, `{}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create default: want 201, got %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID        int64  `json:"id"`
		InviteURL string `json:"invite_url"`
		ExpiresAt int64  `json:"expires_at"`
		Prefix    string `json:"prefix"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.InviteURL, "https://panel.example.com/signin?invite=") {
		t.Fatalf("invite_url=%q", created.InviteURL)
	}
	token := strings.TrimPrefix(created.InviteURL, "https://panel.example.com/signin?invite=")
	if token == "" || !strings.HasPrefix(created.Prefix, token[:1]) {
		t.Fatalf("token/prefix mismatch: prefix=%q", created.Prefix)
	}

	// list: одна активная запись, полный токен в списке отсутствует.
	rec = adminCall(t, r, http.MethodGet, "/panel/api/admin/invites", testPanelToken, "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), token) {
		t.Fatalf("list leaks token: %d %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Invites []struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		} `json:"invites"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Invites) != 1 || list.Invites[0].Status != "active" {
		t.Fatalf("list=%s err=%v", rec.Body.String(), err)
	}

	// Саморегистрация по приглашению.
	regBody := fmt.Sprintf(`{"username":"invited","password":"password-123","invite":%q}`, token)
	rec = machineCall(t, r, http.MethodPost, "/api/auth/register", "", regBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("register with invite: %d %s", rec.Code, rec.Body.String())
	}
	// Повторное использование → 403 invite_used (другое имя, чтобы не
	// поймать 409 duplicate по username); без invite → invite_required.
	regBody2 := fmt.Sprintf(`{"username":"invited-again","password":"password-123","invite":%q}`, token)
	rec = machineCall(t, r, http.MethodPost, "/api/auth/register", "", regBody2)
	if rec.Code != http.StatusForbidden || errCode(t, rec) != "invite_used" {
		t.Fatalf("reuse: %d %q", rec.Code, errCode(t, rec))
	}
	rec = machineCall(t, r, http.MethodPost, "/api/auth/register", "", `{"username":"another","password":"password-123"}`)
	if rec.Code != http.StatusForbidden || errCode(t, rec) != "invite_required" {
		t.Fatalf("no invite: %d %q", rec.Code, errCode(t, rec))
	}
	// Валидационная ошибка полей не расходует НОВОЕ приглашение и даёт 400.
	rec = adminCall(t, r, http.MethodPost, "/panel/api/admin/invites", testPanelToken, `{"ttl_hours":72}`)
	var created2 struct {
		InviteURL string `json:"invite_url"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created2)
	token2 := strings.TrimPrefix(created2.InviteURL, "https://panel.example.com/signin?invite=")
	rec = machineCall(t, r, http.MethodPost, "/api/auth/register", "", fmt.Sprintf(`{"username":"X","password":"short","invite":%q}`, token2))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("field validation: want 400, got %d", rec.Code)
	}
	rec = machineCall(t, r, http.MethodPost, "/api/auth/register", "", fmt.Sprintf(`{"username":"second","password":"password-123","invite":%q}`, token2))
	if rec.Code != http.StatusOK {
		t.Fatalf("invite must survive field-validation failure: %d %s", rec.Code, rec.Body.String())
	}

	// revoke: идемпотентный 204, статус становится revoked (живое
	// приглашение); отзыв использованного — no-op (статус остаётся used).
	rec = adminCall(t, r, http.MethodPost, "/panel/api/admin/invites", testPanelToken, `{"ttl_hours":72}`)
	var created3 struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created3)
	rec = adminCall(t, r, http.MethodDelete, fmt.Sprintf("/panel/api/admin/invites/%d", created3.ID), testPanelToken, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: want 204, got %d", rec.Code)
	}
	rec = adminCall(t, r, http.MethodDelete, fmt.Sprintf("/panel/api/admin/invites/%d", created3.ID), testPanelToken, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("re-revoke must be idempotent: got %d", rec.Code)
	}
	rec = adminCall(t, r, http.MethodGet, "/panel/api/admin/invites", testPanelToken, "")
	if !strings.Contains(rec.Body.String(), `"revoked"`) || !strings.Contains(rec.Body.String(), `"exhausted"`) {
		t.Fatalf("list must show revoked and exhausted statuses: %s", rec.Body.String())
	}
}
