package server

// Durable graph-run engine (hosting-blueprints stage-08b-i). Bounded MVP:
// Nodehost blocks execute through the existing cloud create/install/delete
// paths; Beget ip.* return an explicit not_implemented error. There is no
// atomic outbox between JSON state and control SQLite: ban delivery is
// at-least-once and deduped by ban_deliveries UNIQUE(policy_node_id,episode_id).
// A checkpoint is persisted before any paid dispatch, and an already
// dispatched purchase is never replayed.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"sharedd/registry/internal/blueprint"
	"sharedd/registry/internal/control"
	"sharedd/registry/internal/nodehost"
	"sharedd/registry/internal/provision"
	"sharedd/registry/internal/sshx"
)

var (
	errBlueprintNotImplemented = errors.New("not_implemented")
	errBlueprintUnknown        = errors.New("unknown outcome")
)

func (r *Registry) blueprintNow() time.Time {
	if r.bpNowFn != nil {
		return r.bpNowFn()
	}
	return time.Now()
}

func (r *Registry) blueprintPoll() time.Duration {
	if r.bpPollInterval > 0 {
		return r.bpPollInterval
	}
	return 2 * time.Second
}

func (r *Registry) blueprintWorkers() *provisionWorker {
	r.bpWorkerOnce.Do(func() { r.bpWorker = newProvisionWorker() })
	return r.bpWorker
}

// blueprintLoop is the startup + periodic scheduler: reconcile ban deliveries,
// then claim runs whose next_wake_at is due (including waiting_retry and
// reconciling survivors from a previous process).
func (r *Registry) blueprintLoop(ctx context.Context) {
	if r.control == nil {
		return
	}
	interval := r.bpInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	// Startup: waiting_event runs are not picked by the periodic claim query
	// (new ban episodes create new runs); resume them once so a pending event
	// checkpoint is re-evaluated after a restart.
	if waiting, err := r.control.ListRunsByState(control.RunWaitingEvent, 100); err == nil {
		for _, run := range waiting {
			r.launchBlueprintRun(ctx, run.ID, run.Revision)
		}
	}
	r.blueprintTick(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.blueprintTick(ctx)
		}
	}
}

func (r *Registry) blueprintTick(ctx context.Context) {
	if r.control == nil {
		return
	}
	r.retryBanEpisodePersistence()
	r.finalizeOrphanedCancels(ctx)
	r.reconcileBanDeliveries()
	runs, err := r.control.ListClaimableRuns(r.blueprintNow().Unix(), 50)
	if err != nil {
		log.Printf("blueprint: list claimable: %v", err)
		return
	}
	for _, run := range runs {
		r.launchBlueprintRun(ctx, run.ID, run.Revision)
	}
}

// finalizeOrphanedCancels terminates cancel_requested runs that have no live
// worker (M1: crash/restart or a cancel that raced the claim).
func (r *Registry) finalizeOrphanedCancels(ctx context.Context) {
	runs, err := r.control.ListCancelRequestedRuns(100)
	if err != nil {
		log.Printf("blueprint: list cancelled runs: %v", err)
		return
	}
	w := r.bpWorker
	for _, run := range runs {
		if w != nil && w.runningOp(run.ID) {
			continue
		}
		if _, err := r.control.SetRunState(run.ID, run.Revision, control.RunCancelled, 0, "cancelled", -1); err != nil && !errors.Is(err, control.ErrRevisionConflict) {
			log.Printf("blueprint: finalize cancel %d: %v", run.ID, err)
		}
	}
}

// retryBanEpisodePersistence replays episode inserts that failed when the
// verdict was recorded (M5). The stable episode id is part of the persisted
// JSON state, so the reconciler can recover it after a restart.
func (r *Registry) retryBanEpisodePersistence() {
	r.mu.RLock()
	type ep struct{ node, ip, id string }
	var pending []ep
	for _, rec := range r.state.Terminated {
		if rec != nil && rec.EpisodeID != "" {
			pending = append(pending, ep{node: rec.NodeID, ip: rec.IP, id: rec.EpisodeID})
		}
	}
	r.mu.RUnlock()
	persisted := map[string]bool{}
	for _, e := range pending {
		if err := r.control.AddBanEpisode(e.node, e.ip, e.id); err != nil {
			log.Printf("blueprint: retry ban episode %s/%s: %v", e.node, e.id, err)
			continue
		}
		persisted[e.id] = true
	}
	if len(persisted) == 0 {
		return
	}
	r.mu.Lock()
	changed := false
	for _, rec := range r.state.Terminated {
		if rec != nil && persisted[rec.EpisodeID] {
			rec.EpisodeID = ""
			changed = true
		}
	}
	if changed {
		r.persistStateLocked()
	}
	r.mu.Unlock()
}

// reconcileBanDeliveries scans durable episodes for PROFILE policies and
// creates one observe run per episode. Episodes older than the policy
// activation watermark are historical and skipped by default.
func (r *Registry) reconcileBanDeliveries() {
	cands, err := r.control.UndeliveredBanEpisodes()
	if err != nil {
		log.Printf("blueprint: scan ban episodes: %v", err)
		return
	}
	for _, c := range cands {
		inputs := map[string]any{}
		if c.InputsJSON != "" {
			_ = json.Unmarshal([]byte(c.InputsJSON), &inputs)
		}
		inputs["episode_id"] = c.Episode.EpisodeID
		inputs["banned_ip"] = c.Episode.IP
		raw, _ := json.Marshal(inputs)
		run := control.ProfileRun{State: control.RunPending, InputsJSON: string(raw), VarsJSON: "{}"}
		got, created, err := r.control.CreateRunForBan(c, run)
		if err != nil {
			log.Printf("blueprint: deliver ban %s/%s: %v", c.Episode.NodeID, c.Episode.EpisodeID, err)
			continue
		}
		if created {
			log.Printf("blueprint: ban %s/%s -> run %d", c.Episode.NodeID, c.Episode.EpisodeID, got.ID)
			r.launchBlueprintRun(context.Background(), got.ID, got.Revision)
		}
	}
}

func (r *Registry) launchBlueprintRun(ctx context.Context, runID, revision int64) {
	if !r.blueprintWorkers().run(runID, 30*time.Minute, func(wctx context.Context) {
		// H6: a panic in one run must not take down the registry.
		defer func() {
			if p := recover(); p != nil {
				// Never log the panic payload: it can contain provider data.
				log.Printf("blueprint worker panic run=%d type=%T", runID, p)
				if run, err := r.control.RunByID(runID); err == nil {
					_, _ = r.control.SetRunState(run.ID, run.Revision, control.RunNeedsAttention, 0, "worker_panic", -1)
				}
			}
		}()
		r.executeProfileRun(wctx, runID, revision)
	}) {
		return
	}
}

// bpState is the in-memory view of one run.
type bpState struct {
	r           *Registry
	run         control.ProfileRun
	bp          blueprint.Blueprint
	checkpoints map[string]control.BlockCheckpoint
	outputs     map[string]map[string]any
	inputs      map[string]json.RawMessage
}

