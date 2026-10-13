# Security Review — dev branch: users/auth, quotas, Beget provisioning, cabinet

- Scope: uncommitted + committed work on branch `dev` (independent read-only review).
- Reviewed: `registry/internal/control/**`, `registry/internal/server/{users_admin_api,users_control,cabinet_api,cabinet_beget,cabinet_pages}.go`, `registry/internal/{provision,beget,cryptoutil,ansible}/**`, `registry/internal/machineapi/security.go`, `registry/internal/config/config.go`, `node/internal/agent/healthcheck.go`, `docs/api-contract.md`, cabinet/signin/panel UI sources.
- Method: manual source review, no code changes, no commits. No live providers were contacted.
- Date: 2026-10-12.

## Verdict

**FAIL — not ready to ship as-is.** No remote-code-execution, SQL-injection, or secret-disclosure-on-the-wire defect was found (those controls are solid). The blocking problems are on the **paid-provisioning abuse surface**: the per-user quota and the daily rate/cooldown limits are enforced by a non-atomic pre-check that also measures the wrong table, so a user can exceed both `max_nodes` and `max_creates_per_day` by issuing concurrent requests with distinct `idem_key`s, and can provision paid VPS without ever consuming a quota slot. Login has no rate limiting. Fix CRITICAL/HIGH before release.

Positives confirmed (no action): all SQL parameterized; Beget `login/password` and SSH private key stored AES-GCM-encrypted with per-message random nonce, master key in a separate `0600` file; secrets excluded from all API representations (`BegetLinkView`, `Operation.RequestJSON json:"-"`, `begetLinkJSON`); authkeys stored as sha256 + short prefix only; bcrypt cost 10; session cookie `HttpOnly+Secure+SameSite=Lax`; CSRF double-submit enforced on every mutating cabinet route; owner checks on nodes/operations/beget-link; fail-closed `503` when control DB / master key unavailable; `esc()`-based HTML escaping on all server-derived HTML in cabinet/signin/panel.

---

## CRITICAL

### C1 — Pay-limit checks are non-atomic and raceable; multiple paid `CreateVps` per user

- Files: `registry/internal/server/cabinet_beget.go:243-306`, `registry/internal/provision/provision.go:109-144, 273-299`.
- Impact: `handleMeVps` checks `OperationByIdemKey` (243) and quota `used >= u.MaxNodes` (255-263), then `prov.Create` re-checks idempotency (110) and `checkLimits` (115: `CreatesInLast24h` + cooldown), and only then inserts the operation via `CreateOperation` (`provision.go:119`). None of the limit checks is part of the atomic insert-and-reserve step. Two (or N) concurrent requests from the same user with **different** `idem_key`s all observe `used = 0` / `creates24h = 0`, all pass, and all reach `p.beget.CreateVps` — up to the number of in-flight requests. The only atomic guard, `UNIQUE(idem_key)` (`operations.go:63-79`), is per-key and does not limit distinct keys. `max_creates_per_day` and `cooldown` are therefore bypassable, and the platform's cost/abuse controls on the paid provider API do not hold under concurrency.
- Evidence that the guarantee is only per-key: `cabinet_api_test.go:297 TestCabinetVpsConcurrentSameIdem` (8 goroutines, **same** key) asserts `creates() == 1`. There is no test for distinct keys.
- Fix: make consumption atomic with insertion. In one transaction: `INSERT ... ON CONFLICT(idem_key) DO NOTHING`; if inserted, re-`COUNT` create-ops in `state IN ('pending','sent','confirmed','unknown')` for the user within the window plus `COUNT` active `cloud_resources` for the user, and `ROLLBACK` to `quota_exceeded` / `rate_limited` when over. Alternatively take a per-user row lock (`SELECT ... WHERE user_id=? ` then write) before the provider call. The check must run against the just-reserved row, not a read taken earlier.

---

## HIGH

### H1 — `max_nodes` gate for paid VPS counts the wrong table

