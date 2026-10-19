# Review: Stage-04 Storage + Cabinet API (hosting-connections)

Read-only review against `03-cabinet-api.md` §2–3, `04-storage-and-execution.md` §2,
and `implementation-stage-04-storage-api.md`. Scope: `registry/internal/control/{hosting_connections.go,control.go}`,
`registry/internal/server/{cabinet_hosting.go,cabinet_hosting_test.go}`, wiring in
`cabinet_api.go`/`run_main_impl.go`. No app edits.

## Review Summary

| Severity | Count | Status |
|----------|-------|--------|
| CRITICAL | 0 | pass |
| HIGH     | 0 | pass |
| MEDIUM   | 4 | info — fix before stage-05 |
| LOW      | 3 | noted |

**Verdict: PASS** (bounded stage-04). No CRITICAL/HIGH; the four MEDIUMs are
robustness/spec-hygiene items that become blocking only when later stages start
consuming capabilities or the catalog. Fix R-1/R-2 before any stage that reads
`capabilities_json` or exposes catalog 200s.

## Spec matrix — verified claims

| Claim | Evidence | Result |
|---|---|---|
| Ownership 404 on every id | `hostingOwned` (`cabinet_hosting.go:316`) on PATCH/DELETE/verify/refresh/catalog; list scoped by user; store WHERE `id AND user_id` (`hosting_connections.go:102,145,175`); garbage id → 404 (`parseID` + no rows) | pass, tested (foreign PATCH/DELETE/catalog 404) |
| CSRF on mutations | All 5 mutations wrapped `requireSession(requireCSRF(...))` (`cabinet_hosting.go:334-338`); GETs exempt per §2 | pass, tested |
| Credentials sealed, never plaintext | AES-GCM `cryptoutil.Seal` (`cabinet_hosting.go:187`); `CredentialsEnc`/`VerifiedAt`/`CapabilitiesJSON` `json:"-"` (`hosting_connections.go:33-39`); DTO built field-by-field | pass, `TestHostingCredentialsNeverLeak` covers create/list/refresh/verify |
| seal error texts are fixed strings | `cabinet_hosting.go:147,155,166,169,172,179` — all constants ("api_key is required for nodehost", etc.); none echo credential values | pass |
| Revision CAS conflicts | PATCH `WHERE id=? AND user_id=? AND revision=?` (`hosting_connections.go:145`); 0 rows → NotFound vs RevisionConflict disambiguated by re-read | pass, tested |
| Balance CAS by credential_revision | `>=` in `ON CONFLICT DO UPDATE WHERE` (`hosting_connections.go:231`). Equal-revision failure-over-success overwrite is **intended** stale semantics (later attempt wins at same vintage); strictly-older discarded (wandering pre-rotation snapshot); RowsAffected 0 → caller serves stored state | pass, `TestHostingBalanceCAS` covers equal-rev failure-over-success, newer applies, older discarded |
| Freshness fresh/stale/unavailable | `balanceView` (`cabinet_hosting.go:243`): success `LastAttemptAt<=CheckedAt` → fresh; failure-over-success preserves amount/currency/scope/checked_at → stale; never-succeeded → unavailable | pass, lifecycle test covers all three + amount preservation |
| Min-interval reuse (no external call) | `cabinet_hosting.go:539-545`, incl. reuse-after-failure (stale snapshot has Amount) | pass, tested (call count unchanged) |
| Provider errors never 500 | refresh always 200 with snapshot; only sanitized client codes (`nodehost` `boundCode`, `beget` `sanitizeBound`) reach `safe_error`/`error.code`; provider `Message` never leaves the client packages; storage failure → 503 `control_unavailable` fail-closed | pass |
| No legacy route regressions | `mountCabinet` (`cabinet_api.go:301-321`) unchanged except single added line `r.mountHostingConnections(mux)` | pass |
| Schema additive, double-open safe | Two new `CREATE TABLE IF NOT EXISTS` tables + index (`control.go:221-245`); no existing table touched; `foreign_keys(ON)` set at open | pass, control CRUD test re-opens DB |
| Singleflight coalescing | `balMu`/`balBusy`; waiters block on `call.done` and write the leader's exact `call.out` bytes | pass, 5 concurrent → exactly 1 external call |
| DELETE clears busy entry | `cabinet_hosting.go:445-447` — in-flight waiters still get the leader's snapshot; new requests take fresh path | pass (residual races → R-2/R-3) |
| Capabilities only via verify | No caps field in create/PATCH bodies; empty until `UpdateHostingVerified` at verify (`cabinet_hosting.go:488-494`) | pass, tested (create caps `{}`; beget `change_ip:false`) |
| Context propagation / bounded timeouts | `req.Context()` → `providerCtx` 30s (verify), explicit 30s (refresh) | pass (30s vs 15s §2 — see D-3) |
| Tests genuine | Matrix matches code; no vacuous asserts; `-race -count=2` re-run clean (36 tests, 2 pkgs) | pass |

