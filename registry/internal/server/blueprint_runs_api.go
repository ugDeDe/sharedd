package server

// Profile-run routes (hosting-blueprints stage-08b-i, 03 §6): create, read,
// cancel and resume. Ownership is always enforced via user_id; CSRF is
// required for every mutation; Idempotency-Key is user-scoped.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"sharedd/registry/internal/blueprint"
	"sharedd/registry/internal/control"
)

const (
	opKindProfileRun       = "profile_run"
	opKindProfileRunCancel = "profile_run_cancel"
	opKindProfileRunResume = "profile_run_resume"
)

type checkpointJSON struct {
	ID            int64           `json:"id"`
	NodeID        string          `json:"node_id"`
	Invocation    int64           `json:"invocation"`
	State         string          `json:"state"`
	Attempt       int             `json:"attempt"`
	Error         string          `json:"error,omitempty"`
	Output        json.RawMessage `json:"output,omitempty"`
	NextAttemptAt int64           `json:"next_attempt_at"`
}

type runJSON struct {
	ID               int64            `json:"id"`
	OperationID      *int64           `json:"operation_id,omitempty"`
	ProfileVersionID int64            `json:"profile_version_id"`
	ProfileID        int64            `json:"profile_id"`
	NodeID           string           `json:"node_id,omitempty"`
	Entry            string           `json:"entry"`
	State            string           `json:"state"`
	Revision         int64            `json:"revision"`
	ResourceID       *int64           `json:"resource_id"`
	PolicyRevision   int64            `json:"policy_revision"`
	CancelRequested  bool             `json:"cancel_requested"`
	NextWakeAt       int64            `json:"next_wake_at,omitempty"`
	LastError        string           `json:"last_error,omitempty"`
	Waiting          *runWaitingJSON  `json:"waiting,omitempty"`
	Checkpoints      []checkpointJSON `json:"checkpoints,omitempty"`
}

type runWaitingJSON struct {
	Type           string `json:"type"`
	AfterEpisodeID string `json:"after_episode_id,omitempty"`
}

func runView(run control.ProfileRun, cps []control.BlockCheckpoint) runJSON {
	v := runJSON{
		ID: run.ID, ProfileVersionID: run.ProfileVersionID, ProfileID: run.ProfileID,
		NodeID: run.NodeID, Entry: run.Entry, State: run.State, Revision: run.Revision,
		PolicyRevision: run.PolicyRevision, CancelRequested: run.CancelRequested,
		NextWakeAt: run.NextWakeAt, LastError: run.LastError,
	}
	if run.CurrentResourceID > 0 {
		x := run.CurrentResourceID
		v.ResourceID = &x
	}
	if run.OperationID > 0 {
		x := run.OperationID
		v.OperationID = &x
	}
	if run.State == control.RunWaitingEvent {
		var in struct {
			EpisodeID string `json:"episode_id"`
		}
		_ = json.Unmarshal([]byte(run.InputsJSON), &in)
		v.Waiting = &runWaitingJSON{Type: "ip_ban", AfterEpisodeID: in.EpisodeID}
	}
	for _, cp := range cps {
		c := checkpointJSON{ID: cp.ID, NodeID: cp.NodeID, Invocation: cp.Invocation,
			State: cp.State, Attempt: cp.Attempt, Error: cp.Error, NextAttemptAt: cp.NextAttemptAt}
		if json.Valid([]byte(cp.OutputJSON)) {
			c.Output = json.RawMessage(cp.OutputJSON)
		}
		v.Checkpoints = append(v.Checkpoints, c)
	}
	return v
}

func (r *Registry) mountBlueprintRuns(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/me/profile-runs", r.requireSession(r.requireCSRF(r.handleProfileRunCreate)))
	mux.HandleFunc("GET /api/me/profile-runs/{id}", r.requireSession(r.handleProfileRunGet))
	mux.HandleFunc("POST /api/me/profile-runs/{id}/cancel", r.requireSession(r.requireCSRF(r.handleProfileRunCancel)))
	mux.HandleFunc("POST /api/me/profile-runs/{id}/resume", r.requireSession(r.requireCSRF(r.handleProfileRunResume)))
}

func (r *Registry) profileRunOwned(w http.ResponseWriter, req *http.Request) (control.ProfileRun, bool) {
	id, ok := parseID(req.PathValue("id"))
	if !ok {
		controlAPIError(w, http.StatusNotFound, "not_found", "run not found")
		return control.ProfileRun{}, false
	}
	run, err := r.control.RunOwned(id, sessionFrom(req).user.ID)
	if err != nil {
		if errors.Is(err, control.ErrNotFound) {
			controlAPIError(w, http.StatusNotFound, "not_found", "run not found")
			return control.ProfileRun{}, false
		}
		r.writeControlUnavailable(w)
		return control.ProfileRun{}, false
	}
	return run, true
}