- File: `registry/internal/server/cabinet_beget.go:255-263`; `registry/internal/control/nodes.go:31-35, 42-82`.
- Impact: the gate uses `UsedNodes` = `COUNT(node_owners WHERE user_id=? AND legacy=0)`. A `node_owners` row is created only when an agent later registers that `node_id` (`AuthorizeNode`), **not** when a VPS is provisioned. `handleMeVps` writes only an `operations` row and (on success) a `cloud_resources` row. So a user with `max_nodes=1` can serially provision paid VPS (bounded only by the equally-broken daily cap in C1) while `used` stays 0, because the provisioned server never consumes a node slot unless/until its agent registers under a matching `node_id`. The `TestCabinetRegisterDefaultQuotaBlocksVpsCreate` test only covers `max_nodes=0` and therefore does not catch this.
- Fix: count provisioned/active resources in the same gate — e.g. `COUNT(cloud_resources WHERE user_id=? AND state NOT IN ('deleted'))` plus live `operations` in non-failed/non-deleted states — and reserve atomically (see C1). Decide explicitly whether `max_nodes` caps registrations, paid VPS, or both, and encode that one meaning.

### H2 — `/api/auth/login` has no rate limiting or lockout

- File: `registry/internal/server/cabinet_api.go:263-295`; limiter exists only for register (`:239-246`, `cabinetLimiter` at `:187-194`).
- Impact: unlimited online password guessing. bcrypt cost 10 slows each attempt but does not throttle; there is no per-IP or per-account counter, no lockout, and no CAPTCHA. A leaked/weak `username` (lowercase `[a-z0-9_.-]{3,32}`, so enumerable via register `409 duplicate`) plus unlimited attempts is a direct account-takeover path into the community session and the user's linked Beget account operations.
- Fix: reuse `ipRateLimiter` for login (e.g. 10 attempts / 15 min per IP) **and** add a per-username failure counter with exponential backoff; return a uniform `401 bad_credentials` regardless.

### H3 — No block on a new create while a prior create is pending/unknown (duplicate paid orders across keys)

- Files: `registry/internal/server/cabinet_beget.go:243-306`; `registry/internal/provision/provision.go:146-205`.
- Impact: the documented invariant "повторный платный заказ запрещён" holds only per `idem_key`. If a create times out, the row becomes `unknown` (`provision.go:156`), and the user (or a retrying client that chose a fresh `idem_key`) can immediately issue another create with a new key and different `node_id` — a second paid order for a resource that may already exist. `Reconcile` only resolves existing `unknown` rows; it does not block new creates. Nothing keys the check to "user has an unresolved create".
- Fix: in the atomic gate (C1) refuse a new create while the user has any `pending`/`sent`/`unknown` create operation, unless the request explicitly retries that same `idem_key`. Surface `state=unknown` prominently and require reconciliation before a new key is accepted.

### H4 — Operation is returned to a non-owner in a cross-user `idem_key` race

