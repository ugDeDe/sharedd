package server

// Тесты установки на cloud-ресурс (stage-06): not-ready 409, foreign 404,
// beget 501, missing key 400, ssh_unavailable 409, happy install с инъекцией
// fake-executor + эмуляцией /register-claim → op done и resource.node_id,
// replay без второго воркера, секреты не утекают.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"sharedd/registry/internal/control"
	"sharedd/registry/internal/cryptoutil"
	"sharedd/registry/internal/provision"
	"sharedd/registry/internal/sshx"
)

// cloudReadyResource — созданный через API и доведённый до ready ресурс с
// запечатанным root-паролем (как после stage-05 create).
func cloudReadyResource(t *testing.T, r *Registry, uid, connID int64, cookies map[string]string, csrf, key string) control.CloudResourceV2 {
	t.Helper()
	rec := cloudCreateReq(t, r, cookies, csrf, connID, key)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("cloud create: %d %s", rec.Code, rec.Body)
	}
	var ack createAck
	_ = json.Unmarshal([]byte(rec.Body), &ack)
	return waitForCloudState(t, r, uid, ack.ResourceID, control.CloudResReady)
}

func cloudInstallReq(t *testing.T, r *Registry, cookies map[string]string, csrf string, id int64, key, body string) *httptestResponse {
	t.Helper()
	headers := map[string]string{}
	if key != "" {
		headers["Idempotency-Key"] = key
	}
	return cloudCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/cloud-resources/%d/install", id), cookies, csrf, body, headers)
}

func TestCloudInstallLifecycle(t *testing.T) {
	r, _ := newCloudRegistry(t)
	r.cfg.Security.NodeToken = testNodeToken
	uid, connID, cookies, csrf := cloudSession(t, r, "alice")
	res := cloudReadyResource(t, r, uid, connID, cookies, csrf, "k-create")

	// Мок-исполнитель эмулирует агент: install-шаг зовёт /register (claim
	// pending-секрета) — ровно путь create→install→pool.
	exec := r.sshExec.(*fakeExec)
	exec.runFn = verifyExecFn(r, true)

	rec := cloudInstallReq(t, r, cookies, csrf, res.ID, "inst-1", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("install: %d %s", rec.Code, rec.Body)
	}
	op := unmarshalOp(t, []byte(rec.Body), &operationJSON{})
	if op.Kind != control.OpKindCloudInstall {
		t.Fatalf("kind = %q, want cloud_install", op.Kind)
	}
	final := waitOpTerminal(t, r, op.ID)
	waitWorkerIdle(r, op.ID)
	if final.State != control.OpDone {
		t.Fatalf("install must finish done, got %s (err=%q)", final.State, final.LastError)
	}
	// node_id ресурса привязан после claim'а; label — префикс имени ноды.
	got, err := r.control.CloudResourceOwned(res.ID, uid)
	if err != nil || got.NodeID == "" || got.NodeID != final.NodeID {
		t.Fatalf("resource node binding: err=%v got=%+v final=%+v", err, got, final)
	}
	if !strings.HasPrefix(got.NodeID, "pool-node-") {
		t.Fatalf("node_id %q must start with resource label prefix", got.NodeID)
	}
	// Секрет установки инъектирован в шаги и НЕ появился в HTTP-ответе.
	secret := exec.nodeSecretFromCommands()
	if secret == "" {
		t.Fatal("install steps must carry the injected node_secret")
	}
	if strings.Contains(rec.Body, secret) {
		t.Fatalf("node_secret leaked into install response: %q", rec.Body)
	}
}

