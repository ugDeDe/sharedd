package control

import (
	"errors"
	"testing"
)

// Переустановка manual-сервера (reinstall) завершённой операции: привязка
// ноды переносится, а не дублируется. Покрывает смену node_id (агент
// переустановлен «с чистого листа»): старая строка node_owners должна быть
// снята, иначе осталась бы сиротой, а квота — занята дважды.
func TestReinstallRetiresOldNodeBindingAndKeepsQuota(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "alice", 1) // одно место: reinstall не должен его тратить
	h, err := s.CreateManualHost(ManualHost{
		UserID: u.ID, Label: "box", Host: "8.8.8.8", Port: 22, SSHUser: "root",
		AuthType: "password", SecretEnc: "enc",
	})
	if err != nil {
		t.Fatalf("create host: %v", err)
	}

	// Первая установка → claim ноды box-abcde.
	op, err := s.AuthorizeManualOperation(h, Operation{}, u, 0, "secret-1", "enc-1")
	if err != nil {
		t.Fatalf("first authorize: %v", err)
	}
	if uid, err := s.ClaimPendingNodeSecret("secret-1", "box-abcde"); err != nil || uid != u.ID {
		t.Fatalf("first claim: uid=%d err=%v", uid, err)
	}
	if err := s.UpdateOperationState(op.ID, OpDone, "", ""); err != nil {
		t.Fatal(err)
	}
	h, err = s.ManualHost(h.ID)
	if err != nil {
		t.Fatal(err)
	}
	op, err = s.Operation(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if op.NodeID != "box-abcde" || h.NodeID != "box-abcde" {
		t.Fatalf("after first install: op.node=%q host.node=%q", op.NodeID, h.NodeID)
	}
	if used, _ := s.UsedNodes(u.ID); used != 1 {
		t.Fatalf("used=%d, want 1", used)
	}

	// Переустановка с ДРУГИМ профилем и новой node_id.
	op2, err := s.AuthorizeManualOperation(h, op, u, 7, "secret-1", "enc-1")
	if err != nil {
		t.Fatalf("reinstall authorize: %v", err)
	}
	if op2.State != OpInstalling || op2.NodeID != "" {
		t.Fatalf("reinstall op = %+v, want installing/no-node", op2)
	}
	if op2.ID != op.ID || op2.ProfileID != 7 {
		t.Fatalf("reinstall must keep id and take new profile: %+v", op2)
	}
	// Старая привязка снята сразу при accept — нет окна с двумя местами.
	if _, _, ok, _ := s.NodeOwner("box-abcde"); ok {
		t.Fatal("old node binding must be retired on reinstall")
	}
	if used, _ := s.UsedNodes(u.ID); used != 1 {
		t.Fatalf("reinstall must still hold exactly one slot, used=%d", used)
	}

	// Новая нода claim'ит pending — привязка ровно одна.
	if uid, err := s.ClaimPendingNodeSecret("secret-1", "box-zzzzz"); err != nil || uid != u.ID {
		t.Fatalf("reinstall claim: uid=%d err=%v", uid, err)
	}
	if _, _, ok, _ := s.NodeOwner("box-abcde"); ok {
		t.Fatal("old binding must stay retired after re-claim")
	}
	if owner, _, ok, _ := s.NodeOwner("box-zzzzz"); !ok || owner != u.ID {
		t.Fatalf("new binding missing: owner=%d ok=%t", owner, ok)
	}
	if used, _ := s.UsedNodes(u.ID); used != 1 {
		t.Fatalf("after reinstall: used=%d, want 1", used)
	}
	if _, err := s.PendingSecretByOperation(op.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pending secret must be consumed, err=%v", err)
	}
	got, err := s.Operation(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Профиль обновлён, node_id перепривязан; done выставит воркер после Confirm.
	if got.ProfileID != 7 || got.NodeID != "box-zzzzz" || got.State != OpInstalling {
		t.Fatalf("final op = %+v", got)
	}
}

// Переустановка с тем же node_id (агент сохранил /var/lib/sharedd/node_id):
// привязка переоформляется на того же владельца, места — одно.
func TestReinstallSameNodeIDReclaimsIdempotently(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "alice", 1)
	h, err := s.CreateManualHost(ManualHost{
		UserID: u.ID, Label: "box", Host: "8.8.8.8", Port: 22, SSHUser: "root",
		AuthType: "password", SecretEnc: "enc",
	})
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.AuthorizeManualOperation(h, Operation{}, u, 0, "secret-1", "enc-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimPendingNodeSecret("secret-1", "box-abcde"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateOperationState(op.ID, OpDone, "", ""); err != nil {
		t.Fatal(err)
	}
	h, _ = s.ManualHost(h.ID)
	op, _ = s.Operation(op.ID)

	if _, err := s.AuthorizeManualOperation(h, op, u, 0, "secret-1", "enc-1"); err != nil {
		t.Fatalf("same-node reinstall authorize: %v", err)
	}
	if _, err := s.ClaimPendingNodeSecret("secret-1", "box-abcde"); err != nil {
		t.Fatalf("same-node reinstall claim: %v", err)
	}
	if owner, _, ok, _ := s.NodeOwner("box-abcde"); !ok || owner != u.ID {
		t.Fatalf("binding lost after same-node reinstall: owner=%d ok=%t", owner, ok)
	}
	if used, _ := s.UsedNodes(u.ID); used != 1 {
		t.Fatalf("used=%d, want 1", used)
	}
	if _, err := s.PendingSecretByOperation(op.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pending secret must be consumed, err=%v", err)
	}
}

// Чужой/подменённый host при переустановке по-прежнему отвергается.
func TestReinstallRejectsForeignOwner(t *testing.T) {
	s := openTest(t)
	alice := mustUser(t, s, "alice", 5)
	bob := mustUser(t, s, "bob", 5)
	h, err := s.CreateManualHost(ManualHost{
		UserID: alice.ID, Label: "box", Host: "8.8.8.8", Port: 22, SSHUser: "root",
		AuthType: "password", SecretEnc: "enc",
	})
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.AuthorizeManualOperation(h, Operation{}, alice, 0, "secret-1", "enc-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimPendingNodeSecret("secret-1", "box-abcde"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateOperationState(op.ID, OpDone, "", ""); err != nil {
		t.Fatal(err)
	}
	h, _ = s.ManualHost(h.ID)
	op, _ = s.Operation(op.ID)

	if _, err := s.AuthorizeManualOperation(h, op, bob, 0, "secret-2", "enc-2"); !errors.Is(err, ErrOwnerMismatch) {
		t.Fatalf("foreign owner reinstall must be ErrOwnerMismatch, got %v", err)
	}
}
