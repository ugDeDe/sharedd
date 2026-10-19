package server

// Fleet autopilot worker + uncertain-resource reconciler. The worker is
// deliberately sequential per item: a paid create is never retried
// automatically, a transient install error gets at most one retry, and one
// failed item never stops the rest of the batch.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"sharedd/registry/internal/blueprint"
	"sharedd/registry/internal/control"
	"sharedd/registry/internal/nodehost"
	"sharedd/registry/internal/provision"
)

// fleetReconcileInterval is also the per-resource throttle window.
const fleetReconcileInterval = 60 * time.Second

type fleetSafeError string

func (e fleetSafeError) Error() string { return string(e) }

func fleetSafe(code string) error { return fleetSafeError(code) }

// fleetErrCode maps any internal error to a short, safe DTO code. Raw provider
// or secret-bearing errors never reach the API.
func fleetErrCode(err error) string {
	var se fleetSafeError
	if errors.As(err, &se) {
		code := string(se)
		if len(code) > 64 {
			return "provider_error"
		}
		return code
	}
	switch {
	case errors.Is(err, errIdemConflict):
		return "idempotency_conflict"
	case errors.Is(err, errCloudNotImplemented):
		return "not_implemented"
	case errors.Is(err, errCloudResourceBusy):
		return "resource_busy"
	case errors.Is(err, errCloudNoPassword):
		return "ssh_unavailable"
	case errors.Is(err, control.ErrQuotaExceeded):
		return "quota_exceeded"
	case errors.Is(err, control.ErrUserDisabled):
		return "user_disabled"
	case errors.Is(err, control.ErrOwnerMismatch):
		return "owner_mismatch"
	case errors.Is(err, control.ErrCreateInProgress):
		return "in_progress"
	case errors.Is(err, control.ErrRevisionConflict):
		return "revision_conflict"
	case errors.Is(err, control.ErrValidation):
		return "validation_failed"
	case errors.Is(err, control.ErrNotFound):
		return "not_found"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "timeout"
	default:
		return "provider_error"
	}
}

// startFleetWorker — one in-flight worker per fleet id (dedicated namespace;
// fleet ids never collide with operation-id worker keys). false = already
// running.
func (r *Registry) startFleetWorker(fleetID int64) bool {
	r.fleetMu.Lock()
	if r.fleetWorkers == nil {
		r.fleetWorkers = map[int64]bool{}
	}
	if r.fleetWorkers[fleetID] {
		r.fleetMu.Unlock()
		return false
	}
	r.fleetWorkers[fleetID] = true
	r.fleetMu.Unlock()

	r.fleetWG.Add(1)
	go func() {
		defer r.fleetWG.Done()
		defer func() {
			r.fleetMu.Lock()
			delete(r.fleetWorkers, fleetID)
			r.fleetMu.Unlock()
		}()
		defer func() {
			if p := recover(); p != nil {
				log.Printf("fleet worker panic fleet=%d: %v", fleetID, p)
				_ = r.control.SetFleetState(fleetID, control.FleetFailed)
			}
		}()
		r.runFleet(r.provisionWorkers().context(), fleetID)
	}()
	return true
}

func (r *Registry) runFleet(ctx context.Context, fleetID int64) {
	f, err := r.control.Fleet(fleetID)
	if err != nil {
		log.Printf("fleet %d: load: %v", fleetID, err)
		return
	}
	pv, err := r.control.ProfileVersionByNumericID(f.ProfileVersionID)
	if err != nil {
		_ = r.control.SetFleetState(f.ID, control.FleetFailed)
		r.failFleetItems(f.ID, "profile_unavailable")
		return
	}
	var cfg nodehost.CreateRequest
	if err := json.Unmarshal([]byte(f.ConfigurationJSON), &cfg); err != nil {
		_ = r.control.SetFleetState(f.ID, control.FleetFailed)
		r.failFleetItems(f.ID, "bad_configuration")
		return
	}
	graphVersionID := int64(0)
	if f.AutoReplace {
		graphVersionID, err = r.systemGraphVersionID(blueprint.SystemNodehostReplacement)
		if err != nil {
			log.Printf("fleet %d: system graph unavailable: %v", f.ID, err)
			_ = r.control.SetFleetState(f.ID, control.FleetFailed)
			r.failFleetItems(f.ID, "system_graph_unavailable")
			return
		}
	}

	items, err := r.control.FleetItems(f.ID)
	if err != nil {
		log.Printf("fleet %d: items: %v", f.ID, err)
		return
	}
	for _, it := range items {
		if ctx.Err() != nil {
			break
		}
		if it.State == control.FleetItemAdmitted || it.State == control.FleetItemError {
			continue
		}
		r.runFleetItem(ctx, f, it, pv, cfg, graphVersionID)
	}
	r.finalizeFleet(f.ID)
}

