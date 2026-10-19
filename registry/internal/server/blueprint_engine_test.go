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

func bpTestGraph() []byte {
	return []byte(`{
 "schema_version":1,"name":"test-graph","provider":"nodehost",
 "inputs":{"connection_id":{"type":"number","required":false},"configuration":{"type":"config","required":false},"installation_profile_version_id":{"type":"number","required":false}},
 "entries":{"initial":"create_initial","observe":"ban"},
 "nodes":[
  {"id":"create_initial","type":"server.create","params":{"purpose":"initial"},"in":{"connection":{"ref":"input.connection_id"},"configuration":{"ref":"input.configuration"}}},
  {"id":"ready_initial","type":"server.ready","params":{"timeout_seconds":60},"in":{"operation":{"ref":"create_initial.operation"}}},
  {"id":"install_initial","type":"install.text","params":{"timeout_seconds":60},"in":{"server":{"ref":"ready_initial.server"},"version":{"ref":"input.installation_profile_version_id"}}},
  {"id":"admit_initial","type":"pool.admit","params":{"timeout_seconds":60},"in":{"server":{"ref":"install_initial.server"}}},
  {"id":"arm_initial","type":"observe.arm","params":{},"in":{"server":{"ref":"admit_initial.server"}}},
  {"id":"ban","type":"event.ip_ban","params":{},"in":{"server":{"ref":"run.current_server"}}},
  {"id":"guard","type":"policy.guard","params":{},"in":{"server":{"ref":"ban.server"},"episode":{"ref":"ban.episode"}}},
  {"id":"stop","type":"terminal.stop","params":{"reason":"policy_inactive"},"in":{}},
  {"id":"arm_observe","type":"observe.arm","params":{},"in":{"server":{"ref":"guard.server"}}}
 ],
 "edges":[
  {"from":"create_initial","port":"ok","to":"ready_initial"},
  {"from":"ready_initial","port":"ok","to":"install_initial"},
  {"from":"install_initial","port":"ok","to":"admit_initial"},
  {"from":"admit_initial","port":"ok","to":"arm_initial"},
  {"from":"ban","port":"ok","to":"guard"},
  {"from":"guard","port":"stop","to":"stop"},
  {"from":"guard","port":"active","to":"arm_observe"}
 ]}`)
}

