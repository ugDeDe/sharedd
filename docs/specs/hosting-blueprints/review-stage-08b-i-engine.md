# Review — Stage 08b-i durable run engine (Nodehost)

**Read-only review.** No application code was changed. Scope: AGENTS.md,
[04](04-storage-and-execution.md), [05](05-blueprint-profiles.md),
[03 §5–§6](03-cabinet-api.md), stage-08a and stage-08b-i implementation notes,
plus `registry/internal/control/{blueprint_runs,control,cloud_resources,profiles,operations}.go`
and `registry/internal/server/{blueprint_engine,blueprint_runs_api,blueprint_engine_test,cabinet_cloud,terminate,run_main_impl,cabinet_api}.go`.

## Verification run

From `registry/`:

```
go test -race -count=2 -run 'Blueprint|Run|Ban|Engine|Management' ./internal/server ./internal/control
# ok  sharedd/registry/internal/server  35.461s
# ok  sharedd/registry/internal/control 10.793s

go vet ./internal/server ./internal/control ./internal/blueprint   # clean
staticcheck                                                     # not installed in this environment
```

The existing tests are green, but the paid-action crash/race scenarios below
are not covered by them.

## Summary

| Severity | Count | Status |
|---|---|---|
| CRITICAL | 1 | block |
| HIGH | 7 | block |
| MEDIUM | 5 | fix before merge |
| LOW | 4 | optional |

**Verdict: BLOCK.**

The core journal shape (checkpoint + lease + UNIQUE delivery) is sound and the
documented happy paths work. The blocking problems are all around *when a
paid mutation may be (re)entered* and around lifecycle ownership of pinned
artefacts.

---

## CRITICAL

### C1. Concurrent duplicate `POST /api/me/profile-runs` creates two runs → two paid purchases

`registry/internal/server/blueprint_runs_api.go:280-301`

The run is created **before** the idempotency gate:

```go
run, err := r.control.CreateRun(...)          // 280
...
op, created, err := r.control.CreateOperation(...) // 290
if !created { r.replayProfileRunAck(...); return } // 298
```

`CreateOperation` is the atomic per-key barrier (`operations.idem_key` UNIQUE),
but by then the losing request has already inserted a `profile_runs` row with
state `pending` and `next_wake_at=0`. That row is never claimed by the caller
but **is** claimed by the periodic scheduler (`ListClaimableRuns`), and its
provider key is `bpcreate:<run_id>:<node>` — a *different* run id, therefore a
*different* nodehost Idempotency-Key. Result: identical concurrent requests buy
two VPSs.

Impact: direct double charge on a money path; the API behaves as if only one
run exists.

Fix: make "insert operation ack + insert run" one transaction (new
`control.CreateRunIdempotent(opKey, run)` doing `INSERT operations ... ON
CONFLICT DO NOTHING` then `INSERT profile_runs` and `UPDATE
operations.request_json` only when the op insert won). If the op already
exists, do not create a run. Also link `profile_runs` to the operation id so
the ack is unambiguous.

Acceptance test (`-race -count=5`): two goroutines POST the same key/body;
exactly one `profile_runs` row, one `Create` call on the fake provider, both
responses 202 with the same `run_id`.

---

## HIGH

### H1. `waiting_retry` downgrades a dispatched paid checkpoint, so the next tick replays `server.create`

`blueprint_engine.go:486-495` + `:721-725` + `:422-449`

`applyOutcome` persists every waiting outcome as `CheckpointWaiting`:

```go
if out.waiting != "" {
    _, _ = st.complete(node.ID, control.CheckpointWaiting, ...) // 487
```

Any mutation that returns `waiting` after I/O has already been attempted —
`createServer` when the resource is still `planned/creating`, and
`server.delete` whenever the delete is not confirmed — overwrites the
`dispatched` checkpoint with `waiting`. On the next claim the checkpoint is
`waiting`, which is neither `confirmed` nor in the dispatch/reconcile branch,
so the engine falls through to `saveCheckpoint(dispatched)` + `execNode` again
(`:450-456`). For create this inserts a **second** `cloud_resources2` row and
calls `cloudCreateDo` again with the same provider key; for delete it
re-enters the delete path (see H3), which is how H3's loop becomes permanent.

