package server

import (
	"errors"
	"net/http"
	"testing"

	"sharedd/registry/internal/control"
)

// Удаление владельцем сразу освобождает пул-место и блокирует возврат агента;
// администратор для освобождения места не нужен.
func TestDeleteNodeFreesQuotaAndBlocksReRegister(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	uid := cabinetUser(t, r, "alice", 1)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	u := control.User{ID: uid, MaxNodes: 1}
	if err := r.control.AuthorizeNode("n1-abcde", u); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.state.Candidates["n1-abcde"] = &Candidate{NodeID: "n1-abcde", IP: "1.2.3.4"}
	r.mu.Unlock()
	if used, err := r.control.UsedNodes(uid); err != nil || used != 1 {
		t.Fatalf("used=%d err=%v, want 1", used, err)
	}
	rec := cabinetCall(t, r, http.MethodDelete, "/api/me/nodes/n1-abcde", cookieMap(sess, csrf), csrf, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if used, err := r.control.UsedNodes(uid); err != nil || used != 0 {
		t.Fatalf("quota after delete: used=%d err=%v, want 0", used, err)
	}
	r.mu.RLock()
	_, stillCandidate := r.state.Candidates["n1-abcde"]
	r.mu.RUnlock()
	if stillCandidate {
		t.Fatal("deleted node must leave the pool immediately")
	}
	if err := r.control.AuthorizeNode("n1-abcde", u); !errors.Is(err, control.ErrNodeDeleting) {
		t.Fatalf("deleted identity must not re-register, got %v", err)
	}
}