func TestCloudInstallExpectedIdentityStableUntilRealClaim(t *testing.T) {
	r, _ := newCloudRegistry(t)
	r.cfg.Security.NodeToken = testNodeToken
	uid, connID, cookies, csrf := cloudSession(t, r, "expected-owner")
	res := cloudReadyResource(t, r, uid, connID, cookies, csrf, "expected-create")
	profile, err := r.control.CreateProfile("expected-identity", "", uid)
	if err != nil {
		t.Fatal(err)
	}
	// Reproduce the supplied profile's real arguments without executing curl,
	// bash or SSH: the injected executor only inspects them and calls the mux.
	pv, err := r.control.AddProfileVersion(profile.ID, `# node installer
curl -fsSL https://raw.githubusercontent.com/ugDeDe/sharedd/main/scripts/install_node_web.sh | bash -s -- --registry={{ registry_url }} --registry-token={{ node_secret }} --node-secret={{ node_secret }} --name={{ node_id }}
`, "")
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"profile_version_id":%d,"vars":{}}`, pv.ID)
	bad := fmt.Sprintf(`{"profile_version_id":%d,"vars":{"node_id":"pool-node-zzzzz"}}`, pv.ID)
	if rec := cloudInstallReq(t, r, cookies, csrf, res.ID, "expected-forgery", bad); rec.Code != http.StatusBadRequest {
		t.Fatalf("user-controlled identity must be rejected: %d %s", rec.Code, rec.Body)
	}
	exec := r.sshExec.(*fakeExec)
	var expected, token string
	var operationID int64
	for attempt, claim := range []bool{false, true, false, true} {
		usedBefore := 0
		if attempt > 1 {
			usedBefore = 1
		}
		exec.reset()
		exec.runFn = func(_, stdin string) (sshx.Result, error) {
			id, secret := firstGroup(reNameArg, stdin), firstGroup(reSecretArg, stdin)
			if validateNodeID(id) != nil || !strings.HasPrefix(id, res.Label+"-") || secret == "" {
				return sshx.Result{}, errors.New("expected full node identity/token were not injected")
			}
			if (expected != "" && id != expected) || (token != "" && secret != token) {
				return sshx.Result{}, errors.New("retry changed expected identity or token")
			}
			op, err := r.control.OperationByIdemKey(control.CloudInstallIdem(res.ID))
			if err != nil || op.NodeID != "" {
				return sshx.Result{}, errors.New("expected ID was incorrectly treated as registered")
			}
			persisted, err := control.CloudInstallExpectedNodeID(op)
			if err != nil || persisted != id {
				return sshx.Result{}, errors.New("SSH identity is not durably bound to this operation")
			}
			if used, err := r.control.UsedNodes(uid); err != nil || used != usedBefore {
				return sshx.Result{}, errors.New("install acceptance changed pool quota")
			}
			if !claim {
				return sshx.Result{}, nil // Confirm must fail without a real registration.
			}
			// Claim accepts any NAME-HASH with the pending label prefix: agents
			// (and third-party installers) generate their own suffix, while the
			// secret itself remains the authenticator. The expected full id is
			// still injected for steps that reference it.
			if registerNodeCode(r, id, secret) != http.StatusOK {
				return sshx.Result{}, errors.New("actual expected-ID registration was refused")
			}
			return sshx.Result{}, nil
		}
		rec := cloudInstallReq(t, r, cookies, csrf, res.ID, fmt.Sprintf("expected-attempt-%d", attempt), body)
		if rec.Code != http.StatusOK {
			t.Fatalf("install attempt %d: %d %s", attempt, rec.Code, rec.Body)
		}
		ack := unmarshalOp(t, []byte(rec.Body), &operationJSON{})
		final := waitOpTerminal(t, r, ack.ID)
		waitWorkerIdle(r, ack.ID)
		if operationID != 0 && operationID != ack.ID {
			t.Fatal("retry created a new installation operation")
		}
		operationID = ack.ID
		id := firstGroup(reNameArg, exec.allCommands())
		secret := firstGroup(reSecretArg, exec.allCommands())
		if expected == "" {
			expected, token = id, secret
		}
		if id != expected || secret != token || validateNodeID(id) != nil {
			t.Fatal("expected ID/token were not stable and valid")
		}
		if claim {
			if final.State != control.OpDone || final.NodeID != expected || nodeStatusCode(r, expected, token) != http.StatusOK {
				t.Fatalf("real claim did not complete installation: %+v", final)
			}
			if owner, legacy, ok, err := r.control.NodeOwner(expected); err != nil || !ok || legacy || owner != uid {
				t.Fatal("real claim did not create the durable owner")
			}
		} else if final.State != control.OpFailed || final.NodeID != "" || !strings.Contains(final.LastError, "node did not register") {
			t.Fatalf("Confirm accepted an expected ID without registration: %+v", final)
		}
		wantUsed := usedBefore
		if claim {
			wantUsed = 1
		}
		if used, err := r.control.UsedNodes(uid); err != nil || used != wantUsed {
			t.Fatalf("quota before/after claim: used=%d want=%d err=%v", used, wantUsed, err)
		}
		if attempt == 0 {
			if _, _, ok, err := r.control.NodeOwner(expected); err != nil || ok {
				t.Fatal("expected identity created an owner before /register")
			}
			pending, err := r.control.PendingSecretByOperation(final.ID)
			if err != nil {
				t.Fatal(err)
			}
			u, err := r.control.User(uid)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.control.AuthorizeCloudInstall(res.ID, u.User, profile.ID, "replacement-token", "replacement-ciphertext"); !errors.Is(err, control.ErrOwnerMismatch) {
				t.Fatalf("acceptance rotated the pending credential: %v", err)
			}
			if after, err := r.control.PendingSecretByOperation(final.ID); err != nil || after != pending {
				t.Fatal("failed acceptance changed the pending credential")
			}
			// Reopen the same DB: expected ID and pending credential survive.
			r.control.Close()
			reopened, err := control.Open(r.cfg.Database.ControlDB, r.cfg.Users.LegacyMaxNodes)
			if err != nil {
				t.Fatal(err)
			}
			r.control = reopened
			t.Cleanup(reopened.Close)
		}
	}
}

func TestProvisionStandardAliasesRenderMaskAndSharedPort(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	r.cfg.SharedProxy.Port = 9443
	r.cfg.SharedProxy.TLSDomain = "front.example.com"
	const sharedSecret = "0123456789abcdef0123456789abcdef"
	r.cfg.SharedProxy.Users = map[string]string{"test": sharedSecret}
	const schema = `{"vars":[{"name":"ad_tag","default":"tag'$(no-command)"}]}`
	merged, err := control.MergeProfileVars(schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	vars, err := r.provisionBaseVars(merged)
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"port", "proxy_port"}, {"sni", "proxy_sni"}, {"secret", "proxy_secret"}} {
		if fmt.Sprint(vars[pair[0]]) == "" || vars[pair[0]] != vars[pair[1]] {
			t.Fatalf("alias %s missing or inconsistent", pair[0])
		}
	}
	if _, mutated := merged["proxy_port"]; mutated {
		t.Fatal("base resolver mutated its input")
	}
	profile, err := r.control.CreateProfile("alias-check", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	exec := r.sshExec.(*fakeExec)
	exec.runFn = func(_, stdin string) (sshx.Result, error) {
		return sshx.Result{Stdout: stdin + "\nsecret=" + sharedSecret}, nil
	}
	p := provision.New(r.control, nil, provision.Config{Exec: exec})
	target := sshx.Target{Host: "203.0.113.10", User: "root", Auth: sshx.Auth{Type: "password", Password: "test-only"}}
	run := func(steps string, values map[string]any) (provision.RunResult, error) {
		t.Helper()
		if _, err := r.control.AddProfileVersion(profile.ID, "# aliases\n"+steps, ""); err != nil {
			t.Fatal(err)
		}
		exec.reset()
		return p.ApplyProfile(context.Background(), profile.ID, target, values)
	}
	result, err := run("tool --port={{ port }} --canonical={{.proxy_port}} --sni={{ sni }} --secret={{ secret }} --registry={{ registry_url }} --tag={{ ad_tag }}", vars)
	if err != nil {
		t.Fatal(err)
	}
	want := "tool --port='9443' --canonical='9443' --sni='front.example.com' --secret='" + sharedSecret + "' --registry='https://registry.example.com' --tag='tag'\\''$(no-command)'"
	if !strings.Contains(exec.allCommands(), want) || strings.Contains(result.Output, sharedSecret) {
		t.Fatal("aliases were empty/double-quoted or the secret escaped masking")
	}
	// Cross-name request overrides beat declared defaults, in both directions.
	for _, pair := range [][2]string{{"secret", "proxy_secret"}, {"sni", "proxy_sni"}} {
		for _, names := range [][2]string{pair, {pair[1], pair[0]}} {
			merged, err := control.MergeProfileVars(`{"vars":[{"name":"`+names[0]+`","default":"schema.example.com"}]}`, map[string]any{names[1]: "explicit.example.com"})
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := r.provisionBaseVars(merged)
			if err != nil || resolved[pair[0]] != "explicit.example.com" || resolved[pair[1]] != "explicit.example.com" {
				t.Fatalf("alias precedence lost for %s: %v", names[0], err)
			}
		}
	}
	defaults, err := control.MergeProfileVars(`{"vars":[{"name":"sni","default":"schema.example.com"}]}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := r.provisionBaseVars(defaults)
	if err != nil || resolved["sni"] != "schema.example.com" || resolved["proxy_sni"] != "schema.example.com" {
		t.Fatalf("explicit schema default was overwritten: %v", err)
	}
	for _, input := range []map[string]any{{"port": 9444}, {"proxy_port": 9444}, {"port": 9443, "proxy_port": 9444}} {
		if _, err := r.provisionBaseVars(input); !errors.Is(err, control.ErrValidation) {
			t.Fatalf("port override bypassed registry source of truth: %v", err)
		}
	}
	// Each secret name masks by convention even without the canonical twin or
	// a schema secret flag; echoed shell-escaped credentials are masked too.
	for _, name := range []string{"secret", "proxy_secret"} {
		const sensitive = "test'$(not-executed)"
		exec.runFn = func(_, stdin string) (sshx.Result, error) {
			return sshx.Result{Stdout: stdin + "\n" + sensitive}, nil
		}
		result, err := run("echo {{ "+name+" }}", map[string]any{name: sensitive})
		if err != nil || strings.Contains(result.Output, "not-executed") || !strings.Contains(result.Output, "***") {
			t.Fatalf("%s masking failed: %v", name, err)
		}
		if _, err := run("echo {{ "+name+" }}", map[string]any{name: ""}); err == nil || exec.allCommands() != "" {
			t.Fatalf("empty required %s reached SSH", name)
		}
	}
	for _, name := range []string{"registry_url", "node_id"} {
		if _, err := run("echo {{ "+name+" }}", nil); err == nil || exec.allCommands() != "" {
			t.Fatalf("missing %s was silently rendered empty", name)
		}
	}
}

