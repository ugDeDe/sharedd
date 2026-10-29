# Blueprint safety patch follow-up

Continues the interrupted patch in `quick-safety-audit.md`; retains its existing
control-store gates, engine readiness helper, atomic command handlers and safety
tests. No UI, dependency, provider protocol, schema, commit or deployment changes.

## Decisions and concrete ownership

### 1. Admission and destructive boundaries fail closed

- `registry/internal/server/blueprint_engine.go`: `blueprintResourceReady` reuses
  production selection's `Candidate.IsFullyHealthy`, then requires the owned
  resource to be `ready`, its persisted node ID and IP to match the candidate,
  fresh positive independent GP verification (measurement ID plus timestamp),
  fresh metrics/report evidence, a fresh successful registry TCP sample, an
  explicitly compatible shared port, no quarantine and no blocking termination.
  Missing, stale, pre-registration or future-dated evidence fails closed.
  Registration/heartbeat alone never suffices. `bpAdmit` can veto, not bypass.
- Admission is repeated at fence, switch and immediately before automated
  old-resource deletion. Deletion additionally requires a distinct fenced
  successor that is now the policy's current resource; a run snapshot or an
  earlier successful admission is not authorization to destroy the old VPS.
- `registry/internal/server/run_main_impl.go`: IP changes invalidate the old
  endpoint's GP, metrics and TCP readiness. In-flight TCP results are rejected
  when their port, IP or registration generation changed.
- `registry/internal/server/healthcheck.go`: a port change invalidates old GP/TCP
  proof; only independent verification and a new TCP sample can restore it.
  These are stricter evidence bindings, not relaxed selection/admission gates.

### 2. Unresolved purchases follow resource ancestry, not successor node IDs

- `registry/internal/control/blueprint_runs.go`: the shared
  `unresolvedReplacementWhere` walks both directions of owner-scoped
  `replaces_resource_id`/`replaced_by_resource_id` relationships, rooted in the
  policy node/current resource. Deleted ancestors remain traversable, but only
  nondeleted obligations count. Empty or different successor node IDs cannot
  hide an unresolved paid replacement; unrelated unclaimed resources and other
  owners are not globally blocked.
- Both `CreateRunForBan` delivery and `createServer` pre-purchase dispatch retain
  the same predicate. A cancelled/terminal run does not erase its resources.

### 3. A named initial deployment is not a new-key purchase retry

- The retained `CreateRunIdempotent` gate atomically rejects another initial run
  for `(user_id, supplied node_id)` while a prior run is active or still has a
  nondeleted resource, including resources found through legacy `bpcreate`
  operation metadata after their successor node ID changes.
- The retained pre-dispatch check and `CreateNamedDeploymentResource` reserve a
  named planned obligation under the same SQLite write transaction as the
  final deployment/cancellation check. Cancellation between checking and paid
  dispatch cannot erase the planned obligation and permit a second purchase.
- This returns HTTP 409 `deployment_unresolved`, not an implicit new purchase
  confirmation. Explicit unnamed fresh provisioning remains independent.
  Nondeleted planned/error/unknown resources conservatively require resolution.

**Safety note — the explicit resolution path.** A row with `external_id == ""`
that never got a provider instance is the obligation itself. `DELETE
/api/me/cloud-resources/{id}` may remove it locally only while no worker is in
flight (per-resource guard and last operation id are both checked); `creating`
with a live worker is refused with 409 `resource_busy`, never silently dropped.
A row with `external_id != ""` always takes the provider-delete path. This does
not weaken the unresolved-purchase gate: an in-flight or provider-backed
purchase cannot be erased by a local delete.

### 4. Cancel/resume commands commit atomically

- The retained `ApplyRunCommand` and handlers in
  `registry/internal/server/blueprint_runs_api.go` reserve/validate the operation
  key, owner, command kind, run identity and body hash in the same transaction
  as the revision/cancellation CAS and optional checkpoint reset. A losing or
  stale request rolls back its operation and run/checkpoint changes; workers
  cannot claim an uncommitted resume.
