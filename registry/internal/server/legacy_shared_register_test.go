package server

// «Разрешить общий секрет» = ON: FULL legacy-совместимость (owner intent).
// Валидный общий токен без authkey регистрирует НОВУЮ ноду, привязывая её к
// системному `legacy` в пределах legacy_max_nodes. OFF — строго как раньше.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// newLegacySharedRegistry — user-режим + общий token + настраиваемая квота
// legacy-пользователя (до openControl, иначе max_nodes уже зафиксирован).
func newLegacySharedRegistry(t *testing.T, legacyMaxNodes int) *Registry {
	t.Helper()
	r := newTestRegistry(t)
	r.cfg.Users.Enabled = boolPtr(true)
	r.cfg.Users.LegacyMaxNodes = legacyMaxNodes
	r.cfg.Database.ControlDB = filepath.Join(t.TempDir(), "control.db")
	r.cfg.Security.NodeToken = "shared-secret"
	r.openControl()
	if r.control == nil {
		t.Fatal("control db must open")
	}
	return r
}

func decodeNodeSecret(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var resp struct {
		NodeSecret string `json:"node_secret"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("register body %q: %v", rec.Body.String(), err)
	}
	return resp.NodeSecret
}

// ON: новая нода общим токеном без authkey → 200, владелец legacy, секрет
// выдан; повторная регистрация той же ноды — 200 (без owner_mismatch).
func TestLegacyOnSharedTokenNewNodeBindsLegacy(t *testing.T) {
	r := newLegacySharedRegistry(t, 100)

	rec := registerNode(t, r, "shared-secret", "fresh-abcde", "1.2.3.4", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("new node + shared + ON: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := r.state.Candidates["fresh-abcde"]; !ok {
		t.Fatal("new node must be registered")
	}
	owner, legacy, ok, err := r.control.NodeOwner("fresh-abcde")
	if err != nil || !ok || legacy || owner != r.control.LegacyUserID() {
		t.Fatalf("owner must be legacy user: owner=%d legacy=%t ok=%t err=%v", owner, legacy, ok, err)
	}
	secret := decodeNodeSecret(t, rec)
	if secret == "" {
		t.Fatal("node_secret must be issued for a fresh node")
	}

	// /api/node/status по выданному секрету подтверждает владельца `legacy`.
	req := httptest.NewRequest(http.MethodGet, "/api/node/status?node_id=fresh-abcde", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	statusRec := httptest.NewRecorder()
	r.buildMux().ServeHTTP(statusRec, req)
	if statusRec.Code != http.StatusOK || !strings.Contains(statusRec.Body.String(), `"owner":"legacy"`) {
		t.Fatalf("status: %d %s", statusRec.Code, statusRec.Body.String())
	}

	// Повторная регистрация (тот же агент перезапустился) общим токеном — 200.
	rec = registerNode(t, r, "shared-secret", "fresh-abcde", "1.2.3.4", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("re-register bound node: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// ON: квота legacy_max_nodes соблюдается — лишняя нода 403 quota_exceeded,
// без неограниченной вставки.
func TestLegacyOnSharedTokenQuotaEnforced(t *testing.T) {
	r := newLegacySharedRegistry(t, 2)
	for i, id := range []string{"q-aaaa1", "q-bbbb2", "q-cccc3"} {
		rec := registerNode(t, r, "shared-secret", id, fmt.Sprintf("9.9.9.%d", i+1), "")
		if i < 2 {
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: want 200, got %d %s", id, rec.Code, rec.Body.String())
			}
			continue
		}
		if rec.Code != http.StatusForbidden || errCode(t, rec) != "quota_exceeded" {
			t.Fatalf("over quota: want 403 quota_exceeded, got %d %s", rec.Code, rec.Body.String())
		}
		if _, ok := r.state.Candidates[id]; ok {
			t.Fatalf("%s must not be registered over quota", id)
		}
		if _, _, ok, _ := r.control.NodeOwner(id); ok {
			t.Fatalf("%s must not be bound over quota", id)
		}
	}
}

// OFF: общий токен без authkey новый нодой отклонён (shared_token_disabled).
func TestLegacyOffSharedTokenRejectsNewNode(t *testing.T) {
	r := newLegacySharedRegistry(t, 100)
	adminCall(t, r, http.MethodPost, "/panel/api/admin/legacy-secret", testPanelToken, `{"enabled":false}`)

	rec := registerNode(t, r, "shared-secret", "fresh-abcde", "1.2.3.4", "")
	if rec.Code != http.StatusUnauthorized || errCode(t, rec) != "shared_token_disabled" {
		t.Fatalf("OFF new node: want 401 shared_token_disabled, got %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := r.state.Candidates["fresh-abcde"]; ok {
		t.Fatal("node must not register when OFF")
	}
}

// Существующая legacy-нода перерегистрируется общим токеном без authkey
// (прежнее поведение неизменно) и места в квоте не тратит.
func TestLegacyOnExistingLegacyReRegisterUnchanged(t *testing.T) {
	r := newLegacySharedRegistry(t, 100)
	if err := r.control.BindLegacyNode("legacy-abcde"); err != nil {
		t.Fatal(err)
	}
	rec := registerNode(t, r, "shared-secret", "legacy-abcde", "1.2.3.4", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy re-register: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if used, _ := r.control.UsedNodes(r.control.LegacyUserID()); used != 0 {
		t.Fatalf("legacy re-register must not consume quota: used=%d", used)
	}
}

// Нода реального пользователя по-прежнему требует authkey поверх общего
// токена — legacy-режим её не «отпускает».
func TestLegacyOnUserOwnedNodeStillNeedsAuthkey(t *testing.T) {
	r := newLegacySharedRegistry(t, 100)
	_, key := userKey(t, r, "alice", 5)
	if rec := registerNode(t, r, "shared-secret", "owned-abcde", "1.2.3.4", key); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
	}
	rec := registerNode(t, r, "shared-secret", "owned-abcde", "1.2.3.4", "")
	if rec.Code != http.StatusForbidden || errCode(t, rec) != "bad_authkey" {
		t.Fatalf("user-owned without authkey: want 403 bad_authkey, got %d %s", rec.Code, rec.Body.String())
	}
}

// Неизвестный Bearer (протухший node_secret): ON → 401 stale_node_secret
// (агент сбрасывает секрет и повторяет общим токеном), OFF → 401 unauthorized.
// Произвольная строка НИКОГДА не принимается как валидная.
func TestUnknownBearerStaleNodeSecretCode(t *testing.T) {
	const nodeID = "stale-abcde"

	on := newLegacySharedRegistry(t, 100)
	rec := machineCall(t, on, http.MethodPost, "/heartbeat", "bogus-stale-secret", fmt.Sprintf(`{"node_id":%q}`, nodeID))
	if rec.Code != http.StatusUnauthorized || errCode(t, rec) != "stale_node_secret" {
		t.Fatalf("ON unknown bearer: want 401 stale_node_secret, got %d %s", rec.Code, rec.Body.String())
	}
	// Точный общий токен по-прежнему валиден (не 401) — «фолбэк» работает.
	rec = machineCall(t, on, http.MethodPost, "/heartbeat", "shared-secret", fmt.Sprintf(`{"node_id":%q}`, nodeID))
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("valid shared token must not be rejected: %d %s", rec.Code, rec.Body.String())
	}

	off := newLegacySharedRegistry(t, 100)
	adminCall(t, off, http.MethodPost, "/panel/api/admin/legacy-secret", testPanelToken, `{"enabled":false}`)
	rec = machineCall(t, off, http.MethodPost, "/heartbeat", "bogus-stale-secret", fmt.Sprintf(`{"node_id":%q}`, nodeID))
	if rec.Code != http.StatusUnauthorized || errCode(t, rec) != "unauthorized" {
		t.Fatalf("OFF unknown bearer: want 401 unauthorized, got %d %s", rec.Code, rec.Body.String())
	}
}
