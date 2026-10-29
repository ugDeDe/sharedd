# code-review-report.md — Independent review of branch `dev`

**Reviewer:** @reviewer (Code Reviewer / QA Engineer) · **Read-only** — no application
code edited, nothing committed.
**Scope:** uncommitted working tree on `dev` over `main`, plus the two generated-asset
commits `786721b`, `f622cd2`. Reviewed modules: `registry/internal/{control,provision,
beget,cryptoutil,ansible}`, `registry/internal/server/{users_*,cabinet_*,healthcheck,
dns,selection_core,srmd,terminate,panel,stats,config_editor,run_main_impl}.go`,
`registry/internal/state/state.go`, `registry/internal/config/config.go`,
`registry/internal/machineapi/security.go`, `node/internal/agent/healthcheck.go`,
`ui/pages/{signin,cabinet}/**`, `ui/pages/panel/**`, `docs/**`.
**Against:** `docs/api-contract.md`, `docs/specs/{accounts-quota,dc2-health,
multi-master-dns,beget-provisioning}.md`, AGENTS.md invariants.

---

## Verdict

**READY WITH FIXES — not yet merge-ready as-is.**

No CRITICAL finding (no double-charge, no secret leak, no data-loss path). One HIGH
(cross-tenant availability attack, pre-existing but newly exploitable under M1
multi-tenancy), three MEDIUM (one is a hard `docs/api-contract.md` §2 shape mismatch),
the rest LOW. The money-critical Beget path, the DC2 derivation, and the multi-master
slot/migration logic are correct and well tested. Fix H1 (or explicitly accept it),
M1 and M2 before merge; LOWs are optional.

---

## Verification I actually ran (not author claims)

| Command | Result |
|---|---|
| `registry: go build ./...` | ok |
| `registry: gofmt -l internal/` | clean |
| `registry: go vet ./...` | clean |
| `registry: go test -count=1 ./internal/{state,control,cryptoutil,beget,provision,ansible}` | ok |
| `registry: go test -count=1 -race ./internal/server` | ok (44.2s) |
| `node: gofmt -l internal/ && go vet ./...` | clean |
| `node: go test -count=1 ./internal/{agent,config}` | ok (agent 20.2s) |
| `bash scripts/build_ui.sh -check` | exit 0 — all six pages «без изменений» |

`golangci-lint` is *not* installed and there is no `.golangci.yml`; the summaries'
"golangci-lint → 0 issues" claim could not be reproduced. See L2.

---

## Findings

### HIGH

**H1 — Cross-tenant node eviction via client-supplied `ip` (same-IP replacement).**
`registry/internal/server/run_main_impl.go:526`

```go
for oldID, c := range r.state.Candidates {
    if c.IP == body.IP {          // ← body.IP is client-supplied, only format-checked
        for d, ids := range r.state.Assignments { ... ids[i] = body.NodeID ... }
        delete(r.state.Candidates, oldID)
    }
}
```

`RegisterRequest.IP` is validated only as a public IPv4 (`machineapi.ValidateRegisterRequest`),
never bound to `RemoteAddr`. The user-mode gate (`authorizeUserRegister`, `users_control.go:103`)
runs **before** this loop and only checks the caller's own quota. Therefore any tenant with one
free quota slot can POST `/register` with a victim's public IP and: delete the victim's live
candidate, take over **all** the victim's master slots (and thus its DNS A records), while the
victim's `node_owners` row — and quota slot — stay consumed forever (`release-node` only).
This violates the M1 tenant-isolation guarantee (`api-contract.md` §2.1: «Владелец-проверки
обязательны») and is a tenant-vs-tenant DoS/availability bug. It is pre-existing IP-dedup
behaviour, but M1 is what makes it cross-tenant.

**Fix:** in the same-IP loop, look up the old candidate's owner in `node_owners` and replace
only when the owner matches the registering user (or the old node is legacy); otherwise skip
replacement (log a conflict). If owner lookup needs the DB, do it inside the already-held
write lock via the control store. Alternative/complementary: reject when `body.IP` ≠ caller
IP — but that breaks NAT, so owner-scoping is the correct primary fix.

---

### MEDIUM

**M1 — Admin users API omits `used_nodes`/`disabled` when they are zero → contract §2 mismatch.**
`registry/internal/server/users_admin_api.go:20-27`

