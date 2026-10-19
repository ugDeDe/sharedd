# Stage 05 — Cloud resources & node management (as implemented)

Scope: managed cloud resources (`03-cabinet-api.md` §4) and node management
policy (`03-cabinet-api.md` §5), using the minimal schema extension in
`04-storage-and-execution.md` §2. Everything below is the code as it exists in
`registry/` at the time of writing.

## 1. Files created / changed

Created:

| File | Role |
|---|---|
| `registry/internal/control/cloud_resources.go` | Store layer for `cloud_resources2` + `node_management` (CRUD, state CAS, revision CAS, secrets write). |
| `registry/internal/control/cloud_resources_test.go` | Unit tests for both tables. |
| `registry/internal/server/cabinet_cloud.go` | Routes, provider surface, async workers, DTO mapping. |
| `registry/internal/server/cabinet_cloud_test.go` | HTTP-level lifecycle/isolation/leak tests. |

Changed:

| File | Change |
|---|---|
| `registry/internal/control/control.go` | Schema: `cloud_resources2`, its index + partial unique index, `node_management` (inside the existing `schema` const). |
| `registry/internal/server/run_main_impl.go` | Registry fields `cloudFactory` (provider seam for tests), `cloudMu`/`cloudBusy` (change-ip in-process guard). |
| `registry/internal/server/cabinet_api.go` | `r.mountCloudResources(mux)` wired after `mountHostingConnections` (line 321). |

Not touched: `hosting_connections`, `nodehost`, `beget`, `sshx`, `provision`,
`node/`, and stage 01–04 files.

## 2. Routes as implemented

All routes are session-authenticated; command routes are CSRF-guarded.

| Method / path | Auth extras | Success | Notes |
|---|---|---|---|
| `GET /api/me/cloud-resources` | session | 200 `{"resources":[…]}` | Owner-only; `deleted` rows hidden by default (history lives in operations/logs), `?include_deleted=1` returns them. Zombie `planned`/`creating` rows are reconciled to `error` (see §4.1). No provider call. |
| `GET /api/me/cloud-resources/{id}` | session | 200 `Resource` | Foreign/unknown → 404 `not_found`. Same zombie reconcile as the list. |
| `POST /api/me/cloud-resources` | `Idempotency-Key` | 202 `createAck` | Body `{connection_id,label,configuration}`; rows created before the paid call, worker after the response. |
| `POST /api/me/cloud-resources/{id}/replace` | `Idempotency-Key` | 202 `createAck` | New resource with `replaces_resource_id = old.id`; old row untouched. `configuration` is **required** (no pinned snapshot at this stage). |
| `DELETE /api/me/cloud-resources/{id}` | `Idempotency-Key` + `X-Confirm-Action: delete` | 202 `createAck` / local 202 | No body. Missing confirm → 400 `confirmation_required`; missing key → 400 `idempotency_key_required`. `external_id == ""` (planned/error/stale creating) deletes the local row; an in-flight worker → 409 `resource_busy`. |
| `POST /api/me/cloud-resources/{id}/change-ip` | `Idempotency-Key` | 200 `Resource` | Body `{subnet_id?}`; synchronous, 60s ctx, one in-flight per resource. No auto-create fallback. |
| `POST /api/me/cloud-resources/{id}/install` | `Idempotency-Key` | 200 operation | Installs the agent on a ready resource; beget → 501. |
| `POST /api/me/cloud-resources/{id}/check` | CSRF | 200 `{ssh_ok,error}` | Decrypts the sealed password and runs `sshx.WaitSSH` (3 attempts, ~5s). Not ready → 409 `not_ready`; no sealed password → 409 `ssh_unavailable`; beget → 501 `not_implemented`. State unchanged. |
| `GET /api/me/nodes/{node_id}/management` | session | 200 policy DTO | Row absent → virtual default `{"mode":"manual","revision":1,"inputs":{}}`, **row not created**. |
| `PUT /api/me/nodes/{node_id}/management` | CSRF | 200 policy DTO | `manual` CAS by `expected_revision`; `profile` always 422 (see deviations). |

`createAck = {operation_id, state, resource_id}`. `Resource` DTO = `{id,
connection_id, provider, label, state, revision, external_id, ip, node_id,
replaces_resource_id, replaced_by_resource_id, active_operation_id,
last_error}`. `active_operation_id` is the last create/install/change-ip
operation (`null` when none); `last_error` is the safe provider/worker code
(`insufficient_funds`, `no_ip`, `provider_timeout`, …) and is cleared on
`ready`.

