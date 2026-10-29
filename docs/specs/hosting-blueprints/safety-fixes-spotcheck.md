# Safety fixes: bounded source spot-check

Compared `quick-safety-audit.md` and `safety-fixes-followup.md` with the current
working-tree implementation. Scope: the four reported fixes in
`registry/internal/control/blueprint_runs.go` and
`registry/internal/server/{blueprint_engine,healthcheck,blueprint_runs_api}.go`;
direct readiness/storage/provider helpers were read only to establish their
actual contracts. This is not a repository audit or runtime verification.
The blueprint implementation files are currently untracked, so their actual
contents, not merely tracked `git diff` output, were inspected.

## Review Summary

| Severity | Count | Status |
|----------|-------|--------|
| CRITICAL | 0 | none identified in bounded scope |
| HIGH | 1 | block fresh-proof admission/delete claim |
| MEDIUM | 0 | not expanded into style or general-quality review |

**Verdict: BLOCK. Verification: partially_verified.** Three bounded fixes pass;
the admission fix still permits historical GP evidence to acquire a fresh
verification timestamp. No application changes or delegation were made.

## 1. HIGH — fetching an old GP measurement makes its proof appear fresh

**Locations:** `registry/internal/server/blueprint_engine.go:1208-1217`,
`registry/internal/server/healthcheck.go:118-135,188-194`,
`registry/internal/globalping/client.go:42-49,137-164`.

The new readiness helper checks the age of `candidateLastGlobalpingAt(c)`.
However, after successfully fetching and independently validating a measurement,
`handleHealthReport` sets `LastGlobalpingAt = time.Now()`. The measurement model
does not retain a measurement creation/execution timestamp, and `ValidateBinding`
checks endpoint identity and results, not measurement age or registration epoch.
Thus the checked timestamp dates verification of the stored result, not the
underlying independent probe.

Concrete path: an owned candidate submits a still-retrievable historical
successful measurement for its current IP/port/SNI, with a fresh report
`checked_at`. An ID not in that candidate's replay history passes
`reportReplayed` (`healthcheck.go:455-472`). Fetch/binding validation succeeds,
`GlobalpingOK` becomes true, and `LastGlobalpingAt` becomes current. With fresh
metrics and registry TCP samples, `blueprintResourceReady` accepts even if the
measurement itself predates registration or the GP validity window. This matters
when current regional GP reachability has failed but registry TCP and local
metrics still succeed. Rechecking the same candidate at fence/switch/delete
does not detect the historical proof.

**Required correction:** bind GP readiness to a trusted measurement execution or
creation time and the relevant endpoint/registration epoch; reject missing,
future, pre-epoch or expired measurement times. If the provider cannot supply
trusted freshness, retain a fail-closed gate rather than treating fetch time or
agent-supplied `checked_at` as independent probe time. Backend remains frozen in
this review; no fix was applied.

**Confirmed portion:** missing/unhealthy/unbound evidence now fails closed;
`bpAdmit` cannot bypass the helper (`blueprint_engine.go:1182-1193`). Resource
state, node/IP identity, quarantine/termination, explicit port compatibility,
metrics freshness and successful fresh TCP are checked (`1200-1224`). Fence and
switch recheck readiness (`1333-1339,1362-1368`); deletion requires a distinct
fenced successor that is policy-current and rechecks readiness before
`cloudDeleteDo` (`1390-1422`). Port changes invalidate GP/TCP evidence
(`healthcheck.go:162-168`); IP changes and stale in-flight TCP results are guarded
(`run_main_impl.go:659-664,808-810`). Those protections do not date the GP
measurement itself, so the follow-up's fresh/pre-registration-proof claim is
not fully supported.

## 2. PASS — unresolved replacements follow owner-scoped ancestry

`unresolvedReplacementWhere` roots the recursive family in the policy node or
current resource, traverses both link directions with owner constraints, and
filters deleted obligations only after traversal
(`control/blueprint_runs.go:306-327`). Empty or different successor node IDs
therefore do not remove a linked nondeleted obligation, and deleted ancestors
remain usable links. Unrelated unclaimed resources and other owners are not
globally included. Both ban delivery (`851-861`) and pre-purchase dispatch
(`server/blueprint_engine.go:928-945`, via `UnresolvedFencedResources`) use this
same predicate. Cancellation does not remove the resource obligation.

## 3. PASS — named initial cancel/new-key cannot open a second purchase

The initial-run gate is inside the operation/run transaction
(`control/blueprint_runs.go:221-277,249-257`). Its predicate includes active runs
and nondeleted resources linked by current resource, named node, or legacy
`bpcreate` metadata (`24-31`), so a different request key does not bypass a live
deployment obligation; the API maps it to 409 `deployment_unresolved`
(`server/blueprint_runs_api.go:309-324`).

Before dispatch, the engine checks the named obligation and uses
`CreateNamedDeploymentResource` (`server/blueprint_engine.go:923-926,947-977`).
That helper takes SQLite's write lock, rechecks competing obligations and the
persisted run's cancellation/terminal state, then inserts a named planned
resource before committing (`control/blueprint_runs.go:50-99`). If cancellation
wins, reservation fails; if reservation wins, the nondeleted planned resource
blocks a subsequent new-key run even if cancellation precedes the paid call.
Unnamed fresh provisioning remains deliberately outside this guarantee, as the
follow-up states. Unattributable legacy resources are also outside its claim.

## 4. PASS — cancel/resume 409 paths have no command mutations

Both handlers delegate mutation to `ApplyRunCommand`
(`server/blueprint_runs_api.go:411-430,522-540`). The transaction reserves the
operation key before updating the run; a conflicting owner/kind/run/body fails
before run mutation (`control/blueprint_runs.go:478-512`). A stale run revision,
cancelled/terminal resume, checkpoint mismatch, or checkpoint-write failure
returns before commit, rolling back the operation, run and checkpoint together
(`517-574`). Checkpoint comparison includes the complete validated struct, not
just state/lease (`539-547`).

Exact replays do not reset checkpoints or bump revisions. Resume launches a
worker only after a successfully committed, newly created command
(`server/blueprint_runs_api.go:537-539`); early `unsafe_resume`/idempotency 409
paths are read-only. This is zero mutation attributable to the rejected command,
not a promise that independent workers cannot change state concurrently.

## Verification limits

- After explicit clarification, the user selected **Review with checks**,
  permitting the reviewer's mandatory vet/staticcheck checks only.
- `registry/`: `rtk go vet ./...` completed without diagnostics.
- `registry/`: `staticcheck ./...` was attempted but unavailable
  (`staticcheck: command not found`); nothing was installed.
- No tests, race runs, app builds, live provider calls, UI changes, memory
  operations or commits. Prior test/build claims in the follow-up were not
  revalidated. Only this report was written.