// writeRunAck persists the operation ack body and returns 202.
func writeRunAck(w http.ResponseWriter, opID, runID int64, state string, resourceID int64) {
	out := map[string]any{"operation_id": opID, "run_id": runID, "state": state}
	if resourceID > 0 {
		out["resource_id"] = resourceID
	} else {
		out["resource_id"] = nil
	}
	writeCabinetJSON(w, http.StatusAccepted, out)
}

type profileRunCreateBody struct {
	ProfileID int64 `json:"profile_id"`
	// ProfileVersionID is the legacy alias: resolved to its ProfileID.
	ProfileVersionID int64                      `json:"profile_version_id"`
	Entry            string                     `json:"entry"`
	ResourceID       *int64                     `json:"resource_id"`
	NodeID           string                     `json:"node_id"`
	Inputs           map[string]json.RawMessage `json:"inputs"`
	Vars             map[string]any             `json:"vars"`
	Confirm          bool                       `json:"confirm"`
}

func (r *Registry) handleProfileRunCreate(w http.ResponseWriter, req *http.Request) {
	u := sessionFrom(req).user
	idemKey, ok := cloudIdemKey(w, req)
	if !ok {
		return
	}
	var body profileRunCreateBody
	if err := decodeCabinetJSONLimit(w, req, &body, 256<<10); err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "bad json")
		return
	}
	if !body.Confirm {
		controlAPIError(w, http.StatusBadRequest, "confirmation_required", "confirm:true is required")
		return
	}
	opKey := fmt.Sprintf("profile_run:%d:%s", u.ID, idemKey)
	bodyHash := cloudBodyHash(body)
	if op, err := r.control.OperationByIdemKey(opKey); err == nil {
		r.replayProfileRunAck(w, op, bodyHash)
		return
	} else if !errors.Is(err, control.ErrNotFound) {
		r.writeControlUnavailable(w)
		return
	}

	// Resolve the profile identity: profile_id is authoritative; the legacy
	// profile_version_id only tells us which profile it belongs to. The single
	// content row of that profile is what gets pinned.
	profileID := body.ProfileID
	if profileID == 0 && body.ProfileVersionID > 0 {
		row, err := r.control.ProfileVersionByNumericID(body.ProfileVersionID)
		if err != nil {
			controlAPIError(w, http.StatusUnprocessableEntity, "profile_invalid", "profile version is not available")
			return
		}
		profileID = row.ProfileID
	}
	if profileID <= 0 {
		controlAPIError(w, http.StatusUnprocessableEntity, "profile_invalid", "profile_id is required")
		return
	}
	pv, err := r.control.ProfileVersion(profileID, 0)
	if err != nil {
		controlAPIError(w, http.StatusUnprocessableEntity, "profile_invalid", "profile is not available")
		return
	}
	if pv.Format != control.FormatGraph {
		controlAPIError(w, http.StatusUnprocessableEntity, "profile_invalid", "profile version must be format=graph")
		return
	}
	p, err := r.control.Profile(pv.ProfileID)
	if err != nil || (!p.Published && p.CreatedBy != u.ID) {
		controlAPIError(w, http.StatusNotFound, "not_found", "profile version is not available")
		return
	}
	bp, ds := blueprint.ValidateBytes([]byte(pv.Blueprint))
	if len(ds) > 0 {
		writeBlueprintInvalid(w, "profile_invalid", "graph failed validation", ds)
		return
	}
	if body.Entry != "initial" && body.Entry != "observe" {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "entry must be initial or observe")
		return
	}
	if body.Inputs == nil {
		body.Inputs = map[string]json.RawMessage{}
	}
	if ds := blueprint.ValidateInputs(bp, body.Inputs); len(ds) > 0 {
		writeBlueprintInvalid(w, "profile_invalid", "inputs do not match the pinned graph", ds)
		return
	}
	// M3 (cheap): validate caller-supplied installation vars against the pinned
	// text version BEFORE any paid create. The install block still re-merges.
	if len(body.Vars) > 0 {
		if raw, ok := body.Inputs["installation_profile_version_id"]; ok {
			var ipvID int64
			if err := json.Unmarshal(raw, &ipvID); err == nil && ipvID > 0 {
				if ipv, err := r.control.ProfileVersionByNumericID(ipvID); err == nil && ipv.Format == control.FormatText {
					if _, err := control.MergeProfileVars(ipv.VarsSchema, body.Vars); err != nil {
						controlAPIError(w, http.StatusUnprocessableEntity, "profile_invalid", "vars do not match the installation profile")
						return
					}
				}
			}
		}
	}

	nodeID := body.NodeID
	if nodeID != "" {
		owner, legacy, ok, err := r.control.NodeOwner(nodeID)
		if err != nil {
			r.writeControlUnavailable(w)
			return
		}
		if ok && (legacy || owner != u.ID) {
			controlAPIError(w, http.StatusNotFound, "not_found", "node not found")
			return
		}
	}
	resourceID := int64(0)
	provider := ""
	if body.ResourceID != nil && *body.ResourceID > 0 {
		res, err := r.control.CloudResourceOwned(*body.ResourceID, u.ID)
		if err != nil {
			controlAPIError(w, http.StatusNotFound, "not_found", "resource not found")
			return
		}
		resourceID = res.ID
		provider = res.Provider
		if nodeID == "" {
			nodeID = res.NodeID
		}
	}
	if raw, ok := body.Inputs["connection_id"]; ok {
		var id int64
		if err := json.Unmarshal(raw, &id); err != nil || id <= 0 {
			controlAPIError(w, http.StatusBadRequest, "validation_failed", "input connection_id must be a number")
			return
		}
		conn, err := r.control.HostingConnectionOwned(id, u.ID)
		if err != nil {
			controlAPIError(w, http.StatusNotFound, "not_found", "connection not found")
			return
		}
		provider = conn.Provider
	}

	policyRevision := int64(0)
	policyID := int64(0)
	if body.Entry == "observe" {
		if nodeID == "" {
			controlAPIError(w, http.StatusUnprocessableEntity, "profile_incompatible", "observe requires a node_id or resource_id")
			return
		}
		m, err := r.control.NodeManagement(nodeID, u.ID)
		if err != nil || m.Mode != "profile" || m.ProfileVersionID != pv.ID {
			controlAPIError(w, http.StatusUnprocessableEntity, "profile_incompatible", "node is not in profile mode for this version")
			return
		}
		if resourceID == 0 {
			resourceID = m.CurrentResourceID
		}
		policyRevision = m.Revision
		if pid, err := r.control.NodeManagementPolicyID(nodeID); err == nil {
			policyID = pid
		}
		// Observe runs use the policy input snapshot; a caller-provided body
		// must match it exactly.
		if m.InputsJSON != "" && m.InputsJSON != "{}" {
			var policyInputs map[string]json.RawMessage
			if err := json.Unmarshal([]byte(m.InputsJSON), &policyInputs); err != nil {
				controlAPIError(w, http.StatusUnprocessableEntity, "profile_incompatible", "policy inputs are unreadable")
				return
			}
			if len(body.Inputs) == 0 {
				body.Inputs = policyInputs
			} else {
				a, _ := json.Marshal(body.Inputs)
				b, _ := json.Marshal(policyInputs)
				if string(a) != string(b) {
					controlAPIError(w, http.StatusUnprocessableEntity, "profile_incompatible", "inputs do not match the active policy")
					return
				}
			}
		}
		if provider == "" && m.CurrentResourceID > 0 {
			if res, err := r.control.CloudResourceOwned(m.CurrentResourceID, u.ID); err == nil {
				provider = res.Provider
			}
		}
	} else if resourceID != 0 {
		controlAPIError(w, http.StatusUnprocessableEntity, "profile_incompatible", "initial entry does not accept an existing resource")
		return
	}
	if provider == "" || bp.Provider != provider {
		controlAPIError(w, http.StatusUnprocessableEntity, "profile_incompatible", "pinned graph provider does not match the node")
		return
	}

	if policyID == 0 && nodeID != "" {
		if pid, err := r.control.NodeManagementPolicyID(nodeID); err == nil {
			policyID = pid
		}
	}
	inputsJSON, _ := json.Marshal(body.Inputs)
	varsJSON, _ := json.Marshal(body.Vars)
	// C1: the operation row is the atomic gate. The run is inserted in the
	// same transaction and only when this request won the UNIQUE idem_key.
	run, op, created, err := r.control.CreateRunIdempotent(control.Operation{
		UserID: u.ID, NodeID: nodeID, Kind: opKindProfileRun, State: control.OpPending,
		IdemKey: opKey,
	}, control.ProfileRun{
		UserID: u.ID, ProfileVersionID: pv.ID, ProfileID: pv.ProfileID,
		NodeID: nodeID, Entry: body.Entry, State: control.RunPending,
		InputsJSON: string(inputsJSON), VarsJSON: string(varsJSON),
		CurrentResourceID: resourceID, PolicyID: policyID, PolicyRevision: policyRevision,
	}, bodyHash)
	if err != nil {
		if errors.Is(err, control.ErrDeploymentUnresolved) {
			controlAPIError(w, http.StatusConflict, "deployment_unresolved", "named deployment has an active run or nondeleted resources")
			return
		}
		r.writeControlUnavailable(w)
		return
	}
	if !created {
		// Concurrent duplicate of the same key: reuse its run when possible.
		r.replayProfileRunAck(w, op, bodyHash)
		return
	}
	r.launchBlueprintRun(req.Context(), run.ID, run.Revision)
	writeRunAck(w, op.ID, run.ID, control.RunPending, resourceID)
}

