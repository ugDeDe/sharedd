# Review — Stage 05 cloud resources & node management

Scope: read-only review of stage-05 against
`03-cabinet-api.md` §2.1/§4/§5, `04-storage-and-execution.md`, and the
implementation note `implementation-stage-05-cloud-management.md`.

Artifacts reviewed:
`registry/internal/control/cloud_resources.go` + schema in `control.go`,
`registry/internal/control/operations.go` (CreateOperation/OperationByIdemKey/
MarkCreateSent), `registry/internal/server/cabinet_cloud.go`,
`cabinet_cloud_test.go`, `cloud_resources_test.go`,
`provision_worker.go`, wiring in `cabinet_api.go:321` and
`run_main_impl.go:107-114`.

Spot-run (fresh):
`cd registry && go test -race -count=2 -run 'TestCloud|TestManagement|CloudResource' ./internal/server ./internal/control`
→ `ok internal/server 21.485s`, `ok internal/control 3.759s`.

## Review Summary

| Severity | Count | Status |
|----------|-------|--------|
| CRITICAL | 0 | pass |
| HIGH     | 2 | block |
| MEDIUM   | 4 | info |
| LOW      | 3 | info |

Verdict: **BLOCK** — two HIGH findings (H1 silent worker loss, H2 change-ip
idempotency ignored). Both are new, not covered by the documented deviations.

## Confirmed-conformant (no finding)

- Ownership: every `{id}` route goes through `cloudOwned` →
  `CloudResourceOwned(id, user)`; foreign/unknown → 404 `not_found`
  (`cabinet_cloud.go:163-175`, `control/cloud_resources.go:115-122`). Foreign
  list is empty. Management ownership via `NodeOwner`, legacy → 404
  (`cabinet_cloud.go:685-697`).
- CSRF: all five mutating routes are wrapped in `requireCSRF`
  (`cabinet_cloud.go:203-208`); reads are not. Double-submit constant-time
  compare (`cabinet_api.go:110-121`).
- DELETE confirm: `X-Confirm-Action: delete` enforced, missing →
  400 `confirmation_required` (`cabinet_cloud.go:396-399`).
- Secrets: `CloudResourceV2.SecretsEnc` is `json:"-"`; `cloudResourceJSON`
  carries no secret field; root password is AES-GCM `sealJSON`ed
  (`cabinet_cloud.go:598-608`). Provider text never emitted
  (`cloudSafeErrCode` returns codes only). Verified by
  `TestCloudSecretsNeverLeak`.
- change-ip: singleflight guard with `defer` release
  (`cabinet_cloud.go:477-492`), no auto-create fallback, authoritative
  `ChangeIP` result with `Server()` only as fallback (`:508-515`).
- Replace: new row with `replaces_resource_id`, old untouched, no delete
  (`cabinet_cloud.go:357-384`).
- Management: virtual default GET without INSERT (`:701-718`), CAS via
  `expected_revision` (`:751`), `profile` → 422 (`:736-739`),
  `process_current_ban` defaults false and preserved (`:747-751`).
- State machine CAS on every transition
  (`control/cloud_resources.go:147-173`); create `planned→creating` CAS
  prevents double-worker; delete `ready|error→deleting` CAS
  (`cabinet_cloud.go:614-618`). Budgets bounded: create 10m, delete 3m,
  change-ip 60s (`:31-35`, `:500`).
- `cloud_resources2` is additive; legacy `cloud_resources` untouched
  (`control.go:252-273` vs `:164-173`). `operations.kind` has no CHECK and
  `cloud_create`/`cloud_delete` are valid strings (`control.go:135-150`).
- Idempotency for create/replace/delete: `operations.idem_key` UNIQUE +
  `CreateOperation` `ON CONFLICT DO NOTHING` returning `created=false`
  (`operations.go:79-95`); replay returns the same ack with no provider call
  (`cabinet_cloud.go:269-275`, `:424-430`); resource row tied via
  `request_json.resource_id` and re-owned on replay (`:314-326`). Pinned by
  `TestCloudCreateLifecycle`/`TestCloudCreateConcurrentSingleCall`.

---

## HIGH

### H1 — Cloud workers share the operation-id guard namespace → silent stuck resource

`cabinet_cloud.go:529-539` passes the **resource id** into
`provisionWorkers().run(...)`:

```go
r.provisionWorkers().run(resID, ...)   // create  (line 530)
r.provisionWorkers().run(-resID, ...)  // delete  (line 536)
```

