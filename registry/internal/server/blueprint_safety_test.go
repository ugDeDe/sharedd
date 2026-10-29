package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"sharedd/registry/internal/blueprint"
	"sharedd/registry/internal/control"
	"sharedd/registry/internal/nodehost"
	"sharedd/registry/internal/state"
)

func safetyHealthyCandidate(r *Registry, res control.CloudResourceV2) *Candidate {
	now := time.Now().Add(-time.Second)
	ok := true
	r.cfgMu.Lock()
	r.cfg.ReportFreshnessTTL, r.cfg.GlobalpingValidityTTL, r.cfg.SharedProxy.Port = time.Minute, time.Minute, 443
	r.cfgMu.Unlock()
	c := &Candidate{NodeID: res.NodeID, IP: res.IP, RegisteredAt: now.Add(-time.Second), LastHeartbeat: now,
		Healthy: true, GlobalpingOK: true, MetricsOK: true, MetricsHealthy: true,
		LastReportAt: now, LastMetricsCheckedAt: now, LastGlobalpingAt: now, GlobalpingMeasurementAt: now,
		GlobalpingMeasurementID: "independently-verified", Port: 443, PortCompatible: &ok,
		TCPHist: []state.TCPPoint{{At: now, OK: true}}}
	r.mu.Lock()
	r.state.Candidates[res.NodeID] = c
	r.mu.Unlock()
	return c
}

