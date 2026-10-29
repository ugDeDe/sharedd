package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
)

func loadTemplate(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("../blueprint/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestBlueprintBlocksRoute(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	rec := cabinetCall(t, r, http.MethodGet, "/api/me/blueprint-blocks", cookieMap(sess, csrf), "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("blocks: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		SchemaVersion int `json:"schema_version"`
		Blocks        []struct {
			Type string `json:"type"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.SchemaVersion != 1 || len(out.Blocks) != 18 {
		t.Fatalf("unexpected blocks payload: %+v", out)
	}
}

func TestBlueprintValidateRouteNoSideEffects(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")

	valid := fmt.Sprintf(`{"format":"graph","blueprint":%s}`, loadTemplate(t, "nodehost_replacement.json"))
	rec := cabinetCall(t, r, http.MethodPost, "/api/me/profiles/validate", cookieMap(sess, csrf), csrf, valid)
	if rec.Code != http.StatusOK {
		t.Fatalf("validate valid: %d %s", rec.Code, rec.Body.String())
	}
	var ok struct {
		Valid       bool `json:"valid"`
		Diagnostics []struct {
			Code string `json:"code"`
		} `json:"diagnostics"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &ok)
	if !ok.Valid || len(ok.Diagnostics) != 0 {
		t.Fatalf("expected valid, got %s", rec.Body.String())
	}

	// invalid: unknown block
	bad := `{"format":"graph","blueprint":{"schema_version":1,"name":"x","provider":"nodehost","inputs":{},"entries":{"initial":"a","observe":"b"},"nodes":[{"id":"a","type":"bogus","params":{},"in":{}},{"id":"b","type":"terminal.stop","params":{"reason":"policy_inactive"},"in":{}}],"edges":[]}}`
	rec = cabinetCall(t, r, http.MethodPost, "/api/me/profiles/validate", cookieMap(sess, csrf), csrf, bad)
	if rec.Code != http.StatusOK {
		t.Fatalf("validate invalid: %d %s", rec.Code, rec.Body.String())
	}
	var badOut struct {
		Valid       bool `json:"valid"`
		Diagnostics []struct {
			Code string `json:"code"`
		} `json:"diagnostics"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &badOut)
	if badOut.Valid || len(badOut.Diagnostics) == 0 {
		t.Fatalf("expected invalid diagnostics, got %s", rec.Body.String())
	}
	// no side effects
	profiles, err := r.control.ProfilesByOwner(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 0 {
		t.Fatalf("validate must not create profiles, got %d", len(profiles))
	}
}

func TestGraphProfileCreateVersionAndOwnership(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	cabinetUser(t, r, "bob", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")

	body := fmt.Sprintf(`{"name":"alice-graph","format":"graph","blueprint":%s}`, loadTemplate(t, "beget_ip_or_replacement.json"))
	rec := cabinetCall(t, r, http.MethodPost, "/api/me/profiles", cookieMap(sess, csrf), csrf, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create graph profile: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.ID == 0 {
		t.Fatal("no profile id")
	}

	rec = cabinetCall(t, r, http.MethodGet, fmt.Sprintf("/api/me/profiles/%d/versions/1", created.ID), cookieMap(sess, csrf), "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get version: %d %s", rec.Code, rec.Body.String())
	}
	var v struct {
		ProfileID        int64           `json:"profile_id"`
		Version          int             `json:"version"`
		ProfileVersionID int64           `json:"profile_version_id"`
		Format           string          `json:"format"`
		Blueprint        json.RawMessage `json:"blueprint"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Format != "graph" || v.Version != 1 || v.ProfileVersionID == 0 || len(v.Blueprint) == 0 {
		t.Fatalf("unexpected version DTO: %s", rec.Body.String())
	}

	// the version segment is ignored: any value returns the current row.
	rec = cabinetCall(t, r, http.MethodGet, fmt.Sprintf("/api/me/profiles/%d/versions/latest", created.ID), cookieMap(sess, csrf), "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("version segment must be ignored, got %d %s", rec.Code, rec.Body.String())
	}
	// bob cannot read alice's unpublished version
	bsess, bcsrf := cabinetLogin(t, r, "bob", "password-123")
	rec = cabinetCall(t, r, http.MethodGet, fmt.Sprintf("/api/me/profiles/%d/versions/1", created.ID), cookieMap(bsess, bcsrf), "", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign version must 404, got %d", rec.Code)
	}
}

func TestGraphProfileRejectsConflictAndInvalid(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")

	conflict := fmt.Sprintf(`{"name":"c","steps":"# s\necho x\n","format":"graph","blueprint":%s}`, loadTemplate(t, "nodehost_replacement.json"))
	rec := cabinetCall(t, r, http.MethodPost, "/api/me/profiles", cookieMap(sess, csrf), csrf, conflict)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("conflict: %d %s", rec.Code, rec.Body.String())
	}

	invalid := `{"name":"x","format":"graph","blueprint":{"schema_version":1,"name":"x","provider":"nodehost","inputs":{},"entries":{"initial":"a","observe":"b"},"nodes":[{"id":"a","type":"bogus","params":{},"in":{}},{"id":"b","type":"terminal.stop","params":{"reason":"policy_inactive"},"in":{}}],"edges":[]}}`
	rec = cabinetCall(t, r, http.MethodPost, "/api/me/profiles", cookieMap(sess, csrf), csrf, invalid)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid graph: %d %s", rec.Code, rec.Body.String())
	}
	// the failed create must not leave a profile behind
	if profiles, _ := r.control.ProfilesByOwner(1); len(profiles) != 0 {
		t.Fatalf("invalid graph left %d profiles", len(profiles))
	}
}

// F1: expected_version is accepted but ignored; content is overwritten in place.
func TestGraphProfileUpdateIgnoresExpectedVersion(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")

	create := fmt.Sprintf(`{"name":"g","format":"graph","blueprint":%s}`, loadTemplate(t, "nodehost_replacement.json"))
	rec := cabinetCall(t, r, http.MethodPost, "/api/me/profiles", cookieMap(sess, csrf), csrf, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	path := fmt.Sprintf("/api/me/profiles/%d", created.ID)

	put := fmt.Sprintf(`{"format":"graph","expected_version":1,"blueprint":%s}`, loadTemplate(t, "nodehost_replacement.json"))
	rec = cabinetCall(t, r, http.MethodPut, path, cookieMap(sess, csrf), csrf, put)
	if rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	// Stale expected_version must not 409: the same single row is updated.
	rec = cabinetCall(t, r, http.MethodPut, path, cookieMap(sess, csrf), csrf, put)
	if rec.Code != http.StatusOK {
		t.Fatalf("second put: %d %s", rec.Code, rec.Body.String())
	}
	if n, _ := r.control.CountProfileVersions(created.ID); n != 1 {
		t.Fatalf("version count = %d, want 1", n)
	}
}

// F2: strict envelope decode rejects unknown fields (was silently accepted).
func TestProfileEnvelopeStrictDecode(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")

	rec := cabinetCall(t, r, http.MethodPost, "/api/me/profiles", cookieMap(sess, csrf), csrf,
		`{"name":"x","steps":"# s\necho hi\n","bogus":1}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d %s", rec.Code, rec.Body.String())
	}
	// typo'd format key must be rejected, not fall through to the text path
	rec = cabinetCall(t, r, http.MethodPost, "/api/me/profiles", cookieMap(sess, csrf), csrf,
		fmt.Sprintf(`{"name":"x","blueprnt":%s}`, loadTemplate(t, "nodehost_replacement.json")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown key: %d %s", rec.Code, rec.Body.String())
	}
}

// F3: an invalid graph PUT must not mutate the name or append a version.
func TestGraphProfileUpdateInvalidNoPartialMutation(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")

	create := fmt.Sprintf(`{"name":"orig","format":"graph","blueprint":%s}`, loadTemplate(t, "nodehost_replacement.json"))
	rec := cabinetCall(t, r, http.MethodPost, "/api/me/profiles", cookieMap(sess, csrf), csrf, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	invalid := `{"schema_version":1,"name":"x","provider":"nodehost","inputs":{},"entries":{"initial":"a","observe":"b"},"nodes":[{"id":"a","type":"bogus","params":{},"in":{}},{"id":"b","type":"terminal.stop","params":{"reason":"policy_inactive"},"in":{}}],"edges":[]}`
	put := fmt.Sprintf(`{"name":"renamed","format":"graph","blueprint":%s}`, invalid)
	rec = cabinetCall(t, r, http.MethodPut, fmt.Sprintf("/api/me/profiles/%d", created.ID), cookieMap(sess, csrf), csrf, put)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid graph put: %d %s", rec.Code, rec.Body.String())
	}
	p, err := r.control.Profile(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "orig" {
		t.Fatalf("name mutated on 422: %q", p.Name)
	}
	if n, _ := r.control.CountProfileVersions(created.ID); n != 1 {
		t.Fatalf("version count = %d, want 1", n)
	}
}

// Text PUT path must be unchanged by the graph CAS work.
func TestTextProfileUpdateUnchanged(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")

	rec := cabinetCall(t, r, http.MethodPost, "/api/me/profiles", cookieMap(sess, csrf), csrf,
		jsonBody(t, map[string]any{"name": "t", "steps": "# s\necho one\n"}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	rec = cabinetCall(t, r, http.MethodPut, fmt.Sprintf("/api/me/profiles/%d", created.ID), cookieMap(sess, csrf), csrf,
		jsonBody(t, map[string]any{"steps": "# s\necho two\n"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("text put: %d %s", rec.Code, rec.Body.String())
	}
	v, err := r.control.ProfileVersion(created.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if v.Format != "text" || v.Version != 1 || v.Steps != "# s\necho two\n" {
		t.Fatalf("text version = %+v", v)
	}
}

func TestTextProfileStillWorksAndVersionRoute(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	rec := cabinetCall(t, r, http.MethodPost, "/api/me/profiles", cookieMap(sess, csrf), csrf,
		jsonBody(t, map[string]any{"name": "t", "steps": "# s\necho hi\n"}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("text create: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	rec = cabinetCall(t, r, http.MethodGet, fmt.Sprintf("/api/me/profiles/%d/versions/1", created.ID), cookieMap(sess, csrf), "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("text version: %d %s", rec.Code, rec.Body.String())
	}
	var v struct {
		Format string `json:"format"`
		Steps  string `json:"steps"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if v.Format != "text" || v.Steps == "" {
		t.Fatalf("unexpected text version: %s", rec.Body.String())
	}
}