### Catalog & estimate (added post-stage-05, nodehost only)

Implemented in `registry/internal/server/cabinet_catalog.go`; beget
connections keep answering `501 {"error":"not_implemented",…}` on both
(legacy wizard flow unchanged).

`GET /api/me/hosting-connections/{id}/catalog?location={code}` (session,
owner-only) — 200 for nodehost:

```json
{"provider":"nodehost","checked_at":unix,
 "locations":[{"code":"DE","name":"Германия"}],
 "os":[{"id":"Debian 12","name":"Debian 12"}],
 "cpu_models":["7950X"],
 "disk_types":[{"id":"Storage1","name":"Storage1"}],
 "limits":{"cores":{"min":1,"max":24},"ram_gb":{"min":1,"max":64},
           "disk_gb":{"min":10,"max":1000},"types":{"VPS":true}},
 "network":{"speeds":["…"],"available":true}}
```

Normalization: `os[].id` and `disk_types[].id` are the provider **name**
strings (that is what `CreateRequest.os` / `diskType` expect); locations use
`code`, not `id`; limits are renamed to `*_gb`. With `?location=` the DTO
gains `network` from `NetworkOptions(location)`; any network-options failure
(including an unknown location) degrades to `available:false`, it never fails
the request. Failures of the main catalog calls map like verify:
timeout/transport → `504 provider_timeout`, other provider errors →
`502 provider_error`. Foreign/unknown id → 404.

`POST /api/me/cloud-resources/estimate` (session only — read-only pricing,
no CSRF, no `Idempotency-Key`), body
`{"connection_id":N,"configuration":{cores,ram,disk,networkSpeed?,period?,location}}`
— 200 for nodehost:

```json
{"amount":"2.12","currency":"USD","period":"1m","period_days":30,
 "checked_at":unix,"scope":"nodehost_api_pricing"}
```

`amount` is the exact decimal string from the provider `Decimal` (period
price, falling back to monthly). `cores`/`ram`/`disk` must be positive and
`period` must be in `{1d,3d,1w,1m,3m,6m,1y}` → otherwise `400
validation_failed`; foreign connection → 404; beget → 501; provider
timeout/error → 504/502 as above. Pinned by `TestHostingCatalog*` and
`TestCloudEstimate*` in `cabinet_hosting_test.go`.

Idempotency is keyed via `operations.idem_key`, namespaced by user id
(`cloud_create:<user_id>:<key>`, `cloud_delete:<user_id>:<key>`,
`cloud_change_ip:<user_id>:<key>`, `03 §2.1`). A create/replace replay returns
the same ack and does **not** call the provider again; a different request body
under the same key → 409 `idempotency_conflict`. change-ip is not idempotent at
the provider (`nodehost/methods.go`), so the same key short-circuits to the
stored resource state without a second paid `ChangeIP`. A key reused for a
different command → 409 `idempotency_conflict`.

### Reconcile (added post-stage-06, nodehost only)

Resources stuck in `error`/`planned` with `last_error:
provider_reconcile_required` (create outcome unknown, no `external_id`) have
DELETE blocked by design and could not be cleared by the user. The reconcile
endpoint resolves the dead end read-only against the provider inventory.

`POST /api/me/cloud-resources/{id}/reconcile` (session + CSRF; no body, no
`Idempotency-Key` — no operation row is created and operations/logs are
untouched):

- Lists the provider inventory via `ServersPage` (all pages, hard 100-page
  cap, same-cursor loop guard — `cloudAllServers`).
- Builds the set of `external_id`s claimed by the user's **other**
  non-deleted `cloud_resources2` on the same connection
  (`Store.CloudResourceClaimedExternalIDs`).
- `external_id != ""`: present in inventory → `200 {"resolved":"found",
  "external_id":…}` — the instance is already linked by this very id; stuck
  `planned/creating/error` states are CAS-promoted to `ready` with the
  inventory IP (`last_error` cleared), `ready` is left alone. Absent from
  inventory → proven deleted.
- `external_id == ""`: zero servers unaccounted for → proven absent;
  any unaccounted (unclaimed) server → `409 provider_reconcile_required` with
  the count — ambiguous, nothing is mutated, a server is never silently
  claimed by the stuck resource.
- Proven-absent transition: `error`/`planned`/`creating` → `deleted` by CAS
  (`UpdateCloudResourceState`), then the pending install reservation is
  released (`ReleasePendingSecretsForCloudResource`). `ready`/`deleting` →
  `409 resource_busy`; a live worker (`cloudWorkerActive`) → `409
  resource_busy`. `deleted` → `404` (same as foreign).
