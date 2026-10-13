package server

// Регресс-тесты DEF-A..DEF-D из docs/specs/review-ssh-steps.md «Re-check round 2»:
//   DEF-A — retry-install рендерит РЕАЛЬНЫЙ сохранённый node_secret (не '').
//   DEF-B — /api/node/status подтверждает факт регистрации; verify-шаг падает,
//           если нода не зарегистрировалась.
//   DEF-C — orphaned installing до-подхватывается при старте; retry из
//           installing разрешён, пока воркер не держит операцию.
//   DEF-D — provisionConfig читает конфиг под cfgMu (race-free).
// Платные вызовы — только моки; ноль новых CreateVps в recovery-путях.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"sharedd/registry/internal/control"
	"sharedd/registry/internal/sshx"
)

func unmarshalOp(t *testing.T, b []byte, op *operationJSON) operationJSON {
	t.Helper()
	if err := json.Unmarshal(b, op); err != nil {
		t.Fatalf("decode operation: %v", err)
	}
	return *op
}

// ── DEF-A: retry рендерит реальный секрет ─────────────────────────

func TestRetryInstallRendersRealSecret(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	linkBeget(t, r, sess, csrf)

	rec := cabinetCall(t, r, http.MethodPost, "/api/me/vps", cookieMap(sess, csrf), csrf,
		`{"region":"ru1","idem_key":"defa-retry","confirm":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("vps: %d %s", rec.Code, rec.Body.String())
	}
	var op operationJSON
	_ = unmarshalOp(t, rec.Body.Bytes(), &op)
	waitOpTerminal(t, r, op.ID)

	exec := r.sshExec.(*fakeExec)
	first := exec.nodeSecretFromCommands()
	if first == "" {
		t.Fatal("first install must carry the issued node_secret")
	}
	if owner, ok, err := r.control.NodeSecretOwner(first); err != nil || !ok || owner != op.NodeID {
		t.Fatalf("secret not bound to node: owner=%q ok=%t err=%v", owner, ok, err)
	}

	// Провалившаяся установка → retry. Секрет уже выдан (hash есть) — старый
	// код рендерил `--registry-token=''` и установщик падал.
	if err := r.control.UpdateOperationState(op.ID, control.OpFailed, "", "install boom"); err != nil {
		t.Fatal(err)
	}
	exec.reset()
	rec = cabinetCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/operations/%d/retry-install", op.ID),
		cookieMap(sess, csrf), csrf, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry-install: %d %s", rec.Code, rec.Body.String())
	}
	waitOpTerminal(t, r, op.ID)

	cmds := exec.allCommands()
	if !strings.Contains(cmds, "--registry-token='"+first+"'") {
		t.Fatalf("retry must render the real token; commands:\n%s", cmds)
	}
	if !strings.Contains(cmds, "--node-secret='"+first+"'") {
		t.Fatalf("retry must render the real node_secret; commands:\n%s", cmds)
	}
	if strings.Contains(cmds, "--registry-token=''") || strings.Contains(cmds, "--node-secret=''") {
		t.Fatalf("retry rendered an empty token (install_node.sh would die):\n%s", cmds)
	}
}

// ── DEF-A: fallback для legacy-строки без восстановимой копии ─────

func TestProvisionVarsLegacyFallback(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	u := testUser(t, r, "alice", 5)
	const nodeID = "nodea-abcde"
	if err := r.control.AuthorizeNode(nodeID, u); err != nil {
		t.Fatal(err)
	}
	// legacy: хеш есть, восстановимой копии нет.
	if _, _, err := r.control.SetNodeSecretWithEnc(nodeID, "old-secret", ""); err != nil {
		t.Fatal(err)
	}
	// нода ещё НЕ регистрировалась → выдаётся свежий непустой секрет.
	vars, err := r.provisionVars(u, nodeID, map[string]any{}, false)
	if err != nil {
		t.Fatalf("fallback reissue: %v", err)
	}
	if sec, _ := vars["node_secret"].(string); sec == "" || sec == "old-secret" {
		t.Fatalf("expected fresh non-empty secret, got %q", sec)
	}

	// нода УЖЕ регистрировалась → не перевыдаём (fail-loud), живой агент цел.
	if _, _, err := r.control.SetNodeSecretWithEnc(nodeID, "old-secret-2", ""); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.state.Candidates[nodeID] = &Candidate{NodeID: nodeID}
	r.mu.Unlock()
	if _, err := r.provisionVars(u, nodeID, map[string]any{}, false); err == nil {
		t.Fatal("must refuse to reissue for a registered node without a recoverable copy")
	}
}

// ── DEF-B: /api/node/status ───────────────────────────────────────

func testUser(t *testing.T, r *Registry, name string, maxNodes int) control.User {
	t.Helper()
	uid := cabinetUser(t, r, name, maxNodes)
	u, err := r.control.User(uid)
	if err != nil {
		t.Fatal(err)
	}
	return u.User
}

func nodeStatusCode(r *Registry, nodeID, token string) int {
	req := httptest.NewRequest(http.MethodGet, "/api/node/status?node_id="+nodeID, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	r.buildMux().ServeHTTP(rec, req)
	return rec.Code
}

func registerNodeCode(r *Registry, nodeID, secret string) int {
	body := fmt.Sprintf(`{"node_id":%q,"ip":"8.8.8.8","node_type":"classic"}`, nodeID)
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	r.buildMux().ServeHTTP(rec, req)
	return rec.Code
}

func TestNodeStatusOwnershipAndRegistration(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	r.cfg.Security.NodeToken = testNodeToken
	u := testUser(t, r, "alice", 5)
	const (
		nodeA = "nodea-abcde"
		nodeB = "nodeb-abcde"
		secA  = "secret-A-000000000000"
		secB  = "secret-B-111111111111"
	)
	if err := r.control.AuthorizeNode(nodeA, u); err != nil {
		t.Fatal(err)
	}
	if err := r.control.AuthorizeNode(nodeB, u); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.control.SetNodeSecretWithEnc(nodeA, secA, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.control.SetNodeSecretWithEnc(nodeB, secB, ""); err != nil {
		t.Fatal(err)
	}

	// секрет свой, но нода ещё не регистрировалась → 404 (нельзя выдать done).
	if code := nodeStatusCode(r, nodeA, secA); code != http.StatusNotFound {
		t.Fatalf("before register: got %d, want 404", code)
	}
	if code := registerNodeCode(r, nodeA, secA); code != http.StatusOK {
		t.Fatalf("register nodeA: %d", code)
	}
	// свой секрет + своя нода → 200.
	if code := nodeStatusCode(r, nodeA, secA); code != http.StatusOK {
		t.Fatalf("own secret: got %d, want 200", code)
	}
	// чужой секрет → 403.
	if code := nodeStatusCode(r, nodeA, secB); code != http.StatusForbidden {
		t.Fatalf("foreign secret: got %d, want 403", code)
	}
	// свой секрет под чужим node_id → 403.
	if code := nodeStatusCode(r, nodeB, secA); code != http.StatusForbidden {
		t.Fatalf("own secret foreign node_id: got %d, want 403", code)
	}
	// общий токен с чужим node_id → 403 (личный секрет обязателен).
	if code := nodeStatusCode(r, nodeB, testNodeToken); code != http.StatusForbidden {
		t.Fatalf("shared token foreign node_id: got %d, want 403", code)
	}
	// без токена → 401.
	if code := nodeStatusCode(r, nodeA, ""); code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401", code)
	}
}

// ── DEF-B: verify-шаг падает, если нода не зарегистрировалась ─────

var (
	reToken        = regexp.MustCompile(`TOKEN='([^']*)'`)
	reNodeIDInURL  = regexp.MustCompile(`node_id='?([A-Za-z0-9._-]+)`)
	reSecretArg    = regexp.MustCompile(`--node-secret='([^']*)'`)
	reNameArg      = regexp.MustCompile(`--name='([^']*)'`)
	reStatusMarker = "/api/node/status"
)

