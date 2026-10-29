package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"sharedd/registry/internal/control"
)

func TestProfileDefaultsAPI(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	uid, _, cookies, csrf := cloudSession(t, r, "defaults-api")
	p, err := r.control.CreateProfile("defaults-api-profile", "", uid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.control.AddProfileVersion(p.ID, "# step\ninstall ok", `{"vars":[{"name":"ad_tag"}]}`); err != nil {
		t.Fatal(err)
	}

	rec := cloudCall(t, r, http.MethodGet, "/api/me/profile-defaults?profile_id="+itoa64(p.ID), cookies, "", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get defaults: %d %s", rec.Code, rec.Body)
	}
	var got control.UserProfileDefaults
	if err := json.Unmarshal([]byte(rec.Body), &got); err != nil {
		t.Fatal(err)
	}
	if got.Revision != 0 {
		t.Fatalf("new defaults revision = %d", got.Revision)
	}

	body := `{"profile_id":` + itoa64(p.ID) + `,"vars":{"ad_tag":"0123456789abcdef0123456789abcdef"},"expected_revision":0}`
	rec = cloudCall(t, r, http.MethodPut, "/api/me/profile-defaults", cookies, csrf, body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("put defaults: %d %s", rec.Code, rec.Body)
	}
	var saved control.UserProfileDefaults
	if err := json.Unmarshal([]byte(rec.Body), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Revision != 1 || saved.Vars["ad_tag"] != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("saved defaults: %+v", saved)
	}

	body = `{"profile_id":` + itoa64(p.ID) + `,"vars":{"proxy_secret":"secret"},"expected_revision":1}`
	rec = cloudCall(t, r, http.MethodPut, "/api/me/profile-defaults", cookies, csrf, body, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("secret defaults must be rejected: %d %s", rec.Code, rec.Body)
	}
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
