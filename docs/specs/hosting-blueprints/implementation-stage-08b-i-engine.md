# Stage 08b-i — bounded durable run engine (Nodehost)

**Status: partially_verified.** The bounded Nodehost execution path below is
implemented and tested; full admission parity, Beget `ip.*`, the graph editor
and graph publish admin remain out of scope (see NOT-DONE). No live provider
calls were made.

## 1. Architecture

```
POST /api/me/nodes/{node}/management (mode=profile)
        │  validate pinned graph + inputs + provider, CAS, set watermark
        ▼
node_management(mode=profile, profile_version_id, inputs_json, activated_at)
        │
        ├── reconcile ticker (startup + every 5s)
        │      ban_episodes ⋈ node_management mode=profile
        │      NOT EXISTS ban_deliveries(policy_node_id, episode_id)
        │      → CreateRunForBan (transaction: delivery + run)
        │      → launch run worker
        ▼
profile_runs  ──► run worker (provisionWorker-style guard keyed by run id)
        │             ClaimRun CAS(revision, lease_generation)
        │             load pinned graph, walk entry → control edges
        │             checkpoint dispatched BEFORE I/O
        │             existing dispatched purchase → reconcile, never replay
        ▼
block_checkpoints (UNIQUE(run,node,invocation), lease_generation, output_json)
```

Execution is intentionally not a single transaction across JSON state/control
SQLite/provider. The journal is the checkpoint table; the paid-call boundary is
"persist `dispatched` (with resource/operation intent) before the provider
call". A resumed purchase is resolved by reading the persisted resource, not by
re-calling the provider.

Seams used by tests: `cloudFactory` (existing), `sshExec` (existing),
`bpAdmit` (admission predicate), `bpNowFn` / `bpPollInterval` / `bpInterval`.

## 2. Tables (additive)

`profile_runs`
: `id, user_id, profile_version_id, profile_id, node_id, entry(initial|observe),
state, revision, inputs_json, vars_json, current_resource_id, policy_id,
policy_revision, next_wake_at, cancel_requested, last_error, lease_generation,
created_at, updated_at`.

`block_checkpoints`
: `id, run_id, node_id, invocation, state(ready|claimed|dispatched|confirmed|
rejected|unknown|waiting), attempt, output_json, error, lease_generation,
next_attempt_at, created_at, updated_at`, `UNIQUE(run_id,node_id,invocation)`.

`ban_episodes`
: `id, node_id, ip, episode_id UNIQUE, created_at` — the durable verdict
source written in the same registry path as the `ip_ban` state/history write.

`ban_deliveries`
: `id, policy_node_id, episode_id, run_id, state, created_at`,
`UNIQUE(policy_node_id,episode_id)`.

`node_management.activated_at` (ensureColumn): enable-time watermark. With
`process_current_ban=false` only episodes with `created_at > activated_at` are
delivered; with `true` the server sets `activated_at` to the latest episode's
timestamp so exactly that episode is admitted. Historical bans are therefore
never processed by default, including after a restart.

## 3. Routes

| Method / path | Notes |
|---|---|
| `POST /api/me/profile-runs` | session + CSRF + `Idempotency-Key`; body `{profile_version_id,entry,resource_id?,node_id?,inputs,vars?,confirm:true}` → 202 `{operation_id,run_id,state,resource_id}`. `confirm` missing → 400. Same key/body replays the same run; different body → 409 `idempotency_conflict`. |
| `GET /api/me/profile-runs/{id}` | owner-only (foreign → 404). Includes checkpoints and `waiting` info. |
| `POST /api/me/profile-runs/{id}/cancel` | `{expected_revision}` + key → 202. Durable `cancel_requested`; pending/waiting become `cancelled` immediately, in-flight runs stop at the next checkpoint boundary. |
| `POST /api/me/profile-runs/{id}/resume` | `{expected_revision,checkpoint_id,decision}` + key. `reconcile` → reconnoitre read-only; `retry_safe` only for `read`/`safe-read` blocks (otherwise 409 `unsafe_resume`); `acknowledge` keeps `needs_attention`. |

`POST /api/me/nodes/{node_id}/management` with `mode:"profile"` now validates:
numeric pinned version exists, `format=graph`, graph validates, owner-or-published
profile, declared inputs match (`ValidateInputs`), inputs `connection_id` is
owner-checked, and the graph provider matches the node's provider (current
resource or connection). CAS by `expected_revision`.

## 4. Block implementation matrix

