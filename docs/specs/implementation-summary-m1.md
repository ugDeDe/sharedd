# implementation-summary-m1.md — Milestone 1: users / authkey / quotas / machine auth / panel admin APIs

Branch: `dev`. Contract: `docs/api-contract.md`, behavior: `docs/specs/accounts-quota.md`.
Scope honored: M1 only. DC2 health spec, multi-master DNS, Beget, UI — untouched.

## Files changed

| File | Change |
|---|---|
| `registry/internal/control/control.go` | **new** — control store: `Open` with embedded schema (`CREATE TABLE IF NOT EXISTS`, all 5 tables exactly per contract §1), sentinel errors, `legacy` system user creation, single-connection SQLite (WAL + busy_timeout, like `internal/history`) |
| `registry/internal/control/users.go` | **new** — create/list/get/patch users, bcrypt (cost 10) password hashing, username `[a-z0-9_.-]{3,32}`, password ≥ 10, `max_nodes` 0..10000, last-active-admin guard, legacy-user protection |
| `registry/internal/control/authkeys.go` | **new** — `sda_…` keys: sha256 hash + 8-char prefix stored, full key shown once; rotate revokes ALL prior keys; revoke; lookup of live key → user |
| `registry/internal/control/nodes.go` | **new** — `AuthorizeNode` (transactional owner bind + `quota_reservations` INSERT + COUNT inside tx; own node = 200 without consuming a slot; foreign = `ErrOwnerMismatch`; over quota = rollback + `ErrQuotaExceeded`), `ReleaseNode` (owner → legacy, reservation deleted, row kept forever), `BindLegacyNode` (idempotent migration) |
| `registry/internal/control/sessions.go` | **new** — session token hashing/expiry/purge (foundation for later user-UI; M1 admin APIs run under panel token) |
| `registry/internal/control/control_test.go` | **new** — 16 tests: schema/idempotent reopen, validation matrix, duplicate, bcrypt verify, last-admin, legacy protection, rotate-revokes-old, revoke, owner-bind-once, owner-mismatch, quota-exceeded, **8 concurrent binders → exactly 1 winner**, release-frees-slot, legacy rows don't count toward quota, owners survive reopen, sessions |
| `registry/internal/config/config.go` | `[users]` section (`enabled`, `legacy_max_nodes` default 100000), `[database] control_db` (default `control.db` next to state file), validation: users.enabled ⇒ legacy_max_nodes 1..1M, panel enabled, control_db non-empty; users disabled ⇒ zero behavioral change |
| `registry/internal/machineapi/security.go` | `RegisterRequest.AuthKey` (`json:"authkey"`) body field; wire contract shared with the agent |
| `registry/internal/server/users_control.go` | **new** — `openControl` (fail-closed: error logged once, store stays nil), `migrateLegacyOwners` (all pre-existing candidates → legacy user), `authorizeUserRegister` gate, contract §5 error writer |
| `registry/internal/server/users_admin_api.go` | **new** — six `/panel/api/admin/*` endpoints behind existing `panelAuthorized`; `users_enabled=false → 503 users_disabled`; broken control DB → `503 control_unavailable` |
| `registry/internal/server/run_main_impl.go` | `Registry.control` field; `Run()`: `openControl()` + `migrateLegacyOwners()` after state load, `closeControl()` on shutdown; `/register` gate inserted after terminate-check (dead nodes never consume quota); `mountAdminUsers` in `buildMux` |
| `registry/internal/server/users_api_test.go` | **new** — 14 HTTP tests covering the mandatory accounts-quota.md list (below) |
| `registry/go.mod`, `go.sum` | `golang.org/x/crypto` promoted to direct dep (bcrypt); transitive bumps via `go mod tidy` |

## Endpoint examples

### Machine API — `POST /register` (users enabled)

```jsonc
// request (Bearer node_token still the first barrier)
{ "node_id": "myname-a1b2c", "ip": "203.0.113.7", "authkey": "sda_<64 hex>", "node_type": "classic" }
```

| Case | Status / body |
|---|---|
| valid key, free slot | `200` (empty body, as before) |
| no/unknown/revoked key | `403 {"error":"bad_authkey","message":"…"}` |
| key of disabled user | `403 {"error":"user_disabled"}` |
| foreign key on owned node | `403 {"error":"owner_mismatch"}` |
| quota exhausted | `403 {"error":"quota_exceeded"}` |
| control DB failed to open | `503 {"error":"control_unavailable"}` |
| legacy node re-registering without authkey | `200` (pre-enablement nodes keep working) |
| users.enabled=false | exact current behavior — no gate at all |

### Panel admin API (Bearer `panel.token`)

```jsonc
POST   /panel/api/admin/users                          {"username":"alice","password":"…","role":"user","max_nodes":5}
       → 200 {"id":2,"username":"alice","role":"user","max_nodes":5}
       → 400 validation / 409 {"error":"duplicate"}
GET    /panel/api/admin/users                          → {"users":[{id,username,role,max_nodes,used_nodes,disabled},…]}
PATCH  /panel/api/admin/users/{id}                     {"max_nodes":9} | {"disabled":true} | {"password":"…"} → 200 updated object
POST   /panel/api/admin/users/{id}/authkey/rotate      → 200 {"authkey":"sda_…"}   // full key, shown once
DELETE /panel/api/admin/users/{id}/authkey             → 204 (all user keys revoked)
POST   /panel/api/admin/users/{id}/release-node        {"node_id":"myname-a1b2c"} → 200 {"released":true} | 404
```