func (r *Registry) loadBPState(run control.ProfileRun) (*bpState, error) {
	pv, err := r.control.ProfileVersionByNumericID(run.ProfileVersionID)
	if err != nil {
		return nil, fmt.Errorf("pinned version: %w", err)
	}
	if pv.Format != control.FormatGraph || strings.TrimSpace(pv.Blueprint) == "" {
		return nil, fmt.Errorf("pinned version %d is not a graph", run.ProfileVersionID)
	}
	bp, ds := blueprint.ValidateBytes([]byte(pv.Blueprint))
	if len(ds) > 0 {
		return nil, fmt.Errorf("pinned graph is invalid: %s", ds[0].Code)
	}
	st := &bpState{r: r, run: run, bp: bp, checkpoints: map[string]control.BlockCheckpoint{}, outputs: map[string]map[string]any{}, inputs: map[string]json.RawMessage{}}
	if run.InputsJSON != "" {
		_ = json.Unmarshal([]byte(run.InputsJSON), &st.inputs)
	}
	cps, err := r.control.CheckpointsByRun(run.ID)
	if err != nil {
		return nil, err
	}
	for _, cp := range cps {
		st.checkpoints[cp.NodeID] = cp
		ports := map[string]any{}
		if cp.OutputJSON != "" {
			var env struct {
				Ports map[string]any `json:"ports"`
			}
			if err := json.Unmarshal([]byte(cp.OutputJSON), &env); err == nil && env.Ports != nil {
				ports = env.Ports
			}
		}
		st.outputs[cp.NodeID] = ports
	}
	return st, nil
}

// checkpoint env is {"ports":{...},"__port":"ok"}.
func (st *bpState) saveCheckpoint(nodeID, state string, attempt int, outputs map[string]any, port, errText string) (control.BlockCheckpoint, error) {
	env := map[string]any{}
	if outputs != nil {
		env["ports"] = outputs
	}
	if port != "" {
		env["__port"] = port
	}
	raw, _ := json.Marshal(env)
	cp := control.BlockCheckpoint{
		RunID: st.run.ID, NodeID: nodeID, Invocation: 0, State: state, Attempt: attempt,
		OutputJSON: string(raw), Error: errText, LeaseGeneration: st.run.LeaseGeneration,
	}
	got, err := st.r.control.SaveCheckpoint(cp)
	if err == nil {
		st.checkpoints[nodeID] = got
		if outputs != nil {
			st.outputs[nodeID] = outputs
		}
	}
	return got, err
}

func (st *bpState) complete(nodeID string, state string, outputs map[string]any, port, errText string) (control.BlockCheckpoint, error) {
	env := map[string]any{}
	if outputs != nil {
		env["ports"] = outputs
	}
	if port != "" {
		env["__port"] = port
	}
	raw, _ := json.Marshal(env)
	cp := st.checkpoints[nodeID]
	got, err := st.r.control.CompleteCheckpoint(st.run.ID, nodeID, 0, cp.LeaseGeneration, state, string(raw), errText)
	if err == nil {
		st.checkpoints[nodeID] = got
		if outputs != nil {
			st.outputs[nodeID] = outputs
		}
	}
	return got, err
}

// ── input resolution ─────────────────────────────────────────────

func (st *bpState) runInput(name string) (any, bool, error) {
	raw, ok := st.inputs[name]
	if !ok {
		return nil, false, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false, err
	}
	return v, true, nil
}

func (st *bpState) currentServer() (bpServer, error) {
	if st.run.CurrentResourceID <= 0 {
		return bpServer{}, errors.New("run has no current server")
	}
	res, err := st.r.control.CloudResourceOwned(st.run.CurrentResourceID, st.run.UserID)
	if err != nil {
		return bpServer{}, err
	}
	return serverFromResource(res), nil
}

func (st *bpState) binding(node blueprint.Node, input string) (any, bool, error) {
	b, ok := node.In[input]
	if !ok {
		return nil, false, nil
	}
	if b.Literal != nil {
		var v any
		if err := json.Unmarshal(b.Literal, &v); err != nil {
			return nil, false, err
		}
		return v, true, nil
	}
	if b.Ref == "run.current_server" {
		s, err := st.currentServer()
		return s, true, err
	}
	if strings.HasPrefix(b.Ref, "input.") {
		return st.runInput(strings.TrimPrefix(b.Ref, "input."))
	}
	nodeID, port, ok := strings.Cut(b.Ref, ".")
	if !ok {
		return nil, false, fmt.Errorf("bad ref %q", b.Ref)
	}
	ports, ok := st.outputs[nodeID]
	if !ok {
		return nil, false, fmt.Errorf("output %s.%s is not available", nodeID, port)
	}
	v, ok := ports[port]
	if !ok {
		return nil, false, fmt.Errorf("output %s.%s is missing", nodeID, port)
	}
	return v, true, nil
}

func (st *bpState) required(node blueprint.Node, input string) (any, error) {
	v, ok, err := st.binding(node, input)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("node %s: missing input %s", node.ID, input)
	}
	return v, nil
}

func paramString(node blueprint.Node, key, def string) string {
	var m map[string]any
	if err := json.Unmarshal(node.Params, &m); err != nil {
		return def
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return def
}

func paramInt(node blueprint.Node, key string, def int64) int64 {
	var m map[string]any
	if err := json.Unmarshal(node.Params, &m); err != nil {
		return def
	}
	return toInt64(m[key], def)
}

func toInt64(v any, def int64) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case json.Number:
		i, err := n.Int64()
		if err == nil {
			return i
		}
	case int:
		return int64(n)
	case int64:
		return n
	case string:
		var i int64
		if _, err := fmt.Sscan(n, &i); err == nil {
			return i
		}
	}
	return def
}

type bpServer struct {
	ResourceID int64  `json:"resource_id"`
	NodeID     string `json:"node_id"`
	IP         string `json:"ip"`
	Provider   string `json:"provider"`
	ExternalID string `json:"external_id"`
}

func serverFromResource(res control.CloudResourceV2) bpServer {
	return bpServer{ResourceID: res.ID, NodeID: res.NodeID, IP: res.IP, Provider: res.Provider, ExternalID: res.ExternalID}
}

func serverFromAny(v any) (bpServer, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return bpServer{}, err
	}
	var s bpServer
	if err := json.Unmarshal(raw, &s); err != nil {
		return bpServer{}, err
	}
	if s.ResourceID <= 0 {
		return bpServer{}, errors.New("invalid server handle")
	}
	return s, nil
}

type bpOutcome struct {
	port       string
	outputs    map[string]any
	waiting    string
	waitSecs   int64
	terminal   bool
	runState   string
	resourceID int64 // -1 preserves
	// retryDispatch marks a definitively failed mutation (provider returned an
	// error and the resource is in state=error) that is safe to dispatch again.
	retryDispatch bool
	err           error
}

// blueprintMaxAttempts caps retries per block (M2). Cleanup blocks keep their
// own bounded budget by using the same counter.
const blueprintMaxAttempts = 12

// ── engine ───────────────────────────────────────────────────────

