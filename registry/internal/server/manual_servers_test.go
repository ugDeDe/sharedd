package server

// Тесты manual-серверов и self-rotate authkey (docs/specs/addendum-manual-servers.md
// §4, items 1-6). Детерминированы: SSH подменяется fakeExec, сеть не нужна,
// провайдер не вызывается вовсе (manual путь его не касается).

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode"

	"sharedd/registry/internal/control"
	"sharedd/registry/internal/cryptoutil"
	"sharedd/registry/internal/sshx"
)

// waitWorkerIdle ждёт, пока воркер перестанет держать операцию (иначе retry
// увидит in_progress и даст ложный 409).
func waitWorkerIdle(r *Registry, opID int64) {
	deadline := time.Now().Add(2 * time.Second)
	for r.provisionWorkers().runningOp(opID) && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
}

func decodeManualHost(t *testing.T, rec *httptest.ResponseRecorder) manualHostJSON {
	t.Helper()
	var h manualHostJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatalf("decode manual host: %v", err)
	}
	return h
}

// hasCyrillic — грубая проверка, что строка русскоязычная (B1).
func hasCyrillic(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Cyrillic, r) {
			return true
		}
	}
	return false
}

// ── §4.1 rotate/delete authkey ────────────────────────────────────

func TestAuthkeyRotateDeleteAndIsolation(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	aliceID := cabinetUser(t, r, "alice", 5)
	cabinetUser(t, r, "bob", 5)
	sessA, csrfA := cabinetLogin(t, r, "alice", "password-123")
	sessB, csrfB := cabinetLogin(t, r, "bob", "password-123")

	// bob получает свой ключ — ротация alice не должна его затрагивать.
	recB := cabinetCall(t, r, http.MethodPost, "/api/me/authkey/rotate", cookieMap(sessB, csrfB), csrfB, "")
	if recB.Code != http.StatusOK {
		t.Fatalf("bob rotate: %d %s", recB.Code, recB.Body.String())
	}
	var rb struct {
		Authkey string `json:"authkey"`
	}
	_ = json.Unmarshal(recB.Body.Bytes(), &rb)
	if rb.Authkey == "" {
		t.Fatal("rotate must return the full key once")
	}

	// alice rotate #1 и #2
	rot := func() string {
		rec := cabinetCall(t, r, http.MethodPost, "/api/me/authkey/rotate", cookieMap(sessA, csrfA), csrfA, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("alice rotate: %d %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Authkey string `json:"authkey"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return resp.Authkey
	}
	key1 := rot()
	key2 := rot()
	if key1 == "" || key2 == "" || key1 == key2 {
		t.Fatalf("rotate must issue a fresh key each time: %q vs %q", key1, key2)
	}
	// старый ключ немедленно недействителен
	if _, err := r.control.LookupAuthkey(key1); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("old key must be revoked, got err=%v", err)
	}
	// новый ключ жив и принадлежит alice
	u, err := r.control.LookupAuthkey(key2)
	if err != nil || u.ID != aliceID {
		t.Fatalf("new key: user=%d err=%v, want alice(%d)", u.ID, err, aliceID)
	}
	// чужой (bob) ключ не тронут
	if _, err := r.control.LookupAuthkey(rb.Authkey); err != nil {
		t.Fatalf("bob's key must be untouched, got %v", err)
	}
	// GET — prefix-only, полного ключа нет
	rec := cabinetCall(t, r, http.MethodGet, "/api/me/authkey", cookieMap(sessA, csrfA), "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get authkey: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), key2) {
		t.Fatal("GET /api/me/authkey must never return the full key")
	}
	// DELETE отзывает все ключи alice
	rec = cabinetCall(t, r, http.MethodDelete, "/api/me/authkey", cookieMap(sessA, csrfA), csrfA, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete authkey: want 204, got %d %s", rec.Code, rec.Body.String())
	}
	if _, err := r.control.LookupAuthkey(key2); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("key must be revoked after DELETE, got %v", err)
	}
	rec = cabinetCall(t, r, http.MethodGet, "/api/me/authkey", cookieMap(sessA, csrfA), "", "")
	var ak authkeyJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &ak)
	if ak.Active {
		t.Fatalf("authkey must be inactive after DELETE: %+v", ak)
	}
	// bob's key всё ещё жив
	if _, err := r.control.LookupAuthkey(rb.Authkey); err != nil {
		t.Fatalf("bob's key must survive alice DELETE, got %v", err)
	}
}