| Block | Status | Behaviour / deviation |
|---|---|---|
| `server.create` | **implemented** (nodehost) | Creates `cloud_resources2` row + `operations` row, persists intent, calls the existing `cloudCreateDo`. Beget graph create → explicit `not_implemented`. `configuration_ref` is interpreted as the inline configuration object in run inputs (`configuration`); there is no separate stored-configuration snapshot in this stage. |
| `server.ready` | **implemented** | Polls the persisted resource state up to `timeout_seconds`; timeout → `waiting_retry`. |
| `install.text` | **implemented** (nodehost) | Pinned text `profile_version` by numeric PK (`installation_profile_version_id`), reuses `AuthorizeCloudInstall` + `provision.InstallProfile` with `r.sshExec`; intent persisted before SSH. Confirm uses `run.node_id` when set, else the registry registration. |
| `pool.admit` | **MVP deviation** | Default predicate = resource has `node_id`, candidate is registered with a fresh heartbeat (`heartbeat_ttl`), and has no active terminate. It does **not** perform the full independent GP/metrics/TCP measurement. Tests inject the predicate. |
| `event.ip_ban` | **implemented** | Reads the durable `episode_id`/`banned_ip` injected by delivery; no episode → `waiting_event` (no goroutine/connection held). |
| `policy.guard` | **implemented** | Re-reads `node_management`: mode, pinned version, policy revision, owner-enabled; `active` or `stop`. |
| `server.distinct_ip` | **implemented** | Rejects a new resource whose IP equals the banned episode IP. |
| `resource.fence` | **implemented (MVP)** | Sets `cloud_resources2.replaced_by_resource_id` on the old resource. No separate `retirement_fences` table. |
| `resource.switch` | **implemented (MVP)** | Updates `node_management.current_resource_id` and the run's current resource. Runtime fence proof is static (validator dominance), not re-read here. |
| `server.delete` | **implemented** (nodehost) | Reuses `cloudDeleteDo`; refuses to delete the current policy resource unless it is already fenced. |
| `server.absent` | **implemented** | Polls the persisted resource until `deleted`. |
| `observe.arm` | **implemented** | Sets the policy current resource and ends the run `succeeded`. Delivery is already consumed; the next ban episode creates the successor run (no recursive launch). |
| `ip.order`, `ip.attach`, `ip.switch_advertised`, `ip.cleanup`, `ip.verify` | **not_implemented** | Explicit error `not_implemented: <block> is Beget-only ...`; the run fails honestly, never fakes success. |
| `terminal.stop` | **implemented** | Ends the run `cancelled` with no provider effect. |

## 5. Ban delivery semantics and crash windows

- Episodes are written by the registry where the `ip_ban` verdict is written
  (`terminateNodeLocked`, `quarantineIPChangeLocked`). Episode id is stable
  random `ep_…`; repeated ticks do not create a new episode.
- JSON state and control SQLite are not atomic. The reconciler scans at startup
  and every tick; a crash between the two writes is recovered by the next scan.
- Delivery insert and run creation are one SQLite transaction with
  `UNIQUE(policy_node_id,episode_id)` — duplicate scans are no-ops.
- A crash after delivery insert but before worker launch is recovered because
  the run is left `pending` and is claimable on the next tick.
- `waiting_event` runs are not claimed by the periodic query (their successor
  is a new episode/run); they are resumed once at startup so a pending event
  checkpoint is re-evaluated after a restart.
- A crash after `dispatched` but before the provider call resolves by reading
  the persisted resource; since no provider operation id is persisted yet, a
  resource still `planned/creating` becomes `needs_attention` rather than being
  replayed. SSH/mutation unknown outcomes likewise become `needs_attention`. A
  paid action is never replayed.
- Observe runs never purchase in this stage; only `initial` may call
  `server.create`.

## 6. Cancel semantics

`cancel_requested` is durable. Cancel before dispatch stops the run with zero
provider calls. Cancel during an in-flight checkpoint is observed at the next
block boundary: the completed effect remains recorded, no refund/slot release
is implied, and the run becomes `cancelled` without launching further blocks.

## 7. Test matrix / results

