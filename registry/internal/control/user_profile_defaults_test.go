package control

import (
	"errors"
	"testing"
)

func TestUserProfileDefaultsRevisionAndSecrets(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "defaults-user", 5)
	p, err := s.CreateProfile("defaults-profile", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetUserProfileDefaults(u.ID, p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty defaults: want ErrNotFound, got %v", err)
	}
	d, err := s.SetUserProfileDefaults(u.ID, p.ID, map[string]string{
		"ad_tag": "0123456789abcdef0123456789abcdef",
	}, 0)
	if err != nil || d.Revision != 1 || d.Vars["ad_tag"] == "" {
		t.Fatalf("create defaults: %+v err=%v", d, err)
	}
	d2, err := s.SetUserProfileDefaults(u.ID, p.ID, map[string]string{"ad_tag": ""}, d.Revision)
	if err != nil || d2.Revision != 2 {
		t.Fatalf("update defaults: %+v err=%v", d2, err)
	}
	if _, err := s.SetUserProfileDefaults(u.ID, p.ID, map[string]string{"ad_tag": "x"}, d.Revision); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale revision: want conflict, got %v", err)
	}
	if _, err := s.SetUserProfileDefaults(u.ID, p.ID, map[string]string{"proxy_secret": "secret"}, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("secret defaults must be rejected, got %v", err)
	}
}