// ── §4.2 валидация сервера ────────────────────────────────────────

func TestManualServerValidationMatrix(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	r.cfg.Panel.PublicURL = "https://93.184.216.34" // IP регистратора для теста
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	ck := cookieMap(sess, csrf)

	_, priv, err := cryptoutil.GenerateSSHKeyPair()
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	bigKey := strings.Repeat("A", 17*1024)

	// B1: каждое сообщение валидации — по-русски (кабинет русскоязычный).
	// want — ожидаемая русская подстрока, чтобы фиксировать и язык, и причину.
	bad := []struct {
		name string
		body string
		want string
	}{
		{"private ip", `{"label":"x","host":"10.0.0.1","password":"p"}`, "публичным"},
		{"loopback", `{"label":"x","host":"127.0.0.1","password":"p"}`, "публичным"},
		{"link-local", `{"label":"x","host":"169.254.1.1","password":"p"}`, "публичным"},
		{"not an ip", `{"label":"x","host":"not-an-ip","password":"p"}`, "IPv4"},
		{"registry ip", `{"label":"x","host":"93.184.216.34","password":"p"}`, "регистратор"},
		{"port 0", `{"label":"x","host":"8.8.8.8","port":0,"password":"p"}`, "порт"},
		{"port 70000", `{"label":"x","host":"8.8.8.8","port":70000,"password":"p"}`, "порт"},
		{"both secrets", `{"label":"x","host":"8.8.8.8","password":"p","private_key":"k"}`, "ровно одно"},
		{"no secret", `{"label":"x","host":"8.8.8.8"}`, "ровно одно"},
		{"bad key", `{"label":"x","host":"8.8.8.8","private_key":"not-a-key"}`, "приватный ключ"},
		{"huge key", fmt.Sprintf(`{"label":"x","host":"8.8.8.8","private_key":%q}`, bigKey), "16 КиБ"},
		{"bad ssh_user", `{"label":"x","host":"8.8.8.8","ssh_user":"Root","password":"p"}`, "пользователь SSH"},
		{"empty label", `{"label":"","host":"8.8.8.8","password":"p"}`, "название"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			rec := cabinetCall(t, r, http.MethodPost, "/api/me/servers", ck, csrf, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d %s", rec.Code, rec.Body.String())
			}
			if code := errCode(t, rec); code != "validation_failed" {
				t.Fatalf("want validation_failed, got %q", code)
			}
			var e struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &e)
			if strings.TrimSpace(e.Message) == "" {
				t.Fatal("validation error must carry a human-readable reason")
			}
			if !hasCyrillic(e.Message) {
				t.Fatalf("validation message must be Russian, got %q", e.Message)
			}
			if !strings.Contains(e.Message, tc.want) {
				t.Fatalf("message %q must contain %q", e.Message, tc.want)
			}
		})
	}

	// валидные: пароль и ключ
	if rec := cabinetCall(t, r, http.MethodPost, "/api/me/servers", ck, csrf,
		`{"label":"pw","host":"8.8.8.8","password":"secret"}`); rec.Code != http.StatusCreated {
		t.Fatalf("valid password server: %d %s", rec.Code, rec.Body.String())
	}
	if rec := cabinetCall(t, r, http.MethodPost, "/api/me/servers", ck, csrf,
		fmt.Sprintf(`{"label":"key","host":"1.1.1.1","private_key":%q}`, priv)); rec.Code != http.StatusCreated {
		t.Fatalf("valid key server: %d %s", rec.Code, rec.Body.String())
	}
}