func TestSafetyBoundReadinessAtEveryBoundary(t *testing.T) {
	r, fake := newCloudRegistry(t)
	uid, connID, _, _ := cloudSession(t, r, "safety-health")
	create := func(node, ip string) control.CloudResourceV2 {
		t.Helper()
		res, err := r.control.CreateCloudResource(control.CloudResourceV2{UserID: uid, ConnectionID: connID,
			Provider: control.ProviderNodehost, NodeID: node, State: control.CloudResReady, ExternalID: node})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.control.UpdateCloudResourceState(res.ID, res.State, res.State, "", ip); err != nil {
			t.Fatal(err)
		}
		res.IP = ip
		return res
	}
	old, next := create("old", "203.0.113.1"), create("next", "203.0.113.2")
	if _, err := r.control.PatchNodeManagement(old.NodeID, uid, 1, "manual", 0, false, old.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.control.SetCloudResourceReplacedBy(old.ID, next.ID); err != nil {
		t.Fatal(err)
	}
	st := &bpState{r: r, run: control.ProfileRun{UserID: uid, NodeID: "old", CurrentResourceID: old.ID},
		outputs:     map[string]map[string]any{"old": {"server": serverFromResource(old)}, "next": {"server": serverFromResource(next)}},
		checkpoints: map[string]control.BlockCheckpoint{}}
	admit := blueprint.Node{ID: "admit", In: map[string]blueprint.Binding{"server": {Ref: "next.server"}}}
	pair := blueprint.Node{ID: "pair", In: map[string]blueprint.Binding{"old": {Ref: "old.server"}, "new": {Ref: "next.server"}}}
	del := blueprint.Node{ID: "delete", In: map[string]blueprint.Binding{"server": {Ref: "old.server"}}}
	r.bpAdmit = func(context.Context, control.ProfileRun, control.CloudResourceV2) (bool, error) { return true, nil }
	if out := func() bpOutcome { safetyHealthyCandidate(r, next); return st.poolAdmit(context.Background(), admit) }(); out.err != nil {
		t.Fatalf("healthy admission: %v", out.err)
	}
	for _, tc := range []struct {
		name        string
		breakHealth func(*Candidate)
	}{
		{"heartbeat-only", func(c *Candidate) {
			*c = Candidate{NodeID: next.NodeID, IP: next.IP, LastHeartbeat: time.Now(), RegisteredAt: time.Now()}
		}},
		{"wrong-ip", func(c *Candidate) { c.IP = old.IP }},
		{"no-gp-proof", func(c *Candidate) { c.GlobalpingMeasurementID = "" }},
		{"failed-gp", func(c *Candidate) { c.GlobalpingOK = false }},
		{"stale-gp", func(c *Candidate) { c.GlobalpingMeasurementAt = time.Now().Add(-time.Hour) }},
		{"future-gp", func(c *Candidate) { c.GlobalpingMeasurementAt = time.Now().Add(time.Hour) }},
		{"no-report", func(c *Candidate) { c.LastReportAt = time.Time{} }},
		{"failed-metrics", func(c *Candidate) { c.MetricsOK = false }},
		{"metrics-latch", func(c *Candidate) { c.MetricsHealthy = false }},
		{"stale-metrics", func(c *Candidate) { c.LastMetricsCheckedAt = time.Now().Add(-time.Hour) }},
		{"no-tcp", func(c *Candidate) { c.TCPHist = nil }},
		{"failed-tcp", func(c *Candidate) { c.TCPHist[len(c.TCPHist)-1].OK = false }},
		{"stale-tcp", func(c *Candidate) { c.TCPHist = []state.TCPPoint{{OK: true, At: time.Now().Add(-time.Hour)}} }},
		{"wrong-port", func(c *Candidate) { c.Port = 8443 }},
		{"unknown-port", func(c *Candidate) { c.PortCompatible = nil }},
		{"quarantined", func(c *Candidate) { c.Quarantine = &QuarantineState{} }},
		{"new-registration", func(c *Candidate) { c.RegisteredAt = time.Now() }},
		{"ip-change", func(c *Candidate) {
			c.IP = old.IP // A real re-registration must invalidate the old IP's proof.
			r.register(registerRequest{NodeID: next.NodeID, IP: next.IP})
		}},
		{"port-change", func(c *Candidate) {
			c.Port = 8443
			body, err := json.Marshal(HealthReportPayload{NodeID: c.NodeID, IP: c.IP, Port: 443, MetricsOK: true, CheckedAt: time.Now()})
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			r.handleHealthReport(rec, httptest.NewRequest(http.MethodPost, "/report", strings.NewReader(string(body))))
			if rec.Code != http.StatusOK {
				t.Fatalf("port report: %d %s", rec.Code, rec.Body.String())
			}
		}},
		{"terminated", func(c *Candidate) {
			r.state.Terminated[c.NodeID] = &TerminatedRecord{NodeID: c.NodeID, IP: c.IP, Reason: BanReasonIPBan}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := safetyHealthyCandidate(r, next)
			tc.breakHealth(c)
			for name, out := range map[string]bpOutcome{
				"admit": st.poolAdmit(context.Background(), admit), "fence": st.fenceResource(pair),
				"switch": st.switchResource(pair), "delete-old": st.deleteServer(context.Background(), del),
			} {
				if out.err == nil || !strings.Contains(out.err.Error(), "readiness_unverified") {
					t.Fatalf("%s accepted unsafe resource: %+v", name, out)
				}
			}
			delete(r.state.Terminated, next.NodeID)
		})
	}
	if fake.deleteCalls() != 0 {
		t.Fatalf("unsafe delete calls: %d", fake.deleteCalls())
	}
	res, err := r.control.CloudResourceOwned(old.ID, uid)
	if err != nil || res.State != control.CloudResReady {
		t.Fatalf("old resource mutated: %+v %v", res, err)
	}
	safetyHealthyCandidate(r, next)
	if out := st.deleteServer(context.Background(), del); out.err == nil {
		t.Fatal("fenced but not switched resource was deleted")
	}
	if out := st.fenceResource(pair); out.err != nil {
		t.Fatalf("healthy fence: %v", out.err)
	}
	if out := st.switchResource(pair); out.err != nil {
		t.Fatalf("healthy switch: %v", out.err)
	}
	// Previously admitted/switch-confirmed state is not a health waiver.
	r.state.Candidates[next.NodeID].Quarantine = &QuarantineState{}
	if out := st.deleteServer(context.Background(), del); out.err == nil || fake.deleteCalls() != 0 {
		t.Fatalf("post-switch health loss: %+v", out)
	}
	safetyHealthyCandidate(r, next)
	fake.serverErr = &nodehost.Error{Code: "not_found"}
	if out := st.deleteServer(context.Background(), del); out.err != nil || out.waiting != "" || fake.deleteCalls() != 1 {
		t.Fatalf("healthy deletion: %+v calls=%d", out, fake.deleteCalls())
	}
}

func TestSafetyGlobalpingMeasurementFreshness(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name, createdAt, status, change string
		newEpoch                        bool
		wantReady                       bool
	}{
		{name: "old", createdAt: now.Add(-16 * time.Minute).Format(time.RFC3339Nano), status: "finished"},
		{name: "fresh", createdAt: now.Add(-time.Minute).Format(time.RFC3339Nano), status: "finished", wantReady: true},
		{name: "missing", status: "finished"},
		{name: "invalid", createdAt: "not-a-date", status: "finished"},
		{name: "future", createdAt: now.Add(time.Hour).Format(time.RFC3339Nano), status: "finished"},
		{name: "pre-registration", createdAt: now.Add(-time.Minute).Format(time.RFC3339Nano), status: "finished", newEpoch: true},
		{name: "pre-endpoint", createdAt: now.Add(-time.Minute).Format(time.RFC3339Nano), status: "finished", change: "epoch"},
		{name: "not-finished", createdAt: now.Add(-time.Minute).Format(time.RFC3339Nano)},
		{name: "ip-change", createdAt: now.Add(-time.Minute).Format(time.RFC3339Nano), status: "finished", change: "ip"},
		{name: "port-change", createdAt: now.Add(-time.Minute).Format(time.RFC3339Nano), status: "finished", change: "port"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method != http.MethodGet || req.URL.Path != "/measurements/proof" {
					t.Errorf("unexpected Globalping request: %s %s", req.Method, req.URL.Path)
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				m := map[string]any{"id": "proof", "type": "http", "target": "203.0.113.2", "status": tc.status,
					"measurementOptions": map[string]any{"port": 443, "request": map[string]any{"host": "front.example.com"}},
					"results":            []any{map[string]any{"result": map[string]any{"status": "finished", "statusCode": 200}}}}
				if tc.createdAt != "" {
					m["createdAt"] = tc.createdAt
				}
				if err := json.NewEncoder(w).Encode(m); err != nil {
					t.Errorf("encode measurement: %v", err)
				}
			}))
			defer mock.Close()
			r := newTestRegistry(t)
			r.cfg.Globalping.APIBase, r.cfg.GlobalpingValidityTTL, r.cfg.SharedProxy.Port = mock.URL, 15*time.Minute, 443
			compatible := true
			c := &Candidate{NodeID: "next", IP: "203.0.113.2", RegisteredAt: now.Add(-30 * time.Minute),
				Port: 443, PortCompatible: &compatible, Healthy: true, MetricsOK: true, MetricsHealthy: true, GlobalpingOK: true,
				LastGlobalpingAt: now, GPLast: &GPDetail{At: now}, LastMetricsCheckedAt: now,
				TCPHist: []state.TCPPoint{{At: now, OK: true}}}
			if tc.newEpoch {
				c.RegisteredAt = now.Add(-30 * time.Second)
			}
			if tc.change == "epoch" {
				c.EndpointRegisteredAt = now.Add(-30 * time.Second)
			}
			r.state.Candidates[c.NodeID] = c
			send := func(payload HealthReportPayload) {
				t.Helper()
				body, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				rec := httptest.NewRecorder()
				r.handleHealthReport(rec, httptest.NewRequest(http.MethodPost, "/report", strings.NewReader(string(body))))
				if rec.Code != http.StatusOK {
					t.Fatalf("report: %d %s", rec.Code, rec.Body.String())
				}
			}
			send(HealthReportPayload{NodeID: c.NodeID, IP: c.IP, Port: 443, FakeSNI: "front.example.com",
				MetricsOK: true, GlobalpingOK: true, GlobalpingMeasurementID: "proof", CheckedAt: now})
			if tc.wantReady && (c.GlobalpingMeasurementAt.IsZero() || !c.GlobalpingMeasurementAt.Before(c.LastGlobalpingAt)) {
				t.Fatal("provider creation time must remain separate from verification receipt time")
			}
			if tc.name == "future" && !c.GlobalpingMeasurementAt.IsZero() {
				t.Fatal("future proof must not become usable as the clock catches up")
			}
			switch tc.change {
			case "ip":
				r.register(registerRequest{NodeID: c.NodeID, IP: "203.0.113.3"})
			case "port":
				send(HealthReportPayload{NodeID: c.NodeID, IP: c.IP, Port: 8443, MetricsOK: true, CheckedAt: now.Add(time.Second)})
			}
			if (tc.change == "ip" || tc.change == "port") && (!c.GlobalpingMeasurementAt.IsZero() || c.EndpointRegisteredAt.Before(now)) {
				t.Fatal("endpoint change must clear trusted proof and advance its epoch")
			}
			persisted, err := state.Load(r.cfg.State.File)
			if err != nil {
				t.Fatal(err)
			}
			r.state = *persisted
			res := control.CloudResourceV2{NodeID: c.NodeID, IP: c.IP, State: control.CloudResReady}
			err = r.blueprintResourceReady(res)
			if (err == nil) != tc.wantReady || (err != nil && !strings.Contains(err.Error(), "readiness_unverified")) {
				t.Fatalf("ready=%v, want %v: %v", err == nil, tc.wantReady, err)
			}
		})
	}
}