- beget connection → `501 not_implemented`; provider/inventory failure →
  `502 provider_error` with the safe code.

Files: `registry/internal/control/cloud_resources.go`
(`CloudResourceClaimedExternalIDs`), `registry/internal/server/cabinet_cloud.go`
(`ServersPage` on the `cloudProvider` surface, `cloudAllServers`,
`handleCloudReconcile`, route mount). Pinned by `TestCloudReconcile*` in
`cabinet_cloud_reconcile_test.go` (absent→deleted, unaccounted→409 without
mutation, found→ready link, beget 501, foreign 404).

## 3. Agreed deviations

- **Table name `cloud_resources2`.** The legacy `cloud_resources` table
  (`vps_id TEXT` PK) is part of the beget money/quota contour
  (`ReserveCreateOperation`/`UsedSlots`) and its on-disk format is frozen. The
  new shape (surrogate PK, FK to `hosting_connections`) lives alongside it and
  must not be merged into the legacy table.
- **beget create/delete/change-ip → 501 `not_implemented`.** New cloud routes
  accept only `nodehost` connections. Beget keeps working through the legacy
  `/api/me/vps` flow; the two contours are not unified at this stage.
- **Profile management mode → 422 `profile_incompatible`.** `PUT` with
  `mode:"profile"` is unconditionally rejected until the graph/blueprint engine
  (and `profile_versions.format`) exists. `node_management` has a
  `mode` CHECK that already allows `profile`, so the storage is ready.
- **Async worker is in-process, not crash-durable.** Create/delete run on the
  shared `provisionWorkers` pool, keyed by their own **operation id** (the same
  namespace as the beget/manual install paths — `cloud_resources2.id` is a
  separate sequence and is not used as a guard key). If the key is already taken
  the launch marks the operation `failed` with `worker_busy` instead of silently
  dropping it; a panic inside a cloud worker is recovered and fails the
  operation (`worker_panic`). A registry restart still leaves resources visibly
  stuck in `creating`/`deleting`/`error`; there is no resume-on-boot. This is
  intentional for stage-05 and is surfaced honestly to the user (states, not
  silent success).
- **Root password sealed in `secrets_enc`.** The VPS root password returned by
  the provider is AES-GCM sealed (`sealJSON`) into `secrets_enc` and is
  `json:"-"` on the store struct; it never appears in any DTO. It is kept for
  the future install stage only.
- **change-ip has no auto-create fallback.** A failed change-ip never creates a
  replacement; it returns an error and leaves the resource as-is.
- **change-ip is synchronous (200), not 202.** The spec table describes a 202
  ack; the implementation performs the change inline with a 60s bound and
  returns the refreshed `Resource` (200). Same VPS, same `external_id`.
- **M2 — request body contract not enforced.** `POST .../replace` and
  `POST .../change-ip` ignore the `confirm` and `expected_revision` fields
  (`03 §4`); `POST /api/me/cloud-resources` does not require the mandatory
  `management` block. These checks are deferred to a later stage; the routes
  accept the request without them.
- **M3 — list is not paginated.** `GET /api/me/cloud-resources` returns every
  row with no `limit`/`next_cursor` bound (`03 §4`). `deleted` rows are excluded
  by default; `?include_deleted=1` restores the old behaviour for operators.
- **M4 — change-ip lost-response has no reconciliation beyond keyed replay.**
  Because the provider `ChangeIP` is not idempotent, a timeout after the
  provider applied the change leaves `ip` stale. The keyed `cloud_change_ip`
  operation makes a same-key retry a replay (no second paid call), but a new key
  after an ambiguous timeout is not reconciled locally.
- **L3 — concurrent DELETE with different keys.** Two concurrent DELETEs
  (distinct `Idempotency-Key`) may both create `operations` rows before either
  worker transitions the resource to `deleting`; the resource state CAS bounds
  the overlap, but one operation row can be left `pending`.

## 4.1 Worker failure visibility (zombie reconcile)

- List/detail project a `planned`/`creating` resource whose
  `last_operation_id` points at a terminally `failed` operation (or at an
  `unknown` operation older than 3 minutes) into `state=error` with
  `last_error` = the operation's safe code (empty → `provider_error`). The write
  is a CAS on the expected state, so a concurrent worker that already moved on
  wins. No provider call, no lock held across I/O.
- Create polling now treats provider `401/403/404` on `Operation` as definitive:
  the resource fails immediately with that code instead of waiting out the
  10-minute budget. Transport/5xx/timeout errors remain retryable until the
  deadline.