// ── B2: дубликат host:port отвергается, второго места/ноды нет ────

func TestManualServerDuplicateHostRejected(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	r.cfg.Security.NodeToken = testNodeToken
	id := cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	ck := cookieMap(sess, csrf)

	rec := cabinetCall(t, r, http.MethodPost, "/api/me/servers", ck, csrf,
		`{"label":"box","host":"8.8.8.8","port":22,"password":"secret"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	host := decodeManualHost(t, rec)

	// Установка резервирует одно место и создаёт идентичность ноды manual-<id>.
	r.sshExec.(*fakeExec).runFn = verifyExecFn(r, true)
	rec = cabinetCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/servers/%d/install", host.ID), ck, csrf, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("install: %d %s", rec.Code, rec.Body.String())
	}
	op := unmarshalOp(t, rec.Body.Bytes(), &operationJSON{})
	final := waitOpTerminal(t, r, op.ID)
	if final.State != control.OpDone {
		t.Fatalf("install must finish done, got %s", final.State)
	}

	// Второй POST того же host:port (порт 22 по умолчанию) → 409 duplicate_host,
	// без второго слота и без второй ноды.
	rec = cabinetCall(t, r, http.MethodPost, "/api/me/servers", ck, csrf,
		`{"label":"box-again","host":"8.8.8.8","password":"other"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate host:port must be 409, got %d %s", rec.Code, rec.Body.String())
	}
	if code := errCode(t, rec); code != "duplicate_host" {
		t.Fatalf("want duplicate_host, got %q", code)
	}
	var e struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if !hasCyrillic(e.Message) {
		t.Fatalf("duplicate_host message must be Russian, got %q", e.Message)
	}

	hosts, err := r.control.ManualHostsByUser(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 {
		t.Fatalf("duplicate must not add a record, got %d", len(hosts))
	}
	if used, _ := r.control.UsedNodes(id); used != 1 {
		t.Fatalf("duplicate must not consume a second slot, used=%d", used)
	}
	nodes, err := r.control.NodesByUser(id)
	if err != nil {
		t.Fatal(err)
	}
	// node_id manual-ноды известен только после claim'а (/register), поэтому
	// сверяем с финальным состоянием операции, а не с ответом install.
	if len(nodes) != 1 || nodes[0].NodeID != final.NodeID {
		t.Fatalf("duplicate must not create a second node identity: %+v", nodes)
	}

	// Другой порт того же хоста — допустим (UNIQUE включает port).
	rec = cabinetCall(t, r, http.MethodPost, "/api/me/servers", ck, csrf,
		// label обязан уложиться в 1..10 символов (name-часть NAME-HASH).
		`{"label":"box-ssh","host":"8.8.8.8","port":2222,"password":"p"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("same host on another port must be allowed: %d %s", rec.Code, rec.Body.String())
	}
}

// ── B1: профиль-селект отдаёт русское сообщение ────────────────────

func TestProfileSelectErrorsAreRussian(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	r.cfg.Security.NodeToken = testNodeToken
	r.cfg.Provisioning.UserProfilesEnabled = boolPtr(false)
	aliceID := cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	ck := cookieMap(sess, csrf)

	// Свой неопубликованный профиль при выключенных user-профилях → 403
	// user_profiles_disabled с русским сообщением.
	own, err := r.control.CreateProfile("draft", "", aliceID)
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	if _, err := r.control.AddProfileVersion(own.ID, "- name: noop\n  command: 'true'", ""); err != nil {
		t.Fatalf("create profile version: %v", err)
	}

	rec := cabinetCall(t, r, http.MethodPost, "/api/me/servers", ck, csrf,
		`{"label":"box","host":"8.8.8.8","password":"p"}`)
	host := decodeManualHost(t, rec)
	rec = cabinetCall(t, r, http.MethodPost,
		fmt.Sprintf("/api/me/servers/%d/install", host.ID), ck, csrf,
		fmt.Sprintf(`{"profile_id":%d}`, own.ID))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d %s", rec.Code, rec.Body.String())
	}
	if code := errCode(t, rec); code != "user_profiles_disabled" {
		t.Fatalf("want user_profiles_disabled, got %q", code)
	}
	var e struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if !hasCyrillic(e.Message) {
		t.Fatalf("profile-select message must be Russian, got %q", e.Message)
	}
}

// ── §4.3 один слот на сервер; retry без провайдерских вызовов ─────

func TestManualInstallOneSlotAndRetryNoProviderCalls(t *testing.T) {
	r, fake := newCabinetRegistry(t)
	r.cfg.Security.NodeToken = testNodeToken
	id := cabinetUser(t, r, "alice", 1)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	ck := cookieMap(sess, csrf)

	rec := cabinetCall(t, r, http.MethodPost, "/api/me/servers", ck, csrf,
		`{"label":"box","host":"8.8.8.8","password":"secret"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create server: %d %s", rec.Code, rec.Body.String())
	}
	host := decodeManualHost(t, rec)

	exec := r.sshExec.(*fakeExec)
	// первая установка падает
	exec.runErr = errors.New("install boom")
	rec = cabinetCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/servers/%d/install", host.ID), ck, csrf, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("install: %d %s", rec.Code, rec.Body.String())
	}
	op := unmarshalOp(t, rec.Body.Bytes(), &operationJSON{})
	final := waitOpTerminal(t, r, op.ID)
	waitWorkerIdle(r, op.ID)
	if final.State != control.OpFailed {
		t.Fatalf("first install must fail, got %s", final.State)
	}
	if used, _ := r.control.UsedNodes(id); used != 1 {
		t.Fatalf("one slot per server expected, got %d", used)
	}

	// retry (same endpoint): та же операция, та же нода, ноль провайдерских вызовов
	exec.runErr = nil
	exec.runFn = verifyExecFn(r, true)
	rec = cabinetCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/servers/%d/install", host.ID), ck, csrf, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry install: %d %s", rec.Code, rec.Body.String())
	}
	op2 := unmarshalOp(t, rec.Body.Bytes(), &operationJSON{})
	if op2.ID != op.ID {
		t.Fatalf("retry must reuse the same operation: %d vs %d", op2.ID, op.ID)
	}
	if final := waitOpTerminal(t, r, op2.ID); final.State != control.OpDone {
		t.Fatalf("retry must finish done, got %s (err=%q)", final.State, final.LastError)
	}
	if used, _ := r.control.UsedNodes(id); used != 1 {
		t.Fatalf("retry must not add a slot, used=%d", used)
	}
	ops, _ := r.control.OperationsByUser(id)
	manualOps := 0
	for _, o := range ops {
		if o.Kind == control.OpKindManualInstall {
			manualOps++
		}
	}
	if manualOps != 1 {
		t.Fatalf("exactly one manual_install operation expected, got %d", manualOps)
	}
	if fake.creates() != 0 {
		t.Fatalf("manual install must make zero provider calls, got %d", fake.creates())
	}
}