func TestSafetyInitialNamedHTTPAndDispatch(t *testing.T) {
	r, fake := newCloudRegistry(t)
	uid, connID, cookies, csrf := cloudSession(t, r, "safety-initial")
	pv := mkGraphVersion(t, r, uid, bpTestGraph())
	run, err := r.control.CreateRun(control.ProfileRun{UserID: uid, ProfileVersionID: pv.ID, ProfileID: pv.ProfileID,
		NodeID: "named-abc12", Entry: "initial", State: control.RunPaused})
	if err != nil {
		t.Fatal(err)
	}
	body := engineRunBody(t, r, pv, connID, run.NodeID)
	for _, stage := range []string{"active", "cancelled-paid"} {
		if stage == "cancelled-paid" {
			if _, err := r.control.RequestRunCancel(run.ID, uid, run.Revision); err != nil {
				t.Fatal(err)
			}
			res, err := r.control.CreateCloudResource(control.CloudResourceV2{UserID: uid, ConnectionID: connID, Provider: control.ProviderNodehost, State: control.CloudResReady})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := r.control.CreateOperation(control.Operation{UserID: uid, Kind: control.OpKindCloudCreate, State: control.OpConfirmed,
				IdemKey: fmt.Sprintf("bpcreate:%d:create_initial", run.ID), RequestJSON: fmt.Sprintf(`{"resource_id":%d}`, res.ID)}); err != nil {
				t.Fatal(err)
			}
		}
		rec := cloudCall(t, r, http.MethodPost, "/api/me/profile-runs", cookies, csrf, body, map[string]string{"Idempotency-Key": stage})
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body, "deployment_unresolved") {
			t.Fatalf("%s: %d %s", stage, rec.Code, rec.Body)
		}
		// Even a legacy/bypassed run must fail the later paid dispatch gate.
		var request profileRunCreateBody
		if err := json.Unmarshal([]byte(body), &request); err != nil {
			t.Fatal(err)
		}
		bp, ds := blueprint.ValidateBytes(bpTestGraph())
		if len(ds) != 0 {
			t.Fatal(ds)
		}
		node, _ := nodeOf(bp, "create_initial")
		st := &bpState{r: r, run: control.ProfileRun{ID: run.ID + 100, UserID: uid, NodeID: run.NodeID, Entry: "initial"}, bp: bp, inputs: request.Inputs}
		out := st.createServer(context.Background(), node)
		if out.err == nil || !strings.Contains(out.err.Error(), "deployment_unresolved") {
			t.Fatalf("dispatch %s: %+v", stage, out)
		}
	}
	if fake.createCalls() != 0 {
		t.Fatalf("duplicate paid calls: %d", fake.createCalls())
	}
}