```go
type userJSON struct {
    ...
    UsedNodes int  `json:"used_nodes,omitempty"`
    Disabled  bool `json:"disabled,omitempty"`
}
```

`GET /panel/api/admin/users` and the PATCH response for a fresh/active user therefore emit
`{"id":…,"username":…,"role":…,"max_nodes":…}` — `used_nodes` and `disabled` are **absent**.
`api-contract.md:44` enumerates both fields as part of the response object. The
implementation summary claims "list/patch отдают полное тело" — that is false for zero values.
The bundled UI happens to mask this (`u.used_nodes > 0` and `u.disabled` treat `undefined`
as false), so the defect is invisible today but breaks any conformant client.

**Fix:** drop `omitempty` from `used_nodes` and `disabled` (introduce a distinct
`createUserJSON` for the create response if you want the terse contract §2 body there).

**M2 — VPS create does not reserve a quota slot transactionally.**
`registry/internal/server/cabinet_beget.go:255-263` (soft `used >= u.MaxNodes` check),
`provision.Create` (`provision.go:99`).

The one-call-per-`idem_key` invariant is correct and proven (the `operations.idem_key UNIQUE`
barrier). But two concurrent `POST /api/me/vps` with **different** `idem_key`s both read
`UsedNodes < max_nodes` and then both call `CreateVps`. With `max_nodes=1` a tenant can end up
with two paid VPS. `authorizeUserRegister` reserves quota atomically via `AuthorizeNode`;
the VPS path does not.

**Fix:** reserve the slot in the same transaction as `CreateOperation` (e.g. insert a
`quota_reservations`/owner row keyed by the deterministic `node_id` and count inside the tx,
rollback on over-quota), or serialize create per user with a per-user in-process mutex —
the latter is simpler given the single-connection SQLite store.

**M3 — `fillEmpty` pass (b) collapses a multi-slot domain to one slot on idle-move → DNS churn.**
`registry/internal/server/selection_core.go:404`

```go
st.Assignments[d] = state.AssignmentSlots{idle.NodeID}   // 2 slots → 1
```

When an idle node exists, the whole domain is moved to it as a single slot even though the
desired set is `M`. Until the next tick's pass (a) refills the second slot — possibly on a
different node — the domain is served by one A record (transient DNS shrink/add churn).
Self-healing, so not a correctness break, but it contradicts the "N slots" model and causes
avoidable Cloudflare writes. `TestFillEmptySingleMasterParity`/`TestFillEmptyMigratesOneDomainToIdle`
cover M=1 only.

**Fix:** move **one** slot to the idle node (`nodes := st.Assignments[d]; pick a slot whose
IP ≠ idle.IP; replace that slot`) rather than replacing the whole list.

---

### LOW

**L1 — Production-dead `applyDNSTargets`.**
`registry/internal/server/dns.go:97`. No non-test caller (only `dns_ops_test.go:156,334`);
the selection path uses `enqueueDNSDesiredFromAssignmentsLocked` + `reconcileDNSDomain`.
`implementation-summary-multimaster.md` §60/§116 presents it as *the* replacement for
`upsertARecord` in the live path — inaccurate. Either delete it (and teach the tests to
call `enqueue…`+`reconcile…`) or keep it explicitly as a test helper.

**L2 — Dead `beget.Client.SetTokenCallback`.**
`registry/internal/beget/client.go:57` — no caller anywhere (repo-wide grep). Default
`golangci-lint` (`unused`) would report it, so the "0 issues" claim in the beget/DC2/cabinet
summaries likely predates the final tree. Delete it or wire it.

**L3 — Stale comments referencing removed `upsertARecord`.**
`registry/internal/server/dns.go:360`, `registry/internal/server/srmd.go:22`. The function
was replaced by `reconcileARecord`; update the comments.

**L4 — `api-contract.md` §6.1 is stale and self-contradictory.**
`docs/api-contract.md:149` says "HTTP-эндпоинты Beget/profiles/Ansible ещё НЕ смонтированы"
while §2.1 (same file) lists them, and `cabinet_beget.go`/`cabinet_api.go:206-221` mount
them (`/api/me/beget/link`, `/regions`, `/vps`, `/operations`, `/cancel`). Only the Ansible
*runner* (`ApplyProfile`) is genuinely unwired. Fix §6.1 (and note the Ansible exception).