func TestCloudInstallPinsExplicitProfileVersion(t *testing.T) {
	r, _ := newCloudRegistry(t)
	r.cfg.Security.NodeToken = testNodeToken
	uid, connID, cookies, csrf := cloudSession(t, r, "pin-owner")
	res := cloudReadyResource(t, r, uid, connID, cookies, csrf, "pin-create")
	profile, err := r.control.CreateProfile("custom-mtproxyl", "", uid)
	if err != nil {
		t.Fatal(err)
	}
	addVersion := func(marker string) control.ProfileVersion {
		t.Helper()
		pv, err := r.control.AddProfileVersion(profile.ID,
			"# "+marker+"\necho "+marker+" {{ .selected }} --registry-token={{ .node_secret }} --name={{ node_id }}",
			`{"vars":[{"name":"selected","default":"`+marker+`_SCHEMA"}]}`)
		if err != nil {
			t.Fatal(err)
		}
		return pv
	}
	addVersion("UNSELECTED_FIRST") // Keep version-row PK distinct from profile ID.
	pinned := addVersion("PINNED_MTPROXYL")
	addVersion("LATEST_NOT_PINNED")
	foreign, err := r.control.CreateProfile("foreign-private", "", cabinetUser(t, r, "pin-foreign", 3))
	if err != nil {
		t.Fatal(err)
	}
	foreignVersion, err := r.control.AddProfileVersion(foreign.ID, "# private\ntrue", "")
	if err != nil {
		t.Fatal(err)
	}
	graph, err := r.control.CreateProfileVersionGraph(profile.ID, `{}`, 1, "test-graph")
	if err != nil {
		t.Fatal(err)
	}
	// Invalid selections must fail before even decrypting SSH credentials.
	if err := r.control.SetCloudResourceSecrets(res.ID, "unreadable-test-ciphertext"); err != nil {
		t.Fatal(err)
	}
	exec := r.sshExec.(*fakeExec)
	for _, tc := range []struct {
		name, body, code string
		status           int
	}{
		{"zero", `{"profile_version_id":0}`, "profile_invalid", 422},
		{"null", `{"profile_version_id":null}`, "profile_invalid", 422},
		{"wrong-type", `{"profile_version_id":"26"}`, "profile_invalid", 422},
		{"missing-row", `{"profile_version_id":999999999}`, "not_found", 404},
		{"foreign", fmt.Sprintf(`{"profile_version_id":%d}`, foreignVersion.ID), "not_found", 404},
		{"mismatch", fmt.Sprintf(`{"profile_id":%d,"profile_version_id":%d}`, foreign.ID, pinned.ID), "profile_mismatch", 400},
		{"graph", fmt.Sprintf(`{"profile_version_id":%d}`, graph.ID), "profile_incompatible", 422},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := cloudInstallReq(t, r, cookies, csrf, res.ID, "pin-invalid", tc.body)
			if rec.Code != tc.status || !strings.Contains(rec.Body, tc.code) {
				t.Fatalf("selection must fail closed: %d %s", rec.Code, rec.Body)
			}
			if exec.allCommands() != "" {
				t.Fatal("rejected selection reached SSH")
			}
			if _, err := r.control.OperationByIdemKey(control.CloudInstallIdem(res.ID)); !errors.Is(err, control.ErrNotFound) {
				t.Fatalf("rejected selection created an install operation: %v", err)
			}
		})
	}
	// A newer text version exists when the older version is selected. Loading
	// latest in applyProfile would execute this marker instead of the pinned one.
	addVersion("LATEST_NOT_PINNED")
	if err := r.control.SetCloudResourceSecrets(res.ID, res.SecretsEnc); err != nil {
		t.Fatal(err)
	}
	exec.runFn = verifyExecFn(r, true)
	for _, tc := range []struct{ name, body, marker string }{
		{"version-only", fmt.Sprintf(`{"profile_version_id":%d,"vars":{}}`, pinned.ID), "PINNED_MTPROXYL"},
		{"matching-profile", fmt.Sprintf(`{"profile_id":%d,"profile_version_id":%d,"vars":{}}`, profile.ID, pinned.ID), "PINNED_MTPROXYL"},
		{"legacy-latest", fmt.Sprintf(`{"profile_id":%d,"vars":{}}`, profile.ID), "LATEST_NOT_PINNED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec.reset()
			// Свежий готовый ресурс на кейс: пининг версии, а не повторная
			// установка (её покрывает отдельный тест). Создаём напрямую в
			// control-сторе, чтобы не зависеть от фонового create-воркера.
			fresh, err := r.control.CreateCloudResource(control.CloudResourceV2{
				UserID: uid, ConnectionID: connID, Provider: control.ProviderNodehost,
				Label: "pool-node", State: control.CloudResReady,
				ExternalID: "srv-" + tc.name,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := r.control.UpdateCloudResourceState(fresh.ID, control.CloudResReady, control.CloudResReady,
				"srv-"+tc.name, "203.0.113.10"); err != nil {
				t.Fatal(err)
			}
			sealed, serr := cryptoutil.Seal(r.masterKey, `{"password":"root-pw-secret-123"}`)
			if serr != nil {
				t.Fatal(serr)
			}
			if err := r.control.SetCloudResourceSecrets(fresh.ID, sealed); err != nil {
				t.Fatal(err)
			}
			rec := cloudInstallReq(t, r, cookies, csrf, fresh.ID, "pin-"+tc.name, tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("selected install: %d %s", rec.Code, rec.Body)
			}
			op := unmarshalOp(t, []byte(rec.Body), &operationJSON{})
			final := waitOpTerminal(t, r, op.ID)
			waitWorkerIdle(r, op.ID)
			if final.State != control.OpDone || final.ProfileID != profile.ID {
				t.Fatalf("wrong profile/outcome: %+v", final)
			}
			commands := exec.allCommands()
			if !strings.Contains(commands, tc.marker+" '") || !strings.Contains(commands, tc.marker+"_SCHEMA") {
				t.Fatalf("selected steps/schema not executed: %s", commands)
			}
			exec.mu.Lock()
			calls := exec.calls
			exec.mu.Unlock()
			if calls != 1 {
				t.Fatalf("expected one custom step, not default/mixed steps; calls=%d", calls)
			}
		})
	}
}

