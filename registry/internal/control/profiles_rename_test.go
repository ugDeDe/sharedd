package control

import (
	"errors"
	"testing"
)

// RenameProfile — метаданные: не создаёт версию, не сбрасывает published;
// имя тримится, коллизия без учёта регистра → ErrDuplicateName.
func TestRenameProfile(t *testing.T) {
	s := openTest(t)
	p, err := s.CreateProfile("alpha", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddProfileVersion(p.ID, "# s\necho hi\n", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishProfile(p.ID); err != nil {
		t.Fatal(err)
	}

	renamed, err := s.RenameProfile(p.ID, "  beta  ")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if renamed.Name != "beta" {
		t.Fatalf("name = %q, want trimmed beta", renamed.Name)
	}
	if !renamed.Published {
		t.Fatal("rename must not unpublish")
	}
	if n, _ := s.CountProfileVersions(p.ID); n != 1 {
		t.Fatalf("versions = %d, want 1 (rename is metadata, not a version)", n)
	}

	// коллизия без учёта регистра.
	if _, err := s.CreateProfile("Gamma", "", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenameProfile(p.ID, "GAMMA"); !errors.Is(err, ErrDuplicateName) {
		t.Fatalf("case-insensitive collision = %v, want ErrDuplicateName", err)
	}
	// пустое/пробельное имя.
	if _, err := s.RenameProfile(p.ID, "   "); !errors.Is(err, ErrValidation) {
		t.Fatalf("blank name = %v, want ErrValidation", err)
	}
	// профиля нет.
	if _, err := s.RenameProfile(99999, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing profile = %v, want ErrNotFound", err)
	}
}

// Совпадение с текущим именем — no-op: updated_at не трогаем.
func TestRenameProfileUnchangedIsNoop(t *testing.T) {
	s := openTest(t)
	p, err := s.CreateProfile("same", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.sql.Exec(`UPDATE profiles SET updated_at = 1 WHERE id = ?`, p.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.RenameProfile(p.ID, "same")
	if err != nil {
		t.Fatal(err)
	}
	if got.UpdatedAt != 1 {
		t.Fatalf("no-op rename touched updated_at: %d", got.UpdatedAt)
	}
}