- `TestCloudListVisibilityAndZombieReconcile` and
  `TestCloudCreateFailsFastOn404Operation` pin both behaviours.

## 4. Balance / management semantics

**Balance.** Cloud create/delete/change-ip **do not** read or write
`connection_balances` and perform no quota reservation. Quota/quota-pair logic
from `04 §3` (reserve a slot, allow one linked replacement, release only on
confirmed failure/deletion) is not implemented in stage-05. Balance refresh
remains the stage-04 route
`POST /api/me/hosting-connections/{id}/balance/refresh`. This is a deliberate
deferral, not an oversight.

**Management.** `node_management` is keyed by `node_id` with `user_id NOT NULL`
(FK `users`), so the policy survives candidate prune — ownership is durable, not
derived from the live candidate map. Revision semantics:

- `revision = 1` is the virtual default ("never written"), returned by GET
  without an INSERT.
- Every successful PUT bumps revision by 1; the first write from the virtual
  default (`expected_revision = 1`) yields `revision = 2`.
- CAS mismatch → 409 `revision_conflict`; missing/foreign node → 404
  `not_found`; `current_resource_id` is preserved across manual writes (manual
  bodies carry no policy fields at this stage).
- Ownership lookup goes through `control.NodeOwner`; legacy nodes and foreign
  owners are treated as 404, never as a writable policy.

`inputs_json` is always written as `'{}'` — inputs binding belongs to the graph
stage.

## 5. Test matrix (actual results, all green)

Command: `cd registry && go test -race ./... -count=1`

| Package | Result |
|---|---|
| `internal/control` | ok (50.2s) — incl. `TestCloudResourceCRUD`, `TestNodeManagementCAS` |
| `internal/server` | ok (162.4s) — incl. all cloud tests below |
| `internal/beget`, `cryptoutil`, `history`, `nodehost`, `provision`, `sshx`, `state` | ok |

Server-level cloud tests:

| Test | What it pins |
|---|---|
| `TestCloudCreateLifecycle` | missing key → 400; invalid config → 400 with no rows; happy path 202 → creating → ready with `external_id`/`ip`; password sealed; detail DTO has no password; replay same key → same ack, provider create called once. |
| `TestCloudCreateConcurrentSingleCall` | two concurrent creates, same key → both 202, exactly one provider create, no orphan `planned` row. |
| `TestCloudCreateBegetNotImplemented` | beget connection → 501 `not_implemented`. |
| `TestCloudDeleteLifecycle` | missing confirm → 400; missing key → 400; happy → 202 → `deleted` with provider delete once (confirmed via read); re-delete on deleted → 409 `resource_busy`. |
| `TestCloudChangeIP` | not-ready → 409 `resource_busy`; ready → 200 with the new IP, provider ChangeIP once; beget ready → 501. |
| `TestCloudOwnershipIsolation` | foreign GET/DELETE → 404 `not_found`; foreign list empty. |
| `TestNodeManagementPolicy` | GET default manual/revision 1 without creating a row; foreign/unknown → 404; profile PUT → 422 `profile_incompatible`; manual CAS 1→2 then stale → 409; foreign PUT → 404. |
| `TestCloudSecretsNeverLeak` | provider password appears in no response across create → detail → list → change-ip. |

Fixes applied during this stage to reach green:

1. `cabinet_cloud_test.go` had an unused `uidB` in `TestNodeManagementPolicy`
   (vet failure) — binding dropped.
2. `TestCloudChangeIP` revealed that `handleCloudChangeIP` overwrote the
   authoritative `ChangeIP` response IP with a `Server()` read that the mock
   returns stale. Fixed minimally: the `ChangeIP` result is authoritative,
   `Server()` is only a fallback when the operation returned no IP.
3. `TestCloudOwnershipIsolation` used a helper that logged in a second time to
   obtain a CSRF token, which no longer matched the captured foreign session
   (403 `csrf_invalid`). Fixed the test to capture the CSRF from the same
   session; helper removed.

Review fixes (stage-05 review: H1, H2, M1, L1, L2):

4. **H1** — cloud create/delete workers were keyed by `resID`/`-resID` on the
   shared `provisionWorkers` pool, which the beget/manual paths key by operation
   id; the two independent PK sequences could collide and the ignored `run()`
   result silently left the resource `planned` with a `pending` operation. Now
   keyed by `op.ID`, and a busy guard marks the operation `failed`
   (`worker_busy`) / the resource `error`. Pinned by
   `TestCloudWorkerGuardBusyFailsOp`, `TestCloudWorkerGuardKeyedByOpID`.