func TestCloudInstallReplayNoSecondWorker(t *testing.T) {
	r, _ := newCloudRegistry(t)
	uid, connID, cookies, csrf := cloudSession(t, r, "carol")
	res := cloudReadyResource(t, r, uid, connID, cookies, csrf, "k-create")

	block := make(chan struct{})
	exec := r.sshExec.(*fakeExec)
	exec.runFn = func(string, string) (sshx.Result, error) {
		<-block
		return sshx.Result{Stdout: "ok"}, nil
	}

	rec := cloudInstallReq(t, r, cookies, csrf, res.ID, "inst-key", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("install: %d %s", rec.Code, rec.Body)
	}
	op := unmarshalOp(t, []byte(rec.Body), &operationJSON{})
	deadline := time.Now().Add(2 * time.Second)
	for !r.provisionWorkers().runningOp(op.ID) && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	// Повтор тем же ключом, пока воркер занят → 409, второго воркера нет.
	rec = cloudInstallReq(t, r, cookies, csrf, res.ID, "inst-key", `{}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body, "in_progress") {
		t.Fatalf("replay while running: %d %s", rec.Code, rec.Body)
	}
	close(block)
	waitOpTerminal(t, r, op.ID)
	waitWorkerIdle(r, op.ID)

	ops, _ := r.control.OperationsByUser(uid)
	n := 0
	for _, o := range ops {
		if o.Kind == control.OpKindCloudInstall {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("exactly one cloud_install operation expected, got %d", n)
	}
}

func TestCloudInstallGuards(t *testing.T) {
	r, _ := newCloudRegistry(t)
	uid, connID, cookies, csrf := cloudSession(t, r, "dave")

	// not_ready: ещё не ready (planned строка напрямую) → 409 not_ready.
	planned, err := r.control.CreateCloudResource(control.CloudResourceV2{
		UserID: uid, ConnectionID: connID, Provider: control.ProviderNodehost,
		Label: "pool-node", State: control.CloudResPlanned,
	})
	if err != nil {
		t.Fatalf("create planned: %v", err)
	}
	if rec := cloudInstallReq(t, r, cookies, csrf, planned.ID, "k1", `{}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body, "not_ready") {
		t.Fatalf("not_ready: %d %s", rec.Code, rec.Body)
	}

	// ssh_unavailable: ready без запечатанного пароля.
	bare, err := r.control.CreateCloudResource(control.CloudResourceV2{
		UserID: uid, ConnectionID: connID, Provider: control.ProviderNodehost,
		Label: "pool-node", State: control.CloudResPlanned,
	})
	if err != nil {
		t.Fatalf("create bare: %v", err)
	}
	if err := r.control.UpdateCloudResourceState(bare.ID, control.CloudResPlanned, control.CloudResCreating, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.control.UpdateCloudResourceState(bare.ID, control.CloudResCreating, control.CloudResReady, "srv", "203.0.113.1"); err != nil {
		t.Fatal(err)
	}
	if rec := cloudInstallReq(t, r, cookies, csrf, bare.ID, "k2", `{}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body, "ssh_unavailable") {
		t.Fatalf("ssh_unavailable: %d %s", rec.Code, rec.Body)
	}

	// missing Idempotency-Key → 400.
	res := cloudReadyResource(t, r, uid, connID, cookies, csrf, "k-create")
	if rec := cloudInstallReq(t, r, cookies, csrf, res.ID, "", `{}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body, "idempotency_key_required") {
		t.Fatalf("missing key: %d %s", rec.Code, rec.Body)
	}
}

func TestCloudInstallForeignAndBeget(t *testing.T) {
	r, _ := newCloudRegistry(t)
	aliceID, connID, cookiesA, csrfA := cloudSession(t, r, "alice")
	res := cloudReadyResource(t, r, aliceID, connID, cookiesA, csrfA, "k-create")

	// foreign → 404.
	_, _, cookiesB, csrfB := cloudSession(t, r, "bob")
	if rec := cloudInstallReq(t, r, cookiesB, csrfB, res.ID, "k-b", `{}`); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign install: want 404, got %d %s", rec.Code, rec.Body)
	}

	// beget ready-ресурс → 501 not_implemented.
	rec := cloudCall(t, r, http.MethodPost, "/api/me/hosting-connections", cookiesA, csrfA,
		`{"provider":"beget","label":"b","credentials":{"cloud_login":"l","cloud_password":"p"}}`, nil)
	var c struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal([]byte(rec.Body), &c)
	begetRes, err := r.control.CreateCloudResource(control.CloudResourceV2{
		UserID: aliceID, ConnectionID: c.ID, Provider: control.ProviderBeget,
		Label: "pool-node", State: control.CloudResPlanned,
	})
	if err != nil {
		t.Fatalf("create beget res: %v", err)
	}
	_ = r.control.UpdateCloudResourceState(begetRes.ID, control.CloudResPlanned, control.CloudResCreating, "", "")
	_ = r.control.UpdateCloudResourceState(begetRes.ID, control.CloudResCreating, control.CloudResReady, "srv", "203.0.113.2")
	if rec := cloudInstallReq(t, r, cookiesA, csrfA, begetRes.ID, "k-beget", `{}`); rec.Code != http.StatusNotImplemented || !strings.Contains(rec.Body, "not_implemented") {
		t.Fatalf("beget install: %d %s", rec.Code, rec.Body)
	}
}