func (r *Registry) executeProfileRun(ctx context.Context, runID, expectedRevision int64) {
	run, err := r.control.RunByID(runID)
	if err != nil {
		return
	}
	switch run.State {
	case control.RunSucceeded, control.RunFailed, control.RunCancelled:
		return
	}
	if run.CancelRequested {
		_, _ = r.control.SetRunState(run.ID, run.Revision, control.RunCancelled, 0, "cancelled", -1)
		return
	}
	run, ok, err := r.control.ClaimRun(run.ID, expectedRevision)
	if err != nil || !ok {
		return
	}
	st, err := r.loadBPState(run)
	if err != nil {
		_, _ = r.control.SetRunState(run.ID, run.Revision, control.RunFailed, 0, err.Error(), -1)
		return
	}
	nodeID := st.bp.Entries.Initial
	if run.Entry == "observe" {
		nodeID = st.bp.Entries.Observe
	}
	steps := 0
	for nodeID != "" && steps <= blueprint.MaxNodes {
		steps++
		// Cancellation is observed at every checkpoint boundary; an in-flight
		// effect completed above is reported, not undone.
		run, err = r.control.RunByID(run.ID)
		if err != nil {
			return
		}
		st.run = run
		if run.CancelRequested {
			_, _ = r.control.SetRunState(run.ID, run.Revision, control.RunCancelled, 0, "cancelled", -1)
			return
		}
		node, ok := nodeOf(st.bp, nodeID)
		if !ok {
			_, _ = r.control.SetRunState(run.ID, run.Revision, control.RunNeedsAttention, 0, "node not in graph", -1)
			return
		}
		cp, exists := st.checkpoints[nodeID]
		if exists && cp.State == control.CheckpointConfirmed {
			next := st.nextNode(nodeID, checkpointPort(cp))
			if next == "" {
				_, _ = r.control.SetRunState(run.ID, run.Revision, control.RunSucceeded, 0, "", -1)
				return
			}
			nodeID = next
			continue
		}
		if exists && (cp.State == control.CheckpointDispatched || cp.State == control.CheckpointUnknown || cp.State == control.CheckpointClaimed) {
			// H3: a delete that definitively failed (resource=error) may be
			// dispatched again with a fresh attempt key. Any other dispatched
			// mutation is never re-entered.
			if !(node.Type == "server.delete" && st.deleteRetrySafe(cp)) {
				out, err := st.reconcileNode(ctx, node, cp)
				if err == nil && out.err != nil {
					err = out.err
				}
				if handled, done := r.applyOutcome(st, node, cp, out, err); handled {
					if done {
						return
					}
					nodeID = st.nextNode(nodeID, out.port)
					if nodeID == "" {
						_, _ = r.control.SetRunState(st.run.ID, st.run.Revision, control.RunSucceeded, 0, "", out.resourceID)
						return
					}
					continue
				}
				return
			}
		}
		// H2: re-read policy state immediately before every effect dispatch.
		if !isReadBlock(node) && !st.policyActive() {
			_, _ = st.saveCheckpoint(nodeID, control.CheckpointRejected, cp.Attempt+1, st.outputs[nodeID], "", "policy changed")
			_, _ = r.control.SetRunState(run.ID, run.Revision, control.RunCancelled, 0, "policy changed", -1)
			return
		}
		attempt := cp.Attempt + 1
		if attempt > blueprintMaxAttempts && !isReadBlock(node) {
			_, _ = st.saveCheckpoint(nodeID, control.CheckpointUnknown, attempt, st.outputs[nodeID], "", "retry budget exhausted")
			_, _ = r.control.SetRunState(run.ID, run.Revision, control.RunNeedsAttention, 0, "retry budget exhausted", -1)
			return
		}
		// Persist dispatch BEFORE any external mutation. The block stores the
		// concrete intent (resource/operation ids) before its I/O call.
		if _, err := st.saveCheckpoint(nodeID, control.CheckpointDispatched, attempt, st.outputs[nodeID], "", ""); err != nil {
			return
		}
		out := st.execNode(ctx, node)
		handled, done := r.applyOutcome(st, node, st.checkpoints[nodeID], out, out.err)
		if handled {
			if done {
				return
			}
			nodeID = st.nextNode(nodeID, out.port)
			if nodeID == "" {
				_, _ = r.control.SetRunState(st.run.ID, st.run.Revision, control.RunSucceeded, 0, "", out.resourceID)
				return
			}
			continue
		}
		return
	}
	_, _ = r.control.SetRunState(st.run.ID, st.run.Revision, control.RunNeedsAttention, 0, "run exceeded node budget", -1)
}

// applyOutcome persists the block result and the run state. done=true means
// the run reached a terminal/waiting state and the engine must stop.
func (r *Registry) applyOutcome(st *bpState, node blueprint.Node, cp control.BlockCheckpoint, out bpOutcome, blockErr error) (handled, done bool) {
	if blockErr != nil {
		// H1: once a non-read checkpoint reached dispatched, its intent has
		// already left the process. Never replace it with rejected (which the
		// resume path would re-dispatch); keep the intent/output and require a
		// read-only reconcile.
		if !isReadBlock(node) && (cp.State == control.CheckpointDispatched || cp.State == control.CheckpointUnknown) {
			_, _ = st.saveCheckpoint(node.ID, control.CheckpointUnknown, cp.Attempt, st.outputs[node.ID], "", "mutation outcome unknown")
			_, _ = r.control.SetRunState(st.run.ID, st.run.Revision, control.RunNeedsAttention, 0, safeBlueprintError(blockErr), -1)
			return true, true
		}
		_, _ = st.complete(node.ID, control.CheckpointRejected, nil, "", safeBlueprintError(blockErr))
		state := control.RunFailed
		if errors.Is(blockErr, errBlueprintUnknown) {
			state = control.RunNeedsAttention
		}
		_, _ = r.control.SetRunState(st.run.ID, st.run.Revision, state, 0, safeBlueprintError(blockErr), -1)
		return true, true
	}
	if out.waiting != "" {
		// H1: a mutation that was already dispatched must never be downgraded
		// to waiting. Without a definitive provider failure the outcome is
		// unknown and requires reconciliation.
		if !out.retryDispatch && !isReadBlock(node) && (cp.State == control.CheckpointDispatched || cp.State == control.CheckpointUnknown) {
			_, _ = st.saveCheckpoint(node.ID, control.CheckpointUnknown, cp.Attempt, out.outputs, "", "mutation outcome unknown")
			_, _ = r.control.SetRunState(st.run.ID, st.run.Revision, control.RunNeedsAttention, 0, "mutation outcome unknown", out.resourceID)
			return true, true
		}
		// M2: bounded retries with exponential backoff (ceiling 10 minutes).
		if cp.Attempt >= blueprintMaxAttempts {
			_, _ = st.saveCheckpoint(node.ID, control.CheckpointUnknown, cp.Attempt, out.outputs, "", "retry budget exhausted")
			_, _ = r.control.SetRunState(st.run.ID, st.run.Revision, control.RunNeedsAttention, 0, "retry budget exhausted", out.resourceID)
			return true, true
		}
		_, _ = st.complete(node.ID, control.CheckpointWaiting, out.outputs, "", "")
		wait := out.waitSecs
		if wait <= 0 {
			wait = 15
		}
		if cp.Attempt > 1 {
			shift := cp.Attempt - 1
			if shift > 4 {
				shift = 4
			}
			wait *= 1 << shift
			if wait > 600 {
				wait = 600
			}
		}
		next := r.blueprintNow().Unix() + wait
		if _, err := r.control.SetRunState(st.run.ID, st.run.Revision, out.waiting, next, "", out.resourceID); err != nil {
			return true, true
		}
		return true, true
	}
	if out.terminal {
		_, _ = st.complete(node.ID, control.CheckpointConfirmed, out.outputs, out.port, "")
		state := out.runState
		if state == "" {
			state = control.RunSucceeded
		}
		_, _ = r.control.SetRunState(st.run.ID, st.run.Revision, state, 0, "", out.resourceID)
		return true, true
	}
	if _, err := st.complete(node.ID, control.CheckpointConfirmed, out.outputs, out.port, ""); err != nil {
		return true, true
	}
	updated, err := r.control.SetRunState(st.run.ID, st.run.Revision, control.RunRunning, 0, "", out.resourceID)
	if err == nil {
		st.run = updated
	}
	return true, false
}

