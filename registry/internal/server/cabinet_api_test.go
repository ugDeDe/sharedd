package server

// Тесты кабинета пользователя (docs/api-contract.md §2.1): аутентификация
// session-cookie, CSRF, owner-проверки, идемпотентность VPS-заказа через
// provision, «секреты не отдаются». Реальный Beget не вызывается — фабрика
// клиента подменяется мок-счётчиком.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"sharedd/registry/internal/beget"
	"sharedd/registry/internal/control"
	"sharedd/registry/internal/machineapi"
	"sharedd/registry/internal/sshx"
)

// fakeBeget — мок провайдера: считает CreateVps (деньги-критичный счётчик).
type fakeBeget struct {
	mu          sync.Mutex
	authErr     error
	authCode    string
	regions     []beget.RegionInfo
	createCalls int
	createErr   error
	sshKey      *beget.SSHKey
	// block — если не nil, CreateVps держится на канале (детерминированная
	// гонка: победитель остаётся в незавершённом состоянии).
	block chan struct{}
	// createStarted закрывается на входе в первый CreateVps — детерминированный
	// момент «платный вызов в полёте».
	createStarted chan struct{}
	startedOnce   sync.Once
	// configs — каталог тарифов (nil → дефолт с ценой).
	configs []beget.ConfigurationInfo
}

func (f *fakeBeget) Auth(_ context.Context, code string) error {
	f.mu.Lock()
	f.authCode = code
	f.mu.Unlock()
	return f.authErr
}

func (f *fakeBeget) lastAuthCode() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authCode
}

func (f *fakeBeget) AddSSHKey(_ context.Context, name, _ string) (*beget.SSHKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sshKey == nil {
		f.sshKey = &beget.SSHKey{ID: 7, Name: name}
	}
	return f.sshKey, nil
}

func (f *fakeBeget) GetRegions(context.Context) ([]beget.RegionInfo, error) { return f.regions, nil }

func (f *fakeBeget) GetConfigurations(context.Context) ([]beget.ConfigurationInfo, error) {
	if f.configs != nil {
		return f.configs, nil
	}
	price := 100.0
	return []beget.ConfigurationInfo{{ID: "cfg-1", Name: "Base", CPU: 1, RAM: 1024, Disk: 10240, Price: &price, Available: true}}, nil
}

func (f *fakeBeget) GetInfo(context.Context, string) (*beget.GetInfoResponse, error) {
	return &beget.GetInfoResponse{Vps: &beget.VpsInfo{ID: "beget-vps-123", IPAddress: "203.0.113.9"}}, nil
}

func (f *fakeBeget) GetList(context.Context) (*beget.GetListResponse, error) {
	return &beget.GetListResponse{}, nil
}

func (f *fakeBeget) Remove(context.Context, string) error { return nil }

func (f *fakeBeget) CreateVps(_ context.Context, req beget.CreateVpsRequest) (*beget.CreateVpsResponse, error) {
	f.mu.Lock()
	f.createCalls++
	f.mu.Unlock()
	if f.createStarted != nil {
		f.startedOnce.Do(func() { close(f.createStarted) })
	}
	if f.block != nil {
		<-f.block
	}
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &beget.CreateVpsResponse{Vps: &beget.VpsInfo{ID: "beget-vps-123", Hostname: req.Hostname}}, nil
}

func (f *fakeBeget) creates() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createCalls
}

// newCabinetRegistry — user-режим + контрольная БД + мастер-ключ + мок-Beget.
func newCabinetRegistry(t *testing.T) (*Registry, *fakeBeget) {
	t.Helper()
	r := newTestRegistry(t)
	r.cfg.Users.Enabled = boolPtr(true)
	r.cfg.Users.LegacyMaxNodes = 100000
	dir := t.TempDir()
	r.cfg.Database.ControlDB = filepath.Join(dir, "control.db")
	r.cfg.Database.MasterKeyPath = filepath.Join(dir, "master.key")
	r.cfg.Provisioning.MaxCreatesPerDay = 5
	r.cfg.Provisioning.CooldownMinutes = 30
	r.cfg.Provisioning.MaxUserProfiles = 5
	r.cfg.Provisioning.InstallTimeoutMin = 20
	// SharedProxy.Port — прод-дефолт 443 (config.ApplyRegistryDefaults), из
	// которого provisionBaseVars берёт {{ proxy_port }}. Без него кабинет
	// отклонял установку как validation_failed. newTestRegistry собирает cfg
	// напрямую, минуя applyRegistryDefaults, поэтому ставим явно.
	r.cfg.SharedProxy.Port = 443
	fake := &fakeBeget{regions: []beget.RegionInfo{{ID: "ru1", Name: "Russia", Available: true}}}
	r.begetFactory = func(_, _ string) begetClient { return fake }
	r.openControl()
	if r.control == nil {
		t.Fatal("control db must open")
	}
	if len(r.masterKey) == 0 {
		t.Fatal("master key must load")
	}
	// Мок исполнителя шагов: реальный SSH в тестах не поднимаем.
	r.sshExec = &fakeExec{}
	return r, fake
}

