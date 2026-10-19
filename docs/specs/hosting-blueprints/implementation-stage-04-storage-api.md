# Stage-04 Implementation: Storage + Cabinet API (hosting-connections)

Bounded stage-04 per `04-storage-and-execution.md` §2 and `03-cabinet-api.md` §3.
Status: **partially_verified** (full test matrix green; golangci-lint unavailable in this environment).

## Files Created / Changed

### Implementation (done in earlier interrupted run, unchanged this pass)

- `registry/internal/control/hosting_connections.go` — store layer: CRUD with CAS by `revision`, credentials replace bumps `credential_revision`, `UpdateHostingVerified`, `HostingBalance`, `SaveHostingBalance` (upsert with CAS by `credential_revision`: applies only when the incoming revision is `>=` stored; stale in-flight snapshot after key rotation is discarded, caller returns stored state).
- `registry/internal/control/control.go` — additive schema migration (`CREATE TABLE IF NOT EXISTS`, safe for existing DBs):
  - `hosting_connections`: `id`, `user_id → users(id)`, `provider CHECK IN ('nodehost','beget')`, `label`, `credentials_enc` (AES-GCM sealed), `credential_revision`, `revision`, `verified_at`, `capabilities_json`, `created_at`, `updated_at`; index `idx_hosting_connections_user`.
  - `connection_balances`: `connection_id PK → hosting_connections(id)`, `user_id`, `credential_revision`, `amount`, `currency`, `scope`, `checked_at`, `last_attempt_at`, `safe_error`.
- `registry/internal/server/cabinet_hosting.go` — routes, DTOs, provider adapters (`hostingNodehost`, `hostingBeget`), credential validation/sealing (`cryptoutil.Seal`), verify mapping, balance refresh (singleflight + 60s min-interval), catalog stub.
- `registry/internal/server/cabinet_api.go` — `mountHostingConnections(mux)` mounted.
- `registry/internal/server/run_main_impl.go` — `Registry` fields: `hostingFactory` (test seam), `balNow` (clock injection), plus `balMu`/`balBusy` singleflight state.

### Fixed this pass

- `registry/internal/server/cabinet_hosting_test.go` — repaired broken helpers (see "Test repair" below).
- `registry/internal/server/cabinet_hosting.go` — **one genuine implementation bug found by tests**: a failed balance refresh overwrote the previous successful snapshot with an empty one (`unavailable` instead of `stale`). Fix: on provider failure, `refreshBalance` now merges the prior stored reading (`amount`/`currency`/`scope`/`checked_at`) into the new attempt row before save, so a failure over a previous success yields `stale` with the amount preserved; failure with no prior attempt stays `unavailable`. Storage layer (`control`) untouched.

### Review fixes (post-review pass, per review-stage-04 §findings)

- **R-1 — verify now CAS-guards by `credential_revision`**: `control.UpdateHostingVerified` gained `userID` + `credentialRevision` params; the UPDATE matches `WHERE id AND user_id AND credential_revision` (same shape as `PatchHostingConnection`), and 0 rows disambiguate by re-read → `ErrNotFound` / `ErrRevisionConflict`; the handler maps through the existing `hostingControlErr` (stale revision → `409 revision_conflict`). `revision` itself is not bumped by verify, so a concurrent label-PATCH is never spuriously conflicted. New tests: `TestHostingVerifyCredentialRevisionConflict` (PATCH credentials mid-verify via blocked `Check` → 409, `verified_at`/caps stay empty, re-verify → 200) and control-level CAS cases in `TestHostingConnectionCRUD` (stale revision → conflict, foreign user → not found, current revision → success).
- **R-2/R-3 — singleflight release moved into a `defer`** in `handleHostingBalanceRefresh`: compare-and-delete of `balBusy[c.ID] == call` plus `close(call.done)` run even if `refreshBalance` panics (provider adapter/marshal), so waiters can no longer hang and a successor's busy entry can no longer be evicted by a finishing old leader (R-3). The unconditional end-of-handler delete is gone; the DELETE-path clear at `handleHostingDelete` is untouched and still correct. New test: `TestHostingBalanceRefreshPanicSafety` (panic-injected `fakeHosting.Balance` under mutex → `balBusy` empty after the panic and the next refresh returns 200 fresh).

## Routes (as implemented)

| Method & Path | Auth | CSRF | Result |
|---|---|---|---|
| `GET /api/me/hosting-connections` | session | no | `200 {connections:[ConnectionJSON]}` |
| `POST /api/me/hosting-connections` | session | yes | `201 ConnectionJSON` (provider never called) |
| `PATCH /api/me/hosting-connections/{id}` | session | yes | `200 ConnectionJSON`, CAS `expected_revision` |
| `DELETE /api/me/hosting-connections/{id}` | session | yes | `204`; foreign → `404` |
| `POST /api/me/hosting-connections/{id}/verify` | session | yes | `200 {verified,checked_at,capabilities}` |
| `POST /api/me/hosting-connections/{id}/balance/refresh` | session | yes | `200 ConnectionJSON` (never 500 on provider error) |
| `GET /api/me/hosting-connections/{id}/catalog` | session | no | **`501 not_implemented` stub** |

Ownership enforced by `HostingConnectionOwned(id, userID)` — foreign ids return `404 not_found` (not 403).

### Documented deviations from 03-cabinet-api.md §3