`provisionWorkers()` (`run_main_impl.go:150-153`) is a **single shared pool**
also keyed by **operation id** in the beget/manual install paths
(`cabinet_beget.go:460,783,1030`, `cabinet_manual.go:426`). `operations.id` and
`cloud_resources2.id` are independent `INTEGER PRIMARY KEY` sequences, so a
beget/manual install running under operation id `N` and a cloud create on
resource id `N` collide on the same `running` key. `run` returns `false` for
the loser, but `launchCloudCreate`/`launchCloudDelete` **ignore the bool**
(`:530`, `:536`). Result: the cloud worker never runs, no state transition
happens, and the resource stays `planned` with its operation `pending`
forever — while the client already received `202`. The inline comment at
`:525-527` claims the `resID`/`-resID` split prevents overlap; it does not,
and it does not address the cross-path collision at all.

Fix (minimal): key cloud workers by their own `op.ID` (already passed to
`launchCloudCreate`/`launchCloudDelete`), which is in the same
`operations` sequence as the beget/manual keys and therefore collision-free:

```go
func (r *Registry) launchCloudCreate(opID, resID int64, ...) {
    if !r.provisionWorkers().run(opID, cloudCreateBudget+cloudPollInterval, func(ctx context.Context) {
        r.cloudCreateDo(...)
    }) {
        // guard busy: mark op failed instead of silently dropping
        _ = r.control.UpdateOperationState(opID, control.OpFailed, "", "worker_busy")
    }
}
```

(and symmetrically for delete). Create/delete mutual exclusion is already
enforced by the state CAS + idempotency, so the `-resID` trick is not needed.
Drop the inaccurate `:525-527` comment.

Acceptance test: occupy the shared guard with key `==` a known cloud resource
id (e.g. `r.provisionWorkers().run(resID, time.Hour, ...)`), then POST create;
assert the operation is marked failed / the resource reaches a terminal state
instead of hanging in `planned`. Add a unit test asserting
`launchCloudCreate` uses the operation id as its guard key.

### H2 — change-ip requires `Idempotency-Key` but ignores it → duplicate paid ChangeIP on retry

`cabinet_cloud.go:449` reads and discards the key:

```go
if _, ok := cloudIdemKey(w, req); !ok { return }
```

`03 §2.1` (lines 65-66, 76) requires the key for change-ip and states
"Повтор key не становится разрешением provider retry"; `nodehost/methods.go:229-230`
explicitly documents that ChangeIP has **no provider idempotency** and "must
never be retried". With the key discarded, a client resending the same request
after a timeout re-enters the handler, passes the `ready` check, and calls
`prov.ChangeIP` a second time — a second paid IP change, and if the first
response was lost the local state is stale. The documented deviation only
covers "synchronous 200", not the dropped key.

Fix: persist a keyed record and short-circuit. Minimal shape consistent with
the stage: create an `operations` row `kind=cloud_change_ip`,
`idem_key="cloud_change_ip:"+key` (state pending→sent→confirmed/deleted),
and on replay return the stored resource/operation before calling the
provider.

Acceptance test: two change-ip calls with the same `Idempotency-Key`; assert
provider `ChangeIP` is invoked exactly once and both responses are identical.

---

## MEDIUM

### M1 — Idempotency key namespace is not user-scoped and the request body is not compared

`cabinet_cloud.go:268` uses `"cloud_create:"+idemKey` (global), not the
`(user_id, method, path, key)` namespace required by `03 §2.1` (line 70).
`cloudReplayAck` (`:309-327`) validates ownership against `op.UserID`
(original owner), so a second user submitting a colliding key receives the
first user's `resource_id`/`operation_id`; and because the body is never
hashed/compared, replaying the **same key with a different body** silently
returns the first ack instead of the required `409 idempotency_conflict`
(§2.1 line 73). The `op.Kind != OpKindCloudCreate` conflict branch (`:310`) is
effectively dead because the kind is already encoded in the prefixed key.

Fix: prefix with the user id (`fmt.Sprintf("cloud_create:%d:%s", u.ID, key)`)
and store/compare a hash of the canonical body on replay; return
409 `idempotency_conflict` on mismatch.
Acceptance test: user A create key `K`; user B create key `K` → B does not
receive A's resource; same user, key `K` with a different body → 409.

### M2 — create/replace/change-ip body contract (`confirm`, `expected_revision`, `management`) not enforced

- `POST .../replace` and `POST .../change-ip` ignore `confirm:true` and
  `expected_revision` (`03 §4` lines 151,154; `§2.1` line 78).
  `cabinet_cloud.go:357-384`, `:469-475` only read `configuration`/`subnet_id`.
- `POST /cloud-resources` ignores the mandatory `management` block: spec
  requires `400` when absent (`03 §4` line 172), but `cloudCreateBody`
  (`:241-245`) has no `management` field and the request succeeds.

