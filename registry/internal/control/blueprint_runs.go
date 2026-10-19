package control

// Durable graph-run storage (hosting-blueprints stage-08b-i, 04 §2/§4/§5):
// profile_runs, block_checkpoints, ban_episodes and ban_deliveries.
//
// The engine is deliberately conservative: a run/checkpoint is a journal, not
// a transaction with the external provider. Checkpoints are claimed with a
// lease generation; a stale worker cannot overwrite a newer result.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var ErrDeploymentUnresolved = errors.New("deployment_unresolved")
var ErrRunCommandConflict = errors.New("idempotency_conflict")

// A named deployment is durably identified by (user_id, node_id), including
// before any management policy exists. The operation journal retains resource
// ancestry even when installation binds a different node ID (legacy rows too).
const namedDeploymentWhere = `user_id = ? AND node_id = ? AND entry = 'initial'
 AND ((id <> ? AND state NOT IN ('succeeded','failed','cancelled'))
 OR EXISTS (SELECT 1 FROM cloud_resources2 c WHERE c.user_id = profile_runs.user_id
   AND c.state <> 'deleted' AND (c.id = profile_runs.current_resource_id
     OR c.node_id = profile_runs.node_id
     OR EXISTS (SELECT 1 FROM operations o WHERE o.user_id = profile_runs.user_id
       AND o.idem_key LIKE 'bpcreate:' || profile_runs.id || ':%'
       AND CASE WHEN json_valid(o.request_json) THEN json_extract(o.request_json, '$.resource_id') END = c.id))))`

func (s *Store) CheckNamedDeployment(userID int64, nodeID string, excludeRunID int64) error {
	if nodeID == "" {
		return nil // Explicit unnamed fresh provisioning remains independent.
	}
	var n int
	if err := s.sql.QueryRow(`SELECT COUNT(*) FROM profile_runs WHERE `+namedDeploymentWhere,
		userID, nodeID, excludeRunID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrDeploymentUnresolved
	}
	return nil
}