func TestSafetyReplacementDispatchUsesResourceAncestry(t *testing.T) {
	r, fake := newCloudRegistry(t)
	uid, connID, _, _ := cloudSession(t, r, "safety-orphan")
	old, err := r.control.CreateCloudResource(control.CloudResourceV2{UserID: uid, ConnectionID: connID,
		Provider: control.ProviderNodehost, NodeID: "old-policy", State: control.CloudResReady})
	if err != nil {
		t.Fatal(err)
	}
	bp, ds := blueprint.ValidateBytes(bpTestGraph())
	if len(ds) != 0 {
		t.Fatal(ds)
	}
	node, _ := nodeOf(bp, "create_initial")
	node.Params = json.RawMessage(`{"purpose":"replacement"}`)
	st := &bpState{r: r, bp: bp, run: control.ProfileRun{ID: 123, UserID: uid, NodeID: old.NodeID,
		Entry: "observe", CurrentResourceID: old.ID}, inputs: map[string]json.RawMessage{
		"connection_id": json.RawMessage(fmt.Sprint(connID)),
		"configuration": json.RawMessage(`{"name":"pool-node","cpu":"AMD Ryzen 9 7950X","cores":2,"ram":4,"disk":40,"diskType":"SSD NVMe","os":"ubuntu-24.04","location":"DE","period":"1m"}`),
	}}
	for _, successorNode := range []string{"", "different-node"} {
		successor, err := r.control.CreateCloudResource(control.CloudResourceV2{UserID: uid, ConnectionID: connID,
			Provider: control.ProviderNodehost, NodeID: successorNode, State: control.CloudResReady, ReplacesResourceID: old.ID})
		if err != nil {
			t.Fatal(err)
		}
		if out := st.createServer(context.Background(), node); out.err == nil || !strings.Contains(out.err.Error(), "unresolved_replacement") {
			t.Fatalf("successor %q: %+v", successorNode, out)
		}
		if err := r.control.UpdateCloudResourceState(successor.ID, successor.State, control.CloudResDeleted, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if fake.createCalls() != 0 {
		t.Fatalf("orphan allowed another paid purchase: %d", fake.createCalls())
	}
}

func TestSafetyRunCommandHTTPConflicts(t *testing.T) {
	for _, command := range []string{"cancel", "resume"} {
		t.Run(command, func(t *testing.T) {
			r, _ := newCloudRegistry(t)
			uid, _, cookies, csrf := cloudSession(t, r, "safety-command")
			pv := mkGraphVersion(t, r, uid, bpTestGraph())
			var runs []control.ProfileRun
			var cps []control.BlockCheckpoint
			for i := 0; i < 2; i++ {
				run, err := r.control.CreateRun(control.ProfileRun{UserID: uid, ProfileVersionID: pv.ID, ProfileID: pv.ProfileID, Entry: "initial", State: control.RunPaused})
				if err != nil {
					t.Fatal(err)
				}
				cp, err := r.control.SaveCheckpoint(control.BlockCheckpoint{RunID: run.ID, NodeID: "ready_initial",
					State: control.CheckpointWaiting, OutputJSON: `{"preserve":true}`, LeaseGeneration: 2})
				if err != nil {
					t.Fatal(err)
				}
				runs, cps = append(runs, run), append(cps, cp)
			}
			call := func(i int, key string, revision int64, decision string) *httptestResponse {
				body := fmt.Sprintf(`{"expected_revision":%d}`, revision)
				if command == "resume" {
					body = fmt.Sprintf(`{"expected_revision":%d,"checkpoint_id":%d,"decision":%q}`, revision, cps[i].ID, decision)
				}
				return cloudCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/profile-runs/%d/%s", runs[i].ID, command),
					cookies, csrf, body, map[string]string{"Idempotency-Key": key})
			}
			if rec := call(0, "shared", 1, "acknowledge"); rec.Code != http.StatusAccepted {
				t.Fatalf("winner: %d %s", rec.Code, rec.Body)
			}
			for _, rec := range []*httptestResponse{
				call(1, "shared", 1, "acknowledge"), call(0, "shared", 99, "acknowledge"), call(1, "stale", 99, "retry_safe"),
			} {
				if rec.Code != http.StatusConflict {
					t.Fatalf("conflict: %d %s", rec.Code, rec.Body)
				}
			}
			got, err := r.control.RunByID(runs[1].ID)
			if err != nil || !reflect.DeepEqual(got, runs[1]) {
				t.Fatalf("HTTP 409 mutated run: %+v %v", got, err)
			}
			gotCP, err := r.control.CheckpointByNode(runs[1].ID, cps[1].NodeID, 0)
			if err != nil || !reflect.DeepEqual(gotCP, cps[1]) {
				t.Fatalf("HTTP 409 mutated checkpoint: %+v %v", gotCP, err)
			}
			if rec := call(0, "shared", 1, "acknowledge"); rec.Code != http.StatusAccepted {
				t.Fatalf("replay: %d %s", rec.Code, rec.Body)
			}
			winner, err := r.control.RunByID(runs[0].ID)
			if err != nil || winner.Revision != 2 {
				t.Fatalf("replay changed run: %+v %v", winner, err)
			}
		})
	}
}
