package server

// review-addendum.md фиксы: HIGH-2 (привязка node_secret к своей ноде при
// ВКЛючённом legacy-секрете) и MED-4 (сжатие слотов при уменьшении
// masters_per_domain). Оба — детерминированные, без сети.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sharedd/registry/internal/control"
)

// HIGH-2: секрет ноды A, предъявленный с node_id B, отклоняется 403 на
// /heartbeat и /report — независимо от legacy-переключателя (здесь ВКЛ).
func TestNodeSecretOwnershipEnforcedWithLegacySharedOn(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	r.cfg.Security.NodeToken = testNodeToken
	r.legacyShared.Store(true) // стоковый дефолт: общий секрет разрешён

	u, err := r.control.CreateUser("alice", "password-123", "user", 5)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	const ownNode, otherNode = "alpha-aaaa1", "victim-bbbb2"
	if err := r.control.AuthorizeNode(ownNode, u); err != nil {
		t.Fatalf("authorize node: %v", err)
	}
	if _, ok, err := r.control.SetNodeSecret(ownNode, "secret-of-alpha"); err != nil || !ok {
		t.Fatalf("set secret: ok=%v err=%v", ok, err)
	}
	mux := r.buildMux()

	for _, path := range []string{"/heartbeat", "/report"} {
		// чужой node_id с секретом A → 403.
		req := httptest.NewRequest(http.MethodPost, path,
			strings.NewReader(fmt.Sprintf(`{"node_id":%q}`, otherNode)))
		req.Header.Set("Authorization", "Bearer secret-of-alpha")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s cross-node: got %d %s, want 403", path, rec.Code, rec.Body.String())
		}

		// свой node_id — авторизация проходит (не 401/403; неизвестная нода → 410/400).
		req = httptest.NewRequest(http.MethodPost, path,
			strings.NewReader(fmt.Sprintf(`{"node_id":%q}`, ownNode)))
		req.Header.Set("Authorization", "Bearer secret-of-alpha")
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
			t.Fatalf("%s own node: got %d %s, want authorized", path, rec.Code, rec.Body.String())
		}
	}
}

// MED-4: уменьшение masters_per_domain сжимает существующие слоты домена
// (и потому набор A-записей), а не оставляет залипшие лишние мастера.
func TestMastersPerDomainDecreaseTrimsSlots(t *testing.T) {
	r := newTestRegistry(t)
	r.cfg.Selection.MastersPerDomain = 3

	for i, id := range []string{"A", "B", "C"} {
		r.register(registerRequest{NodeID: id, IP: fmt.Sprintf("1.1.1.%d", i+1)})
		makeFullyHealthyNow(r, id)
	}
	r.evaluateAssignments(time.Now())
	if got := len(r.state.Assignments["d1.example.com"]); got != 3 {
		t.Fatalf("expected 3 slots at M=3, got %d (%v)", got, r.state.Assignments["d1.example.com"])
	}

	r.cfg.Selection.MastersPerDomain = 1
	r.evaluateAssignments(time.Now())
	if got := len(r.state.Assignments["d1.example.com"]); got != 1 {
		t.Fatalf("M decrease must trim slots to 1, got %d (%v)", got, r.state.Assignments["d1.example.com"])
	}
	if got := len(r.state.AssignmentsSince["d1.example.com"]); got > 1 {
		t.Fatalf("AssignmentsSince must shrink with slots, got %d", got)
	}
}

// MED-5 — отмена pending-заказа (до платного вызова) освобождает место.
func TestCancelReleasesQuotaSlot(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	u, err := r.control.CreateUser("bob", "password-123", "user", 5)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	sess, csrf := cabinetLogin(t, r, "bob", "password-123")

	const nodeID = "vps123-abc12"
	if err := r.control.AuthorizeNode(nodeID, u); err != nil {
		t.Fatalf("authorize node: %v", err)
	}
	op, _, err := r.control.CreateOperation(control.Operation{
		UserID: u.ID, NodeID: nodeID, Kind: control.OpKindCreate,
		State: control.OpPending, IdemKey: "cancel-slot",
	})
	if err != nil {
		t.Fatalf("create op: %v", err)
	}
	if used, _ := r.control.UsedSlots(u.ID); used != 1 {
		t.Fatalf("pre-cancel used = %d, want 1", used)
	}

	rec := cabinetCall(t, r, http.MethodPost,
		fmt.Sprintf("/api/me/operations/%d/cancel", op.ID), cookieMap(sess, csrf), csrf, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body.String())
	}
	if used, _ := r.control.UsedSlots(u.ID); used != 0 {
		t.Fatalf("slot must be released on cancel, used = %d", used)
	}
}