// ── §4.4 секреты не утекают ───────────────────────────────────────

func TestManualServerSecretsNeverLeaked(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	r.cfg.Security.NodeToken = testNodeToken
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	ck := cookieMap(sess, csrf)

	const secret = "manual-password-secret-xyz"
	rec := cabinetCall(t, r, http.MethodPost, "/api/me/servers", ck, csrf,
		fmt.Sprintf(`{"label":"box","host":"8.8.8.8","password":%q}`, secret))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	host := decodeManualHost(t, rec)

	// вывод операции эхом отдаёт секрет → он обязан быть замаскирован
	exec := r.sshExec.(*fakeExec)
	exec.runFn = func(command, stdin string) (sshx.Result, error) {
		return sshx.Result{Stdout: "leaked=" + secret, Code: 1}, nil
	}
	rec = cabinetCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/servers/%d/install", host.ID), ck, csrf, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("install: %d %s", rec.Code, rec.Body.String())
	}
	op := unmarshalOp(t, rec.Body.Bytes(), &operationJSON{})
	final := waitOpTerminal(t, r, op.ID)
	if strings.Contains(final.LastError, secret) {
		t.Fatalf("secret leaked into operations.output/last_error: %q", final.LastError)
	}

	// запись в БД — шифротекст, не плейнтекст
	stored, err := r.control.ManualHost(host.ID)
	if err != nil {
		t.Fatalf("load host: %v", err)
	}
	if stored.SecretEnc == secret || strings.Contains(stored.SecretEnc, secret) {
		t.Fatal("secret must be stored encrypted, not in plaintext")
	}

	// ни один read-эндпоинт не отдаёт секрет
	for _, e := range []struct{ method, path string }{
		{http.MethodGet, "/api/me/servers"},
		{http.MethodGet, "/api/me/operations"},
	} {
		rec := cabinetCall(t, r, e.method, e.path, ck, "", "")
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("%s %s leaked secret: %s", e.method, e.path, rec.Body.String())
		}
	}
}