// verifyExecFn — эмулирует реальный verify-шаг: HTTP-запрос к mux по URL/токену
// из отрендеренной команды. registered=true дополнительно эмулирует агент,
// который зовёт /register (шаг установки).
func verifyExecFn(r *Registry, registered bool) func(command, stdin string) (sshx.Result, error) {
	return func(_, stdin string) (sshx.Result, error) {
		cmd := stdin
		if strings.Contains(cmd, reStatusMarker) {
			tok := firstGroup(reToken, cmd)
			id := firstGroup(reNodeIDInURL, cmd)
			if nodeStatusCode(r, id, tok) == http.StatusOK {
				return sshx.Result{Stdout: "ok"}, nil
			}
			return sshx.Result{Code: 22, Stderr: "curl: (22) not registered"},
				errors.New("node did not register")
		}
		if registered && strings.Contains(cmd, "--node-secret='") {
			sec := firstGroup(reSecretArg, cmd)
			id := firstGroup(reNameArg, cmd)
			_ = registerNodeCode(r, id, sec)
		}
		return sshx.Result{Stdout: "ok"}, nil
	}
}

func firstGroup(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); len(m) > 1 {
		return m[1]
	}
	return ""
}

func TestInstallResultReflectsRegistration(t *testing.T) {
	for _, tc := range []struct {
		name       string
		registered bool
		wantState  string
	}{
		{"registered → done", true, control.OpDone},
		{"not registered → failed", false, control.OpFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, fake := newCabinetRegistry(t)
			r.cfg.Security.NodeToken = testNodeToken
			r.sshExec.(*fakeExec).runFn = verifyExecFn(r, tc.registered)
			cabinetUser(t, r, "alice", 5)
			sess, csrf := cabinetLogin(t, r, "alice", "password-123")
			linkBeget(t, r, sess, csrf)

			rec := cabinetCall(t, r, http.MethodPost, "/api/me/vps", cookieMap(sess, csrf), csrf,
				`{"region":"ru1","idem_key":"defb-verify","confirm":true}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("vps: %d %s", rec.Code, rec.Body.String())
			}
			var op operationJSON
			_ = unmarshalOp(t, rec.Body.Bytes(), &op)
			final := waitOpTerminal(t, r, op.ID)
			if final.State != tc.wantState {
				t.Fatalf("state = %s, want %s (last_error=%q)", final.State, tc.wantState, final.LastError)
			}
			if fake.creates() != 1 {
				t.Fatalf("paid calls = %d, want 1", fake.creates())
			}
		})
	}
}

// ── DEF-C: orphaned installing ────────────────────────────────────

// retryFromInstallingAllowed: операция в installing без живого воркера
// повторяется (не 409), воркер не запускается дважды, ноль новых платных вызовов.
func TestRetryInstallAllowedFromOrphanedInstalling(t *testing.T) {
	r, fake := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	linkBeget(t, r, sess, csrf)

	rec := cabinetCall(t, r, http.MethodPost, "/api/me/vps", cookieMap(sess, csrf), csrf,
		`{"region":"ru1","idem_key":"defc-retry","confirm":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("vps: %d %s", rec.Code, rec.Body.String())
	}
	var op operationJSON
	_ = unmarshalOp(t, rec.Body.Bytes(), &op)
	waitOpTerminal(t, r, op.ID)

	// «рестарт посреди установки»: состояние installing, воркера нет.
	if err := r.control.UpdateOperationState(op.ID, control.OpInstalling, "", ""); err != nil {
		t.Fatal(err)
	}
	rec = cabinetCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/operations/%d/retry-install", op.ID),
		cookieMap(sess, csrf), csrf, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry from orphaned installing: want 200, got %d %s", rec.Code, rec.Body.String())
	}
	if final := waitOpTerminal(t, r, op.ID); final.State != control.OpDone {
		t.Fatalf("final state = %s, want done", final.State)
	}
	if fake.creates() != 1 {
		t.Fatalf("retry must not issue a paid call, CreateVps=%d", fake.creates())
	}
}

// resumeInstallingOps до-подхватывает orphaned installing до done, без оплаты.
func TestResumeInstallingOperation(t *testing.T) {
	r, fake := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	linkBeget(t, r, sess, csrf)

	rec := cabinetCall(t, r, http.MethodPost, "/api/me/vps", cookieMap(sess, csrf), csrf,
		`{"region":"ru1","idem_key":"defc-resume","confirm":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("vps: %d %s", rec.Code, rec.Body.String())
	}
	var op operationJSON
	_ = unmarshalOp(t, rec.Body.Bytes(), &op)
	waitOpTerminal(t, r, op.ID)

	if err := r.control.UpdateOperationState(op.ID, control.OpInstalling, "", ""); err != nil {
		t.Fatal(err)
	}
	r.resumeInstallingOps(context.Background())

	if final := waitOpTerminal(t, r, op.ID); final.State != control.OpDone {
		t.Fatalf("resumed state = %s, want done", final.State)
	}
	if fake.creates() != 1 {
		t.Fatalf("resume must not issue a paid call, CreateVps=%d", fake.creates())
	}
}

// ── DEF-D: provisionConfig под cfgMu ──────────────────────────────

func TestProvisionConfigRaceFree(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				r.cfgMu.Lock()
				r.cfg.Provisioning.InstallTimeoutMin++
				r.cfgMu.Unlock()
			}
		}
	}()
	for i := 0; i < 500; i++ {
		if cfg := r.provisionConfig(); cfg.StepTimeout <= 0 {
			t.Fatal("step timeout must be positive")
		}
	}
	close(stop)
	wg.Wait()
}
