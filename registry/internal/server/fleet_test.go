package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"sharedd/registry/internal/blueprint"
	"sharedd/registry/internal/control"
	"sharedd/registry/internal/nodehost"
)

func fleetBody(connID int64, count int, auto bool, profileID int64) string {
	return fmt.Sprintf(`{"connection_id":%d,"name":"base","count":%d,"configuration":{"name":"pool-node","cpu":"AMD Ryzen 9 7950X","cores":2,"ram":4,"disk":40,"diskType":"SSD NVMe","os":"ubuntu-24.04","location":"DE","period":"1m"},"installation_profile_id":%d,"auto_replace":%t}`,
		connID, count, profileID, auto)
}

func fleetPost(t *testing.T, r *Registry, cookies map[string]string, csrf, body, key string) *httptestResponse {
	t.Helper()
	return cloudCall(t, r, http.MethodPost, "/api/me/fleets", cookies, csrf, body,
		map[string]string{"Idempotency-Key": key})
}

func waitFleetTerminal(t *testing.T, r *Registry, id int64) control.Fleet {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		f, err := r.control.Fleet(id)
		if err != nil {
			t.Fatalf("fleet %d: %v", id, err)
		}
		if f.State != control.FleetRunning {
			return f
		}
		time.Sleep(2 * time.Millisecond)
	}
	f, _ := r.control.Fleet(id)
	t.Fatalf("fleet %d never finished: %+v", id, f)
	return f
}

func fleetProfileVersion(t *testing.T, r *Registry) control.ProfileVersion {
	t.Helper()
	p, err := r.control.ProfileByName(control.DefaultProfileName)
	if err != nil {
		t.Fatal(err)
	}
	pv, err := r.control.ProfileVersion(p.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	return pv
}

// multiCloud is a deterministic provider that returns a distinct operation
// and server id per Create (the shared fakeCloud returns the same id, which
// violates the per-connection unique external_id invariant after item 1).
type multiCloud struct {
	mu           sync.Mutex
	creates      int
	ops          map[string]string
	servers      map[string]nodehost.Server
	failCreateAt int
}

func newMultiCloud() *multiCloud {
	return &multiCloud{ops: map[string]string{}, servers: map[string]nodehost.Server{}}
}

func (m *multiCloud) Create(_ context.Context, _ nodehost.CreateRequest, _ string) (*nodehost.CreateResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.creates++
	if m.failCreateAt > 0 && m.creates == m.failCreateAt {
		return nil, &nodehost.Error{Code: "provider_error"}
	}
	opID := fmt.Sprintf("op-%d", m.creates)
	srvID := fmt.Sprintf("srv-%d", m.creates)
	m.ops[opID] = srvID
	m.servers[srvID] = nodehost.Server{ID: srvID, Status: "ACTIVE", IP: fmt.Sprintf("203.0.113.%d", 10+m.creates), Password: "root-pw-secret-123"}
	return &nodehost.CreateResult{OperationID: opID, Status: "pending"}, nil
}

func (m *multiCloud) Operation(_ context.Context, id string) (*nodehost.Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	srv, ok := m.ops[id]
	if !ok {
		return &nodehost.Operation{ID: id, Status: "failed"}, nil
	}
	return &nodehost.Operation{ID: id, Status: "succeeded", ResourceID: srv}, nil
}

func (m *multiCloud) Server(_ context.Context, id string) (*nodehost.Server, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.servers[id]
	if !ok {
		return nil, &nodehost.Error{Code: "not_found", HTTPStatus: http.StatusNotFound}
	}
	return &s, nil
}

func (m *multiCloud) Delete(context.Context, string) (*nodehost.DeleteResult, error) {
	return &nodehost.DeleteResult{}, nil
}

func (m *multiCloud) ChangeIP(context.Context, string, int, bool) (*nodehost.ChangeIPResult, error) {
	return &nodehost.ChangeIPResult{}, nil
}

func (m *multiCloud) ServersPage(context.Context, string, int) (*nodehost.ServerPage, error) {
	return &nodehost.ServerPage{}, nil
}

func (m *multiCloud) createCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.creates
}