5. **H2** — change-ip read the `Idempotency-Key` only to require it and then
   discarded it, so a retry could issue a second paid `ChangeIP` (provider is
   not idempotent). Now a keyed `cloud_change_ip` operation row short-circuits a
   same-key replay to the stored resource state without a provider call. Pinned
   by `TestCloudChangeIPIdempotentReplay`.
6. **M1** — idempotency keys were global (`cloud_create:<key>`) and the body was
   not compared: another user reusing a key received the first user's resource.
   Keys are now user-scoped and a canonical body hash is stored/compared;
   mismatch → 409 `idempotency_conflict`. Pinned by
   `TestCloudIdempotencyUserScopedAndBodyCompared`.
7. **L1/L2** — change-ip provider log now uses `cloudSafeErrCode` (no raw `%v`);
   cloud workers recover panics and fail the operation (`worker_panic`).
8. Test-stability fix found while re-verifying: the create worker wrote
   `secrets_enc` *after* the `creating → ready` CAS, so `ready` was briefly
   observable without the sealed password (flake in `TestCloudCreateLifecycle`).
   The sealed secret is now written before the `ready` transition — a `ready`
   resource always carries its `secrets_enc`.

### Truthfulness + phantom delete (user-reported)

9. **Ready requires a real IP.** After the provider operation succeeds the
   worker polls `Server(extID)` until `IP != ""` **and** status is empty/`ACTIVE`/
   `RUNNING`. Never publishes `ready` with an empty IP; exhaustion fails the
   resource/operation with `no_ip` or `provider_timeout`. Because the worker
   context budget is `cloudCreateBudget + one poll`, a scheduling delay can
   expire the context before the internal deadline check; a
   `context.DeadlineExceeded` cause now also fails the resource instead of
   leaving it silently in `creating`.
10. **Failure codes are persisted before the error state is visible.**
    `failCloud` writes `last_error` and the failed operation first, then flips
    the resource to `error`. `insufficient_funds` (and any other provider
    code) is therefore observable together with the terminal state and is
    exposed in the DTO. `ready` clears `last_error`.
11. **`active_operation_id` is populated.** Create, install and change-ip
    record `last_operation_id` on the resource; the DTO exposes it as
    `active_operation_id`.
12. **Phantom delete.** `DELETE` no longer errors for a resource that never got
    a provider instance. `external_id == ""` and state `planned`/`error` (or a
    stale `creating` left by a restart) deletes the local row in one
    transaction, also clearing `node_management.current_resource_id`. The row
    itself is the named-deployment obligation, so removal resolves it. An
    in-flight worker (per-resource guard or running operation id) → 409
    `resource_busy`; a resource with `external_id != ""` keeps the existing
    provider-delete path.
13. **SSH check endpoint.** `POST …/{id}/check` decrypts the sealed password and
    runs `sshx.WaitSSH` (3 attempts, 5s apart, injectable `sshExec` seam),
    returning `{ssh_ok,error}` without changing resource state.

## 6. Explicitly NOT done

- Install onto a created VPS (root password is stored for it, not consumed).
- Blueprint / graph profile engine; `profile_versions.format`; inputs binding.
- UI for cloud resources or management policy.
- Beget new lifecycle under `/api/me/cloud-resources` (legacy flow only).
- Durable worker restart recovery (no resume-on-boot; stuck states stay visible).
- `GET /api/me/cloud-resources/{id}/ip-options` is **not** implemented
  (catalog + estimate **are**, see "Catalog & estimate" above — nodehost
  only, beget stays 501).
- `connection_balances` gate / quota reservation for cloud create.

## 7. Status

`partially_verified` — `go vet ./...`, `go build -mod=readonly ./...`,
`go test -race ./...`, `gofmt -l .` (empty) and `git diff --check` are all
clean; `golangci-lint`/`staticcheck` are not installed in this environment, so
lint was not run.

After the stage-05 review fixes, `go vet ./...`,
`go test -race -count=2 -run 'TestCloud|TestManagement|CloudResource'
./internal/server ./internal/control`, `go build -mod=readonly ./...`,
`gofmt -l .` and `git diff --check` are all clean.

Truthfulness/phantom-delete change verified with:
`go vet ./internal/control ./internal/server`;
`go test -race -count=1 -run 'Cloud' ./internal/control ./internal/server`;
`go build -mod=readonly ./internal/server ./internal/control`;
`gofmt -l internal/control internal/server` (empty).