func (r *Registry) replayProfileRunAck(w http.ResponseWriter, op control.Operation, bodyHash string) {
	if op.Kind != opKindProfileRun {
		controlAPIError(w, http.StatusConflict, "idempotency_conflict", "idempotency key was used for a different command")
		return
	}
	var meta struct {
		RunID    int64  `json:"run_id"`
		BodyHash string `json:"body_hash"`
	}
	_ = json.Unmarshal([]byte(op.RequestJSON), &meta)
	if meta.RunID == 0 || meta.BodyHash != bodyHash {
		controlAPIError(w, http.StatusConflict, "idempotency_conflict", "idempotency key was reused with a different request body")
		return
	}
	run, err := r.control.RunOwned(meta.RunID, op.UserID)
	if err != nil {
		controlAPIError(w, http.StatusConflict, "idempotency_conflict", "idempotency key has no usable run")
		return
	}
	writeRunAck(w, op.ID, run.ID, run.State, run.CurrentResourceID)
}

func runCommandMatches(op control.Operation, runID int64, hash string) bool {
	var meta struct {
		RunID    int64  `json:"run_id"`
		BodyHash string `json:"body_hash"`
	}
	if json.Unmarshal([]byte(op.RequestJSON), &meta) != nil {
		return false
	}
	return meta.RunID == runID && meta.BodyHash == hash
}