// fakeExec — no-op исполнитель шагов профиля; помнит прогнанные команды
// (для проверки подстановки переменных/профиля без сети).
type fakeExec struct {
	mu       sync.Mutex
	calls    int
	commands []string
	runErr   error
	// runFn — если задан, вызывается вместо дефолтного no-op (для проверки
	// verify-шага против реального mux без сети).
	runFn func(command, stdin string) (sshx.Result, error)
}

func (f *fakeExec) WaitSSH(context.Context, sshx.Target) error { return nil }

func (f *fakeExec) Run(_ context.Context, _ sshx.Target, command, stdin string, _ time.Duration) (sshx.Result, error) {
	f.mu.Lock()
	f.calls++
	f.commands = append(f.commands, command)
	if stdin != "" {
		f.commands = append(f.commands, stdin)
	}
	runFn := f.runFn
	f.mu.Unlock()
	if runFn != nil {
		return runFn(command, stdin)
	}
	if f.runErr != nil {
		return sshx.Result{Stderr: f.runErr.Error()}, f.runErr
	}
	return sshx.Result{Stdout: "ok"}, nil
}

func (f *fakeExec) reset() {
	f.mu.Lock()
	f.calls = 0
	f.commands = nil
	f.mu.Unlock()
}

func (f *fakeExec) allCommands() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.commands, "\n")
}