Trigger for create: a cancelled worker context or a transient failure of the
`planned→creating` CAS makes `cloudCreateDo` return with the resource still
`planned/creating`. Today the 30-minute worker timeout normally fires first in
`cloudCreateDo`'s own 10-minute budget, so this is narrow, but it is a real
violation of "a paid action is never replayed". Nodehost dedupes by key, so
the usual outcome is one charge and one orphaned local resource row — the
first paid row stays `creating` forever, untracked and undeletable through the
graph. If the provider key is scoped/expired differently, it is a second
charge.

Fix: a dispatched mutation checkpoint must never be downgraded. In
`applyOutcome`, if the checkpoint is (or was) `dispatched|unknown` and the
block effect is not `read`, persist `unknown` and move the run to
`needs_attention` (or keep `dispatched` for startup reconcile). In
`createServer`, map `ctx.Err()` from `cloudCreateDo` to
`errBlueprintUnknown`, not to `waiting`.

Acceptance test: fake provider whose operation poll blocks until the worker
context is cancelled; restart the engine (`server` package, reopen store);
assert `createCalls()==1`, no second `cloud_resources2` row, run is
`needs_attention` (not `waiting_retry`), and the first resource id is still
tracked.

### H2. Mode flip / policy revision is checked only at `policy.guard`, not before each effect

`blueprint_engine.go:404-470` (loop) and `:962-991` (`policyGuard`)

03 §6 and 04 §5.2 require the policy revision/mode/owner state to be re-read
*immediately before every effect*. The engine re-reads only `cancel_requested`
each iteration (`:408-416`); `policyGuard` runs once at its graph node. A user
who flips `profile → manual` (which bumps `node_management.revision`) after the
guard checkpoint is confirmed still gets `create_new`, `install`, `fence`,
`switch` and `delete_old` executed. The existing test only flips mode *before*
the guard, so it does not cover this.

Impact: manual mode does not actually invalidate future dispatch; the paid
replacement purchase proceeds after the user's explicit opt-out.

Fix: before `execNode`, for every block with `Effects != blueprint.EffectRead`,
load `NodeManagement(run.NodeID, run.UserID)` and require `mode=profile`,
`profile_version_id==run.ProfileVersionID`, `revision==run.PolicyRevision`,
owner not disabled; otherwise complete the checkpoint as rejected/skipped and
stop the run (`cancelled` / `needs_attention`). Keep the guard node as a
fast-path, not the only check.

Acceptance test: confirm the guard checkpoint, then `PUT mode=manual` with the
current revision, then run the worker; assert `createCalls()==0` and that no
mutation checkpoint was dispatched after the guard.

### H3. `server.delete` retry can never succeed (reused op is no longer `pending`)

`blueprint_engine.go:1095-1110`, `cabinet_cloud.go:920-932`

`deleteServer` uses a fixed op key `bpdelete:<run>:<node>`, so the retry
reuses the same `operations` row. `cloudDeleteDo` starts with
`MarkCreateSent` (pending → sent only). After the first provider error,
`failCloud` sets the op to `failed` and the resource to `error`. On retry:

1. `CreateOperation` returns the existing `failed` op (created=false);
2. `cloudDeleteDo` moves `error → deleting`;
3. `MarkCreateSent` returns false (state is `failed`), so it returns without
   calling the provider;
4. `deleteServer` sees `deleting`, returns `waiting_retry`.

Every subsequent tick hits the same branch: resource `deleting`, op `failed`,
no provider call, run `waiting_retry` forever. The replaced VPS is never
deleted and `server.absent`/`observe.arm` are never reached.

Fix: allocate a fresh operation + idem key per delete attempt (include the
checkpoint `attempt`), or add a delete-specific `MarkDeleteSent` allowing
`failed|error → sent`, and cap attempts (H5) then `needs_attention`.

Acceptance test: fake provider fails the first `Delete`, succeeds the second;
run two ticks; assert deleteCalls()==2 and resource `deleted`, run reaches
`absent`/`arm`.

### H4. Deleting a pinned profile silently orphans runs and disables ban handling

`registry/internal/control/profiles.go:171-191`,
`profiles_user_api.go:286`, `profiles_admin_api.go:255`

03 §6 requires `409 profile_in_use` when a profile has pinned runs/policies
and forbids deleting historical versions. `DeleteProfile` unconditionally
deletes `profile_versions` (no in-use check, no FK from
`profile_runs.profile_version_id` or `node_management.profile_version_id`).
Consequences:

- `loadBPState` fails (`pinned version: not found`) → active runs fail;
- `UndeliveredBanEpisodes` inner-joins `profile_versions`, so a PROFILE policy
  with a deleted version **silently stops receiving ban deliveries** — exactly
  the replacement protection the user enabled;
- reserved: `GET management` keeps showing a stale `profile_version_id`.

Fix: `DeleteProfile` (in the same transaction) returns a sentinel
`ErrProfileInUse` if any `profile_runs` with non-terminal state or any
`node_management` row references the profile's versions; map to
`409 profile_in_use` in both handlers. Keep versions for terminal runs
(historical), or detach with an explicit pinned-snapshot rule.

Acceptance test: enable profile mode for a node, then DELETE the profile →
409; without an active policy/runs → 204. Also: terminal run keeps version
readable (or returns a documented 410).

### H5. Blueprint workers are not stopped on shutdown

`run_main_impl.go:228-241`, `blueprint_engine.go:46-49`

`Run()` cancels/stops `provisionWorkers()` but never calls
`blueprintWorkers().stop()`. `blueprintLoop` returns on the signal context,
yet in-flight run goroutines keep their `context.Background()` base and can
continue provider create/delete/install while `Run()` closes `control` and
`history` and returns from process `main`. That is precisely how a provider
call is cut mid-flight, producing the "dispatched unresolved" windows the
engine is supposed to reconcile (and, on create, the replay window in H1).

Fix: create the blueprint worker with a cancellable base context, call
`r.blueprintWorkers().stop()` next to `provisionWorkers().stop()`, and wait for
the per-run goroutines (add a small WaitGroup to `provisionWorker`) before
closing the stores.

Acceptance test: start a run blocked in the fake provider poll, cancel the
registry context, assert the worker returns, the store is closed only after
the worker exits, and the checkpoint is `dispatched` (not `waiting`).

### H6. No panic recovery in the blueprint worker goroutine

`registry/internal/server/provision_worker.go:46-64`

`provisionWorker.run` executes `fn(ctx)` in a goroutine with no `recover`.
Cloud workers wrap their function with `cloudRecover`, but the blueprint
engine calls `cloudCreateDo`, `cloudDeleteDo` and
`provision.InstallProfile` **directly** from the run goroutine. A panic in any
of those (or in the JSON/checkpoint path) takes down the whole registry, not
just one run. 04 §5.1/§5.2 and the stage's own robustness target require the
run to fail closed to `needs_attention`.

Fix: `defer func(){ if p:=recover(); p!=nil { log...; set run needs_attention } }()`
inside `launchBlueprintRun`'s closure (or a `recover` option on
`provisionWorker.run`). Do not let `needs_attention` be silently re-claimed.

Acceptance test: inject a `cloudFactory` that panics; run a graph; process
survives and the run is `needs_attention` with a sanitized `last_error`.

### H7. No "one active run per policy" / unresolved-replacement guard before a replacement purchase

`control/blueprint_runs.go:466-513` (`CreateRunForBan`),
`blueprint_engine.go:687-697` (`purpose == replacement`)