func mkGraphVersion(t *testing.T, r *Registry, uid int64, raw []byte) control.ProfileVersion {
	t.Helper()
	c := string(raw)
	hash := fmt.Sprintf("hash-%d-%d", uid, time.Now().UnixNano())
	p, err := r.control.CreateProfile(fmt.Sprintf("g-%d-%d", uid, time.Now().UnixNano()), "", uid)
	if err != nil {
		t.Fatal(err)
	}
	v, err := r.control.CreateProfileVersionGraph(p.ID, c, 1, hash)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func waitRun(t *testing.T, r *Registry, runID int64) control.ProfileRun {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		run, err := r.control.RunByID(runID)
		if err != nil {
			t.Fatalf("run %d: %v", runID, err)
		}
		switch run.State {
		case control.RunSucceeded, control.RunFailed, control.RunCancelled, control.RunNeedsAttention, control.RunWaitingEvent:
			return run
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %d did not settle, state=%s err=%s", runID, run.State, run.LastError)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func defaultTextVersion(t *testing.T, r *Registry) int64 {
	t.Helper()
	p, err := r.control.ProfileByName(control.DefaultProfileName)
	if err != nil {
		t.Fatal(err)
	}
	v, err := r.control.ProfileVersion(p.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	return v.ID
}

func engineRunBody(t *testing.T, r *Registry, v control.ProfileVersion, connID int64, nodeID string) string {
	t.Helper()
	pv := defaultTextVersion(t, r)
	return fmt.Sprintf(`{"profile_version_id":%d,"entry":"initial","node_id":%q,"confirm":true,"inputs":{"connection_id":%d,"configuration":{"name":"pool-node","cpu":"AMD Ryzen 9 7950X","cores":2,"ram":4,"disk":40,"diskType":"SSD NVMe","os":"ubuntu-24.04","location":"DE","period":"1m"},"installation_profile_version_id":%d}}`, v.ID, nodeID, connID, pv)
}

// TestBlueprintEngineInitialEndToEnd executes a Nodehost template through the
// real cloud create/install paths with a fake provider and fake SSH executor.
func TestBlueprintEngineInitialEndToEnd(t *testing.T) {
	r, fake := newCloudRegistry(t)
	uid, connID, cookies, csrf := cloudSession(t, r, "engine-alice")
	v := mkGraphVersion(t, r, uid, bpTestGraph())
	r.bpAdmit = func(_ context.Context, _ control.ProfileRun, res control.CloudResourceV2) (bool, error) {
		safetyHealthyCandidate(r, res)
		return true, nil
	}

	body := engineRunBody(t, r, v, connID, "pool-abc12")
	rec := cloudCall(t, r, http.MethodPost, "/api/me/profile-runs", cookies, csrf, body,
		map[string]string{"Idempotency-Key": "run-1"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create run: %d %s", rec.Code, rec.Body)
	}
	var ack struct {
		RunID int64 `json:"run_id"`
	}
	if err := json.Unmarshal([]byte(rec.Body), &ack); err != nil || ack.RunID == 0 {
		t.Fatalf("ack: %v %s", err, rec.Body)
	}
	run := waitRun(t, r, ack.RunID)
	if run.State != control.RunSucceeded {
		t.Fatalf("run state=%s err=%s", run.State, run.LastError)
	}
	if fake.createCalls() != 1 {
		t.Fatalf("expected exactly one paid create, got %d", fake.createCalls())
	}
	cps, err := r.control.CheckpointsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, cp := range cps {
		seen[cp.NodeID] = cp.State
	}
	for _, node := range []string{"create_initial", "ready_initial", "install_initial", "admit_initial", "arm_initial"} {
		if seen[node] != control.CheckpointConfirmed {
			t.Fatalf("%s checkpoint=%q (all=%v)", node, seen[node], seen)
		}
	}
}

// TestBlueprintEngineCreateReconcileDoesNotReplay verifies the paid-call
// boundary: a dispatched create without a confirmed outcome is reconciled from
// its persisted resource, never re-sent to the provider.
func TestBlueprintEngineCreateReconcileDoesNotReplay(t *testing.T) {
	r, fake := newCloudRegistry(t)
	uid, connID, _, _ := cloudSession(t, r, "engine-bob")
	v := mkGraphVersion(t, r, uid, bpTestGraph())
	conn, _ := r.control.HostingConnectionOwned(connID, uid)
	res, err := r.control.CreateCloudResource(control.CloudResourceV2{
		UserID: uid, ConnectionID: conn.ID, Provider: conn.Provider, Label: "pool-node", State: control.CloudResPlanned,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := r.control.CreateRun(control.ProfileRun{
		UserID: uid, ProfileVersionID: v.ID, ProfileID: v.ProfileID, NodeID: "pool-abc12",
		Entry: "initial", State: control.RunWaitingRetry, InputsJSON: "{}",
	})
	if err != nil {
		t.Fatal(err)
	}
	intent := fmt.Sprintf(`{"ports":{"operation":{"operation_id":0,"resource_id":%d}}}`, res.ID)
	if _, err := r.control.SaveCheckpoint(control.BlockCheckpoint{
		RunID: run.ID, NodeID: "create_initial", State: control.CheckpointDispatched,
		Attempt: 1, OutputJSON: intent, LeaseGeneration: 0,
	}); err != nil {
		t.Fatal(err)
	}
	r.executeProfileRun(context.Background(), run.ID, run.Revision)
	got, _ := r.control.RunByID(run.ID)
	if got.State != control.RunNeedsAttention {
		t.Fatalf("dispatched create without provider evidence should need attention, got %s %s", got.State, got.LastError)
	}
	if fake.createCalls() != 0 {
		t.Fatalf("paid create must not be replayed, got %d calls", fake.createCalls())
	}
}

// TestBlueprintEngineWaitingEventAndBanDelivery covers durable waiting_event,
// at-least-once delivery, dedupe and "no purchase on observe".
func TestBlueprintEngineWaitingEventAndBanDelivery(t *testing.T) {
	r, fake := newCloudRegistry(t)
	uid, connID, _, _ := cloudSession(t, r, "engine-carol")
	v := mkGraphVersion(t, r, uid, bpTestGraph())
	conn, _ := r.control.HostingConnectionOwned(connID, uid)
	res, err := r.control.CreateCloudResource(control.CloudResourceV2{
		UserID: uid, ConnectionID: conn.ID, Provider: conn.Provider, Label: "pool-abc12",
		ExternalID: "srv-1", NodeID: "pool-abc12", State: control.CloudResReady, IP: "203.0.113.5",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.control.SetNodeManagementProfile("pool-abc12", uid, 1, v.ID, `{}`, false, res.ID, time.Now().Unix()-1); err != nil {
		t.Fatal(err)
	}
	run, err := r.control.CreateRun(control.ProfileRun{
		UserID: uid, ProfileVersionID: v.ID, ProfileID: v.ProfileID, NodeID: "pool-abc12",
		Entry: "observe", InputsJSON: "{}", CurrentResourceID: res.ID, PolicyRevision: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.executeProfileRun(context.Background(), run.ID, run.Revision)
	waited, _ := r.control.RunByID(run.ID)
	if waited.State != control.RunWaitingEvent {
		t.Fatalf("want waiting_event, got %s %s", waited.State, waited.LastError)
	}
	// Restart survival: a fresh store on the same file still sees the waiter.
	reopened, err := control.Open(r.cfg.Database.ControlDB, r.cfg.Users.LegacyMaxNodes)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := reopened.RunByID(run.ID); err != nil || got.State != control.RunWaitingEvent {
		t.Fatalf("waiting_event not durable: %+v %v", got, err)
	}
	reopened.Close()

	// A new episode is delivered exactly once.
	if err := r.control.AddBanEpisode("pool-abc12", "203.0.113.5", "ep_new"); err != nil {
		t.Fatal(err)
	}
	r.reconcileBanDeliveries()
	var delivered control.ProfileRun
	deadline := time.Now().Add(5 * time.Second)
	for {
		runs, err := r.control.RunsByUser(uid)
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) >= 2 {
			for _, x := range runs {
				if x.ID != run.ID {
					delivered = x
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ban delivery did not create a run: %d runs", len(runs))
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := waitRun(t, r, delivered.ID)
	if got.State != control.RunSucceeded {
		t.Fatalf("observe run state=%s err=%s", got.State, got.LastError)
	}
	// Duplicate scan is a no-op.
	r.reconcileBanDeliveries()
	runs, _ := r.control.RunsByUser(uid)
	if len(runs) != 2 {
		t.Fatalf("dedupe: want 2 runs, got %d", len(runs))
	}
	if fake.createCalls() != 0 {
		t.Fatalf("observe must never purchase, got %d creates", fake.createCalls())
	}
}

// TestBlueprintEngineCancelAndManualMode: manual mode and cancel both stop
// before any provider mutation.
func TestBlueprintEngineCancelAndManualMode(t *testing.T) {
	r, fake := newCloudRegistry(t)
	uid, connID, _, _ := cloudSession(t, r, "engine-dave")
	v := mkGraphVersion(t, r, uid, bpTestGraph())
	conn, _ := r.control.HostingConnectionOwned(connID, uid)
	res, err := r.control.CreateCloudResource(control.CloudResourceV2{
		UserID: uid, ConnectionID: conn.ID, Provider: conn.Provider, Label: "pool-abc12",
		ExternalID: "srv-1", NodeID: "pool-abc12", State: control.CloudResReady, IP: "203.0.113.6",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Observe run with an episode but a manual (absent) policy: guard -> stop.
	run, err := r.control.CreateRun(control.ProfileRun{
		UserID: uid, ProfileVersionID: v.ID, ProfileID: v.ProfileID, NodeID: "pool-abc12",
		Entry: "observe", InputsJSON: `{"episode_id":"ep_x","banned_ip":"203.0.113.6"}`,
		CurrentResourceID: res.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.executeProfileRun(context.Background(), run.ID, run.Revision)
	got, _ := r.control.RunByID(run.ID)
	if got.State != control.RunCancelled {
		t.Fatalf("manual mode should stop the observe run, got %s", got.State)
	}
	if fake.createCalls() != 0 {
		t.Fatalf("manual mode must not mutate provider, got %d", fake.createCalls())
	}

	// Cancel a pending initial run before any dispatch.
	run2, err := r.control.CreateRun(control.ProfileRun{
		UserID: uid, ProfileVersionID: v.ID, ProfileID: v.ProfileID, NodeID: "pool-new",
		Entry: "initial", InputsJSON: "{}",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.control.RequestRunCancel(run2.ID, uid, run2.Revision); err != nil {
		t.Fatal(err)
	}
	r.executeProfileRun(context.Background(), run2.ID, run2.Revision)
	got2, _ := r.control.RunByID(run2.ID)
	if got2.State != control.RunCancelled {
		t.Fatalf("cancel before dispatch: got %s", got2.State)
	}
	if fake.createCalls() != 0 {
		t.Fatalf("cancelled run must not mutate provider, got %d", fake.createCalls())
	}

	// In-flight: the durable flag is set while the run is still running; the
	// worker finalizes it at the next boundary without new effects.
	run3, _ := r.control.CreateRun(control.ProfileRun{
		UserID: uid, ProfileVersionID: v.ID, ProfileID: v.ProfileID, NodeID: "pool-new",
		Entry: "initial", InputsJSON: "{}",
	})
	running, err := r.control.SetRunState(run3.ID, run3.Revision, control.RunRunning, 0, "", -1)
	if err != nil {
		t.Fatal(err)
	}
	flagged, err := r.control.RequestRunCancel(run3.ID, uid, running.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if flagged.State != control.RunRunning || !flagged.CancelRequested {
		t.Fatalf("in-flight cancel must leave running+cancel_requested: %+v", flagged)
	}
	r.executeProfileRun(context.Background(), run3.ID, flagged.Revision)
	got3, _ := r.control.RunByID(run3.ID)
	if got3.State != control.RunCancelled {
		t.Fatalf("in-flight cancel finalization: got %s", got3.State)
	}
	if fake.createCalls() != 0 {
		t.Fatalf("in-flight cancel must not mutate provider, got %d", fake.createCalls())
	}
}

// TestNodeManagementProfileEnableValidationAndHistoricalBan covers PUT
// mode=profile validation and the historical-ban watermark.
func TestNodeManagementProfileEnableValidationAndHistoricalBan(t *testing.T) {
	r, _ := newCloudRegistry(t)
	uid, connID, cookies, csrf := cloudSession(t, r, "engine-erin")
	v := mkGraphVersion(t, r, uid, bpTestGraph())
	u, _ := r.control.User(uid)
	if err := r.control.AuthorizeNode("pool-abc12", u.User); err != nil {
		t.Fatalf("authorize node: %v", err)
	}

	put := func(body string) *httptestResponse {
		return cloudCall(t, r, http.MethodPut, "/api/me/nodes/pool-abc12/management", cookies, csrf, body, nil)
	}
	// Historical ban exists BEFORE activation.
	if err := r.control.AddBanEpisode("pool-abc12", "203.0.113.7", "ep_history"); err != nil {
		t.Fatal(err)
	}
	valid := fmt.Sprintf(`{"mode":"profile","profile_version_id":%d,"inputs":{"connection_id":%d},"process_current_ban":false,"expected_revision":1}`, v.ID, connID)
	if rec := put(valid); rec.Code != http.StatusOK {
		t.Fatalf("enable profile: %d %s", rec.Code, rec.Body)
	}
	m, err := r.control.NodeManagement("pool-abc12", uid)
	if err != nil || m.Mode != "profile" || m.ProfileVersionID != v.ID {
		t.Fatalf("policy: %+v %v", m, err)
	}
	// Historical episode stays skipped by default.
	r.reconcileBanDeliveries()
	if runs, _ := r.control.RunsByUser(uid); len(runs) != 0 {
		t.Fatalf("historical ban must not create a run, got %d", len(runs))
	}

	// Provider mismatch -> 422.
	begetRaw := []byte(fmt.Sprintf(`{"schema_version":1,"name":"beget-test","provider":"beget","inputs":{"connection_id":{"type":"number","required":false}},"entries":{"initial":"stop1","observe":"stop2"},"nodes":[{"id":"stop1","type":"terminal.stop","params":{"reason":"policy_inactive"},"in":{}},{"id":"stop2","type":"terminal.stop","params":{"reason":"policy_inactive"},"in":{}}],"edges":[]}`))
	bv := mkGraphVersion(t, r, uid, begetRaw)
	bad := fmt.Sprintf(`{"mode":"profile","profile_version_id":%d,"inputs":{},"expected_revision":2}`, bv.ID)
	if rec := put(bad); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("provider mismatch: got %d %s", rec.Code, rec.Body)
	}

	// process_current_ban=true admits exactly the current (latest) episode.
	current := fmt.Sprintf(`{"mode":"profile","profile_version_id":%d,"inputs":{"connection_id":%d},"process_current_ban":true,"expected_revision":2}`, v.ID, connID)
	if rec := put(current); rec.Code != http.StatusOK {
		t.Fatalf("enable current ban: %d %s", rec.Code, rec.Body)
	}
	r.reconcileBanDeliveries()
	runs, _ := r.control.RunsByUser(uid)
	if len(runs) != 1 {
		t.Fatalf("process_current_ban should deliver the latest episode, got %d runs", len(runs))
	}
}

// TestProfileRunRoutesOwnershipCSRFAndIdempotency covers the route contract.
func TestProfileRunRoutesOwnershipCSRFAndIdempotency(t *testing.T) {
	r, _ := newCloudRegistry(t)
	uid, connID, cookies, csrf := cloudSession(t, r, "engine-frank")
	cabinetUser(t, r, "engine-grace", 5)
	gsess, gcsrf := cabinetLogin(t, r, "engine-grace", "password-123")
	v := mkGraphVersion(t, r, uid, bpTestGraph())
	body := engineRunBody(t, r, v, connID, "pool-frank")

	// CSRF is mandatory.
	if rec := cloudCall(t, r, http.MethodPost, "/api/me/profile-runs", cookies, "", body, map[string]string{"Idempotency-Key": "k0"}); rec.Code != http.StatusForbidden {
		t.Fatalf("csrf: want 403, got %d %s", rec.Code, rec.Body)
	}
	// confirm:true is mandatory.
	if rec := cloudCall(t, r, http.MethodPost, "/api/me/profile-runs", cookies, csrf, `{"profile_version_id":1,"entry":"initial"}`, map[string]string{"Idempotency-Key": "k1"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("confirm: want 400, got %d %s", rec.Code, rec.Body)
	}
	// First create + replay with the same key/body -> same run id.
	rec := cloudCall(t, r, http.MethodPost, "/api/me/profile-runs", cookies, csrf, body, map[string]string{"Idempotency-Key": "k2"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var ack struct {
		RunID int64 `json:"run_id"`
	}
	_ = json.Unmarshal([]byte(rec.Body), &ack)
	rec2 := cloudCall(t, r, http.MethodPost, "/api/me/profile-runs", cookies, csrf, body, map[string]string{"Idempotency-Key": "k2"})
	var ack2 struct {
		RunID int64 `json:"run_id"`
	}
	_ = json.Unmarshal([]byte(rec2.Body), &ack2)
	if rec2.Code != http.StatusAccepted || ack2.RunID != ack.RunID {
		t.Fatalf("idempotent replay: %d %s (want run %d)", rec2.Code, rec2.Body, ack.RunID)
	}
	// Different body with the same key -> 409.
	other := engineRunBody(t, r, v, connID, "pool-other")
	if rec3 := cloudCall(t, r, http.MethodPost, "/api/me/profile-runs", cookies, csrf, other, map[string]string{"Idempotency-Key": "k2"}); rec3.Code != http.StatusConflict {
		t.Fatalf("idem conflict: want 409, got %d %s", rec3.Code, rec3.Body)
	}
	// Foreign owner sees 404.
	if rec4 := cloudCall(t, r, http.MethodGet, fmt.Sprintf("/api/me/profile-runs/%d", ack.RunID), cookieMap(gsess, gcsrf), "", "", nil); rec4.Code != http.StatusNotFound {
		t.Fatalf("foreign run: want 404, got %d %s", rec4.Code, rec4.Body)
	}
	// Stale expected_revision -> 409.
	if rec5 := cloudCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/profile-runs/%d/cancel", ack.RunID), cookies, csrf, `{"expected_revision":999}`, map[string]string{"Idempotency-Key": "k3"}); rec5.Code != http.StatusConflict {
		t.Fatalf("stale cancel: want 409, got %d %s", rec5.Code, rec5.Body)
	}
	_ = blueprint.SchemaVersion
}

func bpDeleteGraph() []byte {
	return []byte(`{
 "schema_version":1,"name":"delete-graph","provider":"nodehost",
 "inputs":{"connection_id":{"type":"number","required":true},"configuration":{"type":"config","required":true}},
 "entries":{"initial":"create_initial","observe":"stop"},
 "nodes":[
  {"id":"create_initial","type":"server.create","params":{"purpose":"initial"},"in":{"connection":{"ref":"input.connection_id"},"configuration":{"ref":"input.configuration"}}},
  {"id":"ready_initial","type":"server.ready","params":{"timeout_seconds":30},"in":{"operation":{"ref":"create_initial.operation"}}},
  {"id":"delete_old","type":"server.delete","params":{},"in":{"server":{"ref":"ready_initial.server"}}},
  {"id":"stop","type":"terminal.stop","params":{},"in":{}}
 ],
 "edges":[
  {"from":"create_initial","port":"ok","to":"ready_initial"},
  {"from":"ready_initial","port":"ok","to":"delete_old"}
 ]}`)
}

func bpGuardCreateGraph() []byte {
	return []byte(`{
 "schema_version":1,"name":"guard-create","provider":"nodehost",
 "inputs":{"connection_id":{"type":"number","required":false},"configuration":{"type":"config","required":false}},
 "entries":{"initial":"stop","observe":"ban"},
 "nodes":[
  {"id":"ban","type":"event.ip_ban","params":{},"in":{"server":{"ref":"run.current_server"}}},
  {"id":"guard","type":"policy.guard","params":{},"in":{"server":{"ref":"ban.server"},"episode":{"ref":"ban.episode"}}},
  {"id":"stop","type":"terminal.stop","params":{"reason":"policy_inactive"},"in":{}},
  {"id":"create_new","type":"server.create","params":{"purpose":"replacement"},"in":{"connection":{"ref":"input.connection_id"},"configuration":{"ref":"input.configuration"}}}
 ],
 "edges":[
  {"from":"ban","port":"ok","to":"guard"},
  {"from":"guard","port":"stop","to":"stop"},
  {"from":"guard","port":"active","to":"create_new"}
 ]}`)
}

// C1: concurrent identical POSTs must produce one run and one paid create.
func TestProfileRunCreateConcurrentIdempotent(t *testing.T) {
	r, fake := newCloudRegistry(t)
	uid, connID, cookies, csrf := cloudSession(t, r, "engine-c1")
	v := mkGraphVersion(t, r, uid, bpTestGraph())
	body := engineRunBody(t, r, v, connID, "pool-c1")
	var wg sync.WaitGroup
	acks := make(chan int64, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := cloudCall(t, r, http.MethodPost, "/api/me/profile-runs", cookies, csrf, body,
				map[string]string{"Idempotency-Key": "same-key"})
			if rec.Code != http.StatusAccepted {
				t.Errorf("create: %d %s", rec.Code, rec.Body)
				return
			}
			var ack struct {
				RunID int64 `json:"run_id"`
			}
			_ = json.Unmarshal([]byte(rec.Body), &ack)
			acks <- ack.RunID
		}()
	}
	wg.Wait()
	close(acks)
	var runID int64
	for id := range acks {
		if runID == 0 {
			runID = id
		} else if id != runID {
			t.Fatalf("different run ids: %d vs %d", runID, id)
		}
	}
	if runID == 0 {
		t.Fatal("no run id")
	}
	waitRun(t, r, runID)
	runs, err := r.control.RunsByUser(uid)
	if err != nil || len(runs) != 1 {
		t.Fatalf("want exactly one run, got %d err=%v", len(runs), err)
	}
	if fake.createCalls() != 1 {
		t.Fatalf("want one paid create, got %d", fake.createCalls())
	}
}

// H1: a waiting outcome on a dispatched mutation must not downgrade it.
func TestBlueprintDispatchedWaitingBecomesNeedsAttention(t *testing.T) {
	r, fake := newCloudRegistry(t)
	uid, _, _, _ := cloudSession(t, r, "engine-h1")
	v := mkGraphVersion(t, r, uid, bpTestGraph())
	run, err := r.control.CreateRun(control.ProfileRun{
		UserID: uid, ProfileVersionID: v.ID, ProfileID: v.ProfileID, NodeID: "pool-h1",
		Entry: "initial", State: control.RunWaitingRetry, InputsJSON: "{}",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.control.SaveCheckpoint(control.BlockCheckpoint{
		RunID: run.ID, NodeID: "create_initial", State: control.CheckpointDispatched,
		Attempt: 1, OutputJSON: `{"ports":{"operation":{"operation_id":0,"resource_id":1}}}`, LeaseGeneration: 0,
	}); err != nil {
		t.Fatal(err)
	}
	st, err := r.loadBPState(run)
	if err != nil {
		t.Fatal(err)
	}
	node, _ := nodeOf(st.bp, "create_initial")
	cp := st.checkpoints["create_initial"]
	r.applyOutcome(st, node, cp, bpOutcome{waiting: control.RunWaitingRetry}, nil)
	got, _ := r.control.RunByID(run.ID)
	if got.State != control.RunNeedsAttention {
		t.Fatalf("want needs_attention, got %s", got.State)
	}
	cp, _ = r.control.CheckpointByNode(run.ID, "create_initial", 0)
	if cp.State != control.CheckpointUnknown {
		t.Fatalf("dispatched checkpoint downgraded to %s", cp.State)
	}
	if fake.createCalls() != 0 {
		t.Fatalf("must not replay create, got %d", fake.createCalls())
	}
}

// H2: flipping profile -> manual after the guard must stop the next effect.
func TestBlueprintManualFlipStopsNextEffect(t *testing.T) {
	r, fake := newCloudRegistry(t)
	uid, connID, _, _ := cloudSession(t, r, "engine-h2")
	v := mkGraphVersion(t, r, uid, bpGuardCreateGraph())
	conn, _ := r.control.HostingConnectionOwned(connID, uid)
	res, err := r.control.CreateCloudResource(control.CloudResourceV2{
		UserID: uid, ConnectionID: conn.ID, Provider: conn.Provider, Label: "pool-h2",
		ExternalID: "srv-h2", NodeID: "pool-h2", State: control.CloudResReady, IP: "203.0.113.21",
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := r.control.SetNodeManagementProfile("pool-h2", uid, 1, v.ID, `{}`, false, res.ID, time.Now().Unix()-1)
	if err != nil {
		t.Fatal(err)
	}
	run, err := r.control.CreateRun(control.ProfileRun{
		UserID: uid, ProfileVersionID: v.ID, ProfileID: v.ProfileID, NodeID: "pool-h2",
		Entry: "observe", InputsJSON: `{"episode_id":"ep_h2","banned_ip":"203.0.113.21"}`,
		CurrentResourceID: res.ID, PolicyRevision: m.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := `{"resource_id":` + fmt.Sprint(res.ID) + `,"node_id":"pool-h2","provider":"nodehost","external_id":"srv-h2","ip":"203.0.113.21"}`
	for _, cp := range []struct{ node, out string }{
		{"ban", `{"ports":{"server":` + srv + `,"episode":"ep_h2"}}`},
		{"guard", `{"ports":{"server":` + srv + `,"episode":"ep_h2"},"__port":"active"}`},
	} {
		if _, err := r.control.SaveCheckpoint(control.BlockCheckpoint{
			RunID: run.ID, NodeID: cp.node, State: control.CheckpointConfirmed,
			Attempt: 1, OutputJSON: cp.out, LeaseGeneration: 0,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.control.PatchNodeManagement("pool-h2", uid, m.Revision, "manual", 0, false, 0); err != nil {
		t.Fatal(err)
	}
	r.executeProfileRun(context.Background(), run.ID, run.Revision)
	got, _ := r.control.RunByID(run.ID)
	if got.State != control.RunCancelled {
		t.Fatalf("want cancelled after mode flip, got %s (%s)", got.State, got.LastError)
	}
	if fake.createCalls() != 0 {
		t.Fatalf("manual mode must stop the next effect, got %d creates", fake.createCalls())
	}
}

// H3: a definitively failed delete is retried with a fresh attempt key.
func TestBlueprintDeleteRetryDispatchesAgain(t *testing.T) {
	r, fake := newCloudRegistry(t)
	fake.deleteFails = 1
	uid, connID, _, _ := cloudSession(t, r, "engine-h3")
	conn, _ := r.control.HostingConnectionOwned(connID, uid)
	res, err := r.control.CreateCloudResource(control.CloudResourceV2{
		UserID: uid, ConnectionID: conn.ID, Provider: conn.Provider, Label: "pool-h3",
		ExternalID: "srv-h3", NodeID: "pool-h3", State: control.CloudResReady, IP: "203.0.113.31",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Delete retries retain the same real readiness/fence/switch requirements.
	next, err := r.control.CreateCloudResource(control.CloudResourceV2{
		UserID: uid, ConnectionID: conn.ID, Provider: conn.Provider, NodeID: "replacement-h3", State: control.CloudResReady,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.control.UpdateCloudResourceState(next.ID, next.State, next.State, "srv-next-h3", "203.0.113.32"); err != nil {
		t.Fatal(err)
	}
	next.IP = "203.0.113.32"
	safetyHealthyCandidate(r, next)
	if err := r.control.SetCloudResourceReplacedBy(res.ID, next.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.control.PatchNodeManagement("pool-h3", uid, 1, "manual", 0, false, next.ID); err != nil {
		t.Fatal(err)
	}
	run, err := r.control.CreateRun(control.ProfileRun{
		UserID: uid, ProfileVersionID: 1, ProfileID: 1, NodeID: "pool-h3",
		Entry: "initial", State: control.RunRunning, InputsJSON: "{}",
	})
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := json.Marshal(bpServer{ResourceID: res.ID, NodeID: "pool-h3", Provider: "nodehost", ExternalID: "srv-h3", IP: "203.0.113.31"})
	node := blueprint.Node{ID: "delete_old", Type: "server.delete",
		In: map[string]blueprint.Binding{"server": {Literal: srv}}}
	st := &bpState{r: r, run: run, bp: blueprint.Blueprint{}, checkpoints: map[string]control.BlockCheckpoint{
		"delete_old": {RunID: run.ID, NodeID: "delete_old", State: control.CheckpointDispatched, Attempt: 1},
	}, outputs: map[string]map[string]any{"delete_old": {"server": serverFromResource(res)}}}
	out := st.deleteServer(context.Background(), node)
	if out.waiting != control.RunWaitingRetry || !out.retryDispatch {
		t.Fatalf("first delete should be retryable: %+v", out)
	}
	if fresh, _ := r.control.CloudResourceOwned(res.ID, uid); fresh.State != control.CloudResError {
		t.Fatalf("resource should be error after provider failure, got %s", fresh.State)
	}
	// Next dispatch gets a fresh attempt key.
	st.checkpoints["delete_old"] = control.BlockCheckpoint{RunID: run.ID, NodeID: "delete_old", State: control.CheckpointDispatched, Attempt: 2}
	fake.mu.Lock()
	fake.serverErr = &nodehost.Error{Code: "not_found"}
	fake.mu.Unlock()
	out = st.deleteServer(context.Background(), node)
	if out.err != nil || out.waiting != "" {
		t.Fatalf("second delete should succeed: %+v", out)
	}
	if fresh, _ := r.control.CloudResourceOwned(res.ID, uid); fresh.State != control.CloudResDeleted {
		t.Fatalf("resource should be deleted, got %s", fresh.State)
	}
	if fake.deleteCalls() != 2 {
		t.Fatalf("want 2 delete calls, got %d", fake.deleteCalls())
	}
}

// H6: a provider panic is contained and the run becomes needs_attention.
func TestBlueprintWorkerPanicContained(t *testing.T) {
	r, fake := newCloudRegistry(t)
	fake.createPanic = true
	uid, connID, _, _ := cloudSession(t, r, "engine-h6")
	v := mkGraphVersion(t, r, uid, bpTestGraph())
	pvID := defaultTextVersion(t, r)
	inputs := fmt.Sprintf(`{"connection_id":%d,"configuration":{"name":"pool-h6","cpu":"AMD Ryzen 9 7950X","cores":2,"ram":4,"disk":40,"diskType":"SSD NVMe","os":"ubuntu-24.04","location":"DE","period":"1m"},"installation_profile_version_id":%d}`, connID, pvID)
	run, err := r.control.CreateRun(control.ProfileRun{
		UserID: uid, ProfileVersionID: v.ID, ProfileID: v.ProfileID, NodeID: "pool-h6",
		Entry: "initial", State: control.RunPending, InputsJSON: inputs,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.launchBlueprintRun(context.Background(), run.ID, run.Revision)
	got := waitRun(t, r, run.ID)
	if got.State != control.RunNeedsAttention || got.LastError != "worker_panic" {
		t.Fatalf("want needs_attention/worker_panic, got %s/%s", got.State, got.LastError)
	}
}

// H5: stop cancels and waits for the in-flight worker.
func TestProvisionWorkerStopWaits(t *testing.T) {
	w := newProvisionWorker()
	done := make(chan struct{})
	if !w.run(1, time.Minute, func(ctx context.Context) {
		<-ctx.Done()
		close(done)
	}) {
		t.Fatal("run not started")
	}
	w.stop()
	select {
	case <-done:
	default:
		t.Fatal("stop returned before the worker observed cancellation")
	}
}

// M1: a running+cancel_requested run without a live worker is finalized.
func TestBlueprintOrphanedCancelFinalized(t *testing.T) {
	r, _ := newCloudRegistry(t)
	uid, _, _, _ := cloudSession(t, r, "engine-m1")
	v := mkGraphVersion(t, r, uid, bpTestGraph())
	run, err := r.control.CreateRun(control.ProfileRun{
		UserID: uid, ProfileVersionID: v.ID, ProfileID: v.ProfileID, NodeID: "pool-m1",
		Entry: "initial", State: control.RunPending, InputsJSON: "{}",
	})
	if err != nil {
		t.Fatal(err)
	}
	running, err := r.control.SetRunState(run.ID, run.Revision, control.RunRunning, 0, "", -1)
	if err != nil {
		t.Fatal(err)
	}
	flagged, err := r.control.RequestRunCancel(run.ID, uid, running.Revision)
	if err != nil || flagged.State != control.RunRunning {
		t.Fatalf("cancel flag: %+v %v", flagged, err)
	}
	r.finalizeOrphanedCancels(context.Background())
	got, _ := r.control.RunByID(run.ID)
	if got.State != control.RunCancelled {
		t.Fatalf("want cancelled, got %s", got.State)
	}
}

// M2: the waiting retry budget is capped.
func TestBlueprintWaitingAttemptCap(t *testing.T) {
	r, _ := newCloudRegistry(t)
	uid, _, _, _ := cloudSession(t, r, "engine-m2")
	v := mkGraphVersion(t, r, uid, bpTestGraph())
	run, err := r.control.CreateRun(control.ProfileRun{
		UserID: uid, ProfileVersionID: v.ID, ProfileID: v.ProfileID, NodeID: "pool-m2",
		Entry: "initial", State: control.RunWaitingRetry, InputsJSON: "{}",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.control.SaveCheckpoint(control.BlockCheckpoint{
		RunID: run.ID, NodeID: "ready_initial", State: control.CheckpointWaiting,
		Attempt: blueprintMaxAttempts, LeaseGeneration: 0,
	}); err != nil {
		t.Fatal(err)
	}
	st, err := r.loadBPState(run)
	if err != nil {
		t.Fatal(err)
	}
	node, _ := nodeOf(st.bp, "ready_initial")
	r.applyOutcome(st, node, st.checkpoints["ready_initial"], bpOutcome{waiting: control.RunWaitingRetry}, nil)
	got, _ := r.control.RunByID(run.ID)
	if got.State != control.RunNeedsAttention {
		t.Fatalf("want needs_attention at retry cap, got %s", got.State)
	}
}

// M5: an episode id persisted only in JSON state is replayed into control DB.
func TestBanEpisodePersistenceRetry(t *testing.T) {
	r, _ := newCloudRegistry(t)
	r.state.Terminated["pool-m5"] = &TerminatedRecord{
		NodeID: "pool-m5", IP: "203.0.113.55", Reason: BanReasonIPBan, EpisodeID: "ep_retry_m5",
	}
	r.retryBanEpisodePersistence()
	ep, err := r.control.LatestBanEpisode("pool-m5")
	if err != nil || ep.EpisodeID != "ep_retry_m5" {
		t.Fatalf("episode not replayed: %+v %v", ep, err)
	}
}

// H1 residual: a dispatched create that errors becomes unknown, and the
// documented resume decision=reconcile may only read the provider. Create is
// called exactly once and the pending intent is retained.
func TestBlueprintCreateErrorResumeReconcileNeverReplays(t *testing.T) {
	r, fake := newCloudRegistry(t)
	fake.mu.Lock()
	fake.createErr = &nodehost.Error{Code: "provider_error"}
	fake.mu.Unlock()
	uid, connID, cookies, csrf := cloudSession(t, r, "engine-h1resume")
	v := mkGraphVersion(t, r, uid, bpTestGraph())

	rec := cloudCall(t, r, http.MethodPost, "/api/me/profile-runs", cookies, csrf,
		engineRunBody(t, r, v, connID, "pool-h1resume"), map[string]string{"Idempotency-Key": "h1resume"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create run: %d %s", rec.Code, rec.Body)
	}
	var ack struct {
		RunID int64 `json:"run_id"`
	}
	_ = json.Unmarshal([]byte(rec.Body), &ack)
	run := waitRun(t, r, ack.RunID)
	if run.State != control.RunNeedsAttention {
		t.Fatalf("dispatched create error must park: %s/%s", run.State, run.LastError)
	}
	cps, _ := r.control.CheckpointsByRun(run.ID)
	var cp control.BlockCheckpoint
	for _, c := range cps {
		if c.NodeID == "create_initial" {
			cp = c
		}
	}
	if cp.State != control.CheckpointUnknown || !strings.Contains(cp.OutputJSON, "resource_id") {
		t.Fatalf("create checkpoint must be unknown with retained intent: %+v", cp)
	}
	if fake.createCalls() != 1 {
		t.Fatalf("want one provider create, got %d", fake.createCalls())
	}

	body := fmt.Sprintf(`{"expected_revision":%d,"checkpoint_id":%d,"decision":"reconcile"}`, run.Revision, cp.ID)
	rec = cloudCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/profile-runs/%d/resume", run.ID), cookies, csrf, body,
		map[string]string{"Idempotency-Key": "h1resume-resume"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("resume: %d %s", rec.Code, rec.Body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := r.control.RunByID(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != control.RunReconciling && got.State != control.RunRunning {
			if got.State != control.RunNeedsAttention {
				t.Fatalf("reconcile without provider evidence must park, got %s/%s", got.State, got.LastError)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("resume did not settle, state=%s", got.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if fake.createCalls() != 1 {
		t.Fatalf("resume must never replay Create, got %d calls", fake.createCalls())
	}
	resources, err := r.control.CloudResourcesByUser(uid, false)
	if err != nil || len(resources) != 1 {
		t.Fatalf("want exactly one cloud_resources2 row, got %d err=%v", len(resources), err)
	}
}

// H7 residual: a cancelled waiting_retry run does not erase the cleanup
// obligation recorded by the fenced resource. A new ban episode is not
// delivered/purchased until that resource is confirmed deleted.
func TestBlueprintReplacementBlockedByUnresolvedFencedResource(t *testing.T) {
	r, fake := newCloudRegistry(t)
	uid, connID, _, _ := cloudSession(t, r, "engine-h7res")
	v := mkGraphVersion(t, r, uid, bpGuardCreateGraph())
	conn, _ := r.control.HostingConnectionOwned(connID, uid)

	oldRes, err := r.control.CreateCloudResource(control.CloudResourceV2{
		UserID: uid, ConnectionID: conn.ID, Provider: conn.Provider, Label: "old",
		ExternalID: "srv-old", NodeID: "pool-h7res", State: control.CloudResReady, IP: "203.0.113.41",
	})
	if err != nil {
		t.Fatal(err)
	}
	newRes, err := r.control.CreateCloudResource(control.CloudResourceV2{
		UserID: uid, ConnectionID: conn.ID, Provider: conn.Provider, Label: "new",
		ExternalID: "srv-new", NodeID: "pool-h7res", State: control.CloudResReady, IP: "203.0.113.42",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.control.SetCloudResourceReplacedBy(oldRes.ID, newRes.ID); err != nil {
		t.Fatal(err)
	}
	inputs := fmt.Sprintf(`{"connection_id":%d,"configuration":{"name":"pool-node","cpu":"AMD Ryzen 9 7950X","cores":2,"ram":4,"disk":40,"diskType":"SSD NVMe","os":"ubuntu-24.04","location":"DE","period":"1m"}}`, connID)
	m, err := r.control.SetNodeManagementProfile("pool-h7res", uid, 1, v.ID, inputs, false, newRes.ID, time.Now().Unix()-1)
	if err != nil {
		t.Fatal(err)
	}
	run, err := r.control.CreateRun(control.ProfileRun{
		UserID: uid, ProfileVersionID: v.ID, ProfileID: v.ProfileID, NodeID: "pool-h7res",
		Entry: "observe", State: control.RunWaitingRetry,
		InputsJSON:        `{"episode_id":"ep_old","banned_ip":"203.0.113.41"}`,
		CurrentResourceID: newRes.ID, PolicyRevision: m.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.control.RequestRunCancel(run.ID, uid, run.Revision); err != nil {
		t.Fatal(err)
	}
	if err := r.control.AddBanEpisode("pool-h7res", "203.0.113.41", "ep_h7res"); err != nil {
		t.Fatal(err)
	}

	r.reconcileBanDeliveries()
	if fake.createCalls() != 0 {
		t.Fatalf("unresolved fenced resource must block purchase, got %d creates", fake.createCalls())
	}
	if runs, _ := r.control.RunsByUser(uid); len(runs) != 1 {
		t.Fatalf("blocked delivery must not create a run, got %d", len(runs))
	}

	// Confirmed teardown releases the guard; the pending episode is delivered
	// and the replacement is allowed to buy exactly one VPS.
	if err := r.control.UpdateCloudResourceState(oldRes.ID, control.CloudResReady, control.CloudResDeleted, "", ""); err != nil {
		t.Fatal(err)
	}
	r.reconcileBanDeliveries()
	deadline := time.Now().Add(5 * time.Second)
	for {
		runs, _ := r.control.RunsByUser(uid)
		if len(runs) == 2 {
			for _, x := range runs {
				if x.ID != run.ID {
					got := waitRun(t, r, x.ID)
					if got.State != control.RunSucceeded {
						t.Fatalf("replacement run: %s/%s", got.State, got.LastError)
					}
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("delivery after delete was not released, runs=%d", len(runs))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if fake.createCalls() != 1 {
		t.Fatalf("new purchase allowed once after delete, got %d creates", fake.createCalls())
	}
}

// H8 residual: a paid replacement cancelled between create_new and
// resource.fence leaves a ready resource with replaces_resource_id set but
// replaced_by_resource_id NULL and no active run. It must still block the next
// ban purchase until it is deleted.
func TestBlueprintReplacementBlockedByUnfencedOrphan(t *testing.T) {
	r, fake := newCloudRegistry(t)
	uid, connID, _, _ := cloudSession(t, r, "engine-h8orphan")
	v := mkGraphVersion(t, r, uid, bpGuardCreateGraph())
	conn, _ := r.control.HostingConnectionOwned(connID, uid)

	current, err := r.control.CreateCloudResource(control.CloudResourceV2{
		UserID: uid, ConnectionID: conn.ID, Provider: conn.Provider, Label: "current",
		ExternalID: "srv-h8-old", NodeID: "pool-h8", State: control.CloudResReady, IP: "203.0.113.51",
	})
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := r.control.CreateCloudResource(control.CloudResourceV2{
		UserID: uid, ConnectionID: conn.ID, Provider: conn.Provider, Label: "orphan",
		ExternalID: "srv-h8-new", NodeID: "pool-h8", State: control.CloudResReady, IP: "203.0.113.52",
		ReplacesResourceID: current.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	inputs := fmt.Sprintf(`{"connection_id":%d,"configuration":{"name":"pool-node","cpu":"AMD Ryzen 9 7950X","cores":2,"ram":4,"disk":40,"diskType":"SSD NVMe","os":"ubuntu-24.04","location":"DE","period":"1m"}}`, connID)
	if _, err := r.control.SetNodeManagementProfile("pool-h8", uid, 1, v.ID, inputs, false, current.ID, time.Now().Unix()-1); err != nil {
		t.Fatal(err)
	}
	if err := r.control.AddBanEpisode("pool-h8", "203.0.113.51", "ep_h8orphan"); err != nil {
		t.Fatal(err)
	}

	r.reconcileBanDeliveries()
	if fake.createCalls() != 0 {
		t.Fatalf("unfenced orphan replacement must block purchase, got %d creates", fake.createCalls())
	}
	if runs, _ := r.control.RunsByUser(uid); len(runs) != 0 {
		t.Fatalf("blocked delivery must not create a run, got %d", len(runs))
	}

	// Confirmed deletion of the orphan releases the guard; the pending episode
	// is delivered and exactly one replacement VPS is bought.
	if err := r.control.UpdateCloudResourceState(orphan.ID, control.CloudResReady, control.CloudResDeleted, "", ""); err != nil {
		t.Fatal(err)
	}
	r.reconcileBanDeliveries()
	deadline := time.Now().Add(5 * time.Second)
	for {
		runs, _ := r.control.RunsByUser(uid)
		if len(runs) == 1 {
			if got := waitRun(t, r, runs[0].ID); got.State != control.RunSucceeded {
				t.Fatalf("replacement run: %s/%s", got.State, got.LastError)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("delivery after orphan delete was not released, runs=%d", len(runs))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if fake.createCalls() != 1 {
		t.Fatalf("new purchase allowed once after delete, got %d creates", fake.createCalls())
	}
}

// H4 residual: both delete routes return 409 profile_in_use over HTTP.
func TestDeleteProfileInUseHTTP(t *testing.T) {
	r, _ := newCloudRegistry(t)
	r.cfg.Panel.Token = testPanelToken
	uid, _, cookies, csrf := cloudSession(t, r, "engine-h4http")
	v := mkGraphVersion(t, r, uid, bpTestGraph())
	if _, err := r.control.SetNodeManagementProfile("pool-h4http", uid, 1, v.ID, `{}`, false, 0, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	rec := cloudCall(t, r, http.MethodDelete, fmt.Sprintf("/api/me/profiles/%d", v.ProfileID), cookies, csrf, "", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("user delete: want 409, got %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal([]byte(rec.Body), &body)
	if body.Error != "profile_in_use" {
		t.Fatalf("user delete code=%q body=%s", body.Error, rec.Body)
	}

	recAdmin := adminCall(t, r, http.MethodDelete, fmt.Sprintf("/panel/api/admin/profiles/%d", v.ProfileID), testPanelToken, "")
	if recAdmin.Code != http.StatusConflict {
		t.Fatalf("admin delete: want 409, got %d %s", recAdmin.Code, recAdmin.Body)
	}
	_ = json.Unmarshal(recAdmin.Body.Bytes(), &body)
	if body.Error != "profile_in_use" {
		t.Fatalf("admin delete code=%q body=%s", body.Error, recAdmin.Body)
	}
}

// L2 residual: the same key concurrently used for two different runs yields a
// single winning ack; the loser is rejected instead of acknowledging the wrong
// run.
func TestProfileRunCancelConcurrentKeyDifferentRuns(t *testing.T) {
	r, _ := newCloudRegistry(t)
	uid, _, cookies, csrf := cloudSession(t, r, "engine-l2")
	v := mkGraphVersion(t, r, uid, bpTestGraph())
	r1, err := r.control.CreateRun(control.ProfileRun{
		UserID: uid, ProfileVersionID: v.ID, ProfileID: v.ProfileID, Entry: "initial", NodeID: "pool-l2a",
	})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := r.control.CreateRun(control.ProfileRun{
		UserID: uid, ProfileVersionID: v.ID, ProfileID: v.ProfileID, Entry: "initial", NodeID: "pool-l2b",
	})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for _, run := range []control.ProfileRun{r1, r2} {
		wg.Add(1)
		go func(run control.ProfileRun) {
			defer wg.Done()
			body := fmt.Sprintf(`{"expected_revision":%d}`, run.Revision)
			rec := cloudCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/profile-runs/%d/cancel", run.ID), cookies, csrf, body,
				map[string]string{"Idempotency-Key": "shared-cancel"})
			codes <- rec.Code
		}(run)
	}
	wg.Wait()
	close(codes)
	accepted, conflict := 0, 0
	for code := range codes {
		switch code {
		case http.StatusAccepted:
			accepted++
		case http.StatusConflict:
			conflict++
		default:
			t.Fatalf("unexpected status %d", code)
		}
	}
	if accepted != 1 || conflict != 1 {
		t.Fatalf("want one 202 and one 409, got 202=%d 409=%d", accepted, conflict)
	}
}