These are undocumented deviations. Either implement the checks or add them to
§3 of the implementation note. Minimal step for stage-05: document; a cheap
partial fix is to require `expected_revision` on replace/change-ip and reject
`management` bodies explicitly with `400`/`422`.

### M3 — List endpoint has no pagination (cursor/limit/next_cursor)

`03 §4` line 147 specifies `limit=1..100` default 50 and `next_cursor`;
`handleCloudList` (`cabinet_cloud.go:212-223`) returns every row
(including `deleted`) with no bound. Not listed under documented deviations
or "Explicitly NOT done". Add to §6 or implement `limit`/`cursor`.

### M4 — change-ip lost-response leaves local state undetected

Because ChangeIP is not idempotent (H2) and the handler only updates state on
a successful response (`:518`), a timeout after the provider applied the
change leaves `ip` stale with no reconciliation path. This is inherent to the
stage, but it is not documented. Add to §3 deviations (paired with the H2
fix: a keyed operation row would at least let a subsequent retry reconcile).

---

## LOW

- **L1** — `change-ip` logs the raw error `%v` (`cabinet_cloud.go:496`).
  Nothing sensitive today (`cryptoutil.Open`/`json.Unmarshal`/`nodehost.New`
  errors carry no credential value), but it is the one provider-path log that
  could later regress; prefer `cloudSafeErrCode(err)`.
- **L2** — Worker `fn` has no `recover` (`provision_worker.go:55-64`); a panic
  in a cloud worker would take the whole process down. Real-provider returns
  are always non-nil on success (`nodehost/methods.go:131,148,169,187,201,214,249`),
  so no reachable nil-deref, but the seam is test-injectable
  (`cloudFactory`) — a `defer recover` that fails the operation is cheap
  insurance.
- **L3** — Two concurrent DELETEs with different keys both pass the `ready`
  check and create two `operations` rows before either worker transitions to
  `deleting`; the `-resID` guard drops the second launch (also ignoring the
  bool), leaving one operation `pending` forever. State CAS bounds it, but a
  "delete already in flight" check (or per-resource operation lookup) would
  avoid the orphan row. Comment at `:525-527` is inaccurate as written.

---

## Definition-of-done for this bounded stage

Resolve H1 and H2 (both have single-file, minimal fixes), then re-run:

```
cd registry && go vet ./... && go test -race -count=2 -run 'TestCloud|TestManagement|CloudResource' ./internal/server ./internal/control
```

M1–M4 may land as documented deviations for stage-05 if the implementation
note §3/§6 is updated to name them; they are not blockers on their own.
Verdict remains **BLOCK** until H1/H2 are fixed or explicitly accepted as
deviations.

---

## Follow-up re-review — H1/H2/M1/L1/L2 fixes (2026-10-17)

Scope: focused re-review of the fix round described in
`implementation-stage-05-cloud-management.md` §3/§5 (items 4–8) against
`registry/internal/server/cabinet_cloud.go`, `cabinet_cloud_test.go`,
`registry/internal/control/cloud_resources.go`, `operations.go`,
`provision_worker.go`. Original review above is preserved unchanged.

### Evidence reviewed

- H1: `launchCloudCreate`/`launchCloudDelete`
  (`cabinet_cloud.go:585-602`) now call `provisionWorkers().run(opID, …)` — the
  operation id, not `resID`. On `run()==false` both mark the operation
  `failed`/`worker_busy`; create additionally drives `planned→error`. Beget/manual
  paths key the same pool by `op.ID` (`cabinet_beget.go:460,783,1030`,
  `cabinet_manual.go:426`) and `operations.id` is one `INTEGER PRIMARY KEY`
  sequence, so cloud and beget/manual keys are collision-free by construction.
  The inaccurate `-resID` comment is gone.
- H2: `cabinet_cloud.go:504-525` builds `cloud_change_ip:<uid>:<key>`, looks it
  up first and, on hit, returns `cloudResourceFresh` with no `ChangeIP`; the
  concurrent-create branch (`created==false`) does the same. Provider
  `ChangeIP` is reached only on the first, winning request.
- M1: create key is `cloud_create:<uid>:<key>` (`:286`); canonical body hash is
  stored in `request_json.body_hash` (`:303`) and compared in `cloudReplayAck`
  (`:341-344`) → 409 `idempotency_conflict`; the row is re-owned via
  `CloudResourceOwned(meta.ResourceID, op.UserID)` (`:345`), so a colliding key
  from another user never resolves to a foreign `resource_id`.
