package control

// Тесты hosting-connections (stage-04): CRUD с owner-проверками, CAS ревизий,
// балансный upsert с CAS по credential_revision, миграция (повторный Open).

import (
	"errors"
	"path/filepath"
	"testing"
)

func mustConn(t *testing.T, s *Store, userID int64, provider, label, creds string) HostingConnection {
	t.Helper()
	c, err := s.CreateHostingConnection(HostingConnection{
		UserID: userID, Provider: provider, Label: label, CredentialsEnc: creds,
	})
	if err != nil {
		t.Fatalf("create hosting connection: %v", err)
	}
	return c
}

func TestHostingConnectionCRUD(t *testing.T) {
	s := openTest(t)
	alice := mustUser(t, s, "alice", 10)
	bob := mustUser(t, s, "bob", 10)

	c := mustConn(t, s, alice.ID, ProviderNodehost, "main", "sealed-1")
	if c.ID == 0 || c.Revision != 1 || c.CredentialRevision != 1 || c.VerifiedAt != 0 {
		t.Fatalf("unexpected create result: %+v", c)
	}

	// Чужой id и неизвестный id — единый ErrNotFound.
	if _, err := s.HostingConnectionOwned(c.ID, bob.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign read: want ErrNotFound, got %v", err)
	}
	if _, err := s.HostingConnectionOwned(99999, alice.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown read: want ErrNotFound, got %v", err)
	}

	// Валидация: провайдер и длина label.
	if _, err := s.CreateHostingConnection(HostingConnection{UserID: alice.ID, Provider: "aws", Label: "x"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("bad provider: want ErrValidation, got %v", err)
	}
	if _, err := s.CreateHostingConnection(HostingConnection{UserID: alice.ID, Provider: ProviderBeget, Label: ""}); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty label: want ErrValidation, got %v", err)
	}
	long := make([]rune, 101)
	for i := range long {
		long[i] = 'а'
	}
	if _, err := s.CreateHostingConnection(HostingConnection{UserID: alice.ID, Provider: ProviderBeget, Label: string(long)}); !errors.Is(err, ErrValidation) {
		t.Fatalf("long label: want ErrValidation, got %v", err)
	}

	// Patch только label: revision +1, credential_revision на месте.
	newLabel := "renamed"
	got, err := s.PatchHostingConnection(c.ID, alice.ID, 1, &newLabel, nil)
	if err != nil {
		t.Fatalf("patch label: %v", err)
	}
	if got.Label != "renamed" || got.Revision != 2 || got.CredentialRevision != 1 {
		t.Fatalf("patch label mismatch: %+v", got)
	}

	// Patch credentials: полная замена, credential_revision +1.
	newCreds := "sealed-2"
	got, err = s.PatchHostingConnection(c.ID, alice.ID, 2, nil, &newCreds)
	if err != nil {
		t.Fatalf("patch creds: %v", err)
	}
	if got.CredentialsEnc != "sealed-2" || got.CredentialRevision != 2 || got.Revision != 3 {
		t.Fatalf("patch creds mismatch: %+v", got)
	}

	// CAS: устаревший expected_revision → ErrRevisionConflict; чужой user → ErrNotFound.
	if _, err := s.PatchHostingConnection(c.ID, alice.ID, 1, &newLabel, nil); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale revision: want ErrRevisionConflict, got %v", err)
	}
	if _, err := s.PatchHostingConnection(c.ID, bob.ID, 3, &newLabel, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign patch: want ErrNotFound, got %v", err)
	}

	// Verified + capabilities: CAS по credential_revision (устаревшая ревизия
	// → ErrRevisionConflict, чужой user → ErrNotFound, актуальная → успех).
	if err := s.UpdateHostingVerified(c.ID, alice.ID, 1, 12345, `{"create":true}`); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale credential_revision: want ErrRevisionConflict, got %v", err)
	}
	if err := s.UpdateHostingVerified(c.ID, bob.ID, got.CredentialRevision, 12345, `{"create":true}`); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign verify: want ErrNotFound, got %v", err)
	}
	if err := s.UpdateHostingVerified(c.ID, alice.ID, got.CredentialRevision, 12345, `{"create":true}`); err != nil {
		t.Fatalf("verify: %v", err)
	}
	got, err = s.HostingConnectionOwned(c.ID, alice.ID)
	if err != nil || got.VerifiedAt != 12345 || got.CapabilitiesJSON != `{"create":true}` {
		t.Fatalf("verified state: %+v err=%v", got, err)
	}

	// List только свои.
	_ = mustConn(t, s, bob.ID, ProviderBeget, "bob-beget", "sealed-3")
	list, err := s.HostingConnectionsByUser(alice.ID)
	if err != nil || len(list) != 1 || list[0].ID != c.ID {
		t.Fatalf("list: %+v err=%v", list, err)
	}

	// Delete: свой 204-семантика, повторный/чужой — ErrNotFound.
	if err := s.DeleteHostingConnection(c.ID, alice.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.DeleteHostingConnection(c.ID, alice.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete: want ErrNotFound, got %v", err)
	}
	if err := s.DeleteHostingConnection(c.ID, bob.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign delete: want ErrNotFound, got %v", err)
	}
}