Guards: 401 without/with wrong Bearer; 503 `users_disabled` when `[users] enabled=false`; 503 `control_unavailable` when the DB failed to open while enabled.

## Migration behavior

- Control DB schema is created inside `control.Open` (`CREATE TABLE IF NOT EXISTS`) — no external migrations. Reopening is idempotent (pinned by `TestOpenIsIdempotent`).
- On first start with `users.enabled=true`, every node already present in the JSON state is bound to the system user `legacy` (role `user`, `max_nodes=[users] legacy_max_nodes`, cannot be disabled via API) with `legacy=1`. Legacy rows never count toward `used_nodes`.
- `node_owners` rows are permanent: prune/expiry/terminate never remove them; only admin `release-node` frees a slot (owner flips to `legacy`, reservation deleted, row kept).
- Restart: owners/keys/users live in SQLite → `TestOwnersSurviveRegistryRestart` / `TestOwnersSurviveReopen`.
- Fail-closed: if the control DB cannot open, the pool keeps running (legacy registrations still accepted via the shared token are **not** possible in enabled mode — all `/register` in enabled mode answers 503; see deviation note), and admin user APIs answer 503. With `users.enabled=false` the DB is never opened and behavior is byte-for-byte the old one.

## Test status — mandatory list from accounts-quota.md

| Required behavior | Test(s) | Status |
|---|---|---|
| two concurrent `/register`, one free slot → exactly one 200 | `TestRegisterConcurrentOneSlotExactlyOneWinner` (HTTP, 8 goroutines), `TestAuthorizeNodeConcurrentOneSlot` (store level) | ✅ pass |
| foreign authkey | `TestRegisterForeignAuthkeyOwnerMismatch`, `TestRegisterUnknownAndRevokedAuthkey` | ✅ pass |
| re-register own node (no slot consumed) | `TestRegisterOwnNodeAgainNoSlot`, `TestAuthorizeNodeBindsOwnerOnce` | ✅ pass |
| re-register foreign node → owner_mismatch | `TestRegisterForeignAuthkeyOwnerMismatch`, `TestAuthorizeNodeOwnerMismatch` | ✅ pass |
| max_nodes down → new registrations rejected | `TestMaxNodesDownBlocksNewRegistrations` (heartbeat of existing node stays 200), `TestAuthorizeNodeQuotaExceeded` | ✅ pass |
| release-node → slot freed | `TestReleaseNodeFreesSlotViaAPI`, `TestReleaseNodeFreesSlot` | ✅ pass |
| registry restart preserves owners | `TestOwnersSurviveRegistryRestart`, `TestOwnersSurviveReopen` | ✅ pass |
| fail-closed: broken control.db → 503 enabled / legacy works when disabled | `TestFailClosedRegisterWhenControlUnavailable` | ✅ pass |

Plus: legacy migration (`TestLegacyMigrationBindsExistingCandidates`), disabled-user nodes keep heartbeating (`TestDisabledUserKeepsNodesButCannotRegister`), admin CRUD/auth-bearer/503 matrix, 16 control-store tests.

## Verification

```
cd registry
gofmt -l .                      → clean
go vet ./...                    → clean
go test -race ./...             → 198 tests passed, 9 packages
go build -mod=readonly ./...    → ok
golangci-lint run --timeout 5m ./...  → 0 issues
```

Note: the preinstalled `golangci-lint` binary was built with go1.26 and panicked against the go1.27 toolchain (`file requires newer Go version`). Rebuilt from source as CI does (`go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.1`) — 0 issues.

## Deviations / notes

1. **Authkey gate placement**: authorization runs after the terminate/reverify check, so terminally-blocked nodes never consume a quota slot. Conversely, a brand-new node that hits an old `ip_ban` reverify path may consume a slot before being quarantined — admin frees it via `release-node`. Accepted trade-off, documented here.
2. **Create-user defaults**: empty `role` defaults to `user` (contract lists `role?` as optional; strict validation would make the duplicate-username path unreachable). `max_nodes` defaults to 0.
3. **release-node ownership check**: releases only nodes bound to the `{id}` in the path (foreign/unknown → 404). Contract doesn't specify; strictest reading chosen.
4. **Fail-closed for legacy re-registrations in enabled mode**: when the control DB is down while users are enabled, even legacy-bound re-registrations get 503 (owner lookup is impossible without the DB). Contract §1 wording ("любая регистрация, требующая владельца, отвечают 503") supports the fail-closed reading; the mandatory spec test pins exactly this behavior.
5. **`legacy_max_nodes` upper bound**: validation caps at 1,000,000 (contract gives no cap; the config default is 100000).
6. **No heartbeat/report/retire/config changes**: contract §3 explicitly keeps pool behavior unchanged; DC2 health-report fields are out of M1 scope.
7. **Sessions**: table + helpers implemented per contract §1, but no login endpoint in M1 (admin APIs run under `panel.token`).

Not committed — left as working-tree changes on `dev` as instructed.