// ── §4.5 шаги с node_secret + подтверждённая регистрация ──────────

func TestManualInstallInjectsSecretAndConfirmsRegistration(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	r.cfg.Security.NodeToken = testNodeToken
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	ck := cookieMap(sess, csrf)

	exec := r.sshExec.(*fakeExec)
	exec.runFn = verifyExecFn(r, true)

	rec := cabinetCall(t, r, http.MethodPost, "/api/me/servers", ck, csrf,
		`{"label":"box","host":"8.8.8.8","password":"secret"}`)
	host := decodeManualHost(t, rec)
	rec = cabinetCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/servers/%d/install", host.ID), ck, csrf, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("install: %d %s", rec.Code, rec.Body.String())
	}
	op := unmarshalOp(t, rec.Body.Bytes(), &operationJSON{})
	final := waitOpTerminal(t, r, op.ID)
	if final.State != control.OpDone {
		t.Fatalf("want done, got %s (err=%q)", final.State, final.LastError)
	}
	secret := exec.nodeSecretFromCommands()
	if secret == "" {
		t.Fatal("install steps must carry the injected node_secret")
	}
	// node_id известен только после claim'а — берём из финальной операции.
	if owner, ok, err := r.control.NodeSecretOwner(secret); err != nil || !ok || owner != final.NodeID {
		t.Fatalf("node_secret not bound to node: owner=%q ok=%t err=%v", owner, ok, err)
	}
}

// ── §4.6 удаление освобождает место, нода остаётся в пуле ─────────

func TestManualServerDeleteReleasesSlotKeepsPoolNode(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	r.cfg.Security.NodeToken = testNodeToken
	id := cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	ck := cookieMap(sess, csrf)
	r.sshExec.(*fakeExec).runFn = verifyExecFn(r, true)

	rec := cabinetCall(t, r, http.MethodPost, "/api/me/servers", ck, csrf,
		`{"label":"box","host":"8.8.8.8","password":"secret"}`)
	host := decodeManualHost(t, rec)
	rec = cabinetCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/servers/%d/install", host.ID), ck, csrf, `{}`)
	op := unmarshalOp(t, rec.Body.Bytes(), &operationJSON{})
	final := waitOpTerminal(t, r, op.ID)
	if final.State != control.OpDone {
		t.Fatalf("install: got %s", final.State)
	}
	if used, _ := r.control.UsedNodes(id); used != 1 {
		t.Fatalf("want 1 used slot, got %d", used)
	}
	// node_id известен только после claim'а — берём из финальной операции.
	r.mu.RLock()
	_, inPool := r.state.Candidates[final.NodeID]
	r.mu.RUnlock()
	if !inPool {
		t.Fatal("registered node must be in the pool")
	}

	rec = cabinetCall(t, r, http.MethodDelete, fmt.Sprintf("/api/me/servers/%d", host.ID), ck, csrf, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: want 204, got %d %s", rec.Code, rec.Body.String())
	}
	if used, _ := r.control.UsedNodes(id); used != 0 {
		t.Fatalf("delete must release the slot, used=%d", used)
	}
	r.mu.RLock()
	_, stillPool := r.state.Candidates[final.NodeID]
	r.mu.RUnlock()
	if !stillPool {
		t.Fatal("pool node must remain after server delete")
	}
}