// blockInstallWorker — install-воркер зависает на первом SSH-шаге; возвращает
// функцию release + op, доведённую до running. F1: пока воркер держит guard,
// delete/change-ip на том же ресурсе обязаны получить 409 resource_busy.
func blockInstallWorker(t *testing.T, r *Registry, cookies map[string]string, csrf string, resID int64) (func(), int64) {
	t.Helper()
	block := make(chan struct{})
	exec := r.sshExec.(*fakeExec)
	exec.runFn = func(string, string) (sshx.Result, error) {
		<-block
		return sshx.Result{Stdout: "ok"}, nil
	}
	rec := cloudInstallReq(t, r, cookies, csrf, resID, "inst-busy", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("install: %d %s", rec.Code, rec.Body)
	}
	op := unmarshalOp(t, []byte(rec.Body), &operationJSON{})
	deadline := time.Now().Add(2 * time.Second)
	for !r.provisionWorkers().runningOp(op.ID) && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if !r.provisionWorkers().runningOp(op.ID) {
		t.Fatal("install worker never started")
	}
	return func() { close(block) }, op.ID
}

func TestCloudInstallVsDeleteBusy(t *testing.T) {
	r, fake := newCloudRegistry(t)
	uid, connID, cookies, csrf := cloudSession(t, r, "alice")
	res := cloudReadyResource(t, r, uid, connID, cookies, csrf, "k-create")

	release, opID := blockInstallWorker(t, r, cookies, csrf, res.ID)

	del := cloudCall(t, r, http.MethodDelete, fmt.Sprintf("/api/me/cloud-resources/%d", res.ID), cookies, csrf, "",
		map[string]string{"Idempotency-Key": "del-busy", confirmDeleteHeader: "delete"})
	if del.Code != http.StatusConflict || !strings.Contains(del.Body, "resource_busy") {
		t.Fatalf("delete vs install: %d %s", del.Code, del.Body)
	}
	fake.mu.Lock()
	n := fake.deletes
	fake.mu.Unlock()
	if n != 0 {
		t.Fatalf("provider Delete must not be called during install, got %d", n)
	}
	if got, _ := r.control.CloudResourceOwned(res.ID, uid); got.State != control.CloudResReady {
		t.Fatalf("resource must stay ready during install, got %s", got.State)
	}

	release()
	waitOpTerminal(t, r, opID)
	waitWorkerIdle(r, opID)
}

