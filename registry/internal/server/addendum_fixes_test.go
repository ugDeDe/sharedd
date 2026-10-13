package server

// addendum-profiles-vps.md §7 (обязательные фиксы) и §5.7-8:
//   A — предсозданная личность ноды: агент с выданным node_secret
//       регистрируется end-to-end; секрет не утекает в read-эндпоинты.
//   B — retry-install переиспользует provider_vps_id: ноль платных вызовов.
//   D — свой неопубликованный профиль выбирается владельцем; чужой → 404.
//   E — admin JSON профиля отдаёт число версий.
// Все вызовы Beget мокаются; реальный SSH не запускается.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sharedd/registry/internal/control"
)

const testNodeToken = "node-shared-token"

func linkBeget(t *testing.T, r *Registry, sess, csrf string) {
	t.Helper()
	rec := cabinetCall(t, r, http.MethodPut, "/api/me/beget/link", cookieMap(sess, csrf), csrf,
		`{"login":"a@beget","password":"s3cret"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("beget link: %d %s", rec.Code, rec.Body.String())
	}
}

// A/§5.8 — предсозданная личность: node_secret, инъектированный установщику,
// принимается на POST /register для своей ноды; read-эндпоинты его не отдают.
func TestCabinetPreProvisionedIdentityRegister(t *testing.T) {
	r, fake := newCabinetRegistry(t)
	r.cfg.Security.NodeToken = testNodeToken // активирует machine-аутентификацию
	exec := r.sshExec.(*fakeExec)
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	linkBeget(t, r, sess, csrf)

	rec := cabinetCall(t, r, http.MethodPost, "/api/me/vps", cookieMap(sess, csrf), csrf,
		`{"region":"ru1","idem_key":"idem-ident","confirm":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("vps: %d %s", rec.Code, rec.Body.String())
	}
	var created operationJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	waitOpTerminal(t, r, created.ID) // установка идёт фоновым воркером (H1)
	secret := exec.nodeSecretFromCommands()
	if secret == "" {
		t.Fatal("installer steps must carry the pre-created node_secret")
	}
	nodeID := nodeIDForIdem("idem-ident")

	// агент регистрируется персональным секретом (не общим токеном).
	body := fmt.Sprintf(`{"node_id":%q,"ip":"8.8.8.8","node_type":"classic"}`, nodeID)
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	regRec := httptest.NewRecorder()
	r.buildMux().ServeHTTP(regRec, req)
	if regRec.Code != http.StatusOK {
		t.Fatalf("register with pre-created secret: %d %s", regRec.Code, regRec.Body.String())
	}

	// секрет не возвращается read-эндпоинтами.
	for _, path := range []string{"/api/me/operations", "/api/me/nodes"} {
		out := cabinetCall(t, r, http.MethodGet, path, cookieMap(sess, csrf), "", "")
		if strings.Contains(out.Body.String(), secret) {
			t.Fatalf("%s leaked node_secret", path)
		}
	}
	if fake.creates() != 1 {
		t.Fatalf("CreateVps calls = %d, want 1", fake.creates())
	}
}

// B — retry-install после провала установки: ноль новых CreateVps; повтор на
// завершённой операции отклоняется.
func TestCabinetRetryInstallNoPaidCall(t *testing.T) {
	r, fake := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	linkBeget(t, r, sess, csrf)

	rec := cabinetCall(t, r, http.MethodPost, "/api/me/vps", cookieMap(sess, csrf), csrf,
		`{"region":"ru1","idem_key":"idem-retry","confirm":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("vps: %d %s", rec.Code, rec.Body.String())
	}
	var op operationJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &op)
	if fake.creates() != 1 {
		t.Fatalf("CreateVps calls = %d, want 1", fake.creates())
	}
	waitOpTerminal(t, r, op.ID) // первичная установка (фон) завершена

	// имитируем провал установки (VPS уже создан).
	if err := r.control.UpdateOperationState(op.ID, control.OpFailed, "", "install boom"); err != nil {
		t.Fatal(err)
	}
	rec = cabinetCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/operations/%d/retry-install", op.ID),
		cookieMap(sess, csrf), csrf, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry-install: %d %s", rec.Code, rec.Body.String())
	}
	if final := waitOpTerminal(t, r, op.ID); final.State != control.OpDone {
		t.Fatalf("retry state = %s, want done", final.State)
	}
	if fake.creates() != 1 {
		t.Fatalf("retry must not issue a paid call, CreateVps=%d", fake.creates())
	}
	// повтор уже завершённой → 409.
	rec = cabinetCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/operations/%d/retry-install", op.ID),
		cookieMap(sess, csrf), csrf, `{}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("retry on done: want 409, got %d %s", rec.Code, rec.Body.String())
	}
	if fake.creates() != 1 {
		t.Fatalf("no paid call allowed, CreateVps=%d", fake.creates())
	}
}

// D — владелец выбирает свой неопубликованный профиль; чужой неопубликованный
// не подтверждаем существованием (404).
func TestCabinetOwnUnpublishedProfileSelectable(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	cabinetUser(t, r, "bob", 5)
	sessA, csrfA := cabinetLogin(t, r, "alice", "password-123")
	linkBeget(t, r, sessA, csrfA)

	body := jsonBody(t, map[string]any{"name": "alice-local", "steps": "# s\necho hi\n"})
	rec := cabinetCall(t, r, http.MethodPost, "/api/me/profiles", cookieMap(sessA, csrfA), csrfA, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create profile: %d %s", rec.Code, rec.Body.String())
	}
	var created profileAdminJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.ID == 0 || created.Published {
		t.Fatalf("local profile must be unpublished: %+v", created)
	}

	// владелец выбирает свой локальный профиль → 200.
	rec = cabinetCall(t, r, http.MethodPost, "/api/me/vps", cookieMap(sessA, csrfA), csrfA,
		fmt.Sprintf(`{"region":"ru1","idem_key":"own-prof","profile_id":%d,"confirm":true}`, created.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("owner selecting own profile: %d %s", rec.Code, rec.Body.String())
	}

	// bob (чужой) с тем же profile_id → 404.
	sessB, csrfB := cabinetLogin(t, r, "bob", "password-123")
	linkBeget(t, r, sessB, csrfB)
	rec = cabinetCall(t, r, http.MethodPost, "/api/me/vps", cookieMap(sessB, csrfB), csrfB,
		fmt.Sprintf(`{"region":"ru1","idem_key":"bob-foreign","profile_id":%d,"confirm":true}`, created.ID))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign unpublished profile: want 404, got %d %s", rec.Code, rec.Body.String())
	}
}

// §7.2 — retry-install сохраняет профиль, выбранный при создании: operation
// JSON отдаёт profile_id, а retry без profile_id не подменяет профиль дефолтным.
func TestCabinetRetryInstallPreservesProfile(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	linkBeget(t, r, sess, csrf)

	const profASteps = "# a\necho PROFILE_A\n"
	body := jsonBody(t, map[string]any{"name": "alice-prof-a", "steps": profASteps})
	rec := cabinetCall(t, r, http.MethodPost, "/api/me/profiles", cookieMap(sess, csrf), csrf, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create profile: %d %s", rec.Code, rec.Body.String())
	}
	var created profileAdminJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	rec = cabinetCall(t, r, http.MethodPost, "/api/me/vps", cookieMap(sess, csrf), csrf,
		fmt.Sprintf(`{"region":"ru1","idem_key":"prof-preserve","profile_id":%d,"confirm":true}`, created.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("vps: %d %s", rec.Code, rec.Body.String())
	}
	var op operationJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &op)
	if op.ProfileID != created.ID {
		t.Fatalf("create response profile_id = %d, want %d", op.ProfileID, created.ID)
	}

	// список операций тоже отдаёт profile_id.
	rec = cabinetCall(t, r, http.MethodGet, "/api/me/operations", cookieMap(sess, csrf), "", "")
	var list struct {
		Operations []operationJSON `json:"operations"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Operations) == 0 || list.Operations[0].ProfileID != created.ID {
		t.Fatalf("operations JSON must expose profile_id=%d: %s", created.ID, rec.Body.String())
	}

	exec := r.sshExec.(*fakeExec)
	waitOpTerminal(t, r, op.ID) // первичная установка (фон) завершена
	// имитируем провал установки (VPS уже создан).
	if err := r.control.UpdateOperationState(op.ID, control.OpFailed, "", "install boom"); err != nil {
		t.Fatal(err)
	}
	// retry с пустым телом: profile_id=0, но должен восстановиться профиль A.
	rec = cabinetCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/operations/%d/retry-install", op.ID),
		cookieMap(sess, csrf), csrf, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry-install: %d %s", rec.Code, rec.Body.String())
	}
	var retried operationJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &retried)
	if retried.ProfileID != created.ID {
		t.Fatalf("retry response profile_id = %d, want %d", retried.ProfileID, created.ID)
	}
	waitOpTerminal(t, r, op.ID) // retry-установка (фон) завершена
	if !strings.Contains(exec.allCommands(), "PROFILE_A") {
		t.Fatalf("retry applied steps = %q, want profile A", exec.allCommands())
	}
}

// E — admin JSON профиля отдаёт число версий (§7.5).
func TestAdminProfilesExposeVersionCount(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	r.cfg.Panel.Token = testPanelToken
	createBody := jsonBody(t, map[string]any{
		"name": "vcount", "steps": "# s\necho hi\n", "published": true,
	})
	rec := adminCall(t, r, http.MethodPost, "/panel/api/admin/profiles", testPanelToken, createBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created profileAdminJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.Versions != 1 {
		t.Fatalf("versions = %d, want 1", created.Versions)
	}
	upd := jsonBody(t, map[string]any{"steps": "# 2\necho 2\n"})
	rec = adminCall(t, r, http.MethodPut, fmt.Sprintf("/panel/api/admin/profiles/%d", created.ID), testPanelToken, upd)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	var updated profileAdminJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &updated)
	if updated.Versions != 2 {
		t.Fatalf("versions after edit = %d, want 2", updated.Versions)
	}
	// список тоже отдаёт число версий.
	rec = adminCall(t, r, http.MethodGet, "/panel/api/admin/profiles", testPanelToken, "")
	if !strings.Contains(rec.Body.String(), `"versions":2`) {
		t.Fatalf("list must expose versions: %s", rec.Body.String())
	}
}