Spot-run evidence: `go test -race -count=2 -run TestHosting ./internal/server ./internal/control`
→ 36 passed; `go vet`, `gofmt -l`, `go build -mod=readonly` clean. golangci-lint still
unavailable locally (matches stage doc; run in CI).

## Findings

### R-1 · MEDIUM — verify stamps verified_at/capabilities without credential_revision CAS

`cabinet_hosting.go:479-497` + `control/hosting_connections.go:189-194`.
`UpdateHostingVerified` writes unconditionally by `id`. A PATCH replacing credentials
during the in-flight verify (≤30s provider round-trip) leaves `verified_at=true` and
capabilities snapshotted for credentials that are **no longer stored** — the balance
path got a credential_revision CAS; verify did not. Self-inflicted by the owner and
advisory-only today (catalog 501, no consumer of caps in stage-04), hence MEDIUM, not
HIGH. Becomes blocking before any stage granting paid actions off `capabilities_json`.

Fix (one line of SQL + bool):
```go
// control/hosting_connections.go
func (s *Store) UpdateHostingVerified(id, credentialRevision, verifiedAt int64, capabilitiesJSON string) (bool, error) {
    res, err := s.sql.Exec(
        `UPDATE hosting_connections SET verified_at=?, capabilities_json=?, updated_at=?
         WHERE id=? AND credential_revision=?`,
        verifiedAt, capabilitiesJSON, time.Now().Unix(), id, credentialRevision)
    if err != nil { return false, err }
    n, err := res.RowsAffected()
    return n > 0, err
}
```
Handler: pass `c.CredentialRevision`; on `false` → `409 revision_conflict`.
Acceptance test: fake `Check` blocked on channel; start verify; PATCH new creds
mid-verify; release Check → assert 409 and `verified_at`/caps unchanged in list.
(Note: PATCH during verify also bumps `revision`; `UpdateHostingVerified` intentionally
does not bump `revision` — keep it that way so a concurrent label-PATCH is not
spuriously conflicted, but the credential guard above is required.)

### R-2 · MEDIUM — singleflight leader leaks busy entry and hangs waiters on panic

`cabinet_hosting.go:550-554`. `refreshBalance` runs without `defer`; if it panics
(provider adapter, marshal), `net/http` recovers the leader goroutine but
`close(call.done)` and the busy-entry delete never run → every waiter blocks forever
holding its HTTP connection, and all future refreshes for that connection wedge on the
leaked `balBusy` entry until restart.

Fix:
```go
r.balMu.Unlock()
func() {
    defer func() {
        r.balMu.Lock()
        if cur, ok := r.balBusy[c.ID]; ok && cur == call { delete(r.balBusy, c.ID) }
        r.balMu.Unlock()
        close(call.done)
    }()
    r.refreshBalance(req.Context(), call, c, now)
}()
```
Acceptance test: fake whose `Balance` panics → refresh returns 500 (server-level), then
a normal refresh succeeds and busy map is empty (expose via test-only read of `r.balBusy`).