`CreateRunForBan` inserts a run for every undelivered episode without checking
for an active run on the same policy, and `profile_runs.policy_id` is never
populated (always 0). `createServer(replacement)` only records
`replaces_resource_id`; it does not verify that another run for this policy is
not already unresolved (e.g. stuck in H3's delete loop). A new ban episode
while the previous replacement is unresolved therefore buys another VPS.
05 §6 and 04 §3 explicitly forbid growing a chain of paid VPS on repeated
bans.

Fix: populate `policy_id`, add a partial unique index
`UNIQUE(policy_id) WHERE state IN (pending,running,waiting_retry,waiting_event,reconciling,needs_attention,paused)`
(or check in the delivery transaction), and inside `createServer` reject a
`replacement` unless this run is the policy's sole non-terminal run and the
previous resource is fenced with no unresolved delete.

Acceptance test: create a policy with a run stuck in `waiting_retry` (delete
failure); add a second ban episode; assert no second run is created and
`createCalls()` stays 0.

---

## MEDIUM

### M1. Cancel of a run that is `running` without a live worker leaves it `running` forever

`control/blueprint_runs.go:153-168` and `:203-221`

`RequestRunCancel` leaves `running+cancel_requested=1` (correct), but
`ClaimRun` refuses to claim any row with `cancel_requested=1`, and no reaper
finalizes it. If the worker died (crash/restart) or the cancel raced the claim,
the run is permanently `running` in detail; only an accidental in-process
worker re-reading it would finalize. Fix: in `blueprintTick` (or startup),
transition `running|reconciling + cancel_requested=1` with a stale
`lease_generation`/`updated_at` to `cancelled`; or allow `ClaimRun` to claim
cancelled-flagged rows solely to finalize them. Test: row with
`running+cancel_requested`, restart, tick → `cancelled`.

### M2. Unbounded `waiting_retry` attempts, no attempt cap/backoff

`blueprint_engine.go:486-495`, `:759-780`, `:1114-1141`;
`control/blueprint_runs.go:252-265`

`attempt` increments without bound; `server.ready`, `server.absent`,
`server.delete` and `event.ip_ban` requeue forever at fixed 15/30 s. A provider
that never resolves a resource means the run and its provider polling never
stop. 04 §5.3 requires a bounded retry budget separated from cleanup. Fix: cap
attempts per block (`>= MaxRetry` or a new limit) → `needs_attention`; apply
exponential backoff with a ceiling; keep cleanup on its own budget. Test:
provider permanently `creating`; after N ticks the run is `needs_attention`
and no further provider calls are made.

### M3. `vars` are validated only after the purchase; observe runs lose the initial vars snapshot

`blueprint_runs_api.go:278-285`, `blueprint_engine.go:831-838`,
`control/blueprint_runs.go:475-479`

- `handleProfileRunCreate` stores `body.Vars` without validating it against
  the pinned text version (`MergeProfileVars`/`ValidateExtraVars` happen in
  `install.text`, i.e. after `server.create` has paid).
- `CreateRunForBan` hardcodes `VarsJSON: "{}"`, so a replacement install uses
  the pinned version's defaults, not the user's initial choices. 03 §6 and
  05 §6 require the saved installation snapshot for observe/replacement.
- `node_management` has no vars snapshot column either.

Fix: validate initial vars in the API (resolve
`installation_profile_version_id` from graph inputs, merge against its
`vars_schema`) before creating the run; persist the merged vars snapshot on
`cloud_resources2`/`node_management` and copy it into delivered runs. Test:
invalid var → 422 with zero provider calls; initial run with a non-default var
→ delivered replacement run installs with the same value.

### M4. `initial` run accepts an unregistered `node_id` and binds the resource without a claim

`blueprint_runs_api.go:190-201`, `blueprint_engine.go:870-883`

`NodeOwner` is enforced only when the owner is *found*; an unknown node id is
accepted. `install.text` then uses `run.NodeID` in `Confirm` and calls
`SetCloudResourceNode(res.ID, arbitraryNodeID, ready)`, binding the paid
resource to a node the user does not own/that never registered. Admission later
rejects it, but the resource is already bound in the journal (and the same
node id can collide with a future registration). Fix: for an explicit
`node_id`, require `ok && !legacy && owner == u.ID`; otherwise leave it empty
and let the registry registration claim it. Test: POST with a random
`node_id` → 404/422, no run.

### M5. Ban episode persistence is best-effort; a DB failure loses the verdict permanently

`terminate.go:300-313`, `control/blueprint_runs.go:376-385`

`recordBanEpisodeLocked` generates a random id and inserts once. On error it
only logs; no stable episode id is stored in the JSON `TerminatedRecord`, so
the reconciler can never recover that episode (at-least-once becomes
at-most-once). Fix: write the episode id into `TerminatedRecord` (state) and
retry the DB insert from the reconciler scan when the record has no row.
Test: make `AddBanEpisode` fail once, reopen, assert the episode is delivered
exactly once.

---

## LOW

- **L1** `runJSON.OperationID` is never populated (`blueprint_runs_api.go:35-37,
  58-64`); 03 §6 Run DTO promises it. Fill from a `profile_runs.operation_id`
  column or drop the field.
- **L2** Cancel/resume idempotency replay returns an ack for the run in the
  URL without checking the original operation's run/body
  (`blueprint_runs_api.go:362-364, 413-415`). Reusing a key on a different run
  id yields a 202 for the wrong object. Persist `run_id` in the ack and
  compare, as create does.
- **L3** Watermark resolution is 1 second: with `process_current_ban=false` a
  ban recorded in the same second as activation is treated as historical and
  skipped; with `true`, all episodes sharing that second are admitted
  (`control/blueprint_runs.go:444-445`). Store the episode id boundary, not
  just a timestamp.
- **L4** `createServer` never removes an orphan `planned` row if
  `CreateOperation` fails (`blueprint_engine.go:691-707`); the existing cloud
  path does (`DeletePlannedCloudResource`). Add the same cleanup.

---

## Confirmed correct (not findings)

- Ban delivery dedupe uses `UNIQUE(policy_node_id, episode_id)` inside the
  same transaction as run insertion; duplicate scans are no-ops, restart
  recovery is at-least-once (`CreateRunForBan`).
- Historical bans are skipped by default via the activation watermark; `dead`
  and prune do not create episodes, so they cannot trigger a purchase.
- `confirm:true`, session + CSRF, owner-scoped run/connection/resource access,
  and user-scoped `Idempotency-Key` are present on the run routes; foreign
  runs are 404.
- Checkpoint lease CAS and confirmed-immutability work (test:
  `TestProfileRunClaimAndCheckpointLease`); a dispatched create is reconciled
  from its persisted resource and not re-sent in the single-restart case
  (`TestBlueprintEngineCreateReconcileDoesNotReplay`).
- `waiting_event` persists state without holding a goroutine, and startup
  resumes it once.
- `ip.*` blocks fail with an explicit `not_implemented` error; no silent
  success.
- Run/checkpoint DTOs contain resource handles and provider codes, not root
  passwords or connection credentials; provider errors are reduced to codes
  (`safeBlueprintError`).
- Engine calls provider/SSH/Gateway work outside SQLite transactions; no DB
  lock is held across network I/O.

---

# Follow-up review (fixes re-review)

**Read-only.** Scope: code diffs for
`registry/internal/{control/{blueprint_runs,profiles,control,operations}.go,state/state.go,
server/{blueprint_engine,blueprint_runs_api,provision_worker,run_main_impl,terminate,users_admin_api}.go}`
and the new tests.

## Verification run

From `registry/`:

```
go vet ./...                                    # clean
go test -race -count=2 -run 'Blueprint|Run|Ban|Engine|Management|Profile' \
  ./internal/server ./internal/control          # ok (both packages, 126 tests)
go test ./...                                   # ok (628 tests, 15 packages)
```

## Per-finding status

| Finding | Status | Evidence |
|---|---|---|
| C1 double paid create | **closed** | `control.CreateRunIdempotent` gates on the `operations.idem_key` UNIQUE insert and inserts the run in the same tx; loser creates no run. `TestProfileRunCreateConcurrentIdempotent` (2 goroutines, `-race`) asserts one run, same run id, `createCalls()==1`; `TestCreateRunIdempotentGate` covers the store gate. |
| H1 dispatched checkpoint replay | **NOT fully closed** | The `out.waiting` branch preserves `dispatched → unknown` + `needs_attention` and is tested (`TestBlueprintDispatchedWaitingBecomesNeedsAttention`). But the `blockErr` branch (`blueprint_engine.go:568-575`) still writes `CheckpointRejected` (and clears `output_json`) for a mutation whose paid dispatch already happened (`createServer` maps cancelled/unresolved create to `errBlueprintUnknown`). See residual H1 below. |
| H2 policy re-read | **closed** | `policyActive()` runs before every non-read dispatch (`blueprint_engine.go:530-535`) and is exercised by `TestBlueprintManualFlipStopsNextEffect` (guard confirmed, then `mode=manual`; run cancelled, `createCalls()==0`). |
| H3 delete retry | **closed** | Per-attempt key `bpdelete:<run>:<node>:<attempt>`; definitive `state=error` is retryable, in-flight `deleting` becomes `needs_attention`. `TestBlueprintDeleteRetryDispatchesAgain` asserts two provider deletes and `deleted`. |
| H4 profile_in_use | **closed (code), test partial** | `DeleteProfile` is transactional and checks non-terminal runs + `node_management`; both handlers route through `writeControlErr` → `409 profile_in_use`. `TestDeleteProfileInUse` covers the policy-reference case only; no HTTP-level 409 test, and the non-terminal-run case is untested. |
| H5 workers stopped | **closed (workers), residual ordering** | `provisionWorker.stop()` cancels and waits on a WaitGroup; `Run()` calls it before closing stores. `TestProvisionWorkerStopWaits` proves stop waits. The `blueprintLoop` goroutine itself is not joined before stores close (residual M below). |
| H6 panic recovery | **closed** | `defer recover()` in `launchBlueprintRun` marks `needs_attention`/`worker_panic`; `TestBlueprintWorkerPanicContained` makes the provider panic and asserts the process survives. |
| H7 one active run per policy | **partial** | `policy_id` populated, partial UNIQUE index added, `CreateRunForBan` refuses while a non-terminal run exists (`TestBanDeliveryOneActiveRunPerPolicy`), and replacement create checks `ActiveRunsForNode`. Missing the second half of the required guard (previous resource fenced / no unresolved delete) — see residual H7. |
| M1 orphaned cancel | **closed** | `finalizeOrphanedCancels` skips live workers and CAS-finalizes the rest; `TestBlueprintOrphanedCancelFinalized` passes. |
| M2 retry cap | **closed** | `blueprintMaxAttempts=12`, exponential backoff capped at 600 s, `needs_attention` at cap; `TestBlueprintWaitingAttemptCap` asserts it. |
| M5 durable episode | **closed** | `TerminatedRecord.EpisodeID` persisted, `AddBanEpisode` is `ON CONFLICT DO NOTHING`, tick replays and clears; `TestBanEpisodePersistenceRetry` covers replay from JSON state. |
| L1 operation_id DTO | **closed** | Column populated and projected in `runJSON`; store test asserts it. |
| L2 ack run identity | **closed for sequential replay, residual race** | `runAckMatches` rejects key reuse across runs. Concurrent same-key cancels on two different runs can still both pass the pre-check and the loser returns 202 (no `created` check in cancel/resume handlers). Low impact (no money path). |
| L4 orphan planned row | **closed** | `DeletePlannedCloudResource` on create-operation failure. |

## Residual findings

### HIGH — H1: manual `resume decision=reconcile` replays a dispatched paid create

`blueprint_engine.go:568-575` completes the checkpoint as `rejected` and wipes
its output whenever a block returns an error — including `createServer`'s
`errBlueprintUnknown` (worker cancellation, or resource left
`planned/creating`). The run is correctly parked in `needs_attention`, so the
periodic tick does not replay it.

However, `handleProfileRunResume` (`blueprint_runs_api.go:487-515`) only
rejects `confirmed` checkpoints. For `decision=reconcile` it leaves a
`rejected` checkpoint untouched and moves the run to `reconciling`. On the next
claim, `executeProfileRun` sees `rejected` (not in the
`dispatched|unknown|claimed` reconcile branch), re-saves `dispatched`, and
calls `createServer` again — a second `cloud_resources2` row and a second
`prov.Create` with the same provider key. This is exactly the H1 impact
("one charge + orphaned local row; second charge if the provider key is
scoped/expired differently"), now reachable through the documented recovery
API instead of the scheduler. No test covers resume after an unknown create.

Minimum fix: in `applyOutcome`'s `blockErr` path, if the block is a non-read
effect and the checkpoint was `dispatched|unknown`, persist `unknown` (keep the
intent output) instead of `rejected`; and/or make `resume` with `reconcile`
require `dispatched|unknown` and route it through `reconcileNode` rather than
re-dispatch.

### HIGH — H7: replacement guard ignores an unresolved previous resource once the run is terminal

`createServer` only checks `ActiveRunsForNode(run.NodeID, run.ID)`. The
required second clause — previous resource fenced and its delete not
unresolved — is absent (`blueprint_engine.go:847-858`). `RequestRunCancel`
immediately moves `waiting_retry`/`running` runs to `cancelled`
(`control/blueprint_runs.go:322-340`) even when their delete is unresolved
(resource `error`/`deleting`). A subsequent ban episode then sees no active run
and buys another VPS while the previous one is still alive/unreconciled. The
review's acceptance case (`waiting_retry` still non-terminal) is covered; the
terminal-run variant is not.

Minimum fix: before a `replacement` create, reject when the policy's current
resource or any `replaces_resource_id` chain has an unresolved
`creating|deleting|error` state, or persist a resource-level unresolved-delete
marker on cancel and check it.

## Other residual (non-blocking)

- **MEDIUM — H5 ordering:** `blueprintLoop` is not joined before
  `closeControl()`/`history.Close()`. A tick already in `blueprintTick` can
  race shutdown and touch a closing store / start a run with an already-cancelled
  worker context. Close the worker set *and* wait for the loop goroutine.
- **MEDIUM — H4 tests:** no handler-level test asserting `409 profile_in_use`
  from the user and admin delete routes, and no test for the non-terminal-run
  branch (only policy reference).
- **LOW — L2:** concurrent same-key cancel/resume on two runs: handlers ignore
  `created` from `CreateOperation` and return 202 for the loser's run.
- **LOW — H6 logging:** the recovered panic value is logged verbatim; panic
  payloads could contain provider/secret data. Log only the type or a fixed
  token.

## Verdict

**BLOCK.** C1, H2, H3, H5(worker), H6, M1, M2, M5, L1, L4 are genuinely
closed with meaningful tests; H4 is fixed in code with partial tests; H1 and H7
retain money-path replay/purchase paths that the original findings and fix
requirements explicitly forbid. Re-review after the two HIGH residuals and a
regression test for each.


---

# Final focused re-review (H1/H7 residuals + cheap extras)

**Read-only.** No app code changed. Scope: updated
`implementation-stage-08b-i-engine.md` and the current diffs in
`registry/internal/{server/{blueprint_engine,blueprint_runs_api,cabinet_cloud,run_main_impl}.go,control/blueprint_runs.go}`
plus the new tests.

## Verification run

From `registry/`:

```
go vet ./...                                                       # clean
gofmt -l internal/server/blueprint_engine.go \
        internal/server/blueprint_runs_api.go \
        internal/server/cabinet_cloud.go \
        internal/server/run_main_impl.go \
        internal/control/blueprint_runs.go                         # empty
go test -race -count=2 -run 'Blueprint|Run|Ban|Engine|Management|Profile' \
  ./internal/server ./internal/control                             # 134 passed, 2 packages
git diff --check                                                   # clean
```

## Confirmation of requested items

| Item | Result | Evidence |
|---|---|---|
| H1 dispatched/unknown retain intent | **confirmed** | `applyOutcome` blockErr path (`blueprint_engine.go:570-578`) preserves `dispatched\|unknown` non-read checkpoints as `unknown` with `st.outputs` (intent) and parks `needs_attention`; the waiting path (`:587-595`) also refuses to downgrade unless `retryDispatch`. `createServer` maps `ctx.Err()` to `errBlueprintUnknown` (`:973-977`). `TestBlueprintDispatchedWaitingBecomesNeedsAttention`, `TestBlueprintCreateErrorResumeReconcileNeverReplays`. |
| Resume reconcile is read-only and never re-dispatches Create/Delete/ChangeIP | **confirmed** | Resume accepts `reconcile` only for `dispatched\|unknown\|claimed` (`blueprint_runs_api.go:496-503`); `reconcileNode` handles `server.create` via `Operation`/`Server` only (`:739-768`, `:803-856`) and returns `errBlueprintUnknown` for every other mutation. `TestBlueprintCreateErrorResumeReconcileNeverReplays` asserts `createCalls()==1` and exactly one `cloud_resources2` row after resume. |
| `retry_safe` read-only allowlist | **confirmed (code)** | `blueprint_runs_api.go:509-533` requires `Effects==EffectRead || RetryClass==RetrySafeRead`; `server.create` (purchase), `install.text`/`fence`/`switch`/`delete` (mutation) → `409 unsafe_resume`. No negative test yet (non-blocking test gap). |
| Acceptance test: provider Create called once | **confirmed** | `TestBlueprintCreateErrorResumeReconcileNeverReplays` (one create after dispatch error, still one after reconcile resume) and `TestProfileRunCreateConcurrentIdempotent`. |
| H7 fenced unresolved teardown blocks purchase after cancel/terminal | **confirmed** | `UnresolvedFencedResources` (`control/blueprint_runs.go:211-225`) is checked both in `CreateRunForBan` inside the delivery tx (`:630-641`) and immediately before a replacement create (`blueprint_engine.go:930-936`); it does not depend on run state. `TestBlueprintReplacementBlockedByUnresolvedFencedResource` cancels the run, proves zero creates/one run while the fenced resource is not `deleted`, then allows exactly one create after `deleted`. |
| H4 HTTP 409 | **confirmed** | `TestDeleteProfileInUseHTTP` asserts `409 profile_in_use` from both user and admin routes; control test covers the non-terminal branch. |
| H5 join | **confirmed** | `run_main_impl.go:228-241` closes `bpDone` after `stop()` and waits before `blueprintWorkers().stop()` / `closeControl()`; `provisionWorker.stop()` cancels + waits on the WaitGroup. `TestProvisionWorkerStopWaits`. |
| H6 sanitized panic | **confirmed** | `log.Printf("... type=%T", p)` (`blueprint_engine.go:190-197`); payload never logged. `TestBlueprintWorkerPanicContained`. |
| L2 concurrent key | **confirmed** | Cancel/resume check `created` and `runAckMatches` (`blueprint_runs_api.go:428-437`, `:553-560`); `TestProfileRunCancelConcurrentKeyDifferentRuns` asserts one 202 + one 409. |

## Residual HIGH (new path found in this pass)

### H8. H7 guard misses an unfenced paid replacement left by a cancelled run

`blueprint_engine.go:922-940`, `control/blueprint_runs.go:211-225`,
`control/blueprint_runs.go:338-355`

The H7 guard has two clauses, but both are blind to the most likely cancel
point in the replacement graph:

1. `ActiveRunsForNode` only counts non-terminal runs; `RequestRunCancel`
   finalizes an in-flight run as `cancelled` at the next checkpoint boundary.
2. `UnresolvedFencedResources` only counts resources with
   `replaced_by_resource_id IS NOT NULL` — i.e. the old resource *after*
   `resource.fence`.

The replacement graph pays for `create_new` **long before** `fence`
(`ready_new` → `distinct` → `install_new` → `admit_new` → `fence`; install
alone has a 1200 s timeout). If the user cancels during that window:

- the worker stops at the next boundary (`blueprint_engine.go:489-492`), so
  `resource.fence`/`switch`/`delete` never run;
- the newly created VPS is `ready` (usually installed and node-bound) and
  carries `replaces_resource_id = <old>`, but the *old* resource still has
  `replaced_by_resource_id = NULL`;
- the run is terminal (`cancelled`).

A later ban episode then passes both guards (`active == 0`, `fenced == 0`) and
buys another VPS. Result: one paid orphan per cancelled replacement plus a new
purchase — the “chain of paid VPS on repeated bans” that 04 §3 / 05 §6 forbid,
and the exact variant the prior follow-up’s minimum fix named
(“…or any `replaces_resource_id` chain has an unresolved state”).

Minimal fix: extend the resource guard (same predicate in both call sites) to
also count a non-current replacement that is not deleted, e.g.
`replaces_resource_id IS NOT NULL AND id <> <policy current_resource_id> AND state <> 'deleted'`
(or persist an explicit unresolved-purchase marker and clear it only when the
resource becomes the policy current resource or is deleted). Add a regression
test: replacement run cancelled after `create_new` but before `fence`, then a
new ban episode → no second run / `createCalls()` unchanged.

## Other notes (non-blocking)

- **LOW (test):** no test asserts `retry_safe` returns `409 unsafe_resume` for
  `server.create`/`install.text`; the allowlist is correct in code.
- **LOW (behaviour):** any pre-I/O `server.create` validation error now parks
  the run as `unknown`/`needs_attention` because the loop pre-marks the
  checkpoint `dispatched` before `execNode`; safe (no replay) but the run needs
  manual acknowledgement and cannot be failed through resume. Not a money path.

## Verdict

**BLOCK.** All requested items (H1 residual, read-only reconcile, `retry_safe`
allowlist, H7 fenced-teardown guard, H4/H5/H6/L2 extras) are confirmed and the
suite is green. However H8 is a still-reachable money path in the same H7
family: a replacement cancelled between the paid create and `resource.fence`
leaves an unresolved paid resource that the guard does not see, allowing a
subsequent ban to buy another VPS. Re-review after the guard covers non-current
unfenced replacements and has a regression test.