func (r *Registry) handleProfileRunGet(w http.ResponseWriter, req *http.Request) {
	run, ok := r.profileRunOwned(w, req)
	if !ok {
		return
	}
	cps, err := r.control.CheckpointsByRun(run.ID)
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	writeCabinetJSON(w, http.StatusOK, runView(run, cps))
}

func (r *Registry) handleProfileRunCancel(w http.ResponseWriter, req *http.Request) {
	u := sessionFrom(req).user
	run, ok := r.profileRunOwned(w, req)
	if !ok {
		return
	}
	idemKey, ok := cloudIdemKey(w, req)
	if !ok {
		return
	}
	var body struct {
		ExpectedRevision int64 `json:"expected_revision"`
	}
	if err := decodeCabinetJSON(w, req, &body); err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "bad json")
		return
	}
	opKey := fmt.Sprintf("profile_run_cancel:%d:%s", u.ID, idemKey)
	bodyHash := cloudBodyHash(body)
	if op, err := r.control.OperationByIdemKey(opKey); err == nil {
		if op.Kind != opKindProfileRunCancel || !runCommandMatches(op, run.ID, bodyHash) {
			controlAPIError(w, http.StatusConflict, "idempotency_conflict", "idempotency key was reused for a different run")
			return
		}
		writeRunAck(w, op.ID, run.ID, run.State, run.CurrentResourceID)
		return
	} else if !errors.Is(err, control.ErrNotFound) {
		r.writeControlUnavailable(w)
		return
	}
	updated, op, _, err := r.control.ApplyRunCommand(control.Operation{
		UserID: u.ID, NodeID: run.NodeID, Kind: opKindProfileRunCancel, IdemKey: opKey,
	}, run.ID, body.ExpectedRevision, bodyHash, "", nil, false)
	if err != nil {
		if errors.Is(err, control.ErrRunCommandConflict) {
			controlAPIError(w, http.StatusConflict, "idempotency_conflict", "idempotency key was reused for a different command")
			return
		}
		if errors.Is(err, control.ErrRevisionConflict) {
			controlAPIError(w, http.StatusConflict, "revision_conflict", "run changed concurrently")
			return
		}
		if errors.Is(err, control.ErrNotFound) {
			controlAPIError(w, http.StatusNotFound, "not_found", "run not found")
			return
		}
		r.writeControlUnavailable(w)
		return
	}
	writeRunAck(w, op.ID, updated.ID, updated.State, updated.CurrentResourceID)
}