| Area | Test |
|---|---|
| Store CAS/lease/unique | `TestProfileRunClaimAndCheckpointLease` |
| Ban dedupe + delivery | `TestBanDeliveryDedupeAndProfileWatermark` |
| Historical watermark | `TestUndeliveredBanEpisodesSkipHistoricalByDefault` |
| Nodehost end-to-end (fake cloud + fake SSH + injected admit) | `TestBlueprintEngineInitialEndToEnd` |
| No replay of a dispatched paid create | `TestBlueprintEngineCreateReconcileDoesNotReplay` |
| waiting_event durability, restart, delivery dedupe, observe no purchase | `TestBlueprintEngineWaitingEventAndBanDelivery` |
| Cancel before dispatch; manual mode zero mutations | `TestBlueprintEngineCancelAndManualMode` |
| Profile enable validation + historical ban + current-ban option | `TestNodeManagementProfileEnableValidationAndHistoricalBan` |
| Runs routes: CSRF, ownership 404, idempotency replay/conflict, revision conflict | `TestProfileRunRoutesOwnershipCSRFAndIdempotency` |

Commands from `registry/`:

```
go vet ./...                                                  # clean
go test -race ./internal/control ./internal/blueprint ./internal/server -count=1
                                                              # ok
go test ./... -count=1                                        # ok
go build -mod=readonly ./...                                  # ok
gofmt -l .                                                    # empty
git diff --check                                              # clean
```

## 8. NOT DONE

- Full `pool.admit` parity (independent Globalping/metrics/TCP measurement) —
  MVP predicate only.
- Beget IP blocks (`ip.*`) and Beget managed create/install/delete.
- Stored `configuration_ref` snapshot resolution (MVP accepts inline config).
- `retirement_fences` / `replacement_reservations` tables and full quota-pair
  semantics for replacement purchases.
- Threading a successor `observe` subscription inside the run: the next ban
  episode creates a new run through the reconciler (equivalent durable
  behaviour, no in-run loop).
- UI graph editor and admin graph publish.
- Multi-instance worker coordination beyond SQLite CAS (single registry process
  assumed, as in earlier stages).

## 9. Review stage-08b-i fixes (blockers)

The review file `review-stage-08b-i-engine.md` is read-only; this section records
what changed in code.

- **C1:** `control.CreateRunIdempotent` inserts the `operations` row first
  (UNIQUE `idem_key` gate) and the `profile_runs` row in the same transaction;
  the loser gets the existing operation and never creates a run or starts a
  worker. The run stores `operation_id` (L1).
- **H1:** `applyOutcome` never downgrades a dispatched/unknown mutation to
  `waiting`; the checkpoint becomes `unknown` and the run goes
  `needs_attention`. A cancelled create worker maps to `errBlueprintUnknown`.
  Follow-up: the `blockErr` path also preserves a dispatched/unknown non-read
  checkpoint as `unknown` with its intent/output (never `rejected`, which the
  resume path would re-dispatch), and `resume decision=reconcile` accepts only
  `dispatched|unknown|claimed`. The reconcile path is read-only: it consults
  `nodehost Operation`/`Server` using the persisted provider operation id, and
  otherwise parks `needs_attention`; `Create`/`Delete`/`ChangeIP` are never
  re-called. `retry_safe` remains restricted to read-only allowlisted blocks.
- **H2:** before every non-read effect dispatch the engine re-reads
  `node_management` (mode/version/revision) and the owner; a mismatch rejects
  the checkpoint and cancels the run. Initial runs without a policy remain
  exempt (policy is created later).
- **H3:** delete dispatches use a per-attempt key
  `bpdelete:<run>:<node>:<attempt>`; a definitive provider failure
  (`resource=error`) returns a retryable waiting outcome. An unknown delete
  (`deleting`) becomes `needs_attention` instead of looping.
- **H4:** `DeleteProfile` returns `ErrProfileInUse` when a non-terminal run or a
  `node_management` policy references a version; both user and admin handlers
  map it to `409 profile_in_use`.
- **H5:** `provisionWorker.stop()` now cancels and waits for in-flight workers;
  `Run()` calls `blueprintWorkers().stop()` next to the provision workers,
  before closing stores.
- **H6:** the run goroutine has panic recovery; a panic marks the run
  `needs_attention` with `last_error=worker_panic`.
- **H7:** `policy_id` is populated (node_management rowid) for delivered runs,
  a partial UNIQUE index enforces one non-terminal run per policy, and
  `CreateRunForBan`/replacement create refuse a second unresolved run.
  Follow-up: the guard also queries fenced resources
  (`replaced_by_resource_id IS NOT NULL AND state <> 'deleted'`) per user/node,
  both in the ban-delivery transaction and immediately before a replacement
  create. A cancelled/terminal run therefore cannot erase the teardown
  obligation: no new purchase is allowed until the fenced VPS is confirmed
  deleted.