func TestCloudInstallVsChangeIPBusy(t *testing.T) {
	r, fake := newCloudRegistry(t)
	uid, connID, cookies, csrf := cloudSession(t, r, "bob")
	res := cloudReadyResource(t, r, uid, connID, cookies, csrf, "k-create")

	release, opID := blockInstallWorker(t, r, cookies, csrf, res.ID)

	rec := cloudCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/cloud-resources/%d/change-ip", res.ID), cookies, csrf,
		`{"subnet_id":5}`, map[string]string{"Idempotency-Key": "ip-busy"})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body, "resource_busy") {
		t.Fatalf("change-ip vs install: %d %s", rec.Code, rec.Body)
	}
	if n := fake.changeIPs; n != 0 {
		t.Fatalf("provider ChangeIP must not be called during install, got %d", n)
	}

	release()
	waitOpTerminal(t, r, opID)
	waitWorkerIdle(r, opID)
}

// TestCloudInstallRejectedWhileGuardHeld — обратное направление F1: пока
// другой mutation (delete/change-ip) держит guard, install → 409 resource_busy
// и не создаёт операцию. Guard берётся напрямую (in-process primitive).
func TestCloudInstallRejectedWhileGuardHeld(t *testing.T) {
	r, _ := newCloudRegistry(t)
	uid, connID, cookies, csrf := cloudSession(t, r, "carol")
	res := cloudReadyResource(t, r, uid, connID, cookies, csrf, "k-create")

	if !r.cloudGuardAcquire(res.ID) {
		t.Fatal("guard unexpectedly held")
	}
	defer r.cloudGuardRelease(res.ID)

	rec := cloudInstallReq(t, r, cookies, csrf, res.ID, "inst-guard", `{}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body, "resource_busy") {
		t.Fatalf("install while guarded: %d %s", rec.Code, rec.Body)
	}
	ops, _ := r.control.OperationsByUser(uid)
	for _, o := range ops {
		if o.Kind == control.OpKindCloudInstall {
			t.Fatalf("install must not create an operation while guarded: %+v", o)
		}
	}
}