func (r *Registry) handleProfileRunResume(w http.ResponseWriter, req *http.Request) {
	u := sessionFrom(req).user
	run, ok := r.profileRunOwned(w, req)
	if !ok {
		return
	}
	idemKey, ok := cloudIdemKey(w, req)
	if !ok {
		return
	}
	var body struct {
		ExpectedRevision int64  `json:"expected_revision"`
		CheckpointID     int64  `json:"checkpoint_id"`
		Decision         string `json:"decision"`
	}
	if err := decodeCabinetJSON(w, req, &body); err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "bad json")
		return
	}
	opKey := fmt.Sprintf("profile_run_resume:%d:%s", u.ID, idemKey)
	bodyHash := cloudBodyHash(body)
	if op, err := r.control.OperationByIdemKey(opKey); err == nil {
		if op.Kind != opKindProfileRunResume || !runCommandMatches(op, run.ID, bodyHash) {
			controlAPIError(w, http.StatusConflict, "idempotency_conflict", "idempotency key was reused for a different run")
			return
		}
		writeRunAck(w, op.ID, run.ID, run.State, run.CurrentResourceID)
		return
	} else if !errors.Is(err, control.ErrNotFound) {
		r.writeControlUnavailable(w)
		return
	}
	if body.Decision != "reconcile" && body.Decision != "retry_safe" && body.Decision != "acknowledge" {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "decision must be reconcile, retry_safe or acknowledge")
		return
	}
	cps, err := r.control.CheckpointsByRun(run.ID)
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	var cp *control.BlockCheckpoint
	for i := range cps {
		if cps[i].ID == body.CheckpointID {
			cp = &cps[i]
			break
		}
	}
	if cp == nil {
		controlAPIError(w, http.StatusNotFound, "not_found", "checkpoint not found")
		return
	}
	if cp.State == control.CheckpointConfirmed {
		controlAPIError(w, http.StatusConflict, "unsafe_resume", "checkpoint already completed")
		return
	}
	if body.Decision == "reconcile" &&
		cp.State != control.CheckpointDispatched && cp.State != control.CheckpointUnknown && cp.State != control.CheckpointClaimed {
		// H1: reconcile is read-only and only meaningful for a checkpoint whose
		// intent may already have left the process. Anything else (notably a
		// pre-fix `rejected`) must not be turned into a re-dispatch.
		controlAPIError(w, http.StatusConflict, "unsafe_resume", "checkpoint has no dispatched intent to reconcile")
		return
	}

	state := control.RunReconciling
	if body.Decision == "acknowledge" {
		state = control.RunNeedsAttention
	}
	if body.Decision == "retry_safe" {
		// Only read-only allowlisted blocks may be replayed.
		pv, err := r.control.ProfileVersionByNumericID(run.ProfileVersionID)
		if err != nil {
			controlAPIError(w, http.StatusConflict, "unsafe_resume", "pinned graph is unavailable")
			return
		}
		bp, ds := blueprint.ValidateBytes([]byte(pv.Blueprint))
		if len(ds) > 0 {
			controlAPIError(w, http.StatusConflict, "unsafe_resume", "pinned graph is invalid")
			return
		}
		node, ok := nodeOf(bp, cp.NodeID)
		blk, bok := blueprint.BlockFor(node.Type)
		if !ok || !bok || blk.Effects != blueprint.EffectRead {
			controlAPIError(w, http.StatusConflict, "unsafe_resume", "this block is not safe to retry")
			return
		}
		state = control.RunWaitingRetry
	}
	updated, op, created, err := r.control.ApplyRunCommand(control.Operation{
		UserID: u.ID, NodeID: run.NodeID, Kind: opKindProfileRunResume, IdemKey: opKey,
	}, run.ID, body.ExpectedRevision, bodyHash, state, cp, body.Decision == "retry_safe")
	if err != nil {
		if errors.Is(err, control.ErrRunCommandConflict) {
			controlAPIError(w, http.StatusConflict, "idempotency_conflict", "idempotency key was reused for a different command")
			return
		}
		if errors.Is(err, control.ErrRevisionConflict) {
			controlAPIError(w, http.StatusConflict, "revision_conflict", "run changed concurrently")
			return
		}
		r.writeControlUnavailable(w)
		return
	}
	if created && state != control.RunNeedsAttention {
		r.launchBlueprintRun(req.Context(), updated.ID, updated.Revision)
	}
	writeRunAck(w, op.ID, updated.ID, updated.State, updated.CurrentResourceID)
}