// nodeSecretFromCommands вытаскивает инъектированный node_secret из прогнанных
// команд. Штатный профиль передаёт персональный секрет как
// --registry-token='SECRET' (install_node.sh не знает --node-secret; contract §1).
func (f *fakeExec) nodeSecretFromCommands() string {
	re := regexp.MustCompile(`--registry-token='([^']*)'`)
	m := re.FindStringSubmatch(f.allCommands())
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func cabinetCall(t *testing.T, r *Registry, method, target string, cookies map[string]string, csrf, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	if csrf != "" {
		req.Header.Set(csrfHeaderName, csrf)
	}
	rec := httptest.NewRecorder()
	r.buildMux().ServeHTTP(rec, req)
	return rec
}

// cabinetLogin — логин через HTTP, возвращает (sessionToken, csrf).
func cabinetLogin(t *testing.T, r *Registry, username, password string) (string, string) {
	t.Helper()
	rec := cabinetCall(t, r, http.MethodPost, "/api/auth/login", nil, "",
		fmt.Sprintf(`{"username":%q,"password":%q}`, username, password))
	if rec.Code != http.StatusOK {
		t.Fatalf("login %s: %d %s", username, rec.Code, rec.Body.String())
	}
	var sess, csrf string
	for _, c := range rec.Result().Cookies() {
		switch c.Name {
		case sessionCookieName:
			sess = c.Value
		case csrfCookieName:
			csrf = c.Value
		}
	}
	if sess == "" || csrf == "" {
		t.Fatalf("login %s: cookies missing: %s", username, rec.Header().Get("Set-Cookie"))
	}
	return sess, csrf
}

// cabinetUser — создать пользователя с квотой (обходя register-дефолт 0).
func cabinetUser(t *testing.T, r *Registry, name string, maxNodes int) int64 {
	t.Helper()
	u, err := r.control.CreateUser(name, "password-123", "user", maxNodes)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return u.ID
}

func cookieMap(sess, csrf string) map[string]string {
	return map[string]string{sessionCookieName: sess, csrfCookieName: csrf}
}

// waitOpTerminal ждёт завершения фоновой установки (H1): опрос состояния
// операции до одного из терминальных. Воркер использует мок-исполнитель, сеть
// не задействована — ожидание короткое.
func waitOpTerminal(t *testing.T, r *Registry, opID int64) control.Operation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		op, err := r.control.Operation(opID)
		if err != nil {
			t.Fatalf("operation %d: %v", opID, err)
		}
		switch op.State {
		case control.OpDone, control.OpFailed, control.OpWaitingVps, control.OpUnknown, control.OpDeleted:
			return op
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %d did not finish, state=%s", opID, op.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// inviteToken — свежее приглашение для саморегистрации (после введения
// обязательных приглашений открытой регистрации больше нет).
func inviteToken(t *testing.T, r *Registry) string {
	t.Helper()
	_, token, _, _, err := r.control.CreateInvite(72, 1, time.Now())
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	return token
}

// ── T1: дефолтная квота регистрации 0 блокирует создание VPS ──────

func TestCabinetRegisterDefaultQuotaBlocksVpsCreate(t *testing.T) {
	r, fake := newCabinetRegistry(t)
	rec := cabinetCall(t, r, http.MethodPost, "/api/auth/register", nil, "",
		fmt.Sprintf(`{"username":"newbie","password":"password-123","invite":%q}`, inviteToken(t, r)))
	if rec.Code != http.StatusOK {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	var reg struct {
		Role     string `json:"role"`
		MaxNodes int    `json:"max_nodes"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &reg)
	if reg.Role != "user" || reg.MaxNodes != 0 {
		t.Fatalf("register must create role=user max_nodes=0, got %+v", reg)
	}
	sess, csrf := cabinetLogin(t, r, "newbie", "password-123")
	rec = cabinetCall(t, r, http.MethodPost, "/api/me/vps", cookieMap(sess, csrf), csrf,
		`{"region":"ru1","idem_key":"k1","confirm":true}`)
	if rec.Code != http.StatusForbidden || errCode(t, rec) != "quota_exceeded" {
		t.Fatalf("want 403 quota_exceeded, got %d %s", rec.Code, rec.Body.String())
	}
	if fake.creates() != 0 {
		t.Fatalf("no provider call may happen when quota blocks, got %d", fake.creates())
	}
}

// ── T2: чужой объект недоступен (нода и операция) ─────────────────

func TestCabinetCrossUserAccessDenied(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	aliceID := cabinetUser(t, r, "alice", 5)
	cabinetUser(t, r, "bob", 5)
	sessB, csrfB := cabinetLogin(t, r, "bob", "password-123")

	// нода alice
	r.register(registerRequest{NodeID: "node-a1b2c", IP: "1.2.3.4"})
	au, err := r.control.User(aliceID)
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := r.control.AuthorizeNode("node-a1b2c", au.User); err != nil {
		t.Fatalf("authorize: %v", err)
	}
	rec := cabinetCall(t, r, http.MethodDelete, "/api/me/nodes/node-a1b2c", cookieMap(sessB, csrfB), csrfB, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bob deleting alice's node: want 404, got %d %s", rec.Code, rec.Body.String())
	}
	owned, _ := r.control.NodesByUser(aliceID)
	if len(owned) != 1 || owned[0].Deleting {
		t.Fatalf("alice's node must be untouched, got %+v", owned)
	}

	// операция alice
	op, _, err := r.control.CreateOperation(control.Operation{
		UserID: aliceID, NodeID: "node-a1b2c", Kind: control.OpKindCreate,
		State: control.OpPending, IdemKey: "alice-op-1",
	})
	if err != nil {
		t.Fatalf("create op: %v", err)
	}
	rec = cabinetCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/operations/%d/cancel", op.ID), cookieMap(sessB, csrfB), csrfB, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bob cancelling alice's op: want 404, got %d %s", rec.Code, rec.Body.String())
	}
	// bob видит пустой список операций
	rec = cabinetCall(t, r, http.MethodGet, "/api/me/operations", cookieMap(sessB, csrfB), "", "")
	var ops struct {
		Operations []operationJSON `json:"operations"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &ops)
	if len(ops.Operations) != 0 {
		t.Fatalf("bob must not see alice's operations: %+v", ops.Operations)
	}
}

// ── T3: CSRF обязателен на мутирующих запросах ────────────────────

func TestCabinetCSRFRequiredOnPost(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")

	// без заголовка → 403
	rec := cabinetCall(t, r, http.MethodPost, "/api/me/vps", cookieMap(sess, csrf), "",
		`{"region":"ru1","idem_key":"k1","confirm":true}`)
	if rec.Code != http.StatusForbidden || errCode(t, rec) != "csrf_invalid" {
		t.Fatalf("missing csrf: want 403 csrf_invalid, got %d %s", rec.Code, rec.Body.String())
	}
	// неверный заголовок → 403
	rec = cabinetCall(t, r, http.MethodPost, "/api/me/vps", cookieMap(sess, csrf), "wrong-token",
		`{"region":"ru1","idem_key":"k1","confirm":true}`)
	if rec.Code != http.StatusForbidden || errCode(t, rec) != "csrf_invalid" {
		t.Fatalf("bad csrf: want 403 csrf_invalid, got %d %s", rec.Code, rec.Body.String())
	}
}

// ── T4: истёкшая сессия ───────────────────────────────────────────

func TestCabinetSessionExpiry(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	id := cabinetUser(t, r, "alice", 5)

	// прямой контрольный путь: сессия с отрицательным TTL уже истекла
	token, err := r.control.CreateSession(id, -time.Second)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	rec := cabinetCall(t, r, http.MethodGet, "/api/me", map[string]string{sessionCookieName: token}, "", "")
	if rec.Code != http.StatusUnauthorized || errCode(t, rec) != "unauthorized" {
		t.Fatalf("expired session: want 401, got %d %s", rec.Code, rec.Body.String())
	}
	// неизвестный токен и отсутствие cookie — тоже 401
	if rec := cabinetCall(t, r, http.MethodGet, "/api/me", map[string]string{sessionCookieName: "bogus"}, "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown session: want 401, got %d", rec.Code)
	}
	if rec := cabinetCall(t, r, http.MethodGet, "/api/me", nil, "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no session: want 401, got %d", rec.Code)
	}
}

// ── T5: idem_key replay → та же операция, один CreateVps ──────────

func TestCabinetVpsIdempotentReplay(t *testing.T) {
	r, fake := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	// связка Beget (мок)
	if rec := cabinetCall(t, r, http.MethodPut, "/api/me/beget/link", cookieMap(sess, csrf), csrf,
		`{"login":"alice@beget","password":"s3cret"}`); rec.Code != http.StatusOK {
		t.Fatalf("link: %d %s", rec.Code, rec.Body.String())
	}
	const body = `{"region":"ru1","idem_key":"idem-1","confirm":true}`
	rec1 := cabinetCall(t, r, http.MethodPost, "/api/me/vps", cookieMap(sess, csrf), csrf, body)
	rec2 := cabinetCall(t, r, http.MethodPost, "/api/me/vps", cookieMap(sess, csrf), csrf, body)
	if rec1.Code != http.StatusOK || rec2.Code != http.StatusOK {
		t.Fatalf("vps create: %d %s / %d %s", rec1.Code, rec1.Body.String(), rec2.Code, rec2.Body.String())
	}
	var op1, op2 operationJSON
	_ = json.Unmarshal(rec1.Body.Bytes(), &op1)
	_ = json.Unmarshal(rec2.Body.Bytes(), &op2)
	if op1.ID == 0 || op1.ID != op2.ID {
		t.Fatalf("replay must return the same operation: %+v vs %+v", op1, op2)
	}
	if final := waitOpTerminal(t, r, op1.ID); final.State != control.OpDone {
		t.Fatalf("want done, got %s", final.State)
	}
	if fake.creates() != 1 {
		t.Fatalf("exactly one CreateVps expected, got %d", fake.creates())
	}
}

// T5b: параллельный replay того же idem_key — ровно один платный вызов.
func TestCabinetVpsConcurrentSameIdem(t *testing.T) {
	r, fake := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	if rec := cabinetCall(t, r, http.MethodPut, "/api/me/beget/link", cookieMap(sess, csrf), csrf,
		`{"login":"alice@beget","password":"s3cret"}`); rec.Code != http.StatusOK {
		t.Fatalf("link: %d %s", rec.Code, rec.Body.String())
	}
	srv := httptest.NewServer(r.buildMux())
	defer srv.Close()
	const n = 8
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/me/vps", strings.NewReader(`{"region":"ru1","idem_key":"idem-conc","confirm":true}`))
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sess})
			req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: csrf})
			req.Header.Set(csrfHeaderName, csrf)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("post: %v", err)
				return
			}
			defer func() { _ = resp.Body.Close() }()
			codes[i] = resp.StatusCode
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("goroutine %d: want 200, got %d", i, c)
		}
	}
	if fake.creates() != 1 {
		t.Fatalf("concurrent same idem_key must issue exactly one CreateVps, got %d", fake.creates())
	}
}

