package server

// Регресс-тесты правок review-ssh-steps.md: H4 (утечка слота), M5 (node_token
// не инъектится), M6 (retry не перевыдаёт секрет), M8 (default-профиль --name),
// H3 (default-профиль проверяет реальный старт агента). Сеть/платные вызовы —
// только моки.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"sharedd/registry/internal/control"
)

// H4: ошибки валидации переменных НЕ должны утекать местом в квоте (slot
// резервируется только после всей валидации).
func TestMeVpsLateValidationNoSlotLeak(t *testing.T) {
	cases := map[string]string{
		"bad-name":       `{"region":"ru1","idem_key":"h4-bad","vars":{"bad-name":"x"},"confirm":true}`,
		"value-too-long": fmt.Sprintf(`{"region":"ru1","idem_key":"h4-long","vars":{"v":%q},"confirm":true}`, strings.Repeat("a", 5000)),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			r, fake := newCabinetRegistry(t)
			uid := cabinetUser(t, r, "alice", 1) // max_nodes=1: любой лик сразу блокирует
			sess, csrf := cabinetLogin(t, r, "alice", "password-123")
			linkBeget(t, r, sess, csrf)

			before, err := r.control.UsedSlots(uid)
			if err != nil || before != 0 {
				t.Fatalf("used slots before = %d (err=%v), want 0", before, err)
			}
			rec := cabinetCall(t, r, http.MethodPost, "/api/me/vps", cookieMap(sess, csrf), csrf, body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d %s", rec.Code, rec.Body.String())
			}
			after, err := r.control.UsedSlots(uid)
			if err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Fatalf("slot leaked on %s: UsedSlots %d → %d", name, before, after)
			}
			// и место всё ещё доступно: корректный заказ проходит.
			rec = cabinetCall(t, r, http.MethodPost, "/api/me/vps", cookieMap(sess, csrf), csrf,
				`{"region":"ru1","idem_key":"h4-ok","confirm":true}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("valid create after failed one: %d %s", rec.Code, rec.Body.String())
			}
			if fake.creates() != 1 {
				t.Fatalf("paid calls = %d, want 1", fake.creates())
			}
		})
	}
}

// M5: реестровый node_token НЕ попадает в переменные/шаги установки.
func TestProvisionVarsExcludeSharedNodeToken(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	const sharedToken = "SHARED-NODE-TOKEN-SECRET"
	r.cfg.Security.NodeToken = sharedToken
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	linkBeget(t, r, sess, csrf)

	rec := cabinetCall(t, r, http.MethodPost, "/api/me/vps", cookieMap(sess, csrf), csrf,
		`{"region":"ru1","idem_key":"m5-token","confirm":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("vps: %d %s", rec.Code, rec.Body.String())
	}
	var op operationJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &op)
	waitOpTerminal(t, r, op.ID)

	exec := r.sshExec.(*fakeExec)
	if strings.Contains(exec.allCommands(), sharedToken) {
		t.Fatalf("shared node_token leaked into install steps: %q", exec.allCommands())
	}
}

// M6: retry-install НЕ перевыдаёт node_secret при наличии — хеш живого агента
// сохраняется, старый секрет продолжает приниматься.
func TestRetryInstallKeepsExistingSecret(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	linkBeget(t, r, sess, csrf)

	rec := cabinetCall(t, r, http.MethodPost, "/api/me/vps", cookieMap(sess, csrf), csrf,
		`{"region":"ru1","idem_key":"m6-secret","confirm":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("vps: %d %s", rec.Code, rec.Body.String())
	}
	var op operationJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &op)
	waitOpTerminal(t, r, op.ID)

	exec := r.sshExec.(*fakeExec)
	first := exec.nodeSecretFromCommands()
	if first == "" {
		t.Fatal("first install must carry the freshly issued node_secret")
	}
	nodeID := nodeIDForIdem("m6-secret")
	if owner, ok, err := r.control.NodeSecretOwner(first); err != nil || !ok || owner != nodeID {
		t.Fatalf("secret must be bound to node after create: owner=%q ok=%t err=%v", owner, ok, err)
	}

	// имитируем провал установки и повторяем.
	if err := r.control.UpdateOperationState(op.ID, control.OpFailed, "", "install boom"); err != nil {
		t.Fatal(err)
	}
	rec = cabinetCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/operations/%d/retry-install", op.ID),
		cookieMap(sess, csrf), csrf, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry-install: %d %s", rec.Code, rec.Body.String())
	}
	waitOpTerminal(t, r, op.ID)

	// старый секрет по-прежнему валиден (хеш не затёрт).
	if owner, ok, err := r.control.NodeSecretOwner(first); err != nil || !ok || owner != nodeID {
		t.Fatalf("retry rotated the secret and bricked the agent: owner=%q ok=%t err=%v", owner, ok, err)
	}
}

// M8: штатный профиль явно передаёт --name с предсозданным node_id.
func TestDefaultProfilePassesExplicitName(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	linkBeget(t, r, sess, csrf)

	rec := cabinetCall(t, r, http.MethodPost, "/api/me/vps", cookieMap(sess, csrf), csrf,
		`{"region":"ru1","idem_key":"m8-name","confirm":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("vps: %d %s", rec.Code, rec.Body.String())
	}
	var op operationJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &op)
	waitOpTerminal(t, r, op.ID)

	nodeID := nodeIDForIdem("m8-name")
	if !strings.Contains(r.sshExec.(*fakeExec).allCommands(), "--name='"+nodeID+"'") {
		t.Fatalf("default profile must pass --name=<node_id>: %q", r.sshExec.(*fakeExec).allCommands())
	}
}