func (r *Registry) failFleetItems(fleetID int64, code string) {
	items, err := r.control.FleetItems(fleetID)
	if err != nil {
		return
	}
	for _, it := range items {
		if it.State == control.FleetItemAdmitted {
			continue
		}
		_ = r.control.SetFleetItemState(fleetID, it.Idx, control.FleetItemError, "", code)
	}
}

func (r *Registry) finalizeFleet(fleetID int64) {
	items, err := r.control.FleetItems(fleetID)
	if err != nil {
		return
	}
	admitted, failed := 0, 0
	for _, it := range items {
		switch it.State {
		case control.FleetItemAdmitted:
			admitted++
		case control.FleetItemError:
			failed++
		}
	}
	state := control.FleetPartial
	switch {
	case admitted == len(items) && len(items) > 0:
		state = control.FleetSucceeded
	case failed == len(items):
		state = control.FleetFailed
	case admitted == 0 && failed == 0:
		state = control.FleetRunning
	}
	_ = r.control.SetFleetState(fleetID, state)
}

// runFleetItem is one slot: create → ready → install → optional policy. Every
// failure is recorded and returns; the caller continues with the next item.
func (r *Registry) runFleetItem(ctx context.Context, f control.Fleet, it control.FleetItem, pv control.ProfileVersion, cfg nodehost.CreateRequest, graphVersionID int64) {
	u := control.User{ID: f.UserID}
	// Каждый элемент получает собственное имя/хостнейм из label: configuration
	// во флоте общая, а пустое name из формы — норма.
	cfg.Name = it.Label
	_ = r.control.SetFleetItemState(f.ID, it.Idx, control.FleetItemCreating, "", "")

	// Stable per-item create key: a replay of the fleet/worker can never issue
	// a second paid Create for the same slot.
	hashBody := struct {
		ConnectionID  int64                  `json:"connection_id"`
		Label         string                 `json:"label"`
		Configuration nodehost.CreateRequest `json:"configuration"`
	}{f.ConnectionID, it.Label, cfg}
	ack, err := r.cloudCreateAccept(u, f.ConnectionID, it.Label, cfg,
		fmt.Sprintf("fleet:%d:%d", f.ID, it.Idx), cloudBodyHash(hashBody), 0)
	if err != nil {
		r.fleetItemError(f.ID, it.Idx, "create", err)
		return
	}
	if err := r.control.SetFleetItemResource(f.ID, it.Idx, ack.ResourceID); err != nil {
		r.fleetItemError(f.ID, it.Idx, "control", err)
		return
	}
	res, err := r.waitFleetResource(ctx, f.UserID, ack.ResourceID)
	if err != nil {
		r.fleetItemError(f.ID, it.Idx, "create", err)
		return
	}

	_ = r.control.SetFleetItemState(f.ID, it.Idx, control.FleetItemInstalling, "", "")
	// A resumed fleet may find an install that already completed; reuse it
	// instead of silently reinstalling the agent.
	var done control.Operation
	if op, err := r.control.OperationByIdemKey(control.CloudInstallIdem(res.ID)); err == nil && op.State == control.OpDone && op.NodeID != "" {
		done = op
	} else {
		op, err := r.startCloudInstall(u, res, pv, nil)
		if err != nil {
			r.fleetItemError(f.ID, it.Idx, "install", err)
			return
		}
		// Install transient errors get at most one retry. The install
		// operation is reusable from failed; claim-on-first-register keeps the
		// same identity/token.
		done, err = r.waitFleetInstall(ctx, op.ID, pv)
		if err == nil && done.NodeID == "" {
			err = fleetSafe("node_not_registered")
		}
		if err != nil {
			done, err = r.waitFleetInstallRetry(ctx, u, res, pv, op.ID, err)
		}
		if err != nil {
			r.fleetItemError(f.ID, it.Idx, "install", err)
			return
		}
	}
	if done.NodeID == "" {
		r.fleetItemError(f.ID, it.Idx, "install", fleetSafe("node_not_registered"))
		return
	}
	_ = r.control.SetFleetItemState(f.ID, it.Idx, control.FleetItemInstalling, done.NodeID, "")

	res, err = r.control.CloudResourceOwned(ack.ResourceID, f.UserID)
	if err != nil {
		r.fleetItemError(f.ID, it.Idx, "control", err)
		return
	}
	if f.AutoReplace {
		if err := r.armFleetPolicy(f, res, graphVersionID); err != nil {
			r.fleetItemError(f.ID, it.Idx, "policy", err)
			return
		}
	} else if err := r.ensureFleetManual(f, res); err != nil {
		r.fleetItemError(f.ID, it.Idx, "policy", err)
		return
	}
	// Admission: the install operation is done and the node registered. Full
	// queue health is checked when available; right after registration it may
	// still be warming up, which must not turn a successful install into an
	// error (documented deviation in fleet-autopilot.md).
	if err := r.blueprintResourceReady(res); err != nil {
		log.Printf("fleet %d item %d: admitted after registration without full health proof: %v", f.ID, it.Idx, err)
	}
	_ = r.control.SetFleetItemState(f.ID, it.Idx, control.FleetItemAdmitted, done.NodeID, "")
}