**L5 — `DNSOperation.UnmarshalJSON` short-circuit uses `AND` where `OR` is meant.**
`registry/internal/state/state.go:218`:
`if len(DesiredTargets)>0 && len(DesiredNodes)>0 && len(AppliedTargets)>0 { return nil }`.
A normal freshly-enqueued op (empty `applied_targets`) or a CNAME op (nil `desired_nodes`)
never short-circuits, so it always re-decodes the legacy struct. The result is still correct
(the legacy block only fills empty fields), but the guard is pointless and misleading. Use
`||`, or drop the fast path.

**L6 — DC2 diag omits `dc2_rtt_ms` when the dc2 row is absent.**
`node/internal/agent/healthcheck.go:605-616` vs `docs/specs/dc2-health.md:45`
("Diagnostics … rtt dc2"). `dc2Diag` only writes `dc2_alive`/`dc2_fresh` (as 0) for the
nil-row case and skips `dc2_rtt_ms`. Add the (zero) rtt field for a uniform diag shape.

**L7 — IP rate limiter: proxy-blind key + unbounded map.**
`registry/internal/server/cabinet_api.go:155-184,196-202`. Keyed on `RemoteAddr` (behind a
reverse proxy that is a single IP → the 5/10-min register limit becomes global, or trivially
bypassed when `RemoteAddr` is the proxy). `hits` never evicts keys → slow memory growth.
Acknowledged in `implementation-summary-cabinet-api.md` §5 as an "осознанный минимум"; add
`X-Forwarded-For` handling and periodic pruning if registration abuse matters.

**L8 — `ensureLegacyUser` re-hashes a bcrypt password on every `Open`.**
`registry/internal/control/users.go:50-52`: generates `bcrypt(24 random bytes)` before the
`ON CONFLICT DO NOTHING` INSERT, discarding it on every restart (~65 ms + entropy). Move the
hash generation after a preliminary existence check, or skip hashing when the row exists.

**L9 — `isUniqueViolation` matches error text.**
`registry/internal/control/users.go:277-279`: `strings.Contains(err.Error(), "UNIQUE constraint
failed")`. Brittle across driver versions; prefer the `modernc.org/sqlite` error code
(`sqlite3.SQLITE_CONSTRAINT_UNIQUE`). Low risk given the driver is pinned.

**L10 — Duplicated set-equality helpers.**
`state.sameStringSet` (`state.go:248`) and `server.equalStringSet` (`dns.go:53`) are
byte-identical logic in two packages. Export one and reuse.

**L11 — `Remove` doesn't skip an already-`deleted` operation.**
`registry/internal/provision/provision.go:225-252`: if `op.State == OpDeleted` the provider
`Remove` is called again. Guard with an early return when `OpDeleted`.

**L12 — `cryptoutil.LoadMasterKey` writes the key non-atomically.**
`registry/internal/cryptoutil/cryptoutil.go:48`: `os.WriteFile` directly; a crash mid-write
leaves a corrupt key that the next `Open` refuses (by design) → all stored Beget secrets
unreadable. Use temp-file + `fsync` + `rename` like `state.AtomicWriteFile`.

---

## What I verified as correct (spec/AGENTS.md compliance)

- **Beget exactly-one paid call per `idem_key`** — proven at three layers: store
  (`CreateOperation` `ON CONFLICT DO NOTHING` + `UNIQUE(idem_key)`), provision
  (`TestCreateConcurrentOnePaidCall`, 8 goroutines, `createCalls==1`), and HTTP
  (`TestCabinetVpsConcurrentSameIdem`). Operation row is persisted `pending` **before**
  `CreateVps` (`provision.go:119`), classify `failed`/`unknown` correctly, `Reconcile` is the
  only resolver, timeout → `unknown` with no retry. `Remove` failure keeps `provider_vps_id`
  and sets `deleting`+backoff (`TestRemoveFailureKeepsVPSID`). Secrets are `json:"-"` /
  absent from views (`TestCabinetSecretsNeverReturned`, `TestBegetLinkViewHasNoSecrets`).