- **H8:** the shared resource guard (`unresolvedReplacementWhere`) also counts
  an unfenced paid replacement: `replaces_resource_id IS NOT NULL AND
  id <> current_resource_id AND state <> 'deleted'`, independent of
  `replaced_by_resource_id`. A replacement cancelled between `create_new` and
  `resource.fence` (ready, node-bound, no active run) therefore blocks the next
  ban delivery and the pre-create check with the same predicate; confirmed
  deletion releases it.
- **M1:** the blueprint tick finalizes `running/reconciling + cancel_requested`
  rows with no live worker as `cancelled`.
- **M2:** retries are capped at `blueprintMaxAttempts` (12) with exponential
  backoff up to 10 minutes; exceeding the cap sets `needs_attention`.
- **M5:** the stable episode id is stored in `TerminatedRecord.EpisodeID`; the
  blueprint tick replays a failed `AddBanEpisode` (at-least-once delivery).
- **L1:** run DTO `operation_id` is filled from `profile_runs.operation_id`.
- **L2:** cancel/resume replay compares the persisted `run_id` and rejects a key
  reused on a different run with `409 idempotency_conflict`.
- **L4:** a failed create-operation insert deletes the orphan `planned` row.
- **H5 residual:** `Run()` now joins the `blueprintLoop` goroutine (after
  `stop()` cancels it) before stopping workers and closing stores; no scheduler
  tick can touch a closing store.
- **H6 residual:** panic logs record only the panic type, never the payload.
- **H4 residual:** HTTP-level tests assert `409 profile_in_use` from both the
  user and admin delete routes; the control test also covers the
  non-terminal-run branch.
- **L2 residual:** cancel/resume check `created` after the operation insert; a
  concurrent losing request for a different run returns
  `409 idempotency_conflict` instead of acknowledging the wrong run.

### Deviations (documented, not fixed)

- **M3:** initial `vars` are now validated against the pinned text version
  before purchase, but the initial vars snapshot is not yet copied into a
  ban-delivered replacement run (`VarsJSON` stays `{}`); replacement installs
  use the pinned defaults.
- **M4:** an `initial` run still accepts an unregistered explicit `node_id`.
  Rejecting it would break the normal provision-then-register flow; the node is
  bound only on install confirmation. Observe runs continue to require an
  active matching policy.
- **L3:** the ban watermark remains second-resolution
  (`activated_at`/`created_at`); the episode-id boundary is not stored.
- `waiting_event` is excluded from the H7 active-run guard and policy unique
  index: a waiting run is an event subscription, not an unresolved purchase,
  and the review confirms a new episode intentionally creates a new run.

### Regression tests added

| Area | Test |
|---|---|
| C1 concurrent identical POST | `TestProfileRunCreateConcurrentIdempotent` |
| C1/L1 operation-gated store insert | `TestCreateRunIdempotentGate` |
| H1 dispatched waiting not downgraded | `TestBlueprintDispatchedWaitingBecomesNeedsAttention` |
| H1 error after dispatch + resume reconcile | `TestBlueprintCreateErrorResumeReconcileNeverReplays` |
| H7 unresolved fenced resource after cancel | `TestBlueprintReplacementBlockedByUnresolvedFencedResource` |
| H8 unfenced paid replacement blocks ban delivery/purchase | `TestBanDeliveryBlockedByUnfencedOrphan`, `TestBlueprintReplacementBlockedByUnfencedOrphan` |
| H4 HTTP 409 user/admin | `TestDeleteProfileInUseHTTP` |
| L2 concurrent same-key cancel | `TestProfileRunCancelConcurrentKeyDifferentRuns` |
| H2 mode flip after guard | `TestBlueprintManualFlipStopsNextEffect` |
| H3 delete retry | `TestBlueprintDeleteRetryDispatchesAgain` |
| H4 profile in use | `TestDeleteProfileInUse` |
| H5 stop waits | `TestProvisionWorkerStopWaits` |
| H6 panic containment | `TestBlueprintWorkerPanicContained` |
| H7 one active run per policy | `TestBanDeliveryOneActiveRunPerPolicy` |
| M1 orphaned cancel | `TestBlueprintOrphanedCancelFinalized` |
| M2 retry cap | `TestBlueprintWaitingAttemptCap` |
| M5 episode persist replay | `TestBanEpisodePersistenceRetry` |

Verification: `go vet ./...`, `go test -race -count=2 -run 'Blueprint|Run|Ban|Engine|Management|Profile' ./internal/server ./internal/control`,
full `go test -race ./...`, `go build -mod=readonly ./...`, `gofmt -l .` empty,
`git diff --check` clean.
