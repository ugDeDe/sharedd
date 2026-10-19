# Quick blueprint-engine safety audit

Scope: latest H8 follow-up in `review-stage-08b-i-engine.md:506-601`, implementation notes, and the current control/engine/run API/cloud paths. Source inspection only, except mandatory static-analysis commands below. No application edits, tests, app builds, provider calls, or commits.

## Review Summary

| Severity | Count | Status |
|----------|-------|--------|
| CRITICAL | 0 | none identified in bounded scope |
| HIGH | 3 | block auto-run exposure |
| MEDIUM | 1 | command-integrity defect |

**Verdict: BLOCK** next UI auto-run exposure. **Verification: partially_verified** (source evidence; no runtime reproduction).

## Prioritized findings

### 1. HIGH — unsafe automation gate: heartbeat/registration admits deletion without independent health proof

**Locations:** `registry/internal/server/blueprint_engine.go:1181-1198,1282-1331,1347-1368`; `registry/internal/server/cabinet_cloud.go:926-945`.

Default `pool.admit` accepts a candidate when either its heartbeat **or registration** is within TTL and there is no blocking termination. It does not require independently verified Globalping, metrics, or TCP, nor compare the candidate IP with the resource IP. A newly registered candidate can therefore pass without even a fresh heartbeat. Fence/switch do not revalidate health, and deletion checks resource fencing/current identity, not independent replacement readiness, before calling the provider.

The replacement graph explicitly connects `admit_new → fence → switch → delete_old` (`registry/internal/blueprint/testdata/nodehost_replacement.json:18-22,36-40`). Thus this is not merely incomplete admission reporting: it can authorize destruction of the old VPS before the replacement is independently proven usable.

**Required gate:** keep automatic replacement/delete-old unavailable until admission has fresh, resource/IP-bound independent GP + metrics/TCP evidence. Fail closed when evidence is absent; a heartbeat is not the substitute.

### 2. HIGH — H8 predicate is shared, but its node filter excludes unclaimed paid replacements

**Locations:** `registry/internal/control/blueprint_runs.go:211-236,643-653`; `registry/internal/server/blueprint_engine.go:922-945,1133-1145`.

Both delivery and pre-dispatch genuinely use `unresolvedReplacementWhere`. However, its outer condition is `user_id = ? AND node_id = ?`. `createServer` creates a replacement with `ReplacesResourceID` but **no NodeID**. The row stays unbound until installation confirmation. `CreateCloudResource` persists that empty value (`registry/internal/control/cloud_resources.go:80-91`).

Reachable sequence: replacement create completes → cancel before install confirmation → run becomes terminal → another ban episode. The paid row has `replaces_resource_id != NULL`, `state != deleted`, and `node_id = ''`; querying with the policy's nonempty node ID excludes it in **both** guards. No active run remains, so delivery and another purchase can proceed. A replacement registered under a different node ID is likewise outside the exact-match predicate.

**Required fix:** scope unresolved obligations through the replaced resource/policy or an explicit persisted policy identity, including unbound successors. Do not broaden the predicate to all unclaimed resources globally. Retain the shared predicate at both call sites.

### 3. HIGH — initial entry is a separate purchase path without the unresolved-resource guard

**Locations:** `registry/internal/server/blueprint_runs_api.go:209-220,249-305,310-318`; `registry/internal/server/blueprint_engine.go:650-653,922-949`; `registry/internal/control/control.go:355-359`.

An initial request may specify an owned/unregistered node ID but must omit an existing resource. Policy revision remains zero; where no management policy exists, policy ID is also zero. The active-policy unique index deliberately excludes that case. Initial-purpose creates skip both H7/H8 resource checks and record no replacement linkage.

Consequently two different request keys for the same intended node can create separate paid initial runs when no policy exists, or a new-key initial request after cancellation can buy again while the first VPS still exists. The same-key transaction fix is sound, but it is **not** a node/lifecycle-level guard. Generic fresh provisioning with no node identity may intentionally allow multiple purchases; this finding concerns treating initial entry as a safe retry/recovery for the same intended node.

**Required gate:** do not expose initial as automatic retry/restart of an unresolved deployment. Reject/reconcile an existing deployment obligation for a supplied node identity (including terminal runs with live paid resources); distinguish an explicitly confirmed new purchase from recovery.

### 4. MEDIUM — cancel/resume mutate before winning their idempotency gate

**Locations:** `registry/internal/server/blueprint_runs_api.go:394-437,527-560`; `registry/internal/control/blueprint_runs.go:320-358`.

The added `created`/run-identity check only corrects the response. Concurrent requests using the same cancel key on different runs can both pass lookup, both execute `RequestRunCancel`, then one loses `CreateOperation` and returns 409 **after cancelling its run**. Resume similarly changes the run (and for `retry_safe`, the checkpoint) before inserting the operation; the scheduler can claim the changed run even if the handler later returns conflict. Checkpoint reset additionally precedes revision CAS and ignores its write error.

**Required fix:** transactionally reserve/validate the operation key and request identity together with revision/state/checkpoint changes; a losing or stale request must have zero side effects. This is not evidence of paid-create replay, but cancel/resume are not fully idempotent commands as implemented.

## Confirmed protections / limits of the conclusion

- Same-key initial create: operation UNIQUE gate and run insertion share one transaction (`control/blueprint_runs.go:136-185`); concurrent losers do not insert a run. No residual same-key duplicate-create path identified here.
- Dispatched/unknown create: engine reconciles persisted intent rather than re-entering create (`blueprint_engine.go:508-529,570-577,739-767`). Resume reconcile requires dispatched/unknown/claimed; retry_safe rejects non-read/non-safe-read blocks (`blueprint_runs_api.go:492-533`). No paid-create replay through these decisions identified in this pass.
- Cancel flag is durable and remains checked by worker/claim paths; resume does not clear it (`control/blueprint_runs.go:301-306,320-358`; `blueprint_engine.go:462-464,489-491`). A cancelled run is therefore not restored to paid execution just by resume, although the API can misleadingly change its visible state.
- Documentation's H8 closure covers node-bound orphans, not all paid orphans. `implementation-stage-08b-i-engine.md:120-121` also says observe never purchases, contradicting the implemented observe replacement path; do not use that sentence as a safety guarantee.

## Verification and handoff

- `registry/`: `rtk go vet ./...` — passed, no diagnostics.
- `registry/`: `staticcheck ./...` — unavailable (`staticcheck: command not found`); not installed for this audit.
- No tests, race suites, app builds, or repeated verification suites run. Prior green-suite claims in the source documents were not revalidated.
- Fix ownership: **@go-developer** for the three HIGH gates and atomic cancel/resume commands. This review made no application changes. Hold automatic purchase/replacement/delete controls; read-only UI work can continue independently.