func (r *Registry) fleetItemError(fleetID int64, idx int, stage string, err error) {
	code := fleetErrCode(err)
	log.Printf("fleet %d item %d: %s failed: %s", fleetID, idx, stage, code)
	_ = r.control.SetFleetItemState(fleetID, idx, control.FleetItemError, "", code)
}

func (r *Registry) waitFleetResource(ctx context.Context, userID, resourceID int64) (control.CloudResourceV2, error) {
	deadline := time.Now().Add(cloudCreateBudget + cloudPollInterval)
	for {
		res, err := r.control.CloudResourceOwned(resourceID, userID)
		if err != nil {
			return control.CloudResourceV2{}, err
		}
		switch res.State {
		case control.CloudResReady:
			return res, nil
		case control.CloudResError:
			code := res.LastError
			if code == "" {
				code = "provider_error"
			}
			return res, fleetSafe(code)
		case control.CloudResDeleted:
			return res, fleetSafe("deleted")
		}
		if time.Now().After(deadline) {
			return res, fleetSafe("provider_timeout")
		}
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(cloudPollInterval):
		}
	}
}

func (r *Registry) fleetInstallBudget(pv control.ProfileVersion) time.Duration {
	prov := provision.New(r.control, nil, r.provisionConfig())
	return prov.ProvisionBudget(len(control.ParseProfileSteps(pv.Steps))) + time.Minute
}

func (r *Registry) waitFleetInstall(ctx context.Context, opID int64, pv control.ProfileVersion) (control.Operation, error) {
	deadline := time.Now().Add(r.fleetInstallBudget(pv))
	for {
		op, err := r.control.Operation(opID)
		if err != nil {
			return op, err
		}
		switch op.State {
		case control.OpDone:
			return op, nil
		case control.OpFailed:
			code := op.LastError
			if code == "" {
				code = "install_failed"
			}
			return op, fleetSafe(code)
		case control.OpDeleted:
			return op, fleetSafe("install_deleted")
		}
		if time.Now().After(deadline) {
			return op, fleetSafe("install_timeout")
		}
		select {
		case <-ctx.Done():
			return op, ctx.Err()
		case <-time.After(cloudPollInterval):
		}
	}
}

// waitFleetInstallRetry re-accepts the install exactly once. A create/install
// timeout is never retried here (paid outcome uncertainty is reconciled, not
// re-issued).
func (r *Registry) waitFleetInstallRetry(ctx context.Context, u control.User, res control.CloudResourceV2, pv control.ProfileVersion, opID int64, origErr error) (control.Operation, error) {
	op, err := r.control.Operation(opID)
	if err != nil {
		return op, origErr
	}
	if op.State != control.OpFailed {
		return op, origErr
	}
	switch op.LastError {
	case "node did not register with the registry", "provider_reconcile_required",
		"worker_panic", "worker_busy":
		return op, origErr
	}
	again, err := r.startCloudInstall(u, res, pv, nil)
	if err != nil {
		return again, err
	}
	return r.waitFleetInstall(ctx, again.ID, pv)
}

func (r *Registry) armFleetPolicy(f control.Fleet, res control.CloudResourceV2, graphVersionID int64) error {
	if res.NodeID == "" {
		return fleetSafe("node_not_registered")
	}
	pv, err := r.control.ProfileVersionByNumericID(graphVersionID)
	if err != nil {
		return err
	}
	if pv.Format != control.FormatGraph {
		return fleetSafe("system_graph_unavailable")
	}
	bp, ds := blueprint.ValidateBytes([]byte(pv.Blueprint))
	if len(ds) > 0 {
		return fleetSafe("system_graph_invalid")
	}
	if bp.Provider != res.Provider {
		return fleetSafe("system_graph_incompatible")
	}
	inputs := map[string]json.RawMessage{
		"connection_id":                   json.RawMessage(fmt.Sprintf("%d", f.ConnectionID)),
		"configuration_ref":               json.RawMessage(f.ConfigurationJSON),
		"installation_profile_version_id": json.RawMessage(fmt.Sprintf("%d", f.ProfileVersionID)),
	}
	if ds := blueprint.ValidateInputs(bp, inputs); len(ds) > 0 {
		return fleetSafe("system_graph_inputs")
	}
	rev, cur := int64(1), int64(0)
	if m, err := r.control.NodeManagement(res.NodeID, f.UserID); err == nil {
		rev, cur = m.Revision, m.CurrentResourceID
	} else if !errors.Is(err, control.ErrNotFound) {
		return err
	}
	if cur == 0 {
		cur = res.ID
	} else {
		cur = res.ID // this fleet resource is the node's current managed server
	}
	inputsJSON, _ := json.Marshal(inputs)
	_, err = r.control.SetNodeManagementProfile(res.NodeID, f.UserID, rev, pv.ID, string(inputsJSON), false, cur, time.Now().Unix())
	return err
}