- **DC2 derivation** — `healthcheck.go:163-166` uses `payload.DC2OK` iff `DC2Reason != ""`,
  else legacy `MetricsOK`; old agents unaffected, no migration. Failure flows through the
  existing metrics latch only; `TestDC2FailureBlocksSelectionRecoversByLatch` pins **no**
  quarantine/terminate/ip_ban and recovery after `recover_threshold`. Criterion/`dc2_stale`
  (incl. >120s future drift and `generated_at<=0`) matches `dc2-health.md`; boundaries at
  exactly `now-300s`/`now+120s` are healthy (strict inequalities) and tested.
- **Multi-master slots & migration** — `AssignmentSlots`/`SinceSlots` legacy string/`time`
  read as one-element lists, always saved as arrays (`TestSaveLoadRoundTrip`,
  `TestLoadLegacySingleMasterAssignments`); `DNSOperation` singular keys migrate
  (`TestDNSOperationDriftedSetSemantics`). Selection core is pure and table-tested: distinct
  nodes + distinct IPs (test 1/1b), only the dead slot replaced (test 2), one TTL rotation
  per tick with per-slot `AssignmentsSince` (test 3), full-set DNS reconcile add/delete/keep
  (test 5), M=1 parity (test 6), terminate drops all slots and shrinks the desired set
  (test 7). `srmdRequiredDomains(queue,5,2)==ceil(queue/10)` including `37→4`.
- **SQL parameterization** — every query in `control/*` uses `?`; the only string-built SQL
  is `+ opCols=` (a constant) and `PRAGMA table_info(<literal>)` from hard-coded call sites.
  No injection vector.
- **Quota semantics** — `AuthorizeNode` binds once, own-node re-register consumes no slot,
  `node_owners` permanent, legacy rows excluded from `used_nodes`; 8 concurrent binders →
  exactly one winner at both store and HTTP level; fail-closed 503 when `control.db` is
  unreachable with `users.enabled=true` and old behaviour when disabled
  (`TestFailClosedRegisterWhenControlUnavailable`).
- **Test quality / determinism** — new pure-function tests use fixed anchors
  (`time.Unix(1_700_000_000,0)`); concurrency tests use barriers/`WaitGroup` with
  `-race`; no wall-clock calendar-window seeding in the new tests (AGENTS.md ban respected).
  `node` `dc2_test.go` uses a fixed `now` and a 300 s freshness window, so it is not
  time-of-day flaky. `design-qa-report.md` is honest: it marks the rendered-browser a11y pass
  `not_verified` rather than claiming a pass.

---

## Documentation accuracy

- `implementation-summary-{m1,dc2,multimaster,beget,cabinet-api}.md` are detailed and, with
  two exceptions, accurate. Under-claims (safe): cabinet-api correctly states it mounts the
  Beget endpoints.
- **Inaccurate:** `beget-provisioning.md` §6.1 / `implementation-summary-beget.md` §1 say the
  Beget HTTP endpoints are not mounted (they are — see L4); `implementation-summary-multimaster.md`
  §3 describes `applyDNSTargets` as the live replacement for `upsertARecord` (it is
  production-dead — L1); `implementation-summary-cabinet-api.md` §table claims list/patch
  return the "full body" (zero-valued `used_nodes`/`disabled` are dropped — M1).
- **Unverifiable:** all summaries' `golangci-lint run → 0 issues`. No config and no binary in
  this checkout; L2 (dead `SetTokenCallback`) suggests the claim does not hold for the final tree.

---

## Merge readiness

| Gate | Status |
|---|---|
| Build / vet / gofmt (registry + node) | ✅ pass |
| `go test -race` (registry server, control, provision, beget, ansible, state; node agent/config) | ✅ pass |
| Generated UI assets fresh (`build_ui.sh -check`) | ✅ pass |
| Money invariants (one paid call per idem_key) | ✅ verified |
| DC2 derivation + no ban on dc2 failure | ✅ verified |
| Multi-master slots / state migration / DNS set reconcile | ✅ verified by tests |
| Contract compliance (`api-contract.md`) | ❌ M1 (`used_nodes`/`disabled` omission) |
| Tenant isolation | ❌ H1 (IP-based eviction) |
| Secret handling | ✅ verified |
| Dead code / stale docs | ⚠️ L1–L4 |

**Recommendation:** fix **H1** (or record an explicit accepted-risk note) and **M1**; fix
**M2** before any real Beget spend is enabled; sweep L1–L4 (cheap, removes the doc/code
drift). After that the branch is merge-ready — the feature itself is well-scoped, heavily
and deterministically tested, and faithful to the specs on the high-risk paths.
