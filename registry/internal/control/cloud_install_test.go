package control

// Тесты приёма cloud-install (stage-06): атомарный приём, квота, owner-изоляция,
// not-ready, CAS-привязка node_id после claim.

import (
	"errors"
	"testing"
)

// readyCloudResource — ресурс пользователя в состоянии ready с external_id.
func readyCloudResource(t *testing.T, s *Store, uid, connID int64, label string) CloudResourceV2 {
	t.Helper()
	r, err := s.CreateCloudResource(CloudResourceV2{
		UserID: uid, ConnectionID: connID, Provider: ProviderNodehost,
		Label: label, State: CloudResPlanned,
	})
	if err != nil {
		t.Fatalf("create resource: %v", err)
	}
	if err := s.UpdateCloudResourceState(r.ID, CloudResPlanned, CloudResCreating, "", ""); err != nil {
		t.Fatalf("planned→creating: %v", err)
	}
	if err := s.UpdateCloudResourceState(r.ID, CloudResCreating, CloudResReady, "srv-ext", "203.0.113.10"); err != nil {
		t.Fatalf("creating→ready: %v", err)
	}
	return r
}

func TestAuthorizeCloudInstallHappy(t *testing.T) {
	s := newCloudTestStore(t)
	alice := mustUser(t, s, "alice-ci", 10)
	conn := mustConn(t, s, alice.ID, ProviderNodehost, "main", "sealed-1")
	res := readyCloudResource(t, s, alice.ID, conn.ID, "pool-node")

	op, err := s.AuthorizeCloudInstall(res.ID, alice, 5, "sec-1", "enc-1")
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if op.ID == 0 || op.Kind != OpKindCloudInstall || op.State != OpInstalling || op.NodeID != "" || op.ProfileID != 5 {
		t.Fatalf("op: %+v", op)
	}
	// Операция находится по детерминированному ключу ресурса.
	got, err := s.OperationByIdemKey(CloudInstallIdem(res.ID))
	if err != nil || got.ID != op.ID {
		t.Fatalf("lookup by idem: %v %+v", err, got)
	}
	// pending-бронь записана с label ресурса.
	p, err := s.PendingSecretByOperation(op.ID)
	if err != nil || p.Label != "pool-node" || p.SecretEnc != "enc-1" {
		t.Fatalf("pending: %v %+v", err, p)
	}

	// Повтор (retry) той же операции: та же строка, без новой операции.
	op2, err := s.AuthorizeCloudInstall(res.ID, alice, 5, "sec-1", "enc-1")
	if err != nil || op2.ID != op.ID {
		t.Fatalf("retry must reuse op: %v %+v", err, op2)
	}
}

func TestAuthorizeCloudInstallQuotaAndOwnerAndState(t *testing.T) {
	s := newCloudTestStore(t)

	// Install does not consume a slot; zero quota is enforced on registration.
	full := mustUser(t, s, "full-ci", 0)
	conn := mustConn(t, s, full.ID, ProviderNodehost, "main", "sealed-1")
	res := readyCloudResource(t, s, full.ID, conn.ID, "pool-node")
	op, err := s.AuthorizeCloudInstall(res.ID, full, 0, "sec-1", "enc-1")
	if err != nil {
		t.Fatalf("install without admission: %v", err)
	}
	expectedID, err := CloudInstallExpectedNodeID(op)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimPendingNodeSecret("sec-1", expectedID); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("admission: want ErrQuotaExceeded, got %v", err)
	}

	// Owner: чужой пользователь → ErrNotFound.
	alice := mustUser(t, s, "alice-ci2", 10)
	bob := mustUser(t, s, "bob-ci2", 10)
	aliceConn := mustConn(t, s, alice.ID, ProviderNodehost, "main", "sealed-2")
	res2 := readyCloudResource(t, s, alice.ID, aliceConn.ID, "pool-node")
	if _, err := s.AuthorizeCloudInstall(res2.ID, bob, 0, "sec-1", "enc-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign: want ErrNotFound, got %v", err)
	}

	// not-ready: ресурс ещё planned → ErrCreateInProgress.
	planned, err := s.CreateCloudResource(CloudResourceV2{
		UserID: alice.ID, ConnectionID: aliceConn.ID, Provider: ProviderNodehost,
		Label: "pool-node", State: CloudResPlanned,
	})
	if err != nil {
		t.Fatalf("create planned: %v", err)
	}
	if _, err := s.AuthorizeCloudInstall(planned.ID, alice, 0, "sec-1", "enc-1"); !errors.Is(err, ErrCreateInProgress) {
		t.Fatalf("not-ready: want ErrCreateInProgress, got %v", err)
	}

	// Пустой секрет → ErrOwnerMismatch (до любых записей).
	if _, err := s.AuthorizeCloudInstall(res2.ID, alice, 0, "", ""); !errors.Is(err, ErrOwnerMismatch) {
		t.Fatalf("empty secret: want ErrOwnerMismatch, got %v", err)
	}
}

func TestSetCloudResourceNodeCAS(t *testing.T) {
	s := newCloudTestStore(t)
	alice := mustUser(t, s, "alice-setnode", 10)
	conn := mustConn(t, s, alice.ID, ProviderNodehost, "main", "sealed-1")
	res := readyCloudResource(t, s, alice.ID, conn.ID, "pool-node")

	if err := s.SetCloudResourceNode(res.ID, "pool-node-abc12", CloudResReady); err != nil {
		t.Fatalf("set node: %v", err)
	}
	got, _ := s.CloudResourceOwned(res.ID, alice.ID)
	if got.NodeID != "pool-node-abc12" || got.Revision != 4 {
		t.Fatalf("after set: %+v", got)
	}
	// Неверное ожидаемое состояние → ErrRevisionConflict.
	if err := s.SetCloudResourceNode(res.ID, "x", CloudResPlanned); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale state: want ErrRevisionConflict, got %v", err)
	}
	// Неизвестный ресурс → ErrNotFound.
	if err := s.SetCloudResourceNode(99999, "x", CloudResReady); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: want ErrNotFound, got %v", err)
	}
}