func isReadBlock(node blueprint.Node) bool {
	blk, ok := blueprint.BlockFor(node.Type)
	return ok && blk.Effects == blueprint.EffectRead
}

// policyActive re-reads node_management/owner state right before an effect.
// Initial runs without a policy (PolicyID/Revision 0) are exempt; observe runs
// always require an active matching profile policy.
func (st *bpState) policyActive() bool {
	if st.run.Entry != "observe" && st.run.PolicyID == 0 && st.run.PolicyRevision == 0 {
		return true
	}
	if st.run.NodeID == "" {
		return false
	}
	m, err := st.r.control.NodeManagement(st.run.NodeID, st.run.UserID)
	if err != nil || m.Mode != "profile" || m.ProfileVersionID != st.run.ProfileVersionID {
		return false
	}
	if st.run.PolicyRevision > 0 && m.Revision != st.run.PolicyRevision {
		return false
	}
	if u, err := st.r.control.User(st.run.UserID); err != nil || u.Disabled {
		return false
	}
	return true
}

func (st *bpState) deleteRetrySafe(cp control.BlockCheckpoint) bool {
	ports := st.outputs[cp.NodeID]
	if ports == nil {
		return false
	}
	srv, err := serverFromAny(ports["server"])
	if err != nil {
		return false
	}
	res, err := st.r.control.CloudResourceOwned(srv.ResourceID, st.run.UserID)
	if err != nil {
		return false
	}
	return res.State == control.CloudResError || res.State == control.CloudResReady
}

func checkpointPort(cp control.BlockCheckpoint) string {
	if cp.OutputJSON == "" {
		return "ok"
	}
	var env struct {
		Port string `json:"__port"`
	}
	_ = json.Unmarshal([]byte(cp.OutputJSON), &env)
	if env.Port == "" {
		return "ok"
	}
	return env.Port
}

func (st *bpState) nextNode(nodeID, port string) string {
	if port == "" {
		port = "ok"
	}
	for _, e := range st.bp.Edges {
		if e.From == nodeID && e.Port == port {
			return e.To
		}
	}
	return ""
}

func nodeOf(bp blueprint.Blueprint, id string) (blueprint.Node, bool) {
	for _, n := range bp.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return blueprint.Node{}, false
}

func safeBlueprintError(err error) string {
	if errors.Is(err, errBlueprintNotImplemented) {
		return "not_implemented: " + err.Error()
	}
	var ne *nodehost.Error
	if errors.As(err, &ne) {
		return "provider_error: " + ne.Code
	}
	msg := err.Error()
	if len(msg) > 512 {
		msg = msg[:512]
	}
	return msg
}

// reconcileNode is the crash/retry path for a checkpoint that was already
// dispatched. Paid actions are never replayed: a create is resolved by reading
// its persisted resource; unknown shell/provider outcomes go needs_attention.
func (st *bpState) reconcileNode(ctx context.Context, node blueprint.Node, cp control.BlockCheckpoint) (bpOutcome, error) {
	if node.Type == "server.create" {
		resID := st.intentResourceID(cp)
		if resID == 0 {
			return bpOutcome{err: fmt.Errorf("%w: create dispatched without persisted intent; refusing replay", errBlueprintUnknown)}, nil
		}
		res, err := st.r.control.CloudResourceOwned(resID, st.run.UserID)
		if err != nil {
			return bpOutcome{err: fmt.Errorf("%w: create intent resource is missing; refusing replay", errBlueprintUnknown)}, nil
		}
		if res.State == control.CloudResDeleted {
			return bpOutcome{err: fmt.Errorf("%w: create outcome is deleted", errBlueprintUnknown)}, nil
		}
		if res.State == control.CloudResReady {
			opID := st.intentOperationID(cp)
			return bpOutcome{port: "ok", resourceID: -1, outputs: map[string]any{"operation": map[string]any{"operation_id": opID, "resource_id": resID}}}, nil
		}
		// Read-only reconcile: ask the provider whether the create already
		// happened. Never Create/Delete/ChangeIP here.
		if out, ok := st.resolveCreateByRead(ctx, res, cp); ok {
			return out, nil
		}
		return bpOutcome{err: fmt.Errorf("%w: create dispatched but resource is %s; read-only reconcile found no completed operation", errBlueprintUnknown, res.State)}, nil
	}
	// Read-only blocks are safe to re-execute.
	if blk, ok := blueprint.BlockFor(node.Type); ok && (blk.Effects == blueprint.EffectRead || blk.RetryClass == blueprint.RetrySafeRead) {
		return st.execNode(ctx, node), nil
	}
	return bpOutcome{err: fmt.Errorf("%w: checkpoint %s/%s outcome is unknown; manual reconcile required", errBlueprintUnknown, node.ID, cp.State)}, nil
}

func (st *bpState) intentResourceID(cp control.BlockCheckpoint) int64 {
	ports := st.outputs[cp.NodeID]
	if ports == nil {
		return 0
	}
	op, ok := ports["operation"].(map[string]any)
	if !ok {
		return 0
	}
	return toInt64(op["resource_id"], 0)
}

func (st *bpState) intentOperationID(cp control.BlockCheckpoint) int64 {
	ports := st.outputs[cp.NodeID]
	if ports == nil {
		return 0
	}
	op, ok := ports["operation"].(map[string]any)
	if !ok {
		return 0
	}
	return toInt64(op["operation_id"], 0)
}

func (st *bpState) intentProviderOperationID(cp control.BlockCheckpoint) string {
	ports := st.outputs[cp.NodeID]
	if ports == nil {
		return ""
	}
	s, _ := ports["provider_operation_id"].(string)
	return s
}