// ── owner checks + check endpoint ─────────────────────────────────

func TestManualServerOwnerChecks(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	cabinetUser(t, r, "bob", 5)
	sessA, csrfA := cabinetLogin(t, r, "alice", "password-123")
	rec := cabinetCall(t, r, http.MethodPost, "/api/me/servers", cookieMap(sessA, csrfA), csrfA,
		`{"label":"box","host":"8.8.8.8","password":"secret"}`)
	host := decodeManualHost(t, rec)

	sessB, csrfB := cabinetLogin(t, r, "bob", "password-123")
	ckB := cookieMap(sessB, csrfB)
	if rec := cabinetCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/servers/%d/install", host.ID), ckB, csrfB, `{}`); rec.Code != http.StatusNotFound {
		t.Fatalf("bob install alice's server: want 404, got %d", rec.Code)
	}
	if rec := cabinetCall(t, r, http.MethodDelete, fmt.Sprintf("/api/me/servers/%d", host.ID), ckB, csrfB, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("bob delete alice's server: want 404, got %d", rec.Code)
	}
	rec = cabinetCall(t, r, http.MethodGet, "/api/me/servers", ckB, "", "")
	var list struct {
		Servers []manualHostJSON `json:"servers"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Servers) != 0 {
		t.Fatalf("bob must not see alice's servers: %+v", list.Servers)
	}
}

// ── переустановка (reinstall) завершённого manual-сервера ─────────

// publishAltProfile — опубликованный альтернативный профиль (selectable для
// любого пользователя) с теми же шагами, что штатный: verify-эмуляция
// (verifyExecFn) регистрирует ноду по маркерам default-профиля.
func publishAltProfile(t *testing.T, r *Registry) control.Profile {
	t.Helper()
	p, err := r.control.CreateProfile("alt-install", "переустановка", 0)
	if err != nil {
		t.Fatalf("create alt profile: %v", err)
	}
	if _, err := r.control.AddProfileVersion(p.ID, control.DefaultProfileSteps, control.DefaultProfileVarsSchema); err != nil {
		t.Fatalf("alt profile version: %v", err)
	}
	pub, err := r.control.PublishProfile(p.ID)
	if err != nil {
		t.Fatalf("publish alt profile: %v", err)
	}
	return pub
}

// Reinstall a done server: same op/idem_key, NEW selectable profile, quota
// unchanged, node re-binds, no orphan node_owners/pending_node_secrets rows.
func TestManualReinstallDoneAllowsProfileSwitch(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	r.cfg.Security.NodeToken = testNodeToken
	id := cabinetUser(t, r, "alice", 1) // ровно одно место — reinstall не должен его тратить
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	ck := cookieMap(sess, csrf)
	r.sshExec.(*fakeExec).runFn = verifyExecFn(r, true)
	def, err := r.control.ProfileByName(control.DefaultProfileName)
	if err != nil {
		t.Fatal(err)
	}
	alt := publishAltProfile(t, r)

	rec := cabinetCall(t, r, http.MethodPost, "/api/me/servers", ck, csrf,
		`{"label":"box","host":"8.8.8.8","password":"secret"}`)
	host := decodeManualHost(t, rec)

	// первая установка штатным профилем
	rec = cabinetCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/servers/%d/install", host.ID), ck, csrf, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("install: %d %s", rec.Code, rec.Body.String())
	}
	op := unmarshalOp(t, rec.Body.Bytes(), &operationJSON{})
	final := waitOpTerminal(t, r, op.ID)
	if final.State != control.OpDone {
		t.Fatalf("first install: %s (%q)", final.State, final.LastError)
	}
	waitWorkerIdle(r, op.ID)
	if final.ProfileID != def.ID {
		t.Fatalf("first profile = %d, want default %d", final.ProfileID, def.ID)
	}
	oldNodeID := final.NodeID
	if used, _ := r.control.UsedNodes(id); used != 1 {
		t.Fatalf("after first install used=%d, want 1", used)
	}

	// переустановка с ДРУГИМ профилем → 200, тот же op, новый профиль
	rec = cabinetCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/servers/%d/install", host.ID), ck, csrf,
		fmt.Sprintf(`{"profile_id":%d}`, alt.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("reinstall: want 200, got %d %s", rec.Code, rec.Body.String())
	}
	op2 := unmarshalOp(t, rec.Body.Bytes(), &operationJSON{})
	if op2.ID != op.ID {
		t.Fatalf("reinstall must reuse the operation: %d vs %d", op2.ID, op.ID)
	}
	if op2.State != control.OpInstalling {
		t.Fatalf("reinstall must return installing, got %s", op2.State)
	}
	if op2.ProfileID != alt.ID {
		t.Fatalf("reinstall response profile = %d, want %d", op2.ProfileID, alt.ID)
	}
	final2 := waitOpTerminal(t, r, op2.ID)
	if final2.State != control.OpDone {
		t.Fatalf("reinstall: %s (%q)", final2.State, final2.LastError)
	}
	if final2.ProfileID != alt.ID {
		t.Fatalf("final profile = %d, want %d", final2.ProfileID, alt.ID)
	}
	if final2.NodeID == "" {
		t.Fatal("reinstall must re-bind a node")
	}

	// квота та же; ровно одна нода; нет pending-сироты
	if used, _ := r.control.UsedNodes(id); used != 1 {
		t.Fatalf("reinstall must not add a slot, used=%d", used)
	}
	nodes, err := r.control.NodesByUser(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].NodeID != final2.NodeID {
		t.Fatalf("exactly one node expected, got %+v", nodes)
	}
	if _, err := r.control.PendingSecretByOperation(op2.ID); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("pending secret must be consumed after reinstall, err=%v", err)
	}
	// смены node_id в эмуляции не было (label тот же) — старая привязка совпадает
	// с новой; убеждаемся, что сироты нет.
	if oldNodeID != final2.NodeID {
		if _, _, ok, _ := r.control.NodeOwner(oldNodeID); ok {
			t.Fatalf("old node binding %s must be retired", oldNodeID)
		}
	}
	ops, _ := r.control.OperationsByUser(id)
	manualOps := 0
	for _, o := range ops {
		if o.Kind == control.OpKindManualInstall {
			manualOps++
		}
	}
	if manualOps != 1 {
		t.Fatalf("reinstall must not create a second operation, got %d", manualOps)
	}
}

// Reinstall with the SAME profile (no profile_id in body) stays on the current
// profile and completes again.
func TestManualReinstallSameProfile(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	r.cfg.Security.NodeToken = testNodeToken
	id := cabinetUser(t, r, "alice", 2)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	ck := cookieMap(sess, csrf)
	r.sshExec.(*fakeExec).runFn = verifyExecFn(r, true)
	def, err := r.control.ProfileByName(control.DefaultProfileName)
	if err != nil {
		t.Fatal(err)
	}

	rec := cabinetCall(t, r, http.MethodPost, "/api/me/servers", ck, csrf,
		`{"label":"box","host":"8.8.8.8","password":"secret"}`)
	host := decodeManualHost(t, rec)
	rec = cabinetCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/servers/%d/install", host.ID), ck, csrf, `{}`)
	op := unmarshalOp(t, rec.Body.Bytes(), &operationJSON{})
	if final := waitOpTerminal(t, r, op.ID); final.State != control.OpDone {
		t.Fatalf("first install: %s (%q)", final.State, final.LastError)
	}
	waitWorkerIdle(r, op.ID)

	rec = cabinetCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/servers/%d/install", host.ID), ck, csrf, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("same-profile reinstall: want 200, got %d %s", rec.Code, rec.Body.String())
	}
	op2 := unmarshalOp(t, rec.Body.Bytes(), &operationJSON{})
	if op2.ProfileID != def.ID {
		t.Fatalf("same-profile reinstall changed profile: %d != %d", op2.ProfileID, def.ID)
	}
	if final := waitOpTerminal(t, r, op2.ID); final.State != control.OpDone {
		t.Fatalf("same-profile reinstall: %s (%q)", final.State, final.LastError)
	}
	if used, _ := r.control.UsedNodes(id); used != 1 {
		t.Fatalf("used=%d, want 1", used)
	}
}

// In-progress states still refuse a second install (409 in_progress).
func TestManualReinstallInProgressRejected(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	r.cfg.Security.NodeToken = testNodeToken
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	ck := cookieMap(sess, csrf)
	r.sshExec.(*fakeExec).runFn = verifyExecFn(r, true)

	rec := cabinetCall(t, r, http.MethodPost, "/api/me/servers", ck, csrf,
		`{"label":"box","host":"8.8.8.8","password":"secret"}`)
	host := decodeManualHost(t, rec)
	rec = cabinetCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/servers/%d/install", host.ID), ck, csrf, `{}`)
	op := unmarshalOp(t, rec.Body.Bytes(), &operationJSON{})
	if final := waitOpTerminal(t, r, op.ID); final.State != control.OpDone {
		t.Fatalf("install: %s (%q)", final.State, final.LastError)
	}
	waitWorkerIdle(r, op.ID)

	for _, state := range []string{control.OpPending, control.OpSent, control.OpDeleting} {
		if err := r.control.UpdateOperationState(op.ID, state, "", ""); err != nil {
			t.Fatal(err)
		}
		rec := cabinetCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/servers/%d/install", host.ID), ck, csrf, `{}`)
		if rec.Code != http.StatusConflict || errCode(t, rec) != "in_progress" {
			t.Fatalf("state %s: want 409 in_progress, got %d %s", state, rec.Code, rec.Body.String())
		}
	}
}

// Reinstalling someone else's (done) server is still hidden as 404.
func TestManualReinstallForeignServerRejected(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	r.cfg.Security.NodeToken = testNodeToken
	cabinetUser(t, r, "alice", 5)
	cabinetUser(t, r, "bob", 5)
	sessA, csrfA := cabinetLogin(t, r, "alice", "password-123")
	sessB, csrfB := cabinetLogin(t, r, "bob", "password-123")
	r.sshExec.(*fakeExec).runFn = verifyExecFn(r, true)

	rec := cabinetCall(t, r, http.MethodPost, "/api/me/servers", cookieMap(sessA, csrfA), csrfA,
		`{"label":"box","host":"8.8.8.8","password":"secret"}`)
	host := decodeManualHost(t, rec)
	rec = cabinetCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/servers/%d/install", host.ID), cookieMap(sessA, csrfA), csrfA, `{}`)
	op := unmarshalOp(t, rec.Body.Bytes(), &operationJSON{})
	if final := waitOpTerminal(t, r, op.ID); final.State != control.OpDone {
		t.Fatalf("install: %s (%q)", final.State, final.LastError)
	}
	waitWorkerIdle(r, op.ID)

	rec = cabinetCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/servers/%d/install", host.ID), cookieMap(sessB, csrfB), csrfB, `{}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bob reinstall alice's server: want 404, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestManualServerCheck(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	ck := cookieMap(sess, csrf)
	rec := cabinetCall(t, r, http.MethodPost, "/api/me/servers", ck, csrf,
		`{"label":"box","host":"8.8.8.8","password":"secret"}`)
	host := decodeManualHost(t, rec)
	rec = cabinetCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/servers/%d/check", host.ID), ck, csrf, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("check: %d %s", rec.Code, rec.Body.String())
	}
	var res struct {
		Verified bool   `json:"verified"`
		LastErr  string `json:"last_error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if !res.Verified || res.LastErr != "" {
		t.Fatalf("check must verify with the fake executor: %+v", res)
	}
}