func TestFleetHappyPathAutoReplaceAndIdempotency(t *testing.T) {
	r, _ := newCloudRegistry(t)
	prov := newMultiCloud()
	r.cloudFactory = func(control.HostingConnection) (cloudProvider, error) { return prov, nil }
	r.cfg.Security.NodeToken = testNodeToken
	r.seedSystemGraphs()
	r.sshExec.(*fakeExec).runFn = verifyExecFn(r, true)
	uid, connID, cookies, csrf := cloudSession(t, r, "fleet-happy")
	pv := fleetProfileVersion(t, r)

	rec := fleetPost(t, r, cookies, csrf, fleetBody(connID, 2, true, pv.ProfileID), "fleet-key-1")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("fleet create: %d %s", rec.Code, rec.Body)
	}
	var ack struct {
		FleetID int64 `json:"fleet_id"`
	}
	if err := json.Unmarshal([]byte(rec.Body), &ack); err != nil || ack.FleetID == 0 {
		t.Fatalf("ack: %v %s", err, rec.Body)
	}
	f := waitFleetTerminal(t, r, ack.FleetID)
	if f.State != control.FleetSucceeded {
		items, _ := r.control.FleetItems(f.ID)
		t.Fatalf("fleet state=%s items=%+v", f.State, items)
	}
	items, err := r.control.FleetItems(f.ID)
	if err != nil || len(items) != 2 {
		t.Fatalf("items: %v %+v", err, items)
	}
	graphID, err := r.control.SystemGraphVersionID(blueprint.SystemNodehostReplacement)
	if err != nil {
		t.Fatalf("system graph: %v", err)
	}
	for _, it := range items {
		if it.State != control.FleetItemAdmitted || it.NodeID == "" || it.ResourceID == 0 {
			t.Fatalf("item not admitted: %+v", it)
		}
		res, err := r.control.CloudResourceOwned(it.ResourceID, uid)
		if err != nil || res.NodeID != it.NodeID {
			t.Fatalf("resource binding: %v %+v", err, res)
		}
		m, err := r.control.NodeManagement(it.NodeID, uid)
		if err != nil || m.Mode != "profile" || m.ProfileVersionID != graphID || m.ProcessCurrentBan {
			t.Fatalf("management policy: %v %+v", err, m)
		}
		if !strings.Contains(m.InputsJSON, `"configuration_ref"`) {
			t.Fatalf("policy inputs: %s", m.InputsJSON)
		}
	}

	// Replay of the same Idempotency-Key: same fleet, no second batch.
	rec = fleetPost(t, r, cookies, csrf, fleetBody(connID, 2, true, pv.ProfileID), "fleet-key-1")
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body, fmt.Sprint(ack.FleetID)) {
		t.Fatalf("replay: %d %s", rec.Code, rec.Body)
	}
	if n := prov.createCalls(); n != 2 {
		t.Fatalf("provider create calls = %d, want 2", n)
	}
	// Same key, different body → conflict.
	rec = fleetPost(t, r, cookies, csrf, fleetBody(connID, 3, true, pv.ProfileID), "fleet-key-1")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body, "idempotency_conflict") {
		t.Fatalf("conflict: %d %s", rec.Code, rec.Body)
	}

	// Detail DTO exposes owned items and null resource before create.
	rec = cloudCall(t, r, http.MethodGet, fmt.Sprintf("/api/me/fleets/%d", ack.FleetID), cookies, "", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body, `"base-01"`) {
		t.Fatalf("fleet detail: %d %s", rec.Code, rec.Body)
	}
	rec = cloudCall(t, r, http.MethodGet, "/api/me/fleets", cookies, "", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body, `"succeeded":2`) {
		t.Fatalf("fleet list: %d %s", rec.Code, rec.Body)
	}
}

func TestFleetAutoReplaceOffLeavesManual(t *testing.T) {
	r, _ := newCloudRegistry(t)
	r.cfg.Security.NodeToken = testNodeToken
	r.sshExec.(*fakeExec).runFn = verifyExecFn(r, true)
	uid, connID, cookies, csrf := cloudSession(t, r, "fleet-manual")
	pv := fleetProfileVersion(t, r)

	rec := fleetPost(t, r, cookies, csrf, fleetBody(connID, 1, false, pv.ProfileID), "fleet-key-manual")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("fleet create: %d %s", rec.Code, rec.Body)
	}
	var ack struct {
		FleetID int64 `json:"fleet_id"`
	}
	_ = json.Unmarshal([]byte(rec.Body), &ack)
	f := waitFleetTerminal(t, r, ack.FleetID)
	if f.State != control.FleetSucceeded {
		t.Fatalf("state=%s", f.State)
	}
	items, _ := r.control.FleetItems(f.ID)
	if len(items) != 1 || items[0].NodeID == "" {
		t.Fatalf("items: %+v", items)
	}
	if _, err := r.control.NodeManagement(items[0].NodeID, uid); err == nil {
		t.Fatal("auto_replace=false must not create a management policy row")
	}
}