- Files: `registry/internal/server/cabinet_beget.go:242-253, 294-304`; `registry/internal/control/operations.go:63-79`.
- Impact: the ownership check is a read before the write. If user B races user A on the same `idem_key` (B first observes not-found at `:243`, then A inserts, then B's `prov.Create` gets `created=false` and the existing row from `OperationByIdemKey`), `op.ID != 0` and B receives `200` with A's operation object (`id`, `node_id`, `state`, timestamps). `operationJSON` omits `user_id`/`request_json`, so it is limited to a cross-tenant metadata leak and a false success, but it is still an authz boundary crossed. Triggering requires guessing A's key (user-chosen string), so likelihood is low, but the fix is trivial.
- Fix: after `CreateOperation` returns `created=false` (inside `provision.Create`), compare `op.UserID` to the requester and return an error/404 instead of the row; do not rely solely on the server's pre-read.

---

## MEDIUM

### M1 — CSRF cookie is not session-bound and lacks a `__Host-` prefix

- File: `registry/internal/server/cabinet_api.go:99-143`.
- Impact: double-submit is correct in the common case and `SameSite=Lax` blocks cross-site POSTs, but the CSRF cookie (`sharedd_csrf`) is not bound to the session token and has no `__Host-` prefix. If the registry is served from a domain with attacker-influenced sibling subdomains, a sibling can set a parent-domain `sharedd_csrf` cookie (cookie tossing); the browser then sends both, `req.Cookie` may return the attacker's value, and the attacker's page can set a matching header. `Lax` does not stop same-site subdomain attacks.
- Fix: name the cookie `__Host-sharedd_csrf` (`Secure; Path=/; no Domain`) and/or HMAC the CSRF value with the session token so a tossed cookie cannot validate.

### M2 — Username enumeration and disabled-account oracle

- Files: `registry/internal/server/cabinet_api.go:275-286`; `registry/internal/server/users_admin_api.go:45-46`; `registry/internal/control/users.go:234-249`.
- Impact: `Authenticate` returns `ErrUserDisabled` (→ `403 user_disabled`) distinctly from `ErrBadCredentials` (→ `401 bad_credentials`), revealing account existence and status. Register returns `409 duplicate`, also enumerating valid usernames, which feeds H2.
- Fix: fold disabled into a uniform `401 bad_credentials` at the login boundary (keep the disabled signal internal/logged), and/or rate-limit + generic response on register conflict. This is defense-in-depth given H2 is fixed.

### M3 — `nodeIDForIdem` uses only 20 bits

- File: `registry/internal/server/cabinet_beget.go:214-217`.
- Impact: `nodeIDForIdem` = `"vps-" + hex(sha256(idem_key))[:5]` → 5 hex chars = 20 bits. Distinct `idem_key`s collide by birthday bound (~50% near ~1.2k keys). The derived `node_id` feeds `hostnameFor` used both for the create hostname and for `Reconcile` matching (`provision.go:192-194`). Within one Beget account, two operations can share a hostname/node_id, causing `Reconcile` to match the wrong VPS or an agent to bind the wrong node slot. Not directly remote-exploitable, but it undermines the money-safe reconciliation design.
- Fix: derive the node_id from the operation row id (monotonic, unique) or use at least 8–10 hex chars, and validate it against `machineapi.ValidateNodeID`.

### M4 — master.key handling gaps

- File: `registry/internal/cryptoutil/cryptoutil.go:30-52`.
- Impact: (a) when the file already exists, its permissions are never checked — a pre-existing world/group-readable `master.key` is accepted silently; (b) newly-created key file relies on the process umask to be `0600` only for the mode arg (it does pass `0o600`, but the parent directory perms are not checked); (c) there is no key version/rotation: a single key encrypts all users' Beget creds and SSH keys forever, and `LoadMasterKey` refuses to regenerate (correct for safety, but operationally there is no rotation path). Compromise of this file plus the control DB decrypts every stored secret.
- Fix: on load, `os.Stat` and reject (or warn loudly on) mode with group/other bits; document a rotation procedure (dual-key `key_id` column, decrypt-old/encrypt-new) for a later phase.

### M5 — `panelAuthorized` returns `true` for an empty token (latent auth bypass footgun)

- File: `registry/internal/server/panel.go:37-46`.
- Impact: `if tok == "" { return true }`. In production, config validation requires a non-empty `panel.token` whenever the panel is enabled, and `users.enabled` requires the panel — so the admin user APIs (`users_admin_api.go:58-72`) are not currently exposed. But `mountAdminUsers` is registered unconditionally (`run_main_impl.go:430`), so any future config path that reaches these handlers with an empty token makes **all** admin endpoints unauthenticated. This is fragile by construction.
- Fix: fail closed — return `false` on empty token, and gate the test-only behavior behind an explicit test hook instead of production code.

### M6 — Registration rate-limiter map is unbounded

- File: `registry/internal/server/cabinet_api.go:155-184`.
- Impact: `hits map[string][]time.Time` is pruned only for keys that call again; distinct source IPs that never recur are never removed. A large number of distinct source addresses (or a long-running process) grows the map without bound → memory pressure/DoS. Only register uses it today.
- Fix: sweep stale entries on a timer or cap the map size (LRU).

### M7 — Ansible runner hardening (currently unreachable via HTTP)

- File: `registry/internal/ansible/runner.go:73-113`.
- Impact: no live HTTP handler calls `ApplyProfile` (grep confirms only `provision`, tests). If wired later: `req.Host` is not validated as an IP (only the local/registry-host deny list at `:156-169`), and `StrictHostKeyChecking=accept-new` (`:88`) trusts first-seen host keys. Vars are schema-gated by `ValidateVars`, and args go through `exec` (no shell), so no command injection is present today. `-e <json>` uses ansible extra-var precedence; with an admin-authored schema that exposed connection vars (e.g. `ansible_ssh_common_args`), a user var could redirect the SSH connection.
- Fix: before wiring to HTTP, validate `Host` as a routed IPv4 (reuse `machineapi.ValidatePublicIPv4`), deny-list ansible connection var names in `ValidateVars`, and pin host keys to a known fingerprint.

### M8 — Globalping/beget URL id handling and response trust

- File: `registry/internal/beget/client.go:302-308, 347-375`.
- Impact: `GetInfo`/`Remove` interpolate `id` into the URL path without escaping; ids originate from the provider, so this is low risk, and the base URL is a hardcoded const (`BaseURL`, `:26`; `New` always uses it) — no SSRF via user input was found. Response bodies are limited to 4 MiB (`:178`). Noted as a small hardening item, not a finding requiring immediate change.

---

## LOW

- **L1 — Admin 401 is plain text.** `adminUsersGuard` (`users_admin_api.go:59-62`) emits `http.Error` text while the rest of the API returns `{"error":...}`. Consistency only.
- **L2 — Session purge is startup-only.** `PurgeSessions` runs once in `openControl` (`users_control.go:40`); expired `sessions` rows persist and are only rejected at read time (`sessions.go:45`). Add a periodic purge for hygiene.
- **L3 — Password length has no upper bound.** `validateNewUser`/`UpdateUser` enforce only `>=10` (`users.go:80, 203`). bcrypt silently truncates at 72 bytes; set an explicit max (e.g. 72/128) to avoid confusing truncation.
- **L4 — Cookies carry `MaxAge` but no `Expires`**, and `Secure` is unconditional (correct for TLS-only service, but local HTTP dev cannot log in). Cosmetic/operational.
- **L5 — `randomSecret` panics on `crypto/rand` failure** (`users.go:282-288`, `authkeys.go:29-31`, `cabinet_api.go:145-151`). Acceptable on Linux, but a panic in a request path is an availability risk; prefer returning an error.

---

## Coverage summary (what was checked and held)

| Area | Result |
|---|---|
| SQL injection | Pass — all queries parameterized; dynamic SQL limited to constant column lists / internal DDL (`control.go:170-194`). |
| Command injection (Ansible) | Pass today — `exec` argv, no shell; vars schema-gated; runner not HTTP-wired. See M7. |
| SSRF (Beget) | Pass — base URL is a hardcoded const; no user-controlled host. |
| XSS (cabinet/signin/panel) | Pass — `esc()` on all server-derived interpolations (`ui/lib/dom.ts:5`), `textContent` for names/errors. |
| Secret storage | Pass — AES-GCM + random nonce (`cryptoutil.go:55-100`); key outside DB at `0600`. See M4. |
| Secret leakage (API/logs/errors) | Pass — no secrets in `BegetLinkView`, `operationJSON`, `begetLinkJSON`; no logging of creds/tokens (`users_control.go` logs only lifecycle counts). |
| Session cookie flags | Pass — `HttpOnly, Secure, SameSite=Lax` (`cabinet_api.go:121-134`). |
| CSRF coverage | Pass — double-submit on logout, beget-link PUT, vps create, cancel, delete-node (`cabinet_api.go:206-221`). See M1. |
| Owner / privilege boundaries | Pass for steady state — nodes/operations/beget link scoped to session user; admin under `panel.token`, not session. See H4 for the race. |
| authkey hashing/prefix-only | Pass — sha256 + 8-char prefix, full key returned once (`authkeys.go`). |
| bcrypt | Pass — cost 10, `CompareHashAndPassword` (`users.go:96, 262`). See H2/L3. |
| Fail-closed control store | Pass — `Open` error → `control=nil` → `503` on user/admin APIs and authkey registration (`users_control.go:30-51`, `cabinet_api.go:56-66`, `authorizeUserRegister` at `users_control.go:103-150`). |
| Paid-order "one call per idem_key" | Pass for the same key under concurrency (`cabinet_beget.go:242`, `provision.go:110-129`); **fails** for the broader "one paid order per user/quota" invariant — see C1/H1/H3. |

## Recommended fix order

1. C1 + H1 + H3 (single atomic reserve-and-check in `provision.Create`, quota over provisioned resources).
2. H2 (login throttling + uniform error).
3. M1, M4, M5 (cookie hardening, key perms, fail-closed panel auth).
4. H4, M2, M3, M6–M8, L1–L5.

---

# RE-CHECK — fix round (2026-10-12, read-only)

- Scope: confirm the findings above are closed after the fix round recorded in
  `docs/specs/implementation-summary-cabinet-api.md` §"Fix round". Independent
  source review; no application code changed, no commit. Targeted tests were run
  (`cd registry && go test -race -run 'TestCreate|TestReserve' ./internal/provision/ ./internal/control/` → pass).
- Reviewed new/changed code: `control/operations.go` (`ReserveCreateOperation`),
  `provision/provision.go`, `server/{cabinet_beget,cabinet_api,run_main_impl,users_control}.go`,
  `cryptoutil/cryptoutil.go`, and the added tests.

## Umbrella verdict

**PARTIAL — the previously enumerated blockers are closed, but the paid-provisioning
abuse surface is NOT fully closed.** C1 (concurrent, distinct `idem_key`, no cancel),
H1, H2, H3, H4 and the claimed medium items are fixed. One **HIGH** residual remains
(R1): cancelling an operation while its paid `CreateVps` call is in flight frees the
reservation and lets a second distinct-key paid call pass the same slot — two paid VPS
for one slot / daily-cap bypass. See R1. No CRITICAL remains.

## Per-finding verdict

| ID | Claim under test | Verdict | Evidence |
|---|---|---|---|
| C1 | Atomic quota+daily+cooldown over `node_owners ∪ cloud_resources ∪ operations`; N concurrent distinct keys ≤ quota/daily; each paid VPS consumes a slot | **PASS** for the stated concurrency scenario | `ReserveCreateOperation` (`operations.go:102-225`) runs INSERT+counts in one tx on a `SetMaxOpenConns(1)` pool (`control.go:59`), so writers serialize; `TestCabinetVpsConcurrentDistinctIdemOnePaidCall` (8 distinct keys, quota=1 → 1 `CreateVps`). **But see R1.** |
| H1 | `max_nodes` counts provisioned/active resources, not just `node_owners` | **PASS** | Union count includes `cloud_resources(state≠deleted)` and non-failed/deleted ops (`operations.go:142-153`); `TestCreateQuotaCountsProvisionedVps`, `TestCabinetVpsQuotaCountsProvisionedVps`. |
| H2 | Login rate-limit + per-account lockout + uniform error | **PASS** | `cabinet_api.go:354-402`: per-IP 10/15min, per-account 5/15min, uniform `401 bad_credentials`; `TestCabinetLoginRateLimitedPerIP`, `TestCabinetLoginLockoutPerAccount`. |
| H3 | New distinct key refused (409) while a create is pending/sent/unknown | **PASS** | H3 count in `operations.go:211-219`; `ErrCreateInProgress`→`409 create_in_progress` (`cabinet_beget.go:322-323`); `TestCreateDistinctKeyBlockedWhilePending`, `TestCabinetVpsBlockedWhileOperationPending`. |
| H4 | Cross-user `idem_key` cannot return another user's op | **PASS** | Owner check in pre-read and in the `created=false` branch (`provision.go:120-124,146-151`); `TestCreateCrossUserIdemConflict`, `TestCabinetVpsCrossUserIdemNotFound`. |
| M1 (sec) | CSRF session-bound, `__Host-` prefix | **PASS** | Token = `sha256("sharedd-csrf:"+sessionToken)`, cookie `__Host-sharedd_csrf`, verification uses the HttpOnly session token only (`cabinet_api.go:32,106-145`); `TestCabinetCSRFIgnoresTossedCookie`. |
| M2 (sec) | Uniform login error; no disabled oracle | **PASS** | Disabled folded to `401 bad_credentials` (`cabinet_api.go:379-388`); `TestCabinetLoginUniformErrorForDisabled`. |
| M3 (sec) | `nodeIDForIdem` entropy | **PASS** | 44-bit derived id `vps<6hex>-<5hex>`, valid `ValidateNodeID` (`cabinet_beget.go:218-222`); `TestNodeIDForIdemValidAndDistinct`. |
| M4 (sec) | `master.key` permission check on load | **PASS** | Group/other bits rejected (`cryptoutil.go:39-41`); `TestLoadMasterKeyPermissions`. Key rotation (M4b/c) explicitly deferred — acceptable. |
| M6 (sec) | Bounded rate-limiter map | **PASS** | `pruneLocked` hard-caps at `rateLimiterMaxKeys` (`cabinet_api.go:177-222`); `TestIPRateLimiterMapBounded`. |
| M5 (sec) | `panelAuthorized` returns `true` on empty token | **OPEN (unchanged, MEDIUM)** | `panel.go:37-46` still `return true` on empty token. Not in the fix-round mandate; still latent, not a blocker. |

## R1 — HIGH (residual): cancel during an in-flight paid call frees the reservation

- Files: `server/cabinet_beget.go:349-383` (cancel), `provision/provision.go:132-165`
  (commit-then-call), `control/operations.go:102-225` (reservation), `cabinet_api.go:371-375`.
- Mechanism:
  1. Request A `POST /api/me/vps` key K1: `ReserveCreateOperation` commits
     `operations(state=pending)` **and returns before the paid call**; `provision.Create`
     then performs `validateRegion` + `client.CreateVps` (real network round-trip, up to 30s).
     During this whole window the row is still `pending` — no `sent` transition is ever written
     (`OpSent` appears only as a const and in the cancel guard; grep confirms no writer).
  2. Request B `POST /api/me/operations/{A}/cancel`: reads `pending`, which is **not** in the
     refusal set (`OpSent/OpConfirmed/OpUnknown/OpDeleting/OpDeleted`), so it sets `state=failed`.
  3. Request C `POST /api/me/vps` key K2 (distinct): the quota union excludes `failed`, and H3
     counts only `pending/sent/unknown`, so K2 passes and also calls `CreateVps`.
  4. A's call returns → `settleCreate` confirms K1. **Result: two paid `CreateVps`, two confirmed
     ops / two `cloud_resources`, `max_nodes=1`.** `max_creates_per_day` is bypassable the same way.
- Constructible with a concurrency-capable client (create; cancel within the network window;
  create new key) — no special privilege. Impact is self-funded over-provisioning (the user pays
  from their own linked Beget account), so no cross-tenant or platform-cost exposure, but it
  defeats the quota/daily controls that C1/H1/H3 were meant to make airtight. Rated HIGH on the
  same abuse-surface axis as C1; not CRITICAL.
- Fix direction (not implemented): make the reservation transition to `sent` atomically *before*
  invoking `CreateVps` (e.g. CAS `pending→sent` on the single-connection tx, and refuse cancel
  unless the row is still `pending` **and** not owned by an in-flight call — or hold a per-user
  in-flight guard / row lock across the call). Cancel must be refused once the paid call may be
  in flight.

## Validation note

`go test -race -run 'TestCreate|TestReserve' ./internal/provision/ ./internal/control/` passes.
No added test covers cancel-while-in-flight; `TestCabinetCancelRefusedAfterSent` only covers
cancel *after* confirmation. A regression test for R1 would need to cancel a `pending` op while
its `CreateVps` is blocked and then assert a second distinct-key create is refused.
