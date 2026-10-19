package server

// Reconcile-эндпоинт (POST /api/me/cloud-resources/{id}/reconcile): read-only
// сверка с nodehost-инвентарём для ресурсов, застрявших в error/planned с
// provider_reconcile_required. Пины: absent → resolved deleted; unaccounted →
// 409 без мутаций; свой external_id в инвентаре → ready-link; beget → 501;
// чужой ресурс → 404.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"sharedd/registry/internal/control"
	"sharedd/registry/internal/nodehost"
)

// cloudSeedStuck — застрявший ресурс (error/planned) напрямую через store.
func cloudSeedStuck(t *testing.T, r *Registry, uid, connID int64, provider, state, externalID string) control.CloudResourceV2 {
	t.Helper()
	res, err := r.control.CreateCloudResource(control.CloudResourceV2{
		UserID: uid, ConnectionID: connID, Provider: provider,
		Label: "pool-node", State: state, ExternalID: externalID,
	})
	if err != nil {
		t.Fatalf("seed resource: %v", err)
	}
	return res
}

func cloudReconcile(t *testing.T, r *Registry, cookies map[string]string, csrf string, id int64) *httptestResponse {
	t.Helper()
	return cloudCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/cloud-resources/%d/reconcile", id),
		cookies, csrf, "", nil)
}

// absent: застрявший error-ресурс без external_id, инвентарь пуст → ресурс
// CAS-ом уходит в deleted.
func TestCloudReconcileAbsentResolvesDeleted(t *testing.T) {
	r, fake := newCloudRegistry(t)
	uid, connID, cookies, csrf := cloudSession(t, r, "reconcile-absent")
	fake.inventory = nil
	res := cloudSeedStuck(t, r, uid, connID, control.ProviderNodehost, control.CloudResError, "")

	rec := cloudReconcile(t, r, cookies, csrf, res.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("reconcile: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Resolved string `json:"resolved"`
	}
	if err := json.Unmarshal([]byte(rec.Body), &out); err != nil || out.Resolved != "absent" {
		t.Fatalf("body: %v %s", err, rec.Body)
	}
	got, err := r.control.CloudResourceOwned(res.ID, uid)
	if err != nil || got.State != control.CloudResDeleted {
		t.Fatalf("state after absent: %+v %v", got, err)
	}
}

// unaccounted: пустой external_id, но в инвентаре есть сервер, не привязанный
// ни к одному ресурсу → 409 provider_reconcile_required, мутаций нет.
func TestCloudReconcileUnaccountedConflict(t *testing.T) {
	r, fake := newCloudRegistry(t)
	uid, connID, cookies, csrf := cloudSession(t, r, "reconcile-ambig")
	fake.inventory = []nodehost.Server{{ID: "srv-other", Status: "ACTIVE", IP: "203.0.113.77"}}
	res := cloudSeedStuck(t, r, uid, connID, control.ProviderNodehost, control.CloudResError, "")

	rec := cloudReconcile(t, r, cookies, csrf, res.ID)
	if rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body, "provider_reconcile_required") ||
		!strings.Contains(rec.Body, "1 unaccounted servers in provider inventory") {
		t.Fatalf("reconcile: %d %s", rec.Code, rec.Body)
	}
	got, err := r.control.CloudResourceOwned(res.ID, uid)
	if err != nil || got.State != control.CloudResError {
		t.Fatalf("409 must not mutate: %+v %v", got, err)
	}
}

// found: ресурс застрял в error, но external_id указывает на живой инстанс →
// CAS до ready с IP инвентаря, last_error очищен.
func TestCloudReconcileFoundLinksExternal(t *testing.T) {
	r, fake := newCloudRegistry(t)
	uid, connID, cookies, csrf := cloudSession(t, r, "reconcile-found")
	fake.inventory = []nodehost.Server{{ID: "srv-ext-1", Status: "ACTIVE", IP: "198.51.100.5"}}
	res := cloudSeedStuck(t, r, uid, connID, control.ProviderNodehost, control.CloudResError, "srv-ext-1")

	rec := cloudReconcile(t, r, cookies, csrf, res.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("reconcile: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Resolved   string `json:"resolved"`
		ExternalID string `json:"external_id"`
	}
	if err := json.Unmarshal([]byte(rec.Body), &out); err != nil || out.Resolved != "found" || out.ExternalID != "srv-ext-1" {
		t.Fatalf("body: %v %s", err, rec.Body)
	}
	got, err := r.control.CloudResourceOwned(res.ID, uid)
	if err != nil || got.State != control.CloudResReady || got.IP != "198.51.100.5" || got.LastError != "" {
		t.Fatalf("found must link ready: %+v %v", got, err)
	}
}

// beget → 501 not_implemented (решает provider подключения, не ресурса).
func TestCloudReconcileBegetNotImplemented(t *testing.T) {
	r, _ := newCloudRegistry(t)
	uid, _, cookies, csrf := cloudSession(t, r, "reconcile-beget")
	rec := cloudCall(t, r, http.MethodPost, "/api/me/hosting-connections", cookies, csrf,
		`{"provider":"beget","label":"b","credentials":{"cloud_login":"l","cloud_password":"p"}}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("beget conn: %d %s", rec.Code, rec.Body)
	}
	var c struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(rec.Body), &c); err != nil || c.ID == 0 {
		t.Fatalf("beget conn decode: %v %s", err, rec.Body)
	}
	res := cloudSeedStuck(t, r, uid, c.ID, control.ProviderBeget, control.CloudResError, "")

	rec = cloudReconcile(t, r, cookies, csrf, res.ID)
	if rec.Code != http.StatusNotImplemented || !strings.Contains(rec.Body, "not_implemented") {
		t.Fatalf("reconcile: %d %s", rec.Code, rec.Body)
	}
}

// чужой ресурс → 404 (владелец-изоляция).
func TestCloudReconcileForeignNotFound(t *testing.T) {
	r, _ := newCloudRegistry(t)
	_, _, cookiesA, csrfA := cloudSession(t, r, "reconcile-owner")
	uidB, connIDB, cookiesB, csrfB := cloudSession(t, r, "reconcile-foreign")
	foreign := cloudSeedStuck(t, r, uidB, connIDB, control.ProviderNodehost, control.CloudResError, "")

	if rec := cloudReconcile(t, r, cookiesA, csrfA, foreign.ID); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign: %d %s", rec.Code, rec.Body)
	}
	if rec := cloudReconcile(t, r, cookiesB, csrfB, foreign.ID); rec.Code != http.StatusOK {
		t.Fatalf("owner control: %d %s", rec.Code, rec.Body)
	}
}