// T5c (C1): параллельные РАЗНЫЕ idem_key при max_nodes=1 → ровно один
// платный CreateVps и ровно один занятый слот (подтверждённая операция).
func TestCabinetVpsConcurrentDistinctIdemOnePaidCall(t *testing.T) {
	r, fake := newCabinetRegistry(t)
	id := cabinetUser(t, r, "alice", 1)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	if rec := cabinetCall(t, r, http.MethodPut, "/api/me/beget/link", cookieMap(sess, csrf), csrf,
		`{"login":"a@beget","password":"s3cret"}`); rec.Code != http.StatusOK {
		t.Fatalf("link: %d %s", rec.Code, rec.Body.String())
	}
	srv := httptest.NewServer(r.buildMux())
	defer srv.Close()

	const n = 8
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"region":"ru1","idem_key":"distinct-%d","confirm":true}`, i)
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/me/vps", strings.NewReader(body))
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sess})
			req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: csrf})
			req.Header.Set(csrfHeaderName, csrf)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("post: %v", err)
				return
			}
			defer func() { _ = resp.Body.Close() }()
			codes[i] = resp.StatusCode
		}(i)
	}
	wg.Wait()
	if got := fake.creates(); got != 1 {
		t.Fatalf("concurrent distinct idem_keys at quota=1 must issue exactly one CreateVps, got %d", got)
	}
	ok := 0
	for _, c := range codes {
		if c == http.StatusOK {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("exactly one request may succeed, got %d (codes=%v)", ok, codes)
	}
	// ровно один занятый слот: одна завершённая операция пользователя
	ops, _ := r.control.OperationsByUser(id)
	done := 0
	for _, o := range ops {
		final := waitOpTerminal(t, r, o.ID)
		if final.State == control.OpDone {
			done++
		}
	}
	if done != 1 {
		t.Fatalf("exactly one slot consumed (done op), got %d (ops=%d)", done, len(ops))
	}
}

// T5d (H1): подтверждённый VPS расходует место — серийный второй заказ при
// max_nodes=1 отклонён по квоте (раньше место не расходовалось вовсе).
func TestCabinetVpsQuotaCountsProvisionedVps(t *testing.T) {
	r, fake := newCabinetRegistry(t)
	id := cabinetUser(t, r, "alice", 1)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	if rec := cabinetCall(t, r, http.MethodPut, "/api/me/beget/link", cookieMap(sess, csrf), csrf,
		`{"login":"a@beget","password":"s3cret"}`); rec.Code != http.StatusOK {
		t.Fatalf("link: %d %s", rec.Code, rec.Body.String())
	}
	rec := cabinetCall(t, r, http.MethodPost, "/api/me/vps", cookieMap(sess, csrf), csrf,
		`{"region":"ru1","idem_key":"first","confirm":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("first vps: %d %s", rec.Code, rec.Body.String())
	}
	rec = cabinetCall(t, r, http.MethodPost, "/api/me/vps", cookieMap(sess, csrf), csrf,
		`{"region":"ru1","idem_key":"second","confirm":true}`)
	if rec.Code != http.StatusForbidden || errCode(t, rec) != "quota_exceeded" {
		t.Fatalf("second vps: want 403 quota_exceeded, got %d %s", rec.Code, rec.Body.String())
	}
	if fake.creates() != 1 {
		t.Fatalf("only one paid call allowed, got %d", fake.creates())
	}
	ops, _ := r.control.OperationsByUser(id)
	if len(ops) != 1 {
		t.Fatalf("second order must not persist an operation, got %d", len(ops))
	}
}