// resolveCreateByRead consults provider Operation/Server only. ok=false means
// there is not enough evidence to confirm the create, so the caller parks the
// run rather than replaying it.
func (st *bpState) resolveCreateByRead(ctx context.Context, res control.CloudResourceV2, cp control.BlockCheckpoint) (bpOutcome, bool) {
	conn, err := st.r.control.HostingConnectionOwned(res.ConnectionID, st.run.UserID)
	if err != nil {
		return bpOutcome{}, false
	}
	prov, err := st.r.cloudProviderFor(conn)
	if err != nil {
		return bpOutcome{}, false
	}
	extID := res.ExternalID
	if extID == "" {
		providerOpID := st.intentProviderOperationID(cp)
		if providerOpID == "" {
			return bpOutcome{}, false
		}
		op, err := prov.Operation(ctx, providerOpID)
		if err != nil || op == nil {
			return bpOutcome{}, false
		}
		switch op.Status {
		case "succeeded":
			if op.ResourceID == "" {
				return bpOutcome{}, false
			}
			extID = op.ResourceID
		case "failed":
			return bpOutcome{err: fmt.Errorf("%w: provider operation failed", errBlueprintUnknown)}, true
		default:
			return bpOutcome{}, false
		}
	}
	srv, err := prov.Server(ctx, extID)
	if err != nil || srv == nil {
		return bpOutcome{}, false
	}
	if srv.Password != "" {
		if enc, err := st.r.sealJSON(map[string]string{"password": srv.Password}); err == nil {
			_ = st.r.control.SetCloudResourceSecrets(res.ID, enc)
		}
	}
	if err := st.r.control.UpdateCloudResourceState(res.ID, res.State, control.CloudResReady, extID, srv.IP); err != nil {
		fresh, rerr := st.r.control.CloudResourceOwned(res.ID, st.run.UserID)
		if rerr != nil || fresh.State != control.CloudResReady {
			return bpOutcome{}, false
		}
	}
	opID := st.intentOperationID(cp)
	return bpOutcome{port: "ok", resourceID: -1, outputs: map[string]any{
		"operation": map[string]any{"operation_id": opID, "resource_id": res.ID},
	}}, true
}

// ── block implementations ────────────────────────────────────────

func (st *bpState) execNode(ctx context.Context, node blueprint.Node) bpOutcome {
	switch node.Type {
	case "server.create":
		return st.createServer(ctx, node)
	case "server.ready":
		return st.readyServer(ctx, node)
	case "install.text":
		return st.installText(ctx, node)
	case "pool.admit":
		return st.poolAdmit(ctx, node)
	case "event.ip_ban":
		return st.eventIPBan(node)
	case "policy.guard":
		return st.policyGuard(node)
	case "server.distinct_ip":
		return st.distinctIP(node)
	case "resource.fence":
		return st.fenceResource(node)
	case "resource.switch":
		return st.switchResource(node)
	case "server.delete":
		return st.deleteServer(ctx, node)
	case "server.absent":
		return st.absentServer(ctx, node)
	case "observe.arm":
		return st.observeArm(node)
	case "terminal.stop":
		return bpOutcome{terminal: true, runState: control.RunCancelled, resourceID: -1}
	case "ip.order", "ip.attach", "ip.switch_advertised", "ip.cleanup", "ip.verify":
		return bpOutcome{err: fmt.Errorf("%w: %s is Beget-only and not implemented in stage-08b-i", errBlueprintNotImplemented, node.Type)}
	}
	return bpOutcome{err: fmt.Errorf("unsupported block %s", node.Type)}
}

func (st *bpState) createServer(ctx context.Context, node blueprint.Node) bpOutcome {
	purpose := paramString(node, "purpose", "initial")
	connAny, err := st.required(node, "connection")
	if err != nil {
		return bpOutcome{err: err}
	}
	connID := toInt64(connAny, 0)
	cfgAny, err := st.required(node, "configuration")
	if err != nil {
		return bpOutcome{err: err}
	}
	cfg, err := createRequestFrom(cfgAny)
	if err != nil {
		return bpOutcome{err: err}
	}
	// Во флоте configuration общая и name пустой — имя/лейбл элемента задаёт
	// нода. Без этого validCloudConfig отклоняет конфигурацию, а установка
	// получает пустой label (клейм pending по префиксу label- невозможен).
	if strings.TrimSpace(cfg.Name) == "" {
		base := st.run.NodeID
		if i := strings.LastIndex(base, "-"); i > 0 {
			base = base[:i]
		}
		if base == "" {
			base = "node"
		}
		if len(base) > 10 {
			base = base[:10]
		}
		cfg.Name = base
	}
	conn, err := st.r.control.HostingConnectionOwned(connID, st.run.UserID)
	if err != nil {
		return bpOutcome{err: err}
	}
	if conn.Provider != st.bp.Provider {
		return bpOutcome{err: fmt.Errorf("connection provider %s does not match blueprint %s", conn.Provider, st.bp.Provider)}
	}
	if conn.Provider != control.ProviderNodehost {
		return bpOutcome{err: fmt.Errorf("%w: graph create for provider %s", errBlueprintNotImplemented, conn.Provider)}
	}
	if !validCloudConfig(cfg) {
		return bpOutcome{err: control.ErrValidation}
	}
	replaces := int64(0)
	if st.run.Entry == "initial" {
		if err := st.r.control.CheckNamedDeployment(st.run.UserID, st.run.NodeID, st.run.ID); err != nil {
			return bpOutcome{err: err}
		}
	}
	if purpose == "replacement" {
		// H7: one unresolved run per policy; never grow a chain of paid VPS.
		if n, err := st.r.control.ActiveRunsForNode(st.run.NodeID, st.run.ID); err != nil {
			return bpOutcome{err: err}
		} else if n > 0 {
			return bpOutcome{err: errors.New("unresolved_replacement: another active run exists for this policy")}
		}
		// H7/H8: the previous VPS must be confirmed gone before buying another,
		// even if its run was cancelled and is now terminal. This also counts a
		// paid replacement left unfenced by a cancelled run (H8).
		if n, err := st.r.control.UnresolvedFencedResources(st.run.UserID, st.run.NodeID, st.run.CurrentResourceID); err != nil {
			return bpOutcome{err: err}
		} else if n > 0 {
			return bpOutcome{err: errors.New("unresolved_replacement: previous fenced resource is not deleted")}
		}
		if st.run.CurrentResourceID > 0 {
			replaces = st.run.CurrentResourceID
		}
	}
	resource := control.CloudResourceV2{
		UserID: st.run.UserID, ConnectionID: conn.ID, Provider: conn.Provider,
		Label: cfg.Name, State: control.CloudResPlanned, ReplacesResourceID: replaces,
	}
	var res control.CloudResourceV2
	if st.run.Entry == "initial" && st.run.NodeID != "" {
		res, err = st.r.control.CreateNamedDeploymentResource(st.run, resource)
	} else {
		res, err = st.r.control.CreateCloudResource(resource)
	}
	if err != nil {
		return bpOutcome{err: err}
	}
	opKey := fmt.Sprintf("bpcreate:%d:%s", st.run.ID, node.ID)
	body, _ := json.Marshal(map[string]any{"run_id": st.run.ID, "node_id": node.ID, "resource_id": res.ID})
	op, created, err := st.r.control.CreateOperation(control.Operation{
		UserID: st.run.UserID, Kind: control.OpKindCloudCreate, State: control.OpPending,
		IdemKey: opKey, RequestJSON: string(body),
	})
	if err != nil {
		// L4: do not leave an orphan planned row behind.
		_ = st.r.control.DeletePlannedCloudResource(res.ID)
		return bpOutcome{err: err}
	}
	_ = created
	// Persist intent before the paid call.
	if _, err := st.saveCheckpoint(node.ID, control.CheckpointDispatched, st.checkpoints[node.ID].Attempt,
		map[string]any{"operation": map[string]any{"operation_id": op.ID, "resource_id": res.ID}}, "", ""); err != nil {
		return bpOutcome{err: err}
	}
	// Держим общий guard по op.ID, пока идёт создание: фоновый
	// reconcileStuckCloudCreates иначе увидит op без живого воркера и
	// вмешается в создание, которое ведёт ран (был реальный двойной исход).
	var providerOpID string
	createDone := make(chan struct{})
	if !st.r.provisionWorkers().run(op.ID, cloudCreateBudget+cloudPollInterval, func(wctx context.Context) {
		defer close(createDone)
		providerOpID = st.r.cloudCreateDo(wctx, res.ID, conn, cfg, opKey, op.ID)
	}) {
		return bpOutcome{err: fmt.Errorf("%w: create worker busy", errBlueprintUnknown)}
	}
	<-createDone
	// Keep the provider operation id in the intent so a read-only reconcile
	// (Operation/Server) can resolve an ambiguous create without re-sending it.
	if providerOpID != "" {
		_, _ = st.saveCheckpoint(node.ID, control.CheckpointDispatched, st.checkpoints[node.ID].Attempt,
			map[string]any{"operation": map[string]any{"operation_id": op.ID, "resource_id": res.ID},
				"provider_operation_id": providerOpID}, "", "")
	}
	if ctx.Err() != nil {
		// H1: a cancelled worker means the paid outcome is unknown, never a
		// safe retry.
		return bpOutcome{err: fmt.Errorf("%w: create worker cancelled", errBlueprintUnknown)}
	}
	fresh, err := st.r.control.CloudResourceOwned(res.ID, st.run.UserID)
	if err != nil {
		return bpOutcome{err: err}
	}
	if fresh.State == control.CloudResError {
		return bpOutcome{err: fmt.Errorf("provider create failed")}
	}
	if fresh.State != control.CloudResReady {
		return bpOutcome{err: fmt.Errorf("%w: create dispatched but resource is %s; manual reconcile required", errBlueprintUnknown, fresh.State)}
	}
	return bpOutcome{port: "ok", resourceID: -1, outputs: map[string]any{
		"operation": map[string]any{"operation_id": op.ID, "resource_id": res.ID},
	}}
}

