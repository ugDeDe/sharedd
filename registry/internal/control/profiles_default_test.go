package control

import (
	"strings"
	"testing"
)

// Legacy default content is upgraded in place: one row, MTProxyL steps, and the
// profile stays published (there are no versions to append any more).
func TestEnsureDefaultProfileUpgradesLegacyInPlace(t *testing.T) {
	s := openTest(t)
	p, err := s.CreateProfile(DefaultProfileName, legacyDefaultDescription, 0)
	if err != nil {
		t.Fatal(err)
	}
	legacy := "# " + legacyDefaultMarker + "\necho old\n"
	if _, err := s.AddProfileVersion(p.ID, legacy, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishProfile(p.ID); err != nil {
		t.Fatal(err)
	}

	got, err := s.EnsureDefaultProfile()
	if err != nil {
		t.Fatal(err)
	}
	if !got.Published || got.Description != DefaultProfileDescription {
		t.Fatalf("default profile not upgraded: %+v", got)
	}
	if n, _ := s.CountProfileVersions(p.ID); n != 1 {
		t.Fatalf("versions = %d, want 1", n)
	}
	v, err := s.ProfileVersion(p.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if v.Version != 1 || v.Steps != DefaultProfileSteps {
		t.Fatalf("latest row = version %d, upgraded=%v", v.Version, v.Steps == DefaultProfileSteps)
	}
	if !strings.Contains(v.Steps, "MTProxyL") {
		t.Fatalf("steps are not MTProxyL: %q", v.Steps)
	}
}

func TestEnsureMtddProfileIsCreatedOnceAndPublished(t *testing.T) {
	s := openTest(t)
	p, err := s.EnsureMtddProfile()
	if err != nil {
		t.Fatal(err)
	}
	if !p.Published || p.Name != MtddProfileName {
		t.Fatalf("mtdd profile not published: %+v", p)
	}
	v, err := s.ProfileVersion(p.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if v.Steps != MtddProfileSteps || !strings.Contains(v.Steps, "ugDeDe/mtdd") {
		t.Fatalf("unexpected steps: %q", v.Steps)
	}
	// Схема переменных валидна и подставляет значения по умолчанию.
	vars, err := MergeProfileVars(v.VarsSchema, map[string]any{"ad_tag": strings.Repeat("a", 32)})
	if err != nil {
		t.Fatal(err)
	}
	if vars["mtdd_channel"] != "main" || vars["sharedd_channel"] != "dev" || vars["proxy_port"] != "443" {
		t.Fatalf("defaults missing: %v", vars)
	}
	// Повторный вызов идемпотентен и не перезаписывает правки админа.
	if _, err := s.AddProfileVersion(p.ID, "echo custom\n", MtddProfileVarsSchema); err != nil {
		t.Fatal(err)
	}
	again, err := s.EnsureMtddProfile()
	if err != nil || again.ID != p.ID {
		t.Fatalf("second ensure: %+v err=%v", again, err)
	}
	if v2, _ := s.ProfileVersion(p.ID, 0); v2.Steps != "echo custom\n" {
		t.Fatalf("admin edit was overwritten: %q", v2.Steps)
	}
}