### R-3 · MEDIUM→LOW — unconditional `delete(r.balBusy, c.ID)` can evict a successor's entry

`cabinet_hosting.go:552-554`. After DELETE clears the entry (`:446`) and a new leader
R2 registers, the finishing old leader R deletes R2's entry → brief duplicate external
calls (coalescing lost until R2 finishes). Benign: same-credential_revision CAS,
last-write-wins, no corruption. The compare-and-delete in R-2's fix closes it. The
DELETE-path unconditional clear is correct and stays.

### R-4 · MEDIUM — new mutation routes don't meet §2 strict-decode contract

Shared `decodeCabinetJSON` (`cabinet_api.go:323-328`): unknown fields accepted,
trailing second JSON value accepted, oversize body → 400 `validation_failed` instead of
413 `body_too_large`. Spec §2 requires strict decode + 413 for new routes. Inherited
from the whole legacy cabinet (low exploitability — undecoded fields can't inject), so
not blocking for stage-04. Fix when touched: strict variant for hosting routes —
`dec.DisallowUnknownFields()`, second `dec.Decode` must return `io.EOF`,
`errors.As(err, *http.MaxBytesError)` → 413. Acceptance test: body with unknown field
`{"provider":"nodehost","label":"x","credentials":{"api_key":"k"},"x":1}` → 400;
2KiB body against 64KiB reader trick (or direct MaxBytes check) → 413.

### LOW (noted, no action required this stage)

- **R-5 refresh-after-DELETE race**: DELETE clears busy → new refresh on deleted
  connection → wasted provider call, `SaveHostingBalance` insert fails FK
  (`connection_balances` → `hosting_connections`, FKs ON — orphan impossible), logged,
  `emitBalance` falls back to stale in-memory view → 200 for a deleted id. Cosmetic;
  disappears with spec's `connection_in_use` delete-gating.
- **R-6 verify logs `hostingProviderFor` error** (`cabinet_hosting.go:473`): in a
  corrupt-storage pathology `json.Unmarshal` syntax errors could quote a decrypted
  fragment server-side. Response is fixed text; client never sees it. If paranoid,
  log only `c.ID` + a constant.
- **R-7 min-interval check does a SQLite read under `balMu`** (`:539`): blocks all
  connections' refreshes for one PK read. Fine at current scale — ceiling noted.

## Documented deviations — accepted as-is

- Catalog 501 stub; beget verify = Balance() success; `{code}` parsed unused;
  in-process-only coalescing; `change_ip` only for nodehost — all declared in the
  stage doc and reflected in tests.
- 30s provider timeout vs §2's 15s handler deadline: matches the existing legacy
  `providerCtx` (`cabinet_beget.go:60`) used by all beget routes — pre-existing
  pattern, not a stage-04 regression.
- `connection_in_use` never occurs (no referencing tables) — per bounded scope.
- Refresh lacks `requireMasterKey` (create/patch/verify have it): fail-safe — decrypt
  fails → `credentials_unavailable` → 200 unavailable snapshot. Intentional or not,
  safe; align for consistency when convenient.
- DELETE unconditional (no in-flight-effect check) — matches spec's stage-04 scope;
  revisit with `cloud_resources`.
- `emitBalance` marshal-failure fallback `{"error":"internal"}` under HTTP 200 is
  unreachable in practice (all-scalar DTO); fine.

## Verdict

**PASS** for this bounded stage. Requirements before stage-05: R-1 (capability CAS)
and R-2 (panic-safe singleflight release) — both small, test-covered fixes; R-3 rides
along with R-2. R-4 as a tracked deviation or a strict-decode wrapper, reviewer's
choice.