func createRequestFrom(v any) (nodehost.CreateRequest, error) {
	switch x := v.(type) {
	case map[string]any:
		raw, _ := json.Marshal(x)
		var cfg nodehost.CreateRequest
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nodehost.CreateRequest{}, err
		}
		return cfg, nil
	case string:
		var cfg nodehost.CreateRequest
		if err := json.Unmarshal([]byte(x), &cfg); err != nil {
			return nodehost.CreateRequest{}, errors.New("configuration must be an inline object")
		}
		return cfg, nil
	}
	return nodehost.CreateRequest{}, errors.New("configuration input is required")
}

func (st *bpState) readyServer(ctx context.Context, node blueprint.Node) bpOutcome {
	v, err := st.required(node, "operation")
	if err != nil {
		return bpOutcome{err: err}
	}
	op, err := serverOperationFromAny(v)
	if err != nil {
		return bpOutcome{err: err}
	}
	timeout := paramInt(node, "timeout_seconds", 600)
	deadline := st.r.blueprintNow().Add(time.Duration(timeout) * time.Second)
	for {
		res, err := st.r.control.CloudResourceOwned(op.ResourceID, st.run.UserID)
		if err != nil {
			return bpOutcome{err: err}
		}
		switch res.State {
		case control.CloudResReady:
			return bpOutcome{port: "ok", resourceID: -1, outputs: map[string]any{"server": serverFromResource(res)}}
		case control.CloudResError, control.CloudResDeleted:
			return bpOutcome{err: fmt.Errorf("resource is %s", res.State)}
		}
		if st.r.blueprintNow().After(deadline) {
			return bpOutcome{waiting: control.RunWaitingRetry, waitSecs: 30, outputs: map[string]any{"operation": v}}
		}
		select {
		case <-ctx.Done():
			return bpOutcome{err: ctx.Err()}
		case <-time.After(st.r.blueprintPoll()):
		}
	}
}

func serverOperationFromAny(v any) (struct {
	OperationID int64 `json:"operation_id"`
	ResourceID  int64 `json:"resource_id"`
}, error) {
	var out struct {
		OperationID int64 `json:"operation_id"`
		ResourceID  int64 `json:"resource_id"`
	}
	raw, _ := json.Marshal(v)
	if err := json.Unmarshal(raw, &out); err != nil || out.ResourceID <= 0 {
		return out, errors.New("invalid operation handle")
	}
	return out, nil
}

func (st *bpState) installText(ctx context.Context, node blueprint.Node) bpOutcome {
	v, err := st.required(node, "server")
	if err != nil {
		return bpOutcome{err: err}
	}
	srv, err := serverFromAny(v)
	if err != nil {
		return bpOutcome{err: err}
	}
	verAny, err := st.required(node, "version")
	if err != nil {
		return bpOutcome{err: err}
	}
	pvID := toInt64(verAny, 0)
	pv, err := st.r.control.ProfileVersionByNumericID(pvID)
	if err != nil {
		return bpOutcome{err: err}
	}
	if pv.Format != control.FormatText {
		return bpOutcome{err: errors.New("installation version must be format=text")}
	}
	p, err := st.r.control.Profile(pv.ProfileID)
	if err != nil || (!p.Published && p.CreatedBy != st.run.UserID) {
		return bpOutcome{err: control.ErrNotFound}
	}
	res, err := st.r.control.CloudResourceOwned(srv.ResourceID, st.run.UserID)
	if err != nil {
		return bpOutcome{err: err}
	}
	password, err := st.r.cloudInstallPassword(res)
	if err != nil || password == "" {
		return bpOutcome{err: errors.New("no sealed root password for resource")}
	}
	var vars map[string]any
	if st.run.VarsJSON != "" {
		_ = json.Unmarshal([]byte(st.run.VarsJSON), &vars)
	}
	vars, err = control.MergeProfileVars(pv.VarsSchema, vars)
	if err != nil {
		return bpOutcome{err: err}
	}
	vars, err = st.r.provisionBaseVars(vars)
	if err != nil {
		return bpOutcome{err: err}
	}
	secret, enc, err := st.r.manualPendingSecret(control.Operation{})
	if err != nil {
		return bpOutcome{err: err}
	}
	vars["label"], vars["node_secret"] = res.Label, secret
	if err := control.ValidateExtraVars(vars); err != nil {
		return bpOutcome{err: err}
	}
	u, err := st.r.control.User(st.run.UserID)
	if err != nil {
		return bpOutcome{err: err}
	}
	op, err := st.r.control.AuthorizeCloudInstall(res.ID, u.User, pv.ProfileID, secret, enc)
	if err != nil {
		return bpOutcome{err: err}
	}
	// Intent persisted before SSH: an unknown shell result is never replayed.
	if _, err := st.saveCheckpoint(node.ID, control.CheckpointDispatched, st.checkpoints[node.ID].Attempt,
		map[string]any{"server": serverFromResource(res), "operation_id": op.ID}, "", ""); err != nil {
		return bpOutcome{err: err}
	}
	prov := provision.New(st.r.control, nil, st.r.provisionConfig())
	_, _, runErr := prov.InstallProfile(ctx, provision.InstallRequest{
		OperationID: op.ID, ProfileID: pv.ProfileID,
		Host: res.IP, Port: 0, User: "root",
		Auth: sshx.Auth{Type: "password", Password: password},
		Vars: vars, Secrets: []string{password, secret},
		Confirm: func() error {
			nodeID := st.run.NodeID
			if nodeID == "" {
				got, err := st.r.control.Operation(op.ID)
				if err != nil {
					return err
				}
				nodeID = got.NodeID
			}
			if nodeID == "" {
				return errors.New("node did not register with the registry")
			}
			return st.r.control.SetCloudResourceNode(res.ID, nodeID, control.CloudResReady)
		},
	})
	if runErr != nil {
		return bpOutcome{err: runErr}
	}
	fresh, err := st.r.control.CloudResourceOwned(res.ID, st.run.UserID)
	if err != nil {
		return bpOutcome{err: err}
	}
	return bpOutcome{port: "ok", resourceID: -1, outputs: map[string]any{"server": serverFromResource(fresh)}}
}

