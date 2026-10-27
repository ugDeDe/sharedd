package server

// Restart-resume tests for cloud creates: in-process workers die with the
// registry, so a persisted provider operation id must be enough to finish the
// operation using read-only provider calls (Operation/Server) — never Create.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"sharedd/registry/internal/control"
	"sharedd/registry/internal/nodehost"
)

func (f *fakeCloud) serverCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.servers
}

func cloudStuckCreate(t *testing.T, r *Registry, uid, connID int64) (control.CloudResourceV2, control.Operation) {
	t.Helper()
	res, err := r.control.CreateCloudResource(control.CloudResourceV2{
		UserID: uid, ConnectionID: connID, Provider: control.ProviderNodehost,
		Label: "stuck", State: control.CloudResPlanned,
	})
	if err != nil {
		t.Fatalf("create resource: %v", err)
	}
	if err := r.control.UpdateCloudResourceState(res.ID, control.CloudResPlanned, control.CloudResCreating, "", ""); err != nil {
		t.Fatalf("planned→creating: %v", err)
	}
	op, _, err := r.control.CreateOperation(control.Operation{
		UserID: uid, Kind: control.OpKindCloudCreate, State: control.OpPending,
		IdemKey: fmt.Sprintf("cloud_create:%d:resume-%d", uid, res.ID),
	})
	if err != nil {
		t.Fatalf("create op: %v", err)
	}
	if err := r.control.SetCloudResourceOperation(res.ID, op.ID); err != nil {
		t.Fatalf("link op: %v", err)
	}
	return res, op
}

