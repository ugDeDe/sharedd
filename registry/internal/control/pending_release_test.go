package control

// Pending-бронь не должна переживать свой ресурс/хост: удаление ресурса
// (phantom local или подтверждённое провайдером) и manual-хоста освобождает
// место; reconcile убирает только истинные сироты и не трогает retry-путь
// живой установки.

import (
	"errors"
	"testing"
)

func TestCloudResourceLocalDeleteReleasesPending(t *testing.T) {
	s := newCloudTestStore(t)
	alice := mustUser(t, s, "alice-rel-local", 10)
	bob := mustUser(t, s, "bob-rel-local", 10)
	conn := mustConn(t, s, alice.ID, ProviderNodehost, "main", "sealed-1")
	res := readyCloudResource(t, s, alice.ID, conn.ID, "pool-node")

	op, err := s.AuthorizeCloudInstall(res.ID, alice, 0, "sec-local", "enc-local")
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if used, _ := s.UsedNodes(alice.ID); used != 0 {
		t.Fatalf("pending must not occupy a slot, used=%d", used)
	}

	// Чужой delete не должен освобождать бронь владельца.
	if err := s.DeleteCloudResourceLocal(res.ID, bob.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign delete: want ErrNotFound, got %v", err)
	}
	if _, err := s.PendingSecretByOperation(op.ID); err != nil {
		t.Fatalf("foreign delete released owner's pending: %v", err)
	}

	// A paid, ready resource cannot be phantom-deleted. Emulate confirmed
	// provider deletion, then verify that its pending install is revoked.
	if err := s.DeleteCloudResourceLocal(res.ID, alice.ID); !errors.Is(err, ErrCreateInProgress) {
		t.Fatalf("paid resource must require provider deletion: %v", err)
	}
	if err := s.UpdateCloudResourceState(res.ID, CloudResReady, CloudResDeleted, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReleasePendingSecretsForCloudResource(res.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PendingSecretByOperation(op.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pending after resource delete: want ErrNotFound, got %v", err)
	}
	if used, _ := s.UsedNodes(alice.ID); used != 0 {
		t.Fatalf("quota after local delete: used=%d want 0", used)
	}
}

func TestCloudResourceProviderDeleteReleasesPending(t *testing.T) {
	s := newCloudTestStore(t)
	alice := mustUser(t, s, "alice-rel-prov", 10)
	conn := mustConn(t, s, alice.ID, ProviderNodehost, "main", "sealed-1")
	res := readyCloudResource(t, s, alice.ID, conn.ID, "pool-node")

	op, err := s.AuthorizeCloudInstall(res.ID, alice, 0, "sec-prov", "enc-prov")
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	// Провайдер подтвердил удаление: ready→deleting→deleted (строка остаётся).
	if err := s.UpdateCloudResourceState(res.ID, CloudResReady, CloudResDeleting, "", ""); err != nil {
		t.Fatalf("ready→deleting: %v", err)
	}
	if err := s.UpdateCloudResourceState(res.ID, CloudResDeleting, CloudResDeleted, "", ""); err != nil {
		t.Fatalf("deleting→deleted: %v", err)
	}
	if n, err := s.ReleasePendingSecretsForCloudResource(res.ID); err != nil || n != 1 {
		t.Fatalf("release: n=%d err=%v, want n=1", n, err)
	}
	if _, err := s.PendingSecretByOperation(op.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pending after provider delete: want ErrNotFound, got %v", err)
	}
}

func TestReconcileKeepsPendingForFailedInstallWithLiveResource(t *testing.T) {
	s := newCloudTestStore(t)
	alice := mustUser(t, s, "alice-rel-live", 10)
	conn := mustConn(t, s, alice.ID, ProviderNodehost, "main", "sealed-1")
	res := readyCloudResource(t, s, alice.ID, conn.ID, "pool-node")

	op, err := s.AuthorizeCloudInstall(res.ID, alice, 0, "sec-live", "enc-live")
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if err := s.UpdateOperationState(op.ID, OpFailed, "", "ssh_denied"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	n, err := s.ReconcileOrphanPendingSecrets()
	if err != nil || n != 0 {
		t.Fatalf("reconcile: n=%d err=%v, want 0", n, err)
	}
	if _, err := s.PendingSecretByOperation(op.ID); err != nil {
		t.Fatalf("retryable pending on live resource must survive: %v", err)
	}
}

func TestManualHostDeleteReleasesPending(t *testing.T) {
	s := openTest(t)
	alice := mustUser(t, s, "alice-manual-del", 10)
	h, err := s.CreateManualHost(ManualHost{
		UserID: alice.ID, Label: "box", Host: "8.8.8.8", Port: 22, SSHUser: "root",
		AuthType: "password", SecretEnc: "enc",
	})
	if err != nil {
		t.Fatalf("create host: %v", err)
	}
	op, err := s.AuthorizeManualOperation(h, Operation{}, alice, 0, "sec-manual", "enc-manual")
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if err := s.DeleteManualHost(h.ID); err != nil {
		t.Fatalf("delete host: %v", err)
	}
	if _, err := s.PendingSecretByOperation(op.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pending after host delete: want ErrNotFound, got %v", err)
	}
	if used, _ := s.UsedNodes(alice.ID); used != 0 {
		t.Fatalf("quota after host delete: used=%d want 0", used)
	}
}

func TestReconcilePendingRemovesOnlyTrueOrphans(t *testing.T) {
	s := newCloudTestStore(t)
	alice := mustUser(t, s, "alice-reconcile", 10)
	conn := mustConn(t, s, alice.ID, ProviderNodehost, "main", "sealed-1")
	conn2 := mustConn(t, s, alice.ID, ProviderNodehost, "main-2", "sealed-2")

	// Cloud: живая установка на существующем ресурсе — pending остаётся.
	liveRes := readyCloudResource(t, s, alice.ID, conn.ID, "live")
	liveOp, err := s.AuthorizeCloudInstall(liveRes.ID, alice, 0, "sec-live", "enc-live")
	if err != nil {
		t.Fatalf("authorize live: %v", err)
	}
	if err := s.UpdateOperationState(liveOp.ID, OpFailed, "", "retry"); err != nil {
		t.Fatalf("fail live: %v", err)
	}

	// Cloud: ресурс удалён «в обход» — истинная сирота.
	orphanRes := readyCloudResource(t, s, alice.ID, conn2.ID, "orphan")
	orphanOp, err := s.AuthorizeCloudInstall(orphanRes.ID, alice, 0, "sec-orphan", "enc-orphan")
	if err != nil {
		t.Fatalf("authorize orphan: %v", err)
	}
	if err := s.UpdateOperationState(orphanOp.ID, OpFailed, "", "gone"); err != nil {
		t.Fatalf("fail orphan: %v", err)
	}
	if _, err := s.sql.Exec(`DELETE FROM cloud_resources2 WHERE id = ?`, orphanRes.ID); err != nil {
		t.Fatalf("hard delete orphan resource: %v", err)
	}

	// Manual: живой хост — pending остаётся; удалённый — сирота.
	liveHost, err := s.CreateManualHost(ManualHost{
		UserID: alice.ID, Label: "livebox", Host: "8.8.8.8", Port: 22,
		SSHUser: "root", AuthType: "password", SecretEnc: "enc",
	})
	if err != nil {
		t.Fatalf("create live host: %v", err)
	}
	liveHostOp, err := s.AuthorizeManualOperation(liveHost, Operation{}, alice, 0, "sec-mlive", "enc-mlive")
	if err != nil {
		t.Fatalf("authorize live host: %v", err)
	}
	if err := s.UpdateOperationState(liveHostOp.ID, OpFailed, "", "retry"); err != nil {
		t.Fatalf("fail live host: %v", err)
	}

	orphanHost, err := s.CreateManualHost(ManualHost{
		UserID: alice.ID, Label: "orphanbox", Host: "9.9.9.9", Port: 22,
		SSHUser: "root", AuthType: "password", SecretEnc: "enc",
	})
	if err != nil {
		t.Fatalf("create orphan host: %v", err)
	}
	orphanHostOp, err := s.AuthorizeManualOperation(orphanHost, Operation{}, alice, 0, "sec-morphan", "enc-morphan")
	if err != nil {
		t.Fatalf("authorize orphan host: %v", err)
	}
	if err := s.UpdateOperationState(orphanHostOp.ID, OpFailed, "", "gone"); err != nil {
		t.Fatalf("fail orphan host: %v", err)
	}
	if _, err := s.sql.Exec(`DELETE FROM manual_hosts WHERE id = ?`, orphanHost.ID); err != nil {
		t.Fatalf("hard delete orphan host: %v", err)
	}

	n, err := s.ReconcileOrphanPendingSecrets()
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != 2 {
		t.Fatalf("reconcile removed %d, want 2", n)
	}
	for _, opID := range []int64{liveOp.ID, liveHostOp.ID} {
		if _, err := s.PendingSecretByOperation(opID); err != nil {
			t.Fatalf("live pending %d must survive: %v", opID, err)
		}
	}
	for _, opID := range []int64{orphanOp.ID, orphanHostOp.ID} {
		if _, err := s.PendingSecretByOperation(opID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("orphan pending %d must be removed, got %v", opID, err)
		}
	}
	// Идемпотентно: второй прогон ничего не удаляет.
	if n, err := s.ReconcileOrphanPendingSecrets(); err != nil || n != 0 {
		t.Fatalf("second reconcile: n=%d err=%v, want 0", n, err)
	}
}