// Reserve the planned obligation under the same write lock as the deployment
// gate. A cancel/new-run race cannot slip between checking and recording it.
func (s *Store) CreateNamedDeploymentResource(run ProfileRun, resource CloudResourceV2) (CloudResourceV2, error) {
	if run.NodeID == "" || run.UserID != resource.UserID || run.Entry != "initial" ||
		resource.ConnectionID <= 0 || !validHostingProvider(resource.Provider) {
		return CloudResourceV2{}, ErrValidation
	}
	tx, err := s.sql.Begin()
	if err != nil {
		return CloudResourceV2{}, err
	}
	defer func() { _ = tx.Rollback() }()
	// Acquire SQLite's write lock before reading either gate.
	if _, err := tx.Exec(`UPDATE profile_runs SET updated_at = updated_at WHERE id = ?`, run.ID); err != nil {
		return CloudResourceV2{}, err
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM profile_runs WHERE `+namedDeploymentWhere,
		run.UserID, run.NodeID, run.ID).Scan(&n); err != nil {
		return CloudResourceV2{}, err
	}
	if n > 0 {
		return CloudResourceV2{}, ErrDeploymentUnresolved
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM profile_runs WHERE id = ? AND user_id = ? AND node_id = ?
	 AND entry = 'initial' AND cancel_requested = 0 AND state NOT IN ('succeeded','failed','cancelled')`,
		run.ID, run.UserID, run.NodeID).Scan(&n); err != nil {
		return CloudResourceV2{}, err
	}
	if n != 1 {
		return CloudResourceV2{}, ErrRevisionConflict
	}
	now := time.Now().Unix()
	res, err := tx.Exec(`INSERT INTO cloud_resources2
	 (user_id, connection_id, provider, external_id, node_id, label, state, revision, ip, secrets_enc, created_at, updated_at)
	 VALUES (?,?,?,'',?,?,?,1,'','',?,?)`, run.UserID, resource.ConnectionID, resource.Provider,
		run.NodeID, resource.Label, CloudResPlanned, now, now)
	if err != nil {
		return CloudResourceV2{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return CloudResourceV2{}, err
	}
	resource, err = scanCloudResource(tx.QueryRow(`SELECT `+cloudResCols+` FROM cloud_resources2 WHERE id = ?`, id))
	if err != nil {
		return CloudResourceV2{}, err
	}
	if err := tx.Commit(); err != nil {
		return CloudResourceV2{}, err
	}
	return resource, nil
}

// Run states (04 §5.1).
const (
	RunPending        = "pending"
	RunRunning        = "running"
	RunWaitingEvent   = "waiting_event"
	RunWaitingRetry   = "waiting_retry"
	RunReconciling    = "reconciling"
	RunPaused         = "paused"
	RunNeedsAttention = "needs_attention"
	RunSucceeded      = "succeeded"
	RunFailed         = "failed"
	RunCancelled      = "cancelled"
)

// Checkpoint states (04 §5.1).
const (
	CheckpointReady      = "ready"
	CheckpointClaimed    = "claimed"
	CheckpointDispatched = "dispatched"
	CheckpointConfirmed  = "confirmed"
	CheckpointRejected   = "rejected"
	CheckpointUnknown    = "unknown"
	CheckpointWaiting    = "waiting"
)

// BanDeliveryDelivered is the only delivery state persisted today.
const BanDeliveryDelivered = "delivered"

// ProfileRun is one durable graph execution.
type ProfileRun struct {
	ID                int64
	UserID            int64
	ProfileVersionID  int64
	ProfileID         int64
	NodeID            string
	Entry             string // initial | observe
	State             string
	Revision          int64
	InputsJSON        string
	VarsJSON          string
	CurrentResourceID int64
	OperationID       int64
	PolicyID          int64
	PolicyRevision    int64
	NextWakeAt        int64
	CancelRequested   bool
	LastError         string
	LeaseGeneration   int64
	CreatedAt         int64
	UpdatedAt         int64
}

const runCols = `id, user_id, profile_version_id, profile_id, node_id, entry, state, revision, inputs_json, vars_json,
 current_resource_id, operation_id, policy_id, policy_revision, next_wake_at, cancel_requested, last_error, lease_generation, created_at, updated_at`

func scanRun(row interface{ Scan(...any) error }) (ProfileRun, error) {
	var (
		r         ProfileRun
		cur       sql.NullInt64
		cancelled int
	)
	err := row.Scan(&r.ID, &r.UserID, &r.ProfileVersionID, &r.ProfileID, &r.NodeID, &r.Entry,
		&r.State, &r.Revision, &r.InputsJSON, &r.VarsJSON, &cur, &r.OperationID, &r.PolicyID, &r.PolicyRevision,
		&r.NextWakeAt, &cancelled, &r.LastError, &r.LeaseGeneration, &r.CreatedAt, &r.UpdatedAt)
	r.CurrentResourceID = cur.Int64
	r.CancelRequested = cancelled != 0
	return r, err
}

func validRunEntry(e string) bool { return e == "initial" || e == "observe" }

// CreateRun inserts a pending run (revision 1).
func (s *Store) CreateRun(r ProfileRun) (ProfileRun, error) {
	if r.UserID == 0 || r.ProfileVersionID <= 0 || r.ProfileID <= 0 || !validRunEntry(r.Entry) {
		return ProfileRun{}, ErrValidation
	}
	if r.State == "" {
		r.State = RunPending
	}
	if r.InputsJSON == "" {
		r.InputsJSON = "{}"
	}
	if r.VarsJSON == "" {
		r.VarsJSON = "{}"
	}
	now := time.Now().Unix()
	res, err := s.sql.Exec(
		`INSERT INTO profile_runs
		 (user_id, profile_version_id, profile_id, node_id, entry, state, revision, inputs_json, vars_json,
		  current_resource_id, operation_id, policy_id, policy_revision, next_wake_at, cancel_requested, last_error, lease_generation, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,1,?,?,?,?,?,?,?,0,?,0,?,?)`,
		r.UserID, r.ProfileVersionID, r.ProfileID, r.NodeID, r.Entry, r.State, r.InputsJSON, r.VarsJSON,
		nullInt(r.CurrentResourceID), r.OperationID, r.PolicyID, r.PolicyRevision, r.NextWakeAt,
		r.LastError, now, now)
	if err != nil {
		return ProfileRun{}, err
	}
	r.ID, _ = res.LastInsertId()
	r.Revision, r.CreatedAt, r.UpdatedAt = 1, now, now
	return r, nil
}

// CreateRunIdempotent is the C1 barrier: the operation row is inserted first
// (UNIQUE idem_key is the atomic gate) and the run is created only when this
// request won. Concurrent identical POSTs therefore produce exactly one run and
// one provider dispatch. The loser gets the existing operation without a run.
func (s *Store) CreateRunIdempotent(op Operation, r ProfileRun, bodyHash string) (ProfileRun, Operation, bool, error) {
	if r.UserID == 0 || r.ProfileVersionID <= 0 || r.ProfileID <= 0 || !validRunEntry(r.Entry) {
		return ProfileRun{}, Operation{}, false, ErrValidation
	}
	if r.State == "" {
		r.State = RunPending
	}
	if r.InputsJSON == "" {
		r.InputsJSON = "{}"
	}
	if r.VarsJSON == "" {
		r.VarsJSON = "{}"
	}
	tx, err := s.sql.Begin()
	if err != nil {
		return ProfileRun{}, Operation{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().Unix()
	res, err := tx.Exec(
		`INSERT INTO operations (user_id, node_id, kind, state, idem_key, provider_vps_id, region, config_id, profile_id, request_json, last_error, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(idem_key) DO NOTHING`,
		op.UserID, op.NodeID, op.Kind, op.State, op.IdemKey, op.ProviderVPSID, op.Region, op.ConfigID, op.ProfileID, op.RequestJSON, op.LastError, now, now)
	if err != nil {
		return ProfileRun{}, Operation{}, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		existing, err := scanOp(tx.QueryRow(`SELECT `+opCols+` FROM operations WHERE idem_key = ?`, op.IdemKey))
		if errors.Is(err, sql.ErrNoRows) {
			return ProfileRun{}, Operation{}, false, ErrNotFound
		}
		if err != nil {
			return ProfileRun{}, Operation{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return ProfileRun{}, Operation{}, false, err
		}
		return ProfileRun{}, existing, false, nil
	}
	op.ID, _ = res.LastInsertId()
	op.CreatedAt, op.UpdatedAt = now, now
	if r.Entry == "initial" && r.NodeID != "" {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM profile_runs WHERE `+namedDeploymentWhere,
			r.UserID, r.NodeID, 0).Scan(&n); err != nil {
			return ProfileRun{}, Operation{}, false, err
		}
		if n > 0 {
			return ProfileRun{}, Operation{}, false, ErrDeploymentUnresolved
		}
	}
	r.OperationID = op.ID
	ins, err := tx.Exec(
		`INSERT INTO profile_runs
		 (user_id, profile_version_id, profile_id, node_id, entry, state, revision, inputs_json, vars_json,
		  current_resource_id, operation_id, policy_id, policy_revision, next_wake_at, cancel_requested, last_error, lease_generation, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,1,?,?,?,?,?,?,?,0,?,0,?,?)`,
		r.UserID, r.ProfileVersionID, r.ProfileID, r.NodeID, r.Entry, r.State, r.InputsJSON, r.VarsJSON,
		nullInt(r.CurrentResourceID), r.OperationID, r.PolicyID, r.PolicyRevision, r.NextWakeAt,
		r.LastError, now, now)
	if err != nil {
		return ProfileRun{}, Operation{}, false, err
	}
	r.ID, _ = ins.LastInsertId()
	r.Revision, r.CreatedAt, r.UpdatedAt = 1, now, now
	meta := fmt.Sprintf(`{"run_id":%d,"body_hash":%q}`, r.ID, bodyHash)
	if _, err := tx.Exec(`UPDATE operations SET request_json = ?, updated_at = ? WHERE id = ?`, meta, now, op.ID); err != nil {
		return ProfileRun{}, Operation{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return ProfileRun{}, Operation{}, false, err
	}
	return r, op, true, nil
}

// NodeManagementPolicyID returns the stable policy identifier (node_management
// rowid) for a node, or 0 when no policy exists.
func (s *Store) NodeManagementPolicyID(nodeID string) (int64, error) {
	var id int64
	err := s.sql.QueryRow(`SELECT rowid FROM node_management WHERE node_id = ?`, nodeID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// ActiveRunsForNode counts non-terminal runs for a node (optionally excluding
// one run). Used by the replacement/ban guard (H7).
func (s *Store) ActiveRunsForNode(nodeID string, excludeID int64) (int, error) {
	var n int
	err := s.sql.QueryRow(
		`SELECT COUNT(*) FROM profile_runs WHERE node_id = ? AND id <> ? AND state IN
		 (?,?,?,?,?,?)`,
		nodeID, excludeID, RunPending, RunRunning, RunWaitingRetry,
		RunReconciling, RunNeedsAttention, RunPaused).Scan(&n)
	return n, err
}

// unresolvedReplacementWhere is the single H7/H8 resource-level guard shared by
// the ban delivery tx and the pre-create check. A non-deleted resource is
// unresolved when either:
//   - it was superseded (replaced_by_resource_id set) and its teardown is not
//     confirmed, or
//   - it is a paid replacement (replaces_resource_id set) that never became
//     the policy current resource (e.g. the run was cancelled after create_new
//     but before resource.fence), regardless of replaced_by_resource_id.
//
// Traverse owned resource ancestry, not the successor's mutable/empty node_id.
// Args: owner, deleted state, owner, policy node, current ID, owner, current ID.
const unresolvedReplacementWhere = `user_id = ? AND state <> ?
 AND id IN (WITH RECURSIVE family(id, replaces_id, replaced_by_id) AS (
   SELECT id, replaces_resource_id, replaced_by_resource_id FROM cloud_resources2
     WHERE user_id = ? AND (node_id = ? OR id = ?)
   UNION SELECT c.id, c.replaces_resource_id, c.replaced_by_resource_id FROM cloud_resources2 c JOIN family f
     ON c.replaces_resource_id = f.id OR c.replaced_by_resource_id = f.id
       OR c.id = f.replaces_id OR c.id = f.replaced_by_id
      WHERE c.user_id = ?
 ) SELECT id FROM family)
 AND (replaced_by_resource_id IS NOT NULL
      OR (replaces_resource_id IS NOT NULL AND id <> ?))`

// UnresolvedFencedResources counts resources that still block a new purchase.
// It is the resource-level half of the H7/H8 guard: a cancelled/terminal run
// must not erase the obligation to finish (or account for) a paid replacement
// before another purchase.
func (s *Store) UnresolvedFencedResources(userID int64, nodeID string, currentResourceID int64) (int, error) {
	if userID <= 0 || nodeID == "" {
		return 0, nil
	}
	var n int
	err := s.sql.QueryRow(
		`SELECT COUNT(*) FROM cloud_resources2 WHERE `+unresolvedReplacementWhere,
		userID, CloudResDeleted, userID, nodeID, currentResourceID, userID, currentResourceID).Scan(&n)
	return n, err
}

// ListCancelRequestedRuns returns runs whose cancel flag is set but whose
// state is still non-terminal (M1 restart/worker-death path).
func (s *Store) ListCancelRequestedRuns(limit int) ([]ProfileRun, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.sql.Query(
		`SELECT `+runCols+` FROM profile_runs WHERE cancel_requested = 1 AND state IN (?,?,?)
		 ORDER BY id ASC LIMIT ?`, RunRunning, RunReconciling, RunPaused, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ProfileRun{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) runBy(query string, args ...any) (ProfileRun, error) {
	r, err := scanRun(s.sql.QueryRow(query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return ProfileRun{}, ErrNotFound
	}
	return r, err
}

// RunByID is the engine lookup (no ownership).
func (s *Store) RunByID(id int64) (ProfileRun, error) {
	return s.runBy(`SELECT `+runCols+` FROM profile_runs WHERE id = ?`, id)
}

// RunOwned enforces object ownership; foreign/unknown -> ErrNotFound.
func (s *Store) RunOwned(id, userID int64) (ProfileRun, error) {
	return s.runBy(`SELECT `+runCols+` FROM profile_runs WHERE id = ? AND user_id = ?`, id, userID)
}

// RunsByUser lists runs newest first.
func (s *Store) RunsByUser(userID int64) ([]ProfileRun, error) {
	rows, err := s.sql.Query(`SELECT `+runCols+` FROM profile_runs WHERE user_id = ? ORDER BY id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ProfileRun{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ClaimRun is the CAS lease that lets exactly one worker own a run revision.
func (s *Store) ClaimRun(id, expectedRevision int64) (ProfileRun, bool, error) {
	res, err := s.sql.Exec(
		`UPDATE profile_runs SET state = ?, revision = revision + 1, lease_generation = lease_generation + 1, updated_at = ?
		 WHERE id = ? AND revision = ? AND cancel_requested = 0
		   AND state NOT IN (?,?,?)`,
		RunRunning, time.Now().Unix(), id, expectedRevision, RunSucceeded, RunFailed, RunCancelled)
	if err != nil {
		return ProfileRun{}, false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ProfileRun{}, false, nil
	}
	r, err := s.RunByID(id)
	return r, err == nil, err
}

// SetRunState is the CAS transition used by the engine and API.
// currentResourceID >= 0 replaces the value; -1 preserves it.
func (s *Store) SetRunState(id, expectedRevision int64, state string, nextWakeAt int64, lastError string, currentResourceID int64) (ProfileRun, error) {
	now := time.Now().Unix()
	var res sql.Result
	var err error
	if currentResourceID < 0 {
		res, err = s.sql.Exec(
			`UPDATE profile_runs SET state = ?, next_wake_at = ?, last_error = ?, revision = revision + 1, updated_at = ?
			 WHERE id = ? AND revision = ?`,
			state, nextWakeAt, lastError, now, id, expectedRevision)
	} else {
		res, err = s.sql.Exec(
			`UPDATE profile_runs SET state = ?, next_wake_at = ?, last_error = ?, current_resource_id = ?,
			 revision = revision + 1, updated_at = ?
			 WHERE id = ? AND revision = ?`,
			state, nextWakeAt, lastError, nullInt(currentResourceID), now, id, expectedRevision)
	}
	if err != nil {
		return ProfileRun{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.RunByID(id); err != nil {
			return ProfileRun{}, err
		}
		return ProfileRun{}, ErrRevisionConflict
	}
	return s.RunByID(id)
}

// RequestRunCancel is the durable cancel flag. Pending/waiting runs terminate
// immediately; an in-flight run keeps running until its next checkpoint
// boundary (no refund / slot release).
func (s *Store) RequestRunCancel(id, userID, expectedRevision int64) (ProfileRun, error) {
	res, err := s.sql.Exec(
		`UPDATE profile_runs SET cancel_requested = 1,
		 state = CASE WHEN state IN (?,?,?,?) THEN ? ELSE state END,
		 revision = revision + 1, updated_at = ?
		 WHERE id = ? AND user_id = ? AND revision = ? AND state NOT IN (?,?,?)`,
		RunPending, RunWaitingEvent, RunWaitingRetry, RunPaused, RunCancelled, time.Now().Unix(),
		id, userID, expectedRevision, RunSucceeded, RunFailed, RunCancelled)
	if err != nil {
		return ProfileRun{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.RunOwned(id, userID); err != nil {
			return ProfileRun{}, err
		}
		return ProfileRun{}, ErrRevisionConflict
	}
	return s.RunOwned(id, userID)
}

// ApplyRunCommand reserves the key, CASes the run, and optionally resets a
// validated read-only checkpoint in one transaction. A conflict rolls back all
// three writes; no scheduler can observe an uncommitted resume.
func (s *Store) ApplyRunCommand(op Operation, runID, revision int64, bodyHash, state string, cp *BlockCheckpoint, reset bool) (ProfileRun, Operation, bool, error) {
	tx, err := s.sql.Begin()
	if err != nil {
		return ProfileRun{}, Operation{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	meta, err := json.Marshal(struct {
		RunID    int64  `json:"run_id"`
		BodyHash string `json:"body_hash"`
	}{runID, bodyHash})
	if err != nil {
		return ProfileRun{}, Operation{}, false, err
	}
	now := time.Now().Unix()
	res, err := tx.Exec(`INSERT INTO operations (user_id,node_id,kind,state,idem_key,request_json,created_at,updated_at)
	 VALUES (?,?,?,?,?,?,?,?) ON CONFLICT(idem_key) DO NOTHING`,
		op.UserID, op.NodeID, op.Kind, OpConfirmed, op.IdemKey, string(meta), now, now)
	if err != nil {
		return ProfileRun{}, Operation{}, false, err
	}
	created, err := res.RowsAffected()
	if err != nil {
		return ProfileRun{}, Operation{}, false, err
	}
	if created == 0 {
		existing, err := scanOp(tx.QueryRow(`SELECT `+opCols+` FROM operations WHERE idem_key = ?`, op.IdemKey))
		if err != nil {
			return ProfileRun{}, Operation{}, false, err
		}
		if existing.UserID != op.UserID || existing.Kind != op.Kind || existing.RequestJSON != string(meta) {
			return ProfileRun{}, Operation{}, false, ErrRunCommandConflict
		}
		run, err := scanRun(tx.QueryRow(`SELECT `+runCols+` FROM profile_runs WHERE id = ? AND user_id = ?`, runID, op.UserID))
		return run, existing, false, err // Read-only replay; deferred rollback releases tx.
	}
	op.ID, err = res.LastInsertId()
	if err != nil {
		return ProfileRun{}, Operation{}, false, err
	}
	if state == "" {
		res, err = tx.Exec(`UPDATE profile_runs SET cancel_requested = 1,
		 state = CASE WHEN state IN (?,?,?,?) THEN ? ELSE state END,
		 revision = revision + 1, updated_at = ?
		 WHERE id = ? AND user_id = ? AND revision = ? AND state NOT IN (?,?,?)`,
			RunPending, RunWaitingEvent, RunWaitingRetry, RunPaused, RunCancelled, now,
			runID, op.UserID, revision, RunSucceeded, RunFailed, RunCancelled)
	} else {
		res, err = tx.Exec(`UPDATE profile_runs SET state = ?, next_wake_at = ?, last_error = '',
		 revision = revision + 1, updated_at = ? WHERE id = ? AND user_id = ? AND revision = ?
		 AND cancel_requested = 0 AND state NOT IN (?,?,?)`, state, now, now,
			runID, op.UserID, revision, RunSucceeded, RunFailed, RunCancelled)
	}
	if err != nil {
		return ProfileRun{}, Operation{}, false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		if err != nil {
			return ProfileRun{}, Operation{}, false, err
		}
		return ProfileRun{}, Operation{}, false, ErrRevisionConflict
	}
	if cp != nil {
		// CAS even for reconcile/acknowledge: validation must still describe
		// the same checkpoint when the transaction commits.
		current, err := scanCheckpoint(tx.QueryRow(`SELECT `+checkpointCols+` FROM block_checkpoints WHERE id = ? AND run_id = ?`, cp.ID, runID))
		if errors.Is(err, sql.ErrNoRows) || (err == nil && current != *cp) {
			return ProfileRun{}, Operation{}, false, ErrRevisionConflict
		}
		if err != nil {
			return ProfileRun{}, Operation{}, false, err
		}
		if reset {
			res, err = tx.Exec(`UPDATE block_checkpoints SET state = ?, output_json = '', error = '',
			 next_attempt_at = 0, lease_generation = lease_generation + 1, updated_at = ?
			 WHERE id = ? AND run_id = ? AND state = ? AND lease_generation = ? AND state <> ?`,
				CheckpointReady, now, cp.ID, runID, cp.State, cp.LeaseGeneration, CheckpointConfirmed)
		} else {
			res, err = tx.Exec(`UPDATE block_checkpoints SET updated_at = updated_at
			 WHERE id = ? AND run_id = ? AND state = ? AND lease_generation = ? AND state <> ?`,
				cp.ID, runID, cp.State, cp.LeaseGeneration, CheckpointConfirmed)
		}
		if err != nil {
			return ProfileRun{}, Operation{}, false, err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			if err != nil {
				return ProfileRun{}, Operation{}, false, err
			}
			return ProfileRun{}, Operation{}, false, ErrRevisionConflict
		}
	}
	run, err := scanRun(tx.QueryRow(`SELECT `+runCols+` FROM profile_runs WHERE id = ?`, runID))
	if err != nil {
		return ProfileRun{}, Operation{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return ProfileRun{}, Operation{}, false, err
	}
	return run, op, true, nil
}

// ListClaimableRuns returns runs a worker may pick up now. waiting_event is
// deliberately excluded: a new ban episode creates a new run; the waiter only
// persists state.
func (s *Store) ListClaimableRuns(now int64, limit int) ([]ProfileRun, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.sql.Query(
		`SELECT `+runCols+` FROM profile_runs
		 WHERE state IN (?,?,?,?) AND (next_wake_at = 0 OR next_wake_at <= ?)
		 ORDER BY id ASC LIMIT ?`,
		RunPending, RunRunning, RunWaitingRetry, RunReconciling, now, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ProfileRun{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ── checkpoints ──────────────────────────────────────────────────

type BlockCheckpoint struct {
	ID              int64
	RunID           int64
	NodeID          string
	Invocation      int64
	State           string
	Attempt         int
	OutputJSON      string
	Error           string
	LeaseGeneration int64
	NextAttemptAt   int64
	CreatedAt       int64
	UpdatedAt       int64
}

const checkpointCols = `id, run_id, node_id, invocation, state, attempt, output_json, error,
 lease_generation, next_attempt_at, created_at, updated_at`

func scanCheckpoint(row interface{ Scan(...any) error }) (BlockCheckpoint, error) {
	var c BlockCheckpoint
	err := row.Scan(&c.ID, &c.RunID, &c.NodeID, &c.Invocation, &c.State, &c.Attempt,
		&c.OutputJSON, &c.Error, &c.LeaseGeneration, &c.NextAttemptAt, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

func (s *Store) CheckpointByNode(runID int64, nodeID string, invocation int64) (BlockCheckpoint, error) {
	c, err := scanCheckpoint(s.sql.QueryRow(
		`SELECT `+checkpointCols+` FROM block_checkpoints WHERE run_id = ? AND node_id = ? AND invocation = ?`,
		runID, nodeID, invocation))
	if errors.Is(err, sql.ErrNoRows) {
		return BlockCheckpoint{}, ErrNotFound
	}
	return c, err
}

func (s *Store) CheckpointsByRun(runID int64) ([]BlockCheckpoint, error) {
	rows, err := s.sql.Query(`SELECT `+checkpointCols+` FROM block_checkpoints WHERE run_id = ? ORDER BY id ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []BlockCheckpoint{}
	for rows.Next() {
		c, err := scanCheckpoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SaveCheckpoint upserts. A stale invoice generation may not overwrite a newer
// one and a confirmed checkpoint is immutable.
func (s *Store) SaveCheckpoint(c BlockCheckpoint) (BlockCheckpoint, error) {
	if c.RunID == 0 || c.NodeID == "" || c.State == "" {
		return BlockCheckpoint{}, ErrValidation
	}
	now := time.Now().Unix()
	res, err := s.sql.Exec(
		`INSERT INTO block_checkpoints
		 (run_id, node_id, invocation, state, attempt, output_json, error, lease_generation, next_attempt_at, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(run_id, node_id, invocation) DO UPDATE SET
		   state = excluded.state, attempt = excluded.attempt, output_json = excluded.output_json,
		   error = excluded.error, lease_generation = excluded.lease_generation,
		   next_attempt_at = excluded.next_attempt_at, updated_at = excluded.updated_at
		 WHERE excluded.lease_generation >= block_checkpoints.lease_generation
		   AND block_checkpoints.state <> ?`,
		c.RunID, c.NodeID, c.Invocation, c.State, c.Attempt, c.OutputJSON, c.Error,
		c.LeaseGeneration, c.NextAttemptAt, now, now, CheckpointConfirmed)
	if err != nil {
		return BlockCheckpoint{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.CheckpointByNode(c.RunID, c.NodeID, c.Invocation); err != nil {
			return BlockCheckpoint{}, err
		}
		return BlockCheckpoint{}, ErrRevisionConflict
	}
	return s.CheckpointByNode(c.RunID, c.NodeID, c.Invocation)
}

// ClaimCheckpoint CASes ready|waiting|rejected -> claimed and bumps the lease.
func (s *Store) ClaimCheckpoint(runID int64, nodeID string, invocation int64, expectedState string) (BlockCheckpoint, error) {
	res, err := s.sql.Exec(
		`UPDATE block_checkpoints SET state = ?, lease_generation = lease_generation + 1, updated_at = ?
		 WHERE run_id = ? AND node_id = ? AND invocation = ? AND state = ?`,
		CheckpointClaimed, time.Now().Unix(), runID, nodeID, invocation, expectedState)
	if err != nil {
		return BlockCheckpoint{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return BlockCheckpoint{}, ErrRevisionConflict
	}
	return s.CheckpointByNode(runID, nodeID, invocation)
}

// CompleteCheckpoint writes an outcome only for the current lease generation.
func (s *Store) CompleteCheckpoint(runID int64, nodeID string, invocation, lease int64, state, outputJSON, errText string) (BlockCheckpoint, error) {
	res, err := s.sql.Exec(
		`UPDATE block_checkpoints SET state = ?, output_json = ?, error = ?, updated_at = ?
		 WHERE run_id = ? AND node_id = ? AND invocation = ? AND lease_generation = ?`,
		state, outputJSON, errText, time.Now().Unix(), runID, nodeID, invocation, lease)
	if err != nil {
		return BlockCheckpoint{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return BlockCheckpoint{}, ErrRevisionConflict
	}
	return s.CheckpointByNode(runID, nodeID, invocation)
}

// ── ban episodes / deliveries ────────────────────────────────────

type BanEpisode struct {
	ID        int64
	NodeID    string
	IP        string
	EpisodeID string
	CreatedAt int64
}

// AddBanEpisode persists the stable episode id exactly once.
func (s *Store) AddBanEpisode(nodeID, ip, episodeID string) error {
	if nodeID == "" || episodeID == "" {
		return ErrValidation
	}
	_, err := s.sql.Exec(
		`INSERT INTO ban_episodes (node_id, ip, episode_id, created_at) VALUES (?,?,?,?)
		 ON CONFLICT(episode_id) DO NOTHING`,
		nodeID, ip, episodeID, time.Now().Unix())
	return err
}

func (s *Store) LatestBanEpisode(nodeID string) (BanEpisode, error) {
	var e BanEpisode
	err := s.sql.QueryRow(
		`SELECT id, node_id, ip, episode_id, created_at FROM ban_episodes WHERE node_id = ? ORDER BY id DESC LIMIT 1`,
		nodeID).Scan(&e.ID, &e.NodeID, &e.IP, &e.EpisodeID, &e.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return BanEpisode{}, ErrNotFound
	}
	return e, err
}

func (s *Store) BanDelivered(policyNodeID, episodeID string) (bool, error) {
	var n int64
	err := s.sql.QueryRow(
		`SELECT COUNT(*) FROM ban_deliveries WHERE policy_node_id = ? AND episode_id = ?`,
		policyNodeID, episodeID).Scan(&n)
	return n > 0, err
}

// MarkBanDelivered records the dedupe key. A duplicate is not an error.
func (s *Store) MarkBanDelivered(policyNodeID, episodeID string, runID int64) error {
	if policyNodeID == "" || episodeID == "" {
		return ErrValidation
	}
	_, err := s.sql.Exec(
		`INSERT INTO ban_deliveries (policy_node_id, episode_id, run_id, state, created_at)
		 VALUES (?,?,?,?,?) ON CONFLICT(policy_node_id, episode_id) DO NOTHING`,
		policyNodeID, episodeID, nullInt(runID), BanDeliveryDelivered, time.Now().Unix())
	return err
}

// BanDeliveryCandidate is a profile policy + undelivered episode.
type BanDeliveryCandidate struct {
	Episode           BanEpisode
	UserID            int64
	ProfileVersionID  int64
	ProfileID         int64
	InputsJSON        string
	CurrentResourceID int64
	PolicyRevision    int64
}

// UndeliveredBanEpisodes scans persisted episodes against PROFILE policies.
// activated_at is the enable-time watermark: older episodes are historical.
// process_current_ban=true also admits the latest episode that existed at
// enable time (the server sets activated_at to that episode's timestamp).
func (s *Store) UndeliveredBanEpisodes() ([]BanDeliveryCandidate, error) {
	rows, err := s.sql.Query(
		`SELECT e.id, e.node_id, e.ip, e.episode_id, e.created_at,
		        m.user_id, m.profile_version_id, pv.profile_id, m.inputs_json,
		        COALESCE(m.current_resource_id, 0), m.revision
		 FROM ban_episodes e
		 JOIN node_management m ON m.node_id = e.node_id
		 JOIN profile_versions pv ON pv.id = m.profile_version_id
		 WHERE m.mode = 'profile' AND m.activated_at > 0
		   AND NOT EXISTS (SELECT 1 FROM ban_deliveries d
		                   WHERE d.policy_node_id = e.node_id AND d.episode_id = e.episode_id)
		   AND ((m.process_current_ban = 0 AND e.created_at > m.activated_at)
		     OR (m.process_current_ban = 1 AND e.created_at >= m.activated_at))
		 ORDER BY e.id ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []BanDeliveryCandidate{}
	for rows.Next() {
		var c BanDeliveryCandidate
		if err := rows.Scan(&c.Episode.ID, &c.Episode.NodeID, &c.Episode.IP, &c.Episode.EpisodeID,
			&c.Episode.CreatedAt, &c.UserID, &c.ProfileVersionID, &c.ProfileID, &c.InputsJSON,
			&c.CurrentResourceID, &c.PolicyRevision); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CreateRunForBan atomically dedupes the delivery and inserts the observe run.
// A duplicate delivery returns the existing state without a second run.
func (s *Store) CreateRunForBan(c BanDeliveryCandidate, r ProfileRun) (ProfileRun, bool, error) {
	tx, err := s.sql.Begin()
	if err != nil {
		return ProfileRun{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().Unix()
	if r.State == "" {
		r.State = RunPending
	}
	if r.InputsJSON == "" {
		r.InputsJSON = "{}"
	}
	if r.VarsJSON == "" {
		r.VarsJSON = "{}"
	}
	// H7: one non-terminal run per node/policy. A new ban episode while the
	// previous replacement is unresolved is not delivered yet; it stays
	// undelivered and is retried by the reconciler once the run settles.
	var active int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM profile_runs WHERE node_id = ? AND state IN
		 (?,?,?,?,?,?)`, c.Episode.NodeID, RunPending, RunRunning, RunWaitingRetry,
		RunReconciling, RunNeedsAttention, RunPaused).Scan(&active); err != nil {
		return ProfileRun{}, false, err
	}
	if active > 0 {
		return ProfileRun{}, false, nil
	}
	// H7/H8 second clause: an unresolved fenced resource or an unfenced paid
	// replacement left by a cancelled run blocks a new replacement even when
	// the owning run is terminal/cancelled.
	var unresolved int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM cloud_resources2 WHERE `+unresolvedReplacementWhere,
		c.UserID, CloudResDeleted, c.UserID, c.Episode.NodeID, c.CurrentResourceID, c.UserID, c.CurrentResourceID).Scan(&unresolved); err != nil {
		return ProfileRun{}, false, err
	}
	if unresolved > 0 {
		return ProfileRun{}, false, nil
	}
	policyID := int64(0)
	if err := tx.QueryRow(`SELECT rowid FROM node_management WHERE node_id = ?`, c.Episode.NodeID).Scan(&policyID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return ProfileRun{}, false, err
	}
	res, err := tx.Exec(
		`INSERT INTO ban_deliveries (policy_node_id, episode_id, state, created_at) VALUES (?,?,?,?)
		 ON CONFLICT(policy_node_id, episode_id) DO NOTHING`,
		c.Episode.NodeID, c.Episode.EpisodeID, BanDeliveryDelivered, now)
	if err != nil {
		return ProfileRun{}, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ProfileRun{}, false, nil
	}
	ins, err := tx.Exec(
		`INSERT INTO profile_runs
		 (user_id, profile_version_id, profile_id, node_id, entry, state, revision, inputs_json, vars_json,
		  current_resource_id, operation_id, policy_id, policy_revision, next_wake_at, cancel_requested, last_error, lease_generation, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,1,?,?,?,0,?,?,0,0,'',0,?,?)`,
		c.UserID, c.ProfileVersionID, c.ProfileID, c.Episode.NodeID, "observe", r.State,
		r.InputsJSON, r.VarsJSON, nullInt(c.CurrentResourceID), policyID, c.PolicyRevision, now, now)
	if err != nil {
		return ProfileRun{}, false, err
	}
	r.ID, _ = ins.LastInsertId()
	r.Revision, r.CreatedAt, r.UpdatedAt = 1, now, now
	r.UserID, r.ProfileVersionID, r.ProfileID, r.NodeID, r.Entry = c.UserID, c.ProfileVersionID, c.ProfileID, c.Episode.NodeID, "observe"
	r.CurrentResourceID, r.PolicyRevision, r.PolicyID = c.CurrentResourceID, c.PolicyRevision, policyID
	if _, err := tx.Exec(`UPDATE ban_deliveries SET run_id = ? WHERE policy_node_id = ? AND episode_id = ?`,
		r.ID, c.Episode.NodeID, c.Episode.EpisodeID); err != nil {
		return ProfileRun{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return ProfileRun{}, false, err
	}
	return r, true, nil
}

// ListRunsByState is a small startup helper (e.g. resume waiting_event runs).
func (s *Store) ListRunsByState(state string, limit int) ([]ProfileRun, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.sql.Query(
		`SELECT `+runCols+` FROM profile_runs WHERE state = ? ORDER BY id ASC LIMIT ?`, state, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ProfileRun{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
