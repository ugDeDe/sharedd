package control

// Тесты cloud_resources2 + node_management (stage-05): CRUD, CAS по state,
// CAS по revision политики, owner-изоляция, secrets-запись.

import (
	"errors"
	"testing"
)

func newCloudTestStore(t *testing.T) *Store {
	t.Helper()
	return openTest(t)
}

func TestCloudResourceCRUD(t *testing.T) {
	s := newCloudTestStore(t)
	alice := mustUser(t, s, "alice-cr", 10)
	bob := mustUser(t, s, "bob-cr", 10)
	conn := mustConn(t, s, alice.ID, ProviderNodehost, "main", "sealed-1")

	r, err := s.CreateCloudResource(CloudResourceV2{
		UserID: alice.ID, ConnectionID: conn.ID, Provider: ProviderNodehost,
		Label: "pool-node", State: CloudResPlanned,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if r.ID == 0 || r.Revision != 1 || r.State != CloudResPlanned || r.SecretsEnc != "" {
		t.Fatalf("created: %+v", r)
	}

	// Валидация: неизвестный провайдер/состояние.
	if _, err := s.CreateCloudResource(CloudResourceV2{UserID: alice.ID, ConnectionID: conn.ID, Provider: "x", State: CloudResPlanned}); !errors.Is(err, ErrValidation) {
		t.Fatalf("bad provider: want ErrValidation, got %v", err)
	}

	// Чужой read → ErrNotFound; свой → ok.
	if _, err := s.CloudResourceOwned(r.ID, bob.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign read: want ErrNotFound, got %v", err)
	}
	if got, err := s.CloudResourceOwned(r.ID, alice.ID); err != nil || got.ID != r.ID {
		t.Fatalf("owned read: %v %+v", err, got)
	}

	// CAS-переходы: planned→creating, затем creating→ready с external_id+ip.
	if err := s.UpdateCloudResourceState(r.ID, CloudResPlanned, CloudResCreating, "", ""); err != nil {
		t.Fatalf("planned→creating: %v", err)
	}
	if err := s.UpdateCloudResourceState(r.ID, CloudResCreating, CloudResReady, "srv-1", "203.0.113.5"); err != nil {
		t.Fatalf("creating→ready: %v", err)
	}
	// Повтор того же перехода → ErrRevisionConflict (состояние уже ready).
	if err := s.UpdateCloudResourceState(r.ID, CloudResCreating, CloudResReady, "x", "y"); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale state: want ErrRevisionConflict, got %v", err)
	}
	// Неизвестный id → ErrNotFound.
	if err := s.UpdateCloudResourceState(99999, CloudResReady, CloudResDeleted, "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: want ErrNotFound, got %v", err)
	}

	got, err := s.CloudResourceOwned(r.ID, alice.ID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if got.ExternalID != "srv-1" || got.IP != "203.0.113.5" || got.Revision != 3 {
		t.Fatalf("after CAS: %+v", got)
	}

	// Пустые externalID/ip не затирают значения.
	if err := s.UpdateCloudResourceState(r.ID, CloudResReady, CloudResReady, "", "203.0.113.9"); err != nil {
		t.Fatalf("ready→ready ip: %v", err)
	}
	if got, _ := s.CloudResourceOwned(r.ID, alice.ID); got.ExternalID != "srv-1" || got.IP != "203.0.113.9" {
		t.Fatalf("empty-ext clobber: %+v", got)
	}

	// Secrets + list.
	if err := s.SetCloudResourceSecrets(r.ID, "sealed-blob"); err != nil {
		t.Fatalf("secrets: %v", err)
	}
	if got, _ := s.CloudResourceOwned(r.ID, alice.ID); got.SecretsEnc != "sealed-blob" {
		t.Fatalf("secrets not stored: %+v", got)
	}
	list, err := s.CloudResourcesByUser(alice.ID, false)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %+v", err, list)
	}

	// Orphan planned removal: CAS — не-planned не удаляется.
	if err := s.DeletePlannedCloudResource(r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete non-planned: want ErrNotFound, got %v", err)
	}
	p2, err := s.CreateCloudResource(CloudResourceV2{UserID: alice.ID, ConnectionID: conn.ID, Provider: ProviderNodehost, State: CloudResPlanned})
	if err != nil {
		t.Fatalf("create orphan: %v", err)
	}
	if err := s.DeletePlannedCloudResource(p2.ID); err != nil {
		t.Fatalf("delete planned: %v", err)
	}
}

func TestNodeManagementCAS(t *testing.T) {
	s := newCloudTestStore(t)
	alice := mustUser(t, s, "alice-mgmt", 10)
	bob := mustUser(t, s, "bob-mgmt", 10)

	// GET-семантика: строки нет → ErrNotFound (дефолт отдаёт сервер).
	if _, err := s.NodeManagement("pool-abc12", alice.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent: want ErrNotFound, got %v", err)
	}

	// Первая запись от виртуального дефолта (expected 1) → revision 2.
	m, err := s.PatchNodeManagement("pool-abc12", alice.ID, 1, "manual", 0, false, 0)
	if err != nil || m.Revision != 2 {
		t.Fatalf("first patch: %v %+v", err, m)
	}
	if m.Mode != "manual" || m.InputsJSON != "{}" || m.ProcessCurrentBan {
		t.Fatalf("defaults: %+v", m)
	}

	// Повторная запись: CAS по revision.
	m, err = s.PatchNodeManagement("pool-abc12", alice.ID, 2, "manual", 0, true, 0)
	if err != nil || m.Revision != 3 || !m.ProcessCurrentBan {
		t.Fatalf("second patch: %v %+v", err, m)
	}
	// Устаревший expected → ErrRevisionConflict; чужой user → ErrNotFound.
	if _, err := s.PatchNodeManagement("pool-abc12", alice.ID, 2, "manual", 0, false, 0); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale revision: want ErrRevisionConflict, got %v", err)
	}
	if _, err := s.PatchNodeManagement("pool-abc12", bob.ID, 3, "manual", 0, false, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign patch: want ErrNotFound, got %v", err)
	}
	// Чужой/неизвестный GET.
	if _, err := s.NodeManagement("pool-abc12", bob.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign get: want ErrNotFound, got %v", err)
	}

	// Невалидный mode.
	if _, err := s.PatchNodeManagement("pool-abc12", alice.ID, m.Revision, "auto", 0, false, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("bad mode: want ErrValidation, got %v", err)
	}

	// current_resource_id привязка.
	if err := s.SetNodeManagementCurrentResource("pool-abc12", alice.ID, 42); err != nil {
		t.Fatalf("set current: %v", err)
	}
	if got, err := s.NodeManagement("pool-abc12", alice.ID); err != nil || got.CurrentResourceID != 42 {
		t.Fatalf("current: %v %+v", err, got)
	}
	if err := s.SetNodeManagementCurrentResource("pool-abc12", bob.ID, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign set current: want ErrNotFound, got %v", err)
	}
}