- L1: change-ip error path logs and returns `cloudSafeErrCode(err)`
  (`:547-559`); the only remaining `%v` logs are the control-layer error and a
  worker panic value (`:173`,`:608`) — neither carries provider credentials.
- L2: `cloudRecover` (`:606-611`) is `defer`red in both launch closures and
  marks the op `failed`/`worker_panic`.
- secrets ordering: `SetCloudResourceSecrets` (`:677-679`) runs before the
  `creating→ready` CAS (`:680`), so a `ready` row always carries `secrets_enc`.

### Verification (fresh)

```
cd registry && go vet ./...    → exit 0
go test -race -count=2 -run 'TestCloud|TestManagement|CloudResource' \
    ./internal/server ./internal/control
    → ok internal/server 29.851s · ok internal/control 3.527s
```

New tests present and executing (verbose):

| Test | Asserts | Result |
|---|---|---|
| `TestCloudWorkerGuardBusyFailsOp` | occupied guard key (== res id) → op `failed`/`worker_busy`, resource `error`, provider create 0 calls | PASS |
| `TestCloudWorkerGuardKeyedByOpID` | resource-id key occupied but op-id free → create proceeds, resource `ready`, provider 1 call | PASS |
| `TestCloudChangeIPIdempotentReplay` | same key twice → both 200, byte-identical body, `ChangeIP` exactly 1 | PASS |
| `TestCloudIdempotencyUserScopedAndBodyCompared` | user B same key → own resource (diff id); A same key diff body → 409 `idempotency_conflict` | PASS |

### Finding closure

| ID | Status |
|---|---|
| H1 | **Closed** — code + `TestCloudWorkerGuardBusyFailsOp` + `TestCloudWorkerGuardKeyedByOpID` |
| H2 | **Closed** — code + `TestCloudChangeIPIdempotentReplay` |
| M1 | **Closed** — code + `TestCloudIdempotencyUserScopedAndBodyCompared` |
| L1 | **Closed (code)** — no dedicated test, but the raw provider `%v` is gone |
| L2 | **Closed (code), test gap** — `cloudRecover` present; no test asserts the panic path (see R-1) |
| secrets-before-ready | **Closed** — ordering fixed; `TestCloudCreateLifecycle` asserts `ready ⇒ SecretsEnc != ""`; `-race -count=2` clean |

### Residual findings (non-blocking)

- **R-1 · LOW — L2 panic recovery is untested.** `cloudRecover` is wired, and
  the panic seam is reachable (`r.cloudFactory` is injectable), yet no test
  forces a worker panic and asserts `failed`/`worker_panic`. Add one small test
  through the existing seam.
- **R-2 · MEDIUM — change-ip replay is not scoped to the target resource or the
  `subnet_id` body.** `opKey` is `(user, key)` only; the replay branch
  (`cabinet_cloud.go:505-507`, `:520-524`) returns `cloudResourceFresh(res.ID,…)`
  for whatever resource the path names, without calling `ChangeIP` and without
  comparing the body. Consequences: (a) the same `Idempotency-Key` reused on a
  *different* resource's change-ip yields 200 on that resource's unchanged state
  (silent no-op, no 409); (b) the same key with a different `subnet_id` is
  silently ignored. This is the change-ip analogue of M1. It does not violate the
  Stage-05 requirement (exactly-one-provider-call replay holds), but note §2's
  "key reused for a different command → 409" is implemented only for
  `cloud_create` (the create-key prefix already encodes the kind, making the
  `op.Kind` check at `:328` dead there too). Cheapest hardening: include
  `res.ExternalID`/`subnet_id` in the hash and 409 on mismatch, mirroring create.

### Verdict

| Severity | Count | Status |
|----------|-------|--------|
| CRITICAL | 0 | pass |
| HIGH     | 0 | pass |
| MEDIUM   | 0 new (R-2 downgraded, non-blocking) | info |
| LOW      | 1 | info |

**Verdict: PASS.** H1, H2 and M1 are fixed and pinned by tests that genuinely
assert the behaviour (each fails if the fix is reverted); L1 and L2 are fixed in
code; the secrets-before-ready ordering is correct and race-clean under
`-count=2`. Residual items R-1 (missing L2 panic test) and R-2 (change-ip replay
not path/body-scoped) are non-blocking follow-ups for a later stage.

**Follow-up (stage-05b):** `PUT /nodes/{id}/management` profile-mode now
auto-links `current_resource_id` from the owner's bound non-deleted/non-error
`cloud_resources2` row when the policy has none (`CloudResourceIDByNode`);
enabling PROFILE on a freshly installed node no longer requires a prior
blueprint run.