func waitCloudResource(t *testing.T, r *Registry, uid, resID int64, want string) control.CloudResourceV2 {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		got, err := r.control.CloudResourceOwned(resID, uid)
		if err == nil && got.State == want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("resource %d: want state %q, got %+v err=%v", resID, want, got, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCloudReconcileResumesCreateAfterRestart(t *testing.T) {
	r, fake := newCloudRegistry(t)
	uid, connID, _, _ := cloudSession(t, r, "resume")
	res, op := cloudStuckCreate(t, r, uid, connID)
	if err := r.control.SetOperationProviderOpID(op.ID, "op-ext-1"); err != nil {
		t.Fatalf("persist provider op id: %v", err)
	}

	fake.opStatus = "succeeded"
	fake.serverID = "srv-ext-1"
	fake.serverIP = "203.0.113.10"
	fake.serverStatus = "ACTIVE"
	fake.password = "root-pw"

	r.reconcileStuckCloudCreates(context.Background())

	got := waitCloudResource(t, r, uid, res.ID, control.CloudResReady)
	if got.IP != "203.0.113.10" || got.ExternalID != "srv-ext-1" {
		t.Fatalf("ready row: %+v", got)
	}
	if fake.createCalls() != 0 {
		t.Fatalf("resume must never call Create, creates=%d", fake.createCalls())
	}
	if fake.operationCalls() == 0 || fake.servers == 0 {
		t.Fatalf("resume must poll Operation/Server, ops=%d servers=%d", fake.operationCalls(), fake.serverCalls())
	}
	if gotOp, err := r.control.Operation(op.ID); err != nil || gotOp.State != control.OpConfirmed || gotOp.ProviderVPSID != "srv-ext-1" {
		t.Fatalf("op after resume: %+v err=%v", gotOp, err)
	}
}

func TestCloudDeleteResumeConfirmsAbsentWithoutDelete(t *testing.T) {
	r, fake := newCloudRegistry(t)
	fake.serverNotFound = true
	uid, connID, _, _ := cloudSession(t, r, "delete-resume")
	res, err := r.control.CreateCloudResource(control.CloudResourceV2{
		UserID: uid, ConnectionID: connID, Provider: control.ProviderNodehost,
		Label: "gone", State: control.CloudResReady, ExternalID: "srv-gone", IP: "203.0.113.10",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.control.UpdateCloudResourceState(res.ID, control.CloudResReady, control.CloudResDeleting, "", ""); err != nil {
		t.Fatal(err)
	}
	op, _, err := r.control.CreateOperation(control.Operation{
		UserID: uid, Kind: control.OpKindCloudDelete, State: control.OpSent,
		IdemKey: "delete-resume-key", ProviderVPSID: "srv-gone",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.control.SetCloudResourceOperation(res.ID, op.ID); err != nil {
		t.Fatal(err)
	}
	r.reconcileDeletingCloudResources(context.Background())
	got := waitCloudResource(t, r, uid, res.ID, control.CloudResDeleted)
	if got.ExternalID != "srv-gone" {
		t.Fatalf("external id must survive: %+v", got)
	}
	if fake.deleteCalls() != 0 {
		t.Fatalf("absent VPS must not be deleted again, calls=%d", fake.deleteCalls())
	}
	if gotOp, err := r.control.Operation(op.ID); err != nil || gotOp.State != control.OpDeleted {
		t.Fatalf("delete op: %+v err=%v", gotOp, err)
	}
}

func TestCloudReconcileConfirmedReadsServer(t *testing.T) {
	r, fake := newCloudRegistry(t)
	uid, connID, _, _ := cloudSession(t, r, "resume-confirmed")
	res, op := cloudStuckCreate(t, r, uid, connID)
	if err := r.control.UpdateOperationState(op.ID, control.OpConfirmed, "srv-ext-2", ""); err != nil {
		t.Fatalf("confirm op: %v", err)
	}

	fake.serverIP = "203.0.113.20"
	fake.serverStatus = "ACTIVE"

	r.reconcileStuckCloudCreates(context.Background())

	got := waitCloudResource(t, r, uid, res.ID, control.CloudResReady)
	if got.IP != "203.0.113.20" || got.LastError != "" {
		t.Fatalf("ready row: %+v", got)
	}
	if fake.createCalls() != 0 || fake.operationCalls() != 0 {
		t.Fatalf("confirmed resume must only read Server, creates=%d ops=%d", fake.createCalls(), fake.operationCalls())
	}
}

func TestCloudReconcileFailedOpBecomesError(t *testing.T) {
	r, fake := newCloudRegistry(t)
	uid, connID, _, _ := cloudSession(t, r, "resume-failed")
	res, op := cloudStuckCreate(t, r, uid, connID)
	if err := r.control.UpdateOperationState(op.ID, control.OpFailed, "", "quota_exceeded"); err != nil {
		t.Fatalf("fail op: %v", err)
	}

	r.reconcileStuckCloudCreates(context.Background())

	got, err := r.control.CloudResourceOwned(res.ID, uid)
	if err != nil || got.State != control.CloudResError || got.LastError != "quota_exceeded" {
		t.Fatalf("failed op projection: %+v err=%v", got, err)
	}
	if fake.createCalls() != 0 || fake.operationCalls() != 0 {
		t.Fatalf("failed op must not hit provider, creates=%d ops=%d", fake.createCalls(), fake.operationCalls())
	}
}

func TestCloudCreateResumeReadOnlySafety(t *testing.T) {
	for _, tc := range []struct {
		name, opState, providerID, code string
	}{
		{"missing-pending", control.OpPending, "", "provider_reconcile_required"},
		{"missing-sent", control.OpSent, "", "provider_reconcile_required"},
		{"invalid-schema", control.OpSent, "op-ext", "invalid_response"},
		{"failed-string", control.OpSent, "op-ext", "insufficient_funds"},
		{"ready", control.OpSent, "op-ext", ""},
		{"no-ip", control.OpSent, "op-ext", "no_ip"},
		{"no-password", control.OpSent, "op-ext", "no_password"},
		{"missing-server", control.OpSent, "op-ext", "not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, fake := newCloudRegistry(t)
			uid, connID, cookies, csrf := cloudSession(t, r, "resume-test")
			res, op := cloudStuckCreate(t, r, uid, connID)
			if err := r.control.UpdateOperationState(op.ID, tc.opState, "", ""); err != nil {
				t.Fatal(err)
			}
			if tc.providerID != "" {
				if err := r.control.SetOperationProviderOpID(op.ID, tc.providerID); err != nil {
					t.Fatal(err)
				}
			}
			fake.serverID, fake.serverIP, fake.serverStatus, fake.password = "srv-live", "203.0.113.10", "ACTIVE", "sealed-password"
			switch tc.name {
			case "invalid-schema":
				fake.opErr = &nodehost.Error{Code: "invalid_response"}
			case "failed-string":
				fake.opStatus = "failed"
				var failed nodehost.Operation
				if err := json.Unmarshal([]byte(`{"status":"failed","error":"Insufficient funds: token=DO-NOT-LEAK"}`), &failed); err != nil {
					t.Fatal(err)
				}
				fake.opFailure = failed.Error
			case "no-ip":
				fake.serverIP = ""
			case "no-password":
				fake.password = ""
			case "missing-server":
				fake.serverErr = &nodehost.Error{Code: "not_found", HTTPStatus: http.StatusNotFound}
			}
			if tc.providerID == "" || tc.code == "" {
				r.reconcileStuckCloudCreates(context.Background())
			} else {
				// A single read suffices for definitive errors and readiness gaps;
				// never sleep out the ten-minute production budget in this check.
				r.cloudCreatePoll(context.Background(), res.ID, fake, tc.providerID, op.ID, control.CloudResCreating, time.Now())
			}
			want := control.CloudResError
			if tc.code == "" {
				want = control.CloudResReady
			}
			got := waitCloudResource(t, r, uid, res.ID, want)
			waitWorkerIdle(r, op.ID)
			if got.LastError != tc.code || fake.createCalls() != 0 {
				t.Fatalf("resume: state=%s error=%q creates=%d", got.State, got.LastError, fake.createCalls())
			}
			gotOp, err := r.control.Operation(op.ID)
			if err != nil || gotOp.ProviderOpID != tc.providerID || strings.Contains(gotOp.LastError, "DO-NOT-LEAK") {
				t.Fatalf("durable safe outcome: op=%+v err=%v", gotOp, err)
			}
			if tc.code == "" {
				password, err := r.cloudInstallPassword(got)
				if got.IP == "" || password == "" || err != nil || gotOp.State != control.OpConfirmed {
					t.Fatalf("ready without usable credentials: %+v err=%v", got, err)
				}
			} else if gotOp.State != control.OpFailed {
				t.Fatalf("failure remained in flight: %+v", gotOp)
			}
			if tc.providerID == "" {
				rec := cloudCall(t, r, http.MethodDelete, fmt.Sprintf("/api/me/cloud-resources/%d", res.ID), cookies, csrf, "",
					map[string]string{"Idempotency-Key": "delete-uncertain", confirmDeleteHeader: "delete"})
				if rec.Code != http.StatusConflict || !strings.Contains(rec.Body, "provider_reconcile_required") {
					t.Fatalf("uncertain paid outcome was deletable: %d %s", rec.Code, rec.Body)
				}
			}
		})
	}
}