- The checkpoint CAS now compares the complete validated checkpoint snapshot,
  not only state/lease: even a changed output at the same lease causes rollback.
  Checkpoint write errors are propagated and roll back the run and key too.
- Exact replays acknowledge without revision bumps, checkpoint resets or worker
  launch. Changed bodies/different runs return 409 `idempotency_conflict`; stale
  revisions/checkpoints return 409 `revision_conflict`. Cancelled/terminal runs
  are not resurrected. Existing CSRF, ownership and safe-read retry gates remain.

## Correction to the earlier implementation note

This section supersedes `implementation-stage-08b-i-engine.md:120-121` and any
description of observe as categorically purchase-free: **observe can purchase a
replacement** when the active pinned graph reaches
`server.create` with `purpose: replacement` after an eligible ban episode.
The small test graph whose observe branch only re-arms is not the replacement
template and is not a general safety guarantee. Safety comes from the policy,
unresolved-obligation and readiness gates, not the entry name.

## Files continued or changed

- `registry/internal/control/blueprint_runs.go`
- `registry/internal/control/blueprint_safety_test.go`
- `registry/internal/server/blueprint_engine.go`
- `registry/internal/server/blueprint_runs_api.go` (retained partial handler fix;
  inspected, no additional handler change)
- `registry/internal/server/blueprint_safety_test.go`
- `registry/internal/server/blueprint_engine_test.go` (existing delete-retry
  fixture now supplies a healthy, fenced, switched replacement)
- `registry/internal/server/run_main_impl.go`
- `registry/internal/server/healthcheck.go`
- `registry/internal/server/main_test.go` (existing metrics-only preservation
  fixture explicitly identifies the unchanged port of its prior GP proof)
- `docs/specs/hosting-blueprints/safety-fixes-followup.md`

## Bounded verification

New safety test names, run once per owning package (no repetition flag):

```sh
# Working directory: registry/
go test -race -vet=off -run '^(TestSafetyReplacementAncestry|TestSafetyNamedDeployment|TestSafetyRunCommandAtomic|TestSafetyRunCommandCheckpointRollback)$' ./internal/control
go test -race -vet=off -run '^(TestSafetyBoundReadinessAtEveryBoundary|TestSafetyInitialNamedHTTPAndDispatch|TestSafetyReplacementDispatchUsesResourceAncestry|TestSafetyRunCommandHTTPConflicts)$' ./internal/server
go build -mod=readonly ./internal/server ./internal/control
```

**Verification: verified within the requested bounded scope.** Both exact-name
test selections passed with the race detector, each invoked once through `rtk`;
the single `go build -mod=readonly ./internal/server ./internal/control` also
passed. Changed Go files were formatted with `gofmt`; `git diff --check` passed.
No full suites, repetition flags, vet sweeps, live external provider calls or UI
regeneration were run. The existing fixture-only tests listed above were
compiled as part of their package, not executed by these safety-only selections.

## Remaining limitations

- Health is a bounded-freshness snapshot of the production registry's trusted
  signals, not a guarantee that a replacement cannot fail after the final check.
  SQLite, JSON health state and external provider deletion cannot be one atomic
  transaction. There is no claimed transactional external outbox here.
- Stale/legacy evidence fails closed until new reports/probes arrive. A blocked
  mutation can remain `needs_attention`; no automatic purchase replay or cleanup
  is added. Planned/error/ambiguous resources may need operator reconciliation.
- The named guard is not global purchase deduplication: unnamed fresh purchases
  remain intentional, and legacy resources with no run, node or operation
  linkage cannot be attributed automatically to a named deployment.
- Actual provider billing/deletion, SSH installation, unrelated existing tests
  and the rest of registry/node are not verified by the bounded commands above.