// T5e (H3): пока заказ пользователя не завершён (pending/unknown), новый
// заказ с другим idem_key отклоняется 409 create_in_progress.
func TestCabinetVpsBlockedWhileOperationPending(t *testing.T) {
	r, fake := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	if rec := cabinetCall(t, r, http.MethodPut, "/api/me/beget/link", cookieMap(sess, csrf), csrf,
		`{"login":"a@beget","password":"s3cret"}`); rec.Code != http.StatusOK {
		t.Fatalf("link: %d %s", rec.Code, rec.Body.String())
	}
	fake.block = make(chan struct{})
	srv := httptest.NewServer(r.buildMux())
	defer srv.Close()

	post := func(key string) int {
		body := fmt.Sprintf(`{"region":"ru1","idem_key":%q,"confirm":true}`, key)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/me/vps", strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sess})
		req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: csrf})
		req.Header.Set(csrfHeaderName, csrf)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Errorf("post: %v", err)
			return 0
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}

	done := make(chan int, 1)
	go func() { done <- post("winner") }()
	// ждём, пока победитель войдёт в платный вызов (pending-строка уже есть)
	deadline := time.Now().Add(3 * time.Second)
	for fake.creates() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if fake.creates() != 1 {
		t.Fatalf("winner never entered CreateVps")
	}
	rec := cabinetCall(t, r, http.MethodPost, "/api/me/vps", cookieMap(sess, csrf), csrf,
		`{"region":"ru1","idem_key":"loser","confirm":true}`)
	if rec.Code != http.StatusConflict || errCode(t, rec) != "create_in_progress" {
		t.Fatalf("second order while pending: want 409 create_in_progress, got %d %s", rec.Code, rec.Body.String())
	}
	close(fake.block)
	if code := <-done; code != http.StatusOK {
		t.Fatalf("winner status = %d, want 200", code)
	}
	if fake.creates() != 1 {
		t.Fatalf("exactly one paid call, got %d", fake.creates())
	}
}