func (st *bpState) poolAdmit(ctx context.Context, node blueprint.Node) bpOutcome {
	v, err := st.required(node, "server")
	if err != nil {
		return bpOutcome{err: err}
	}
	srv, err := serverFromAny(v)
	if err != nil {
		return bpOutcome{err: err}
	}
	res, err := st.r.control.CloudResourceOwned(srv.ResourceID, st.run.UserID)
	if err != nil {
		return bpOutcome{err: err}
	}
	if st.r.bpAdmit != nil {
		ok, err := st.r.bpAdmit(ctx, st.run, res)
		if err != nil {
			return bpOutcome{err: err}
		}
		if !ok {
			return bpOutcome{err: errors.New("admission denied")}
		}
	}
	if err := st.r.blueprintResourceReady(res); err != nil {
		return bpOutcome{err: err}
	}
	return bpOutcome{port: "ok", resourceID: -1, outputs: map[string]any{"server": serverFromResource(res)}}
}

// Reuse queue health without changing queue rules. Destructive automation also
// requires explicit current resource binding, fresh independent GP/TCP proof,
// and a known compatible port; test admission callbacks can only add a veto.
func (r *Registry) blueprintResourceReady(res control.CloudResourceV2) error {
	r.cfgMu.RLock()
	freshness, gpTTL, port := r.cfg.ReportFreshnessTTL, r.cfg.GlobalpingValidityTTL, r.cfg.SharedProxy.Port
	r.cfgMu.RUnlock()
	r.mu.RLock()
	defer r.mu.RUnlock()
	c := r.state.Candidates[res.NodeID]
	now := r.blueprintNow()
	fresh := func(at time.Time, ttl time.Duration) bool {
		// Zero future-clock tolerance; both registration and endpoint epochs bind proof.
		return ttl > 0 && !at.IsZero() && !at.Before(c.RegisteredAt) &&
			!at.Before(c.EndpointRegisteredAt) && !at.After(now) && now.Sub(at) <= ttl
	}
	if res.State != control.CloudResReady || res.NodeID == "" || res.IP == "" ||
		c == nil || c.RegisteredAt.IsZero() || c.NodeID != res.NodeID || c.IP != res.IP || c.Quarantine != nil ||
		r.terminatedBlockingLocked(res.NodeID, res.IP) != nil ||
		!c.IsFullyHealthy(freshness) || !c.MetricsOK ||
		c.PortCompatible == nil || !*c.PortCompatible || port <= 0 || c.Port != port ||
		c.GlobalpingMeasurementID == "" || !fresh(c.GlobalpingMeasurementAt, gpTTL) ||
		!fresh(c.LastReportAt, freshness) || !fresh(c.LastMetricsCheckedAt, freshness) || len(c.TCPHist) == 0 {
		return errors.New("readiness_unverified: resource-bound independent health proof required")
	}
	tcp := c.TCPHist[len(c.TCPHist)-1]
	if !tcp.OK || !fresh(tcp.At, freshness) {
		return errors.New("readiness_unverified: fresh TCP proof required")
	}
	return nil
}

func (st *bpState) readyReplacement(resourceID int64) error {
	res, err := st.r.control.CloudResourceOwned(resourceID, st.run.UserID)
	if err != nil {
		return err
	}
	return st.r.blueprintResourceReady(res)
}

func (st *bpState) eventIPBan(node blueprint.Node) bpOutcome {
	v, err := st.required(node, "server")
	if err != nil {
		return bpOutcome{err: err}
	}
	srv, err := serverFromAny(v)
	if err != nil {
		return bpOutcome{err: err}
	}
	epAny, _, _ := st.binding(node, "episode")
	episode, _ := epAny.(string)
	if episode == "" {
		// event.ip_ban produces the episode output; the durable delivery
		// injects it as a run input, not as an input binding.
		if v, ok, _ := st.runInput("episode_id"); ok {
			episode, _ = v.(string)
		}
	}
	if episode == "" {
		return bpOutcome{waiting: control.RunWaitingEvent, resourceID: -1, outputs: map[string]any{"server": srv}}
	}
	return bpOutcome{port: "ok", resourceID: -1, outputs: map[string]any{"server": srv, "episode": episode}}
}

func (st *bpState) policyGuard(node blueprint.Node) bpOutcome {
	v, err := st.required(node, "server")
	if err != nil {
		return bpOutcome{err: err}
	}
	srv, err := serverFromAny(v)
	if err != nil {
		return bpOutcome{err: err}
	}
	epAny, _ := st.required(node, "episode")
	episode, _ := epAny.(string)
	outs := map[string]any{"server": srv, "episode": episode}
	m, err := st.r.control.NodeManagement(st.run.NodeID, st.run.UserID)
	if err != nil {
		if errors.Is(err, control.ErrNotFound) {
			return bpOutcome{port: "stop", resourceID: -1, outputs: outs}
		}
		return bpOutcome{err: err}
	}
	if m.Mode != "profile" || m.ProfileVersionID != st.run.ProfileVersionID {
		return bpOutcome{port: "stop", resourceID: -1, outputs: outs}
	}
	if st.run.PolicyRevision > 0 && m.Revision != st.run.PolicyRevision {
		return bpOutcome{port: "stop", resourceID: -1, outputs: outs}
	}
	if u, err := st.r.control.User(st.run.UserID); err != nil || u.Disabled {
		return bpOutcome{port: "stop", resourceID: -1, outputs: outs}
	}
	return bpOutcome{port: "active", resourceID: -1, outputs: outs}
}

func (st *bpState) distinctIP(node blueprint.Node) bpOutcome {
	oldAny, err := st.required(node, "old")
	if err != nil {
		return bpOutcome{err: err}
	}
	newAny, err := st.required(node, "new")
	if err != nil {
		return bpOutcome{err: err}
	}
	oldS, err := serverFromAny(oldAny)
	if err != nil {
		return bpOutcome{err: err}
	}
	newS, err := serverFromAny(newAny)
	if err != nil {
		return bpOutcome{err: err}
	}
	banned, _, _ := st.runInput("banned_ip")
	bannedIP, _ := banned.(string)
	if bannedIP != "" && newS.IP == bannedIP {
		return bpOutcome{err: fmt.Errorf("new resource reuses banned IP %s", bannedIP)}
	}
	_ = oldS
	return bpOutcome{port: "ok", resourceID: -1, outputs: map[string]any{"server": newS}}
}