func TestFleetPerItemFailureContinues(t *testing.T) {
	r, _ := newCloudRegistry(t)
	flaky := newMultiCloud()
	flaky.failCreateAt = 2
	r.cloudFactory = func(control.HostingConnection) (cloudProvider, error) { return flaky, nil }
	r.cfg.Security.NodeToken = testNodeToken
	r.sshExec.(*fakeExec).runFn = verifyExecFn(r, true)
	_, connID, cookies, csrf := cloudSession(t, r, "fleet-flaky")
	pv := fleetProfileVersion(t, r)

	rec := fleetPost(t, r, cookies, csrf, fleetBody(connID, 3, false, pv.ProfileID), "fleet-key-flaky")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("fleet create: %d %s", rec.Code, rec.Body)
	}
	var ack struct {
		FleetID int64 `json:"fleet_id"`
	}
	_ = json.Unmarshal([]byte(rec.Body), &ack)
	f := waitFleetTerminal(t, r, ack.FleetID)
	if f.State != control.FleetPartial {
		t.Fatalf("state=%s, want partial", f.State)
	}
	items, _ := r.control.FleetItems(f.ID)
	admitted, failed := 0, 0
	for _, it := range items {
		switch it.State {
		case control.FleetItemAdmitted:
			admitted++
		case control.FleetItemError:
			failed++
			if it.LastError == "" {
				t.Fatalf("error item must carry a safe code: %+v", it)
			}
		}
	}
	if admitted != 2 || failed != 1 {
		t.Fatalf("admitted=%d failed=%d items=%+v", admitted, failed, items)
	}
}

func TestFleetReconcileCleansUncertainResources(t *testing.T) {
	r, _ := newCloudRegistry(t)
	uid := cabinetUser(t, r, "fleet-reconcile", 10)
	conn, err := r.control.CreateHostingConnection(control.HostingConnection{
		UserID: uid, Provider: control.ProviderNodehost, Label: "nh", CredentialsEnc: "sealed",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Provider inventory is empty for this fake unless test sets it.
	planned, err := r.control.CreateCloudResource(control.CloudResourceV2{
		UserID: uid, ConnectionID: conn.ID, Provider: control.ProviderNodehost,
		Label: "gone", State: control.CloudResPlanned,
	})
	if err != nil {
		t.Fatal(err)
	}
	withExt, err := r.control.CreateCloudResource(control.CloudResourceV2{
		UserID: uid, ConnectionID: conn.ID, Provider: control.ProviderNodehost,
		Label: "gone-ext", State: control.CloudResPlanned,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.control.UpdateCloudResourceState(withExt.ID, control.CloudResPlanned, control.CloudResError, "ext-gone", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.control.SetCloudResourceError(withExt.ID, "provider_reconcile_required"); err != nil {
		t.Fatal(err)
	}

	r.reconcileUncertainCloudResources(context.Background())
	for _, id := range []int64{planned.ID, withExt.ID} {
		res, err := r.control.CloudResourceOwned(id, uid)
		if err != nil || res.State != control.CloudResDeleted {
			t.Fatalf("resource %d must be deleted: %v %+v", id, err, res)
		}
	}
}

func TestSystemGraphSeedIdempotent(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	r.seedSystemGraphs()
	r.seedSystemGraphs()
	for _, g := range blueprint.SystemGraphs() {
		p, err := r.control.ProfileByName(g.Name)
		if err != nil {
			t.Fatalf("%s: %v", g.Name, err)
		}
		if !p.Published || p.CreatedBy != 0 {
			t.Fatalf("%s profile: %+v", g.Name, p)
		}
		v, err := r.control.ProfileVersion(p.ID, 0)
		if err != nil {
			t.Fatalf("%s version: %v", g.Name, err)
		}
		if v.Format != control.FormatGraph || v.ContentHash != g.ContentHash || v.Version != 1 {
			t.Fatalf("%s version: %+v", g.Name, v)
		}
	}
}
