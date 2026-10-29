package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sharedd/registry/internal/control"
)

// Legacy reverify is intentionally the stage-02 contract: /register alone
// cannot clear quarantine, even when the client claims GP success in /report.
func TestSameIPRepeatedRecoveryRequiresIndependentGP(t *testing.T) {
	mock := newGPMock(t)
	r := newTerminateTestRegistry(t)
	r.cfg.Globalping.APIBase = mock.URL
	mux := r.buildMux()
	id, ip := "node-repeat", "8.8.8.8"
	r.register(registerRequest{NodeID: id, IP: ip})
	reg := func() int {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(`{"node_id":"node-repeat","ip":"8.8.8.8"}`)))
		return rec.Code
	}
	for i := 0; i < 10; i++ {
		r.mu.Lock()
		r.terminateNodeLocked(r.state.Candidates[id], time.Now(), BanReasonIPBan, "test episode")
		r.mu.Unlock()
		if code := reg(); code != http.StatusOK {
			t.Fatalf("cycle %d register=%d", i, code)
		}
		if q := r.state.Candidates[id].Quarantine; q == nil || !q.Reverify {
			t.Fatal("register bypassed quarantine")
		}
		mock.setBad(true)
		sendReport(t, r, id, true, true) // client says true; GET verifier says false
		if rec := r.state.Terminated[id]; rec == nil || !rec.ReverifyFailed {
			t.Fatal("trusted client success bool")
		}
		if code := reg(); code != http.StatusForbidden {
			t.Fatalf("cooldown bypassed: %d", code)
		}
		r.mu.Lock()
		r.state.Terminated[id].At = time.Now().Add(-reverifyCooldown - time.Second)
		r.mu.Unlock()
		if code := reg(); code != http.StatusOK {
			t.Fatalf("cooldown never reopened: %d", code)
		}
		mock.setBad(false)
		sendReport(t, r, id, true, true)
		if c := r.state.Candidates[id]; c == nil || c.Quarantine != nil || !c.GlobalpingOK {
			t.Fatalf("cycle %d failed recovery", i)
		}
	}
}

// Real pending-secret auth/claim handlers: a cooldown refusal must not grant
// config access, consume the claim, rotate its credential, or spend two slots.
// Agent retry scheduling is exercised separately in the node module (internal
// packages cannot import each other across these two module roots).
func TestPendingSecretBootstrapAfterBan(t *testing.T) {
	for _, changeIP := range []bool{false, true} {
		t.Run(fmt.Sprintf("change_ip=%t", changeIP), func(t *testing.T) {
			r, _ := newCabinetRegistry(t)
			r.cfg.Security.NodeToken = testNodeToken
			r.cfg.QuarantineAttempts = 3
			r.cfg.SharedProxy.TLSDomain = "front.example.com"
			gp := newGPMock(t)
			r.cfg.Globalping.APIBase = gp.URL
			uid := cabinetUser(t, r, "bootstrap", 1)
			op, _, err := r.control.CreateOperation(control.Operation{UserID: uid, Kind: control.OpKindManualInstall, State: control.OpInstalling, IdemKey: "bootstrap"})
			if err != nil {
				t.Fatal(err)
			}
			const id, secret = "boot-abc12", "pending-bootstrap-test-secret"
			if err := r.control.UpsertPendingSecret(uid, op.ID, "boot", secret, ""); err != nil {
				t.Fatal(err)
			}
			r.state.Terminated[id] = &TerminatedRecord{NodeID: id, IP: "8.8.8.8", Reason: BanReasonIPBan, At: time.Now(), ReverifyFailed: true}
			mux := r.buildMux()
			call := func(method, path, nodeID, body string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(method, path, strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+secret)
				req.Header.Set("X-ShareDD-Node-ID", nodeID)
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, req)
				return rec
			}
			reg := func(ip string) *httptest.ResponseRecorder {
				return call(http.MethodPost, "/register", id, fmt.Sprintf(`{"node_id":%q,"ip":%q,"node_type":"classic"}`, id, ip))
			}
			if rec := reg("8.8.8.8"); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"terminate":true`) {
				t.Fatalf("initial register: %d %s", rec.Code, rec.Body.String())
			}
			if rec := call(http.MethodGet, "/config", id, ""); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "pending_secret") {
				t.Fatalf("unclaimed config: %d %s", rec.Code, rec.Body.String())
			}
			if nodes, err := r.control.NodesByUser(uid); err != nil || len(nodes) != 0 {
				t.Fatalf("early claim: %v %v", nodes, err)
			}
			if _, ok, err := r.control.PendingSecret(secret); err != nil || !ok {
				t.Fatalf("pending lost: %t %v", ok, err)
			}
			ip := "8.8.8.8"
			if changeIP {
				ip = "1.1.1.1"
			} else {
				r.state.Terminated[id].At = time.Now().Add(-reverifyCooldown - time.Second)
			}
			for i := 0; i < 2; i++ {
				if rec := reg(ip); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "node_secret") {
					t.Fatalf("claim/retry: %d %s", rec.Code, rec.Body.String())
				}
			}
			if _, ok, err := r.control.PendingSecret(secret); err != nil || ok {
				t.Fatalf("claim not consumed: %t %v", ok, err)
			}
			if owner, ok, err := r.control.NodeSecretOwner(secret); err != nil || !ok || owner != id {
				t.Fatalf("secret not retained: %q %t %v", owner, ok, err)
			}
			if nodes, err := r.control.NodesByUser(uid); err != nil || len(nodes) != 1 || nodes[0].NodeID != id {
				t.Fatalf("owner slots: %v %v", nodes, err)
			}
			if used, err := r.control.UsedNodes(uid); err != nil || used != 1 {
				t.Fatalf("quota slots: %d %v", used, err)
			}
			if rec := call(http.MethodGet, "/config", id, ""); rec.Code != http.StatusOK {
				t.Fatalf("claimed config: %d %s", rec.Code, rec.Body.String())
			}
			if rec := call(http.MethodGet, "/config", "other-abc12", ""); rec.Code != http.StatusForbidden {
				t.Fatalf("cross-identity config: %d", rec.Code)
			}
			c := r.state.Candidates[id]
			if c.GlobalpingOK {
				t.Fatal("claim alone admitted GP")
			}
			if !changeIP && (c.Quarantine == nil || !c.Quarantine.Reverify) {
				t.Fatal("same-IP claim bypassed reverify")
			}
			payload := HealthReportPayload{NodeID: id, IP: ip, Port: 443, FakeSNI: "front.example.com", MetricsOK: true, GlobalpingOK: true, CheckedAt: time.Now()}
			payload.GlobalpingMeasurementID = fmt.Sprintf("m-test-%s-%d", ip, payload.CheckedAt.UnixNano())
			body, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			if rec := call(http.MethodPost, "/report", id, string(body)); rec.Code != http.StatusOK {
				t.Fatalf("verified report: %d %s", rec.Code, rec.Body.String())
			}
			if c := r.state.Candidates[id]; !c.GlobalpingOK || c.Quarantine != nil || c.GPChecksTotal != 1 {
				t.Fatalf("GP not independently verified: %+v", c)
			}
			// Pin the real contract consumed by the agent's missing-candidate
			// recovery path: auth survives prune; /report says 404, not 410.
			r.mu.Lock()
			delete(r.state.Candidates, id)
			r.mu.Unlock()
			if rec := call(http.MethodPost, "/report", id, string(body)); rec.Code != http.StatusNotFound {
				t.Fatalf("missing-candidate report: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}