func (st *bpState) fenceResource(node blueprint.Node) bpOutcome {
	oldAny, err := st.required(node, "old")
	if err != nil {
		return bpOutcome{err: err}
	}
	newAny, err := st.required(node, "new")
	if err != nil {
		return bpOutcome{err: err}
	}
	oldS, err := serverFromAny(oldAny)
	if err != nil {
		return bpOutcome{err: err}
	}
	newS, err := serverFromAny(newAny)
	if err != nil {
		return bpOutcome{err: err}
	}
	if oldS.ResourceID == newS.ResourceID {
		return bpOutcome{err: errors.New("fence requires distinct resources")}
	}
	if err := st.readyReplacement(newS.ResourceID); err != nil {
		return bpOutcome{err: err}
	}
	if err := st.r.control.SetCloudResourceReplacedBy(oldS.ResourceID, newS.ResourceID); err != nil {
		return bpOutcome{err: err}
	}
	return bpOutcome{port: "ok", resourceID: -1, outputs: map[string]any{"server": newS}}
}

func (st *bpState) switchResource(node blueprint.Node) bpOutcome {
	oldAny, err := st.required(node, "old")
	if err != nil {
		return bpOutcome{err: err}
	}
	newAny, err := st.required(node, "new")
	if err != nil {
		return bpOutcome{err: err}
	}
	oldS, err := serverFromAny(oldAny)
	if err != nil {
		return bpOutcome{err: err}
	}
	newS, err := serverFromAny(newAny)
	if err != nil {
		return bpOutcome{err: err}
	}
	if oldS.ResourceID == newS.ResourceID {
		return bpOutcome{err: errors.New("switch requires distinct resources")}
	}
	if err := st.readyReplacement(newS.ResourceID); err != nil {
		return bpOutcome{err: err}
	}
	if err := st.r.control.SetNodeManagementCurrentResource(st.run.NodeID, st.run.UserID, newS.ResourceID); err != nil {
		return bpOutcome{err: err}
	}
	return bpOutcome{port: "ok", resourceID: newS.ResourceID, outputs: map[string]any{"server": newS}}
}

func (st *bpState) deleteServer(ctx context.Context, node blueprint.Node) bpOutcome {
	v, err := st.required(node, "server")
	if err != nil {
		return bpOutcome{err: err}
	}
	srv, err := serverFromAny(v)
	if err != nil {
		return bpOutcome{err: err}
	}
	res, err := st.r.control.CloudResourceOwned(srv.ResourceID, st.run.UserID)
	if err != nil {
		return bpOutcome{err: err}
	}
	if res.State == control.CloudResDeleted {
		return bpOutcome{port: "ok", resourceID: -1, outputs: map[string]any{"server": srv}}
	}
	// Automated deletion is only teardown of a fenced, switched replacement.
	// Do not trust a run snapshot or an earlier successful pool.admit checkpoint.
	if res.ReplacedByResourceID == 0 || res.ReplacedByResourceID == res.ID {
		return bpOutcome{err: errors.New("readiness_unverified: deletion requires a distinct fenced replacement")}
	}
	if err := st.readyReplacement(res.ReplacedByResourceID); err != nil {
		return bpOutcome{err: err}
	}
	policy, err := st.r.control.NodeManagement(st.run.NodeID, st.run.UserID)
	if err != nil {
		return bpOutcome{err: err}
	}
	if policy.CurrentResourceID != res.ReplacedByResourceID {
		return bpOutcome{err: errors.New("readiness_unverified: replacement is not the current policy resource")}
	}
	conn, err := st.r.control.HostingConnectionOwned(res.ConnectionID, st.run.UserID)
	if err != nil {
		return bpOutcome{err: err}
	}
	// H3: each dispatch gets its own attempt key, so a failed previous
	// operation never blocks MarkCreateSent (pending→sent).
	opKey := fmt.Sprintf("bpdelete:%d:%s:%d", st.run.ID, node.ID, st.checkpoints[node.ID].Attempt)
	op, _, err := st.r.control.CreateOperation(control.Operation{
		UserID: st.run.UserID, NodeID: res.NodeID, Kind: control.OpKindCloudDelete,
		State: control.OpPending, IdemKey: opKey, ProviderVPSID: res.ExternalID,
	})
	if err != nil {
		return bpOutcome{err: err}
	}
	if err := st.readyReplacement(res.ReplacedByResourceID); err != nil {
		return bpOutcome{err: err}
	}
	st.r.cloudDeleteDo(ctx, res.ID, conn, res.ExternalID, op.ID)
	if ctx.Err() != nil {
		return bpOutcome{waiting: control.RunWaitingRetry, waitSecs: 30, outputs: map[string]any{"server": srv}}
	}
	fresh, err := st.r.control.CloudResourceOwned(res.ID, st.run.UserID)
	if err != nil {
		return bpOutcome{err: err}
	}
	if fresh.State != control.CloudResDeleted {
		// A provider error (state=error) is definitive and retryable. Any
		// other non-deleted state is an unknown in-flight delete.
		return bpOutcome{waiting: control.RunWaitingRetry, waitSecs: 30,
			retryDispatch: fresh.State == control.CloudResError, outputs: map[string]any{"server": srv}}
	}
	return bpOutcome{port: "ok", resourceID: -1, outputs: map[string]any{"server": srv}}
}

func (st *bpState) absentServer(ctx context.Context, node blueprint.Node) bpOutcome {
	v, err := st.required(node, "server")
	if err != nil {
		return bpOutcome{err: err}
	}
	srv, err := serverFromAny(v)
	if err != nil {
		return bpOutcome{err: err}
	}
	timeout := paramInt(node, "timeout_seconds", 600)
	deadline := st.r.blueprintNow().Add(time.Duration(timeout) * time.Second)
	for {
		res, err := st.r.control.CloudResourceOwned(srv.ResourceID, st.run.UserID)
		if err == nil && res.State == control.CloudResDeleted {
			return bpOutcome{port: "ok", resourceID: -1, outputs: map[string]any{"server": srv}}
		}
		if err != nil && errors.Is(err, control.ErrNotFound) {
			return bpOutcome{port: "ok", resourceID: -1, outputs: map[string]any{"server": srv}}
		}
		if st.r.blueprintNow().After(deadline) {
			return bpOutcome{waiting: control.RunWaitingRetry, waitSecs: 30, outputs: map[string]any{"server": srv}}
		}
		select {
		case <-ctx.Done():
			return bpOutcome{err: ctx.Err()}
		case <-time.After(st.r.blueprintPoll()):
		}
	}
}

func (st *bpState) observeArm(node blueprint.Node) bpOutcome {
	v, err := st.required(node, "server")
	if err != nil {
		return bpOutcome{err: err}
	}
	srv, err := serverFromAny(v)
	if err != nil {
		return bpOutcome{err: err}
	}
	if st.run.NodeID != "" {
		_ = st.r.control.SetNodeManagementCurrentResource(st.run.NodeID, st.run.UserID, srv.ResourceID)
	}
	// Delivery is already consumed by ban_deliveries; the next episode creates
	// the successor run. No recursive launch from inside this run.
	return bpOutcome{terminal: true, runState: control.RunSucceeded, resourceID: srv.ResourceID, outputs: map[string]any{"server": srv}}
}