- **Catalog GET → 501 `not_implemented`** — real provider catalog is the next stage.
- **beget verify** — success of hosting `Balance()` call is treated as verified; cloud-JWT verify flow deferred. Without a hosting API pair, balance returns `422 balance_credentials_missing` class errors.
- **`{code}` in verify body** — accepted and parsed but unused (2FA flow not wired; documented in handler comment).
- **Singleflight coalescing is in-process only** (`balMu`/`balBusy` map) — not multi-instance. Concurrent registry replicas could each hit the provider once per window.
- **beget capabilities: `change_ip=false`** — `hostingCapsFor` grants `{create:true, change_ip: provider==nodehost, delete:true}`; caps are snapshotted into `capabilities_json` at verify time.
- **Hosting routes use the shared `decodeCabinetJSON`, not a strict decode** — unknown fields accepted, trailing second JSON value accepted, oversize body → `400 validation_failed` instead of `413 body_too_large`. This is the whole-cabinet legacy behavior (`cabinet_api.go`); a hosting-only strict variant would fork the shared helper for one route group (deviation R-4 from review-stage-04, accepted: undecoded fields can't inject, low exploitability). Revisit if §2 strictness is enforced cabinet-wide.

## Balance Semantics

- **fresh** — last attempt succeeded (`amount` set, `last_attempt_at <= checked_at`).
- **stale** — newer attempt failed over a previous success: `amount`/`checked_at` preserved from the success, `error.code` set, `last_attempt_at` updated.
- **unavailable** — no successful attempt ever (or none on record): `amount=null`, `error.code` set.
- **Min-interval 60s** — refresh within 60s of the last attempt reuses the stored snapshot with **no external call**.
- **CAS by `credential_revision`** — a wandering in-flight request with old credentials cannot overwrite a newer-vintage snapshot.
- **Provider error → HTTP 200 with the snapshot, never 500.** Only safe error codes leave the process (`provider_error`, `provider_timeout`, `credentials_unavailable`, client codes); provider message text never reaches the client.
- Credentials are AES-GCM sealed (`cryptoutil.Seal`, master key); plaintext never appears in JSON responses, logs, or errors.

## Test Matrix (actual results)

| Test | Scope | Result |
|---|---|---|
| `TestHostingCRUDLifecycle` | create 201/rev1·cr1, list, PATCH label (rev2/cr1), PATCH creds (cr2/rev3), revision conflict 409, foreign PATCH/DELETE 404, delete 204 + repeat 404 | pass |
| `TestHostingCreateRequiresCSRF` | missing CSRF → 403 `csrf_invalid` | pass |
| `TestHostingCreateValidation` | 7 invalid bundles → 400 `validation_failed`; valid beget full pair → 201 | pass |
| `TestHostingVerify` | nodehost ok → caps{create,change_ip,delete}; auth fail → 422 `provider_auth_failed`; transport → 504 `provider_timeout`; beget → `change_ip:false`; beget BalanceError → 422 | pass |
| `TestHostingBalanceLifecycle` | fail→unavailable+error.code (200); success→fresh `42.50`; fail-over-success→stale preserved; min-interval reuse (0 external calls) | pass |
| `TestHostingBalanceCoalescing` | 5 concurrent refreshes → all 200, exactly 1 external call | pass |
| `TestHostingCredentialsNeverLeak` | secret absent in create/list/refresh/verify responses | pass |
| `TestHostingCatalogStub` | 501 `not_implemented`; foreign 404; garbage id 404 | pass |
| `TestHostingBalanceCAS` (control) | CAS accept/reject by credential_revision | pass |
| `TestHostingVerifyCredentialRevisionConflict` | PATCH creds mid-verify → 409, verified_at/caps unchanged; re-verify 200 | pass |
| `TestHostingBalanceRefreshPanicSafety` | panic in provider → balBusy empty, next refresh 200 fresh | pass |

### Test repair (test-file only)

`cabinet_hosting_test.go` had a placeholder `unwrap(rec *recorderAlias)` (undefined type) and illegal casts `(*httptestResponse)(cabinetCall(...))` — `cabinetCall` returns `*httptest.ResponseRecorder`, which cannot be pointer-cast to an unrelated struct. Fix: deleted the placeholder, added `wrapRec(rr *httptest.ResponseRecorder) *httptestResponse`, replaced all casts, wrapped the two direct `.Body` uses (buffer, not string), and switched the fake balance fixture `Currency` from `"RUB"` to `"USD"` for nodehost scope consistency.

## NOT DONE (explicit, deferred)

- `cloud_resources` table / linkage — connection delete is unconditional (`connection_in_use` never occurs).
- Management policy routes (per the full 04 spec §3).
- Blueprint runs / execution stage.
- UI (cabinet hosting page).
- Catalog real implementation (501 stub).
- Multi-instance coalescing (single-instance only).

## Verification

- `go vet ./...` — clean
- `go test -race ./internal/control ./internal/server -count=1` — pass
- `go test -race -count=2 -run 'TestHosting' ./internal/server` — pass
- `go test -race ./internal/control -count=1` — pass
- `go build -mod=readonly ./...` — ok
- `gofmt -l .` — empty
- `git diff --check` — clean
- `golangci-lint run ./...` — **unavailable** in this environment (not installed); run in CI

Status: **partially_verified** — everything runnable locally is green; lint pending CI.

## Review fixes applied (R-1..R-4)

R-1, R-2, R-3 fixed and test-covered (see "Review fixes" above); R-4 recorded as a documented deviation (`decodeCabinetJSON` shared legacy decode kept; 400 vs 413 + unknown-fields tolerance noted in deviations list). Review findings R-5..R-7 remain noted-only per the review.
