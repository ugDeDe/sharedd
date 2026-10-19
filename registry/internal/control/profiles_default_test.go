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