func TestHostingBalanceCAS(t *testing.T) {
	s := openTest(t)
	alice := mustUser(t, s, "alice", 10)
	c := mustConn(t, s, alice.ID, ProviderNodehost, "main", "sealed-1")

	if _, err := s.HostingBalance(c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no snapshot yet: want ErrNotFound, got %v", err)
	}

	// Первая запись (успешное снятие) — применена.
	ok, err := s.SaveHostingBalance(ConnectionBalance{
		ConnectionID: c.ID, UserID: alice.ID, CredentialRevision: 1,
		Amount: "42.50", Currency: "USD", Scope: "nodehost_api",
		CheckedAt: 100, LastAttemptAt: 100,
	})
	if err != nil || !ok {
		t.Fatalf("first save: ok=%v err=%v", ok, err)
	}

	// Неудачная попытка: вызывающий слой переносит последние удачные поля
	// (amount/checked_at) вперёд, control тупо заменяет строку целиком.
	ok, err = s.SaveHostingBalance(ConnectionBalance{
		ConnectionID: c.ID, UserID: alice.ID, CredentialRevision: 1,
		Amount: "42.50", Currency: "USD", Scope: "nodehost_api",
		CheckedAt: 100, LastAttemptAt: 200, SafeError: "provider_error",
	})
	if err != nil || !ok {
		t.Fatalf("failed attempt save: ok=%v err=%v", ok, err)
	}
	b, err := s.HostingBalance(c.ID)
	if err != nil || b.Amount != "42.50" || b.CheckedAt != 100 || b.LastAttemptAt != 200 || b.SafeError != "provider_error" {
		t.Fatalf("snapshot after failed attempt: %+v err=%v", b, err)
	}

	// CAS: снимок старой винтажности (после ротации кредов) отбрасывается.
	// Сначала применяем снимок новой винтажности credential_revision=2.
	newCreds := "sealed-2"
	if _, err := s.PatchHostingConnection(c.ID, alice.ID, 1, nil, &newCreds); err != nil {
		t.Fatalf("patch creds: %v", err)
	}
	ok, err = s.SaveHostingBalance(ConnectionBalance{
		ConnectionID: c.ID, UserID: alice.ID, CredentialRevision: 2,
		Amount: "7.00", Currency: "USD", Scope: "nodehost_api",
		CheckedAt: 400, LastAttemptAt: 400,
	})
	if err != nil || !ok {
		t.Fatalf("post-rotation save: ok=%v err=%v", ok, err)
	}
	// Странствующий in-flight запрос со старыми кредами (rev 1) не проходит.
	ok, err = s.SaveHostingBalance(ConnectionBalance{
		ConnectionID: c.ID, UserID: alice.ID, CredentialRevision: 1,
		Amount: "1.00", CheckedAt: 300, LastAttemptAt: 300,
	})
	if err != nil {
		t.Fatalf("stale save transport: %v", err)
	}
	if ok {
		t.Fatal("stale credential_revision must not be applied")
	}
	if b, _ := s.HostingBalance(c.ID); b.Amount != "7.00" || b.CredentialRevision != 2 {
		t.Fatalf("snapshot mutated by stale write: %+v", b)
	}

	// «Успешный» пустой снимок — не записывается (страховка).
	if ok, err := s.SaveHostingBalance(ConnectionBalance{
		ConnectionID: c.ID, UserID: alice.ID, CredentialRevision: 2,
		CheckedAt: 500, LastAttemptAt: 500,
	}); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty successful snapshot: want ErrValidation, got ok=%v err=%v", ok, err)
	}

	// Удаление подключения снимает и баланс.
	if err := s.DeleteHostingConnection(c.ID, alice.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.HostingBalance(c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("balance after delete: want ErrNotFound, got %v", err)
	}
}

// Миграция: повторный Open по существующей БД не ломает схему и данные.
func TestHostingOpenTwice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	s, err := Open(path, 100000)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	alice, err := s.CreateUser("alice", "password-123", "user", 10)
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	c, err := s.CreateHostingConnection(HostingConnection{
		UserID: alice.ID, Provider: ProviderBeget, Label: "cloud", CredentialsEnc: "sealed",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.SaveHostingBalance(ConnectionBalance{
		ConnectionID: c.ID, UserID: alice.ID, CredentialRevision: 1,
		Amount: "5.00", CheckedAt: 10, LastAttemptAt: 10,
	}); err != nil {
		t.Fatalf("balance: %v", err)
	}
	s.Close()

	s2, err := Open(path, 100000)
	if err != nil {
		t.Fatalf("open 2 (migration over existing data): %v", err)
	}
	defer s2.Close()
	got, err := s2.HostingConnectionOwned(c.ID, alice.ID)
	if err != nil || got.Provider != ProviderBeget || got.CredentialsEnc != "sealed" {
		t.Fatalf("reopen: %+v err=%v", got, err)
	}
	if b, err := s2.HostingBalance(c.ID); err != nil || b.Amount != "5.00" {
		t.Fatalf("reopen balance: %+v err=%v", b, err)
	}
}