// T5f (H4): чужой idem_key не отдаётся — cross-user replay → 404.
func TestCabinetVpsCrossUserIdemNotFound(t *testing.T) {
	r, fake := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	cabinetUser(t, r, "bob", 5)
	sessA, csrfA := cabinetLogin(t, r, "alice", "password-123")
	if rec := cabinetCall(t, r, http.MethodPut, "/api/me/beget/link", cookieMap(sessA, csrfA), csrfA,
		`{"login":"a@beget","password":"s3cret"}`); rec.Code != http.StatusOK {
		t.Fatalf("link: %d %s", rec.Code, rec.Body.String())
	}
	if rec := cabinetCall(t, r, http.MethodPost, "/api/me/vps", cookieMap(sessA, csrfA), csrfA,
		`{"region":"ru1","idem_key":"alice-secret-key","confirm":true}`); rec.Code != http.StatusOK {
		t.Fatalf("alice vps: %d %s", rec.Code, rec.Body.String())
	}
	sessB, csrfB := cabinetLogin(t, r, "bob", "password-123")
	rec := cabinetCall(t, r, http.MethodPost, "/api/me/vps", cookieMap(sessB, csrfB), csrfB,
		`{"region":"ru1","idem_key":"alice-secret-key","confirm":true}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bob replaying alice's idem_key: want 404, got %d %s", rec.Code, rec.Body.String())
	}
	if fake.creates() != 1 {
		t.Fatalf("cross-user replay must not trigger a paid call, got %d", fake.creates())
	}
}

// ── T6: секреты отсутствуют во всех ответах ───────────────────────

func TestCabinetSecretsNeverReturned(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	ck := cookieMap(sess, csrf)

	const login, password = "beget-login-secret", "beget-password-secret"
	if rec := cabinetCall(t, r, http.MethodPut, "/api/me/beget/link", ck, csrf,
		fmt.Sprintf(`{"login":%q,"password":%q}`, login, password)); rec.Code != http.StatusOK {
		t.Fatalf("link: %d %s", rec.Code, rec.Body.String())
	}
	if rec := cabinetCall(t, r, http.MethodPost, "/api/me/vps", ck, csrf,
		`{"region":"ru1","idem_key":"k1","confirm":true}`); rec.Code != http.StatusOK {
		t.Fatalf("vps: %d %s", rec.Code, rec.Body.String())
	}

	endpoints := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/me"},
		{http.MethodGet, "/api/me/beget/link"},
		{http.MethodGet, "/api/me/nodes"},
		{http.MethodGet, "/api/me/operations"},
		{http.MethodGet, "/api/me/authkey"},
	}
	for _, e := range endpoints {
		rec := cabinetCall(t, r, e.method, e.path, ck, "", "")
		body := rec.Body.String()
		for _, secret := range []string{login, password, "beget-vps-123"} {
			if strings.Contains(body, secret) {
				t.Fatalf("%s %s leaked secret %q: %s", e.method, e.path, secret, body)
			}
		}
	}
}

// ── authkey prefix only ───────────────────────────────────────────

func TestCabinetAuthkeyPrefixOnly(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	id := cabinetUser(t, r, "alice", 5)
	full, err := r.control.RotateAuthkey(id)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	rec := cabinetCall(t, r, http.MethodGet, "/api/me/authkey", cookieMap(sess, csrf), "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("authkey: %d %s", rec.Code, rec.Body.String())
	}
	var ak authkeyJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &ak)
	if !ak.Active || !strings.HasPrefix(ak.Prefix, "sda_") {
		t.Fatalf("active prefix expected, got %+v", ak)
	}
	if strings.Contains(rec.Body.String(), full) {
		t.Fatal("full authkey must never be returned by /api/me/authkey")
	}
}

// ── logout ────────────────────────────────────────────────────────

func TestCabinetLogout(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	rec := cabinetCall(t, r, http.MethodPost, "/api/auth/logout", cookieMap(sess, csrf), csrf, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout: want 204, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := cabinetCall(t, r, http.MethodGet, "/api/me", map[string]string{sessionCookieName: sess}, "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("session must be dead after logout, got %d", rec.Code)
	}
}

// ── rate limit регистрации по IP ──────────────────────────────────
func TestCabinetRegisterRateLimited(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	for i := 0; i < 5; i++ {
		rec := cabinetCall(t, r, http.MethodPost, "/api/auth/register", nil, "",
			fmt.Sprintf(`{"username":"u%dser","password":"password-123","invite":%q}`, i, inviteToken(t, r)))
		if rec.Code != http.StatusOK {
			t.Fatalf("register %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	rec := cabinetCall(t, r, http.MethodPost, "/api/auth/register", nil, "",
		`{"username":"sixther","password":"password-123"}`)
	if rec.Code != http.StatusTooManyRequests || errCode(t, rec) != "rate_limited" {
		t.Fatalf("6th register: want 429 rate_limited, got %d %s", rec.Code, rec.Body.String())
	}
}

// ── cancel: отказ после платного вызова ───────────────────────────

func TestCabinetCancelRefusedAfterSent(t *testing.T) {
	r, fake := newCabinetRegistry(t)
	id := cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	if rec := cabinetCall(t, r, http.MethodPut, "/api/me/beget/link", cookieMap(sess, csrf), csrf,
		`{"login":"a@b","password":"p"}`); rec.Code != http.StatusOK {
		t.Fatalf("link: %d", rec.Code)
	}
	if rec := cabinetCall(t, r, http.MethodPost, "/api/me/vps", cookieMap(sess, csrf), csrf,
		`{"region":"ru1","idem_key":"k1","confirm":true}`); rec.Code != http.StatusOK {
		t.Fatalf("vps: %d %s", rec.Code, rec.Body.String())
	}
	ops, _ := r.control.OperationsByUser(id)
	if len(ops) != 1 {
		t.Fatalf("want 1 op, got %d", len(ops))
	}
	rec := cabinetCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/operations/%d/cancel", ops[0].ID), cookieMap(sess, csrf), csrf, "")
	if rec.Code != http.StatusConflict || errCode(t, rec) != "already_sent" {
		t.Fatalf("cancel after sent: want 409 already_sent, got %d %s", rec.Code, rec.Body.String())
	}
	if fake.creates() != 1 {
		t.Fatalf("cancel must not trigger provider calls, got %d", fake.creates())
	}
}

// R1: отмена во время платного вызова не освобождает слот. max_nodes=1 +
// create + cancel-in-flight НЕ даёт второго CreateVps.
func TestCabinetCancelDuringInFlightCreateDoesNotFreeSlot(t *testing.T) {
	r, fake := newCabinetRegistry(t)
	id := cabinetUser(t, r, "alice", 1)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	if rec := cabinetCall(t, r, http.MethodPut, "/api/me/beget/link", cookieMap(sess, csrf), csrf,
		`{"login":"a@b","password":"p"}`); rec.Code != http.StatusOK {
		t.Fatalf("link: %d", rec.Code)
	}
	fake.block = make(chan struct{})
	fake.createStarted = make(chan struct{})

	first := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		first <- cabinetCall(t, r, http.MethodPost, "/api/me/vps", cookieMap(sess, csrf), csrf,
			`{"region":"ru1","idem_key":"k1","confirm":true}`)
	}()
	<-fake.createStarted // платный вызов в полёте (state=sent)
	ops, _ := r.control.OperationsByUser(id)
	if len(ops) != 1 || ops[0].State != control.OpSent {
		t.Fatalf("want 1 sent op in flight, got %+v", ops)
	}
	// отмена в полёте запрещена
	rec := cabinetCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/operations/%d/cancel", ops[0].ID), cookieMap(sess, csrf), csrf, "")
	if rec.Code != http.StatusConflict || errCode(t, rec) != "already_sent" {
		t.Fatalf("cancel in flight: want 409 already_sent, got %d %s", rec.Code, rec.Body.String())
	}
	// второй заказ другим ключом не проходит — слот занят живым заказом
	rec = cabinetCall(t, r, http.MethodPost, "/api/me/vps", cookieMap(sess, csrf), csrf,
		`{"region":"ru1","idem_key":"k2","confirm":true}`)
	if rec.Code != http.StatusForbidden && rec.Code != http.StatusConflict {
		t.Fatalf("second create: want refusal (403/409), got %d %s", rec.Code, rec.Body.String())
	}
	close(fake.block)
	if rec := <-first; rec.Code != http.StatusOK {
		t.Fatalf("first create: %d %s", rec.Code, rec.Body.String())
	}
	if fake.creates() != 1 {
		t.Fatalf("CreateVps calls = %d, want exactly 1", fake.creates())
	}
}

// ── H2: лимит логина и униформа ошибки ────────────────────────────

// 10 попыток логина с одного IP → 11-я 429 (per-IP).
func TestCabinetLoginRateLimitedPerIP(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	for i := 0; i < 10; i++ {
		rec := cabinetCall(t, r, http.MethodPost, "/api/auth/login", nil, "",
			fmt.Sprintf(`{"username":"ghost%duser","password":"password-123"}`, i))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: want 401, got %d %s", i, rec.Code, rec.Body.String())
		}
	}
	rec := cabinetCall(t, r, http.MethodPost, "/api/auth/login", nil, "",
		`{"username":"eleventh","password":"password-123"}`)
	if rec.Code != http.StatusTooManyRequests || errCode(t, rec) != "rate_limited" {
		t.Fatalf("11th login: want 429 rate_limited, got %d %s", rec.Code, rec.Body.String())
	}
}

// 5 неудач для одной учётки → лок-аут даже с верным паролем (per-account).
func TestCabinetLoginLockoutPerAccount(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	for i := 0; i < 5; i++ {
		rec := cabinetCall(t, r, http.MethodPost, "/api/auth/login", nil, "",
			`{"username":"alice","password":"wrong-password"}`)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong %d: want 401, got %d", i, rec.Code)
		}
	}
	// верный пароль тоже отклонён — учётка под лок-аутом
	rec := cabinetCall(t, r, http.MethodPost, "/api/auth/login", nil, "",
		`{"username":"alice","password":"password-123"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("locked account with correct password: want 429, got %d %s", rec.Code, rec.Body.String())
	}
}

// Заблокированный пользователь на логине даёт униформенный 401 (M2), не 403.
func TestCabinetLoginUniformErrorForDisabled(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	id := cabinetUser(t, r, "alice", 5)
	if _, err := r.control.UpdateUser(id, control.UserPatch{Disabled: boolPtr(true)}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	rec := cabinetCall(t, r, http.MethodPost, "/api/auth/login", nil, "",
		`{"username":"alice","password":"password-123"}`)
	if rec.Code != http.StatusUnauthorized || errCode(t, rec) != "bad_credentials" {
		t.Fatalf("disabled login: want 401 bad_credentials, got %d %s", rec.Code, rec.Body.String())
	}
}

// ── CSRF привязан к сессии (cookie-tossing не проходит) ───────────

func TestCabinetCSRFIgnoresTossedCookie(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	// атакующий сиблинг-домен подбросил свою csrf-cookie; заголовок — верный
	rec := cabinetCall(t, r, http.MethodPost, "/api/auth/logout",
		map[string]string{sessionCookieName: sess, csrfCookieName: "attacker-value"}, csrf, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("session-bound csrf must ignore tossed cookie, got %d %s", rec.Code, rec.Body.String())
	}
	// неверный заголовок — отказ
	r2, _ := newCabinetRegistry(t)
	cabinetUser(t, r2, "bob", 5)
	sess2, csrf2 := cabinetLogin(t, r2, "bob", "password-123")
	rec = cabinetCall(t, r2, http.MethodPost, "/api/auth/logout",
		map[string]string{sessionCookieName: sess2, csrfCookieName: csrf2}, "wrong", "")
	if rec.Code != http.StatusForbidden || errCode(t, rec) != "csrf_invalid" {
		t.Fatalf("wrong header: want 403 csrf_invalid, got %d %s", rec.Code, rec.Body.String())
	}
}

// ── M3: node_id из idem_key валиден и без коллизий ───────────────

func TestNodeIDForIdemValidAndDistinct(t *testing.T) {
	// Старый install_node_web.sh валидирует --name как 1..10 символов, поэтому
	// сгенерированный id обязан уложиться в 10 и при этом совпасть с NAME-HASH.
	oldInstallerName := regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,8}[A-Za-z0-9])?$`)
	seen := map[string]bool{}
	for i := 0; i < 5000; i++ {
		key := fmt.Sprintf("idem-key-%d", i)
		id := nodeIDForIdem(key)
		if err := machineapi.ValidateNodeID(id); err != nil {
			t.Fatalf("nodeIDForIdem(%q)=%q invalid: %v", key, id, err)
		}
		if len(id) > 10 {
			t.Fatalf("nodeIDForIdem(%q)=%q length %d > 10 (old installer rejects)", key, id, len(id))
		}
		if !oldInstallerName.MatchString(id) {
			t.Fatalf("nodeIDForIdem(%q)=%q rejected by old install_node_web.sh --name regex", key, id)
		}
		hash := id[strings.LastIndexByte(id, '-')+1:]
		if hash != strings.ToLower(hash) || !regexp.MustCompile(`^[a-z0-9]{5}$`).MatchString(hash) {
			t.Fatalf("nodeIDForIdem(%q)=%q hash part not 5 lowercase alnum: %q", key, id, hash)
		}
		if seen[id] {
			t.Fatalf("collision at key %q: %q", key, id)
		}
		seen[id] = true
	}
	// детерминизм: повтор ключа даёт ту же ноду.
	a, b := nodeIDForIdem("idem-stable"), nodeIDForIdem("idem-stable")
	if a != b {
		t.Fatalf("nodeIDForIdem not deterministic: %q != %q", a, b)
	}
	// регрессия: старый 20-битный вывод отличался (5 hex от хэша ключа)
	sum := sha256.Sum256([]byte("idem-key-0"))
	old := "vps-" + hex.EncodeToString(sum[:])[:5]
	if nodeIDForIdem("idem-key-0") == old {
		t.Fatalf("nodeIDForIdem still uses the 20-bit truncation: %q", old)
	}
}

// ── M6: карта rate-limiter ограничена ────────────────────────────

func TestIPRateLimiterMapBounded(t *testing.T) {
	l := newIPRateLimiter(3, time.Minute)
	for i := 0; i < rateLimiterMaxKeys+2000; i++ {
		l.record(fmt.Sprintf("ip-%d", i))
	}
	l.mu <- struct{}{}
	n := len(l.hits)
	<-l.mu
	if n > rateLimiterMaxKeys {
		t.Fatalf("rate limiter map unbounded: %d > %d", n, rateLimiterMaxKeys)
	}
}