func (r *Registry) ensureFleetManual(f control.Fleet, res control.CloudResourceV2) error {
	m, err := r.control.NodeManagement(res.NodeID, f.UserID)
	if errors.Is(err, control.ErrNotFound) {
		return nil // default is manual; do not create a policy row
	}
	if err != nil {
		return err
	}
	if m.Mode == "manual" {
		return nil
	}
	_, err = r.control.PatchNodeManagement(res.NodeID, f.UserID, m.Revision, "manual", 0, false, m.CurrentResourceID)
	return err
}

// ── auto-reconcile ────────────────────────────────────────────────

func (r *Registry) fleetReconcileLoop(ctx context.Context) {
	t := time.NewTicker(fleetReconcileInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.reconcileUncertainCloudResources(ctx)
		}
	}
}

// reconcileUncertainCloudResources — no-button cleanup of rows whose paid
// outcome is unknown. It only deletes when provider inventory proves absence;
// nothing is ever re-created here. Per-resource throttle prevents a 60s tick
// from hammering the provider.
func (r *Registry) reconcileUncertainCloudResources(ctx context.Context) {
	if r.control == nil {
		return
	}
	list, err := r.control.CloudResourcesUncertain()
	if err != nil {
		log.Printf("fleet reconcile: list uncertain: %v", err)
		return
	}
	type inv struct {
		servers map[string]nodehost.Server
		err     error
	}
	cache := map[int64]inv{}
	now := time.Now()
	for _, res := range list {
		if ctx.Err() != nil {
			return
		}
		if r.cloudWorkerActive(res) {
			continue
		}
		if r.fleetThrottled(res.ID, now) {
			continue
		}
		conn, err := r.control.HostingConnectionOwned(res.ConnectionID, res.UserID)
		if err != nil || conn.Provider != control.ProviderNodehost {
			continue
		}
		got, ok := cache[conn.ID]
		if !ok {
			prov, err := r.cloudProviderFor(conn)
			if err != nil {
				continue
			}
			servers, err := cloudAllServers(ctx, prov)
			got = inv{servers: map[string]nodehost.Server{}, err: err}
			for _, s := range servers {
				if s.ID != "" {
					got.servers[s.ID] = s
				}
			}
			cache[conn.ID] = got
		}
		if got.err != nil {
			continue
		}
		if res.ExternalID != "" {
			if _, found := got.servers[res.ExternalID]; !found {
				r.deleteUncertainResource(res)
			}
			continue
		}
		claimed, err := r.control.CloudResourceClaimedExternalIDs(res.UserID, res.ConnectionID, res.ID)
		if err != nil {
			continue
		}
		unaccounted := 0
		for id := range got.servers {
			if !claimed[id] {
				unaccounted++
			}
		}
		if unaccounted == 0 {
			r.deleteUncertainResource(res)
		}
	}
}

func (r *Registry) fleetThrottled(resourceID int64, now time.Time) bool {
	r.fleetReconcileMu.Lock()
	defer r.fleetReconcileMu.Unlock()
	if r.fleetReconcileAt == nil {
		r.fleetReconcileAt = map[int64]time.Time{}
	}
	if last, ok := r.fleetReconcileAt[resourceID]; ok && now.Sub(last) < fleetReconcileInterval {
		return true
	}
	r.fleetReconcileAt[resourceID] = now
	return false
}

func (r *Registry) deleteUncertainResource(res control.CloudResourceV2) {
	if err := r.control.UpdateCloudResourceState(res.ID, res.State, control.CloudResDeleted, "", ""); err != nil {
		log.Printf("fleet reconcile: resource %d delete: %v", res.ID, err)
		return
	}
	if _, err := r.control.ReleasePendingSecretsForCloudResource(res.ID); err != nil {
		log.Printf("fleet reconcile: release pending for resource %d: %v", res.ID, err)
	}
}
