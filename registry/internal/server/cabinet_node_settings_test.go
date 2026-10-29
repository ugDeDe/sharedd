package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"sharedd/registry/internal/control"
)

func TestNodeSettingsRenameKeepsTechnicalIdentity(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	uid, _, cookies, csrf := cloudSession(t, r, "settings-user")
	u, err := r.control.User(uid)
	if err != nil {
		t.Fatal(err)
	}
	const nodeID = "alpha-aaaaa"
	if err := r.control.AuthorizeNode(nodeID, u.User); err != nil {
		t.Fatal(err)
	}

	rec := cloudCall(t, r, http.MethodGet, "/api/me/nodes/"+nodeID+"/settings", cookies, "", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get settings: %d %s", rec.Code, rec.Body)
	}
	var got nodeSettingsJSON
	if err := json.Unmarshal([]byte(rec.Body), &got); err != nil {
		t.Fatal(err)
	}
	if got.Name == "" || got.Number <= 0 || got.Revision <= 0 {
		t.Fatalf("settings: %+v", got)
	}
	if got.Runtime["ad_tag"].State != "unmanaged" {
		t.Fatalf("runtime ad_tag must start unmanaged, got %+v", got.Runtime)
	}

	body := `{"expected_revision":` + strconv.FormatInt(got.Revision, 10) + `,"name":"renamed"}`
	rec = cloudCall(t, r, http.MethodPatch, "/api/me/nodes/"+nodeID+"/settings", cookies, csrf, body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch settings: %d %s", rec.Code, rec.Body)
	}
	var renamed nodeSettingsJSON
	if err := json.Unmarshal([]byte(rec.Body), &renamed); err != nil {
		t.Fatal(err)
	}
	if renamed.Name != "renamed" || renamed.Revision != got.Revision+1 || renamed.Number != got.Number {
		t.Fatalf("rename result: %+v", renamed)
	}
	if _, ok, err := r.control.NodeSlotForNode(nodeID); err != nil || !ok {
		t.Fatalf("technical node lost after rename: ok=%v err=%v", ok, err)
	}

	// Stale revision is a conflict, not a silent overwrite.
	rec = cloudCall(t, r, http.MethodPatch, "/api/me/nodes/"+nodeID+"/settings", cookies, csrf, body, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale rename: %d %s", rec.Code, rec.Body)
	}

	// Runtime settings are stored as a desired revision; success is only
	// reported after the agent acknowledges it.
	body = `{"expected_revision":` + strconv.FormatInt(renamed.Revision, 10) + `,"runtime":{"ad_tag":"0123456789abcdef0123456789abcdef"}}`
	rec = cloudCall(t, r, http.MethodPatch, "/api/me/nodes/"+nodeID+"/settings", cookies, csrf, body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("runtime patch: %d %s", rec.Code, rec.Body)
	}
	var runtimeSet nodeSettingsJSON
	if err := json.Unmarshal([]byte(rec.Body), &runtimeSet); err != nil {
		t.Fatal(err)
	}
	if runtimeSet.Runtime["ad_tag"].State != "pending" || runtimeSet.Runtime["ad_tag"].DesiredRevision == 0 {
		t.Fatalf("runtime desired state: %+v", runtimeSet.Runtime)
	}

	body = `{"expected_revision":` + strconv.FormatInt(renamed.Revision, 10) + `,"runtime":{"ad_tag":"not-hex"}}`
	rec = cloudCall(t, r, http.MethodPatch, "/api/me/nodes/"+nodeID+"/settings", cookies, csrf, body, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid ad_tag must be 400: %d %s", rec.Code, rec.Body)
	}
}

func TestConfigExposesRuntimeSettingsOnlyToPersonalSecret(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	r.cfg.Security.NodeToken = testNodeToken
	uid, _, _, _ := cloudSession(t, r, "runtime-config")
	u, err := r.control.User(uid)
	if err != nil {
		t.Fatal(err)
	}
	const nodeID = "runtime-aaaaa"
	const secret = "node-personal-secret"
	if err := r.control.AuthorizeNode(nodeID, u.User); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := r.control.SetNodeSecret(nodeID, secret); err != nil || !ok {
		t.Fatalf("set node secret: ok=%v err=%v", ok, err)
	}
	if _, err := r.control.SetNodeAdTag(uid, nodeID, control.AdTagSet, "0123456789abcdef0123456789abcdef", 0); err != nil {
		t.Fatal(err)
	}
	get := func(bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/config", nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("X-ShareDD-Node-ID", nodeID)
		rec := httptest.NewRecorder()
		r.buildMux().ServeHTTP(rec, req)
		return rec
	}
	rec := get(secret)
	if rec.Code != http.StatusOK {
		t.Fatalf("personal config: %d %s", rec.Code, rec.Body)
	}
	var cfg sharedConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.NodeSettingsProtocol != 1 || cfg.NodeSettings == nil || cfg.NodeSettings.AdTag.Value != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("personal node settings: %+v", cfg.NodeSettings)
	}
	rec = get(testNodeToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("shared config: %d %s", rec.Code, rec.Body)
	}
	cfg = sharedConfigResponse{}
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.NodeSettings != nil {
		t.Fatalf("shared token must not receive node settings: %+v", cfg.NodeSettings)
	}
}

func TestNodeSettingsStatusAckFromPersonalSecret(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	r.cfg.Security.NodeToken = testNodeToken
	uid, _, _, _ := cloudSession(t, r, "runtime-ack")
	u, err := r.control.User(uid)
	if err != nil {
		t.Fatal(err)
	}
	const nodeID = "runtime-bbbbb"
	const secret = "node-personal-secret-2"
	if err := r.control.AuthorizeNode(nodeID, u.User); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := r.control.SetNodeSecret(nodeID, secret); err != nil || !ok {
		t.Fatalf("set node secret: ok=%v err=%v", ok, err)
	}
	if _, err := r.control.SetNodeAdTag(uid, nodeID, control.AdTagSet, "0123456789abcdef0123456789abcdef", 0); err != nil {
		t.Fatal(err)
	}
	body := `{"node_id":"` + nodeID + `","protocol_version":1,"capabilities":["ad_tag"],"attempted_revision":1,"applied_revision":1,"state":"applied"}`
	req := httptest.NewRequest(http.MethodPost, "/api/node/settings/status", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.buildMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status ack: %d %s", rec.Code, rec.Body)
	}
	rt, err := r.control.RuntimeSettingsForNode(nodeID)
	if err != nil || rt.State != control.RuntimeApplied || rt.AppliedRevision != 1 {
		t.Fatalf("runtime after ack: %+v err=%v", rt, err)
	}
}

func TestNodeSettingsRejectsForeignNode(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	_, _, cookies, csrf := cloudSession(t, r, "owner-a")
	uidB, _, otherCookies, otherCSRF := cloudSession(t, r, "owner-b")
	ub, err := r.control.User(uidB)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.control.AuthorizeNode("bravo-bbbbb", ub.User); err != nil {
		t.Fatal(err)
	}
	rec := cloudCall(t, r, http.MethodPatch, "/api/me/nodes/bravo-bbbbb/settings", otherCookies, otherCSRF, `{"expected_revision":1,"name":"bravo"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner patch: %d %s", rec.Code, rec.Body)
	}
	rec = cloudCall(t, r, http.MethodGet, "/api/me/nodes/bravo-bbbbb/settings", cookies, csrf, "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign node must be 404, got %d %s", rec.Code, rec.Body)
	}
}
