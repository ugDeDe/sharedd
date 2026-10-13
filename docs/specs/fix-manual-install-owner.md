# Fix: manual-install identity collision after SQLite host ID reuse

## Scope and evidence

Development checkout only; existing dirty work is preserved. No production
changes, SSH, agent execution, install steps, paid API calls, commits or pushes.
No tests are added or run, as explicitly requested.

Read-only production evidence supplied by the operator:

- `manual_hosts`: id=1, user_id=4, created_at=1791904950,
  node_id='', operation_id=0 (a recreated record).
- `operations`: id=1, user_id=4, node_id='vps4cfeae-8e2ee',
  kind='manual_install', state='failed', profile_id=2,
  idem_key='manual-1', created_at=1791904385.
- `node_owners` for that node: user_id=1 (legacy system user), legacy=1,
  registered_at=1791904385, node_secret_hash length=64,
  node_secret_enc length=96. No reservation or live claimant; user 4 quota=3.
- Quota reservations belong only to the legacy user. The specific node must
  have **no** reservation for the recovery branch; a reservation still attached
  to it is conflicting evidence, not permission to transfer it.

The old owner and operation precede the current host by 565 seconds. The reclaim
constraint correctly refuses them: they do **not** belong to this host lifetime.
Do not weaken the lifetime check or transfer the old legacy owner.

## Exact source cause and limits of the diagnosis

SQLite `INTEGER PRIMARY KEY` can reuse a deleted host ID. The former
`handleMeServerInstall` derived `manual-{host.ID}` and its node ID,
then called `AuthorizeNode` **before** retry preparation. `ReleaseNode` retains
the owner row and secret but sets user_id to the system legacy user, legacy=1,
and deletes its reservation. `AuthorizeNode` correctly refuses that different
user with `ErrOwnerMismatch`. The handler incorrectly classified that sentinel
as `writeControlUnavailable` / 503 rather than 403 `owner_mismatch`. Generic
authorization was correct. Operation-scoped recovery alone cannot solve this
case: `manual-1` collides with a deleted record's history. The fix is immutable
record-generation identity, not broader recovery permission.

## Immutable identity and migration

- `Open` uses `ensureColumn` to add `manual_hosts.install_key TEXT NOT NULL
  DEFAULT ''`, then fills **only empty** keys with 32 bytes from `crypto/rand`,
  hex-encoded. Backfilled keys have a `legacy-` marker; this is compatibility
  metadata, not a user-supplied value. Filled keys never change on later opens.
  Partial migration failure fails startup; a later open completes empty rows
  without changing already-filled identities. No operation/owner/secret is edited.
- Every `CreateManualHost` INSERT generates a fresh unmarked 256-bit key,
  ignoring any supplied key. This separates records even when SQLite reuses the
  same ID and both creations occur in the same Unix second. No timestamp-only
  generation scheme or AUTOINCREMENT workaround is used.
- New operation idem is `manual-{host.ID}-{host.InstallKey}`; its node ID uses
  the existing SHA-256-derived `vpsXXXXXX-XXXXX` format. Hash collisions fail
  closed against existing claimants; they never grant takeover permission.
- Resolution prefers explicit `operation_id`, then the generation idem. Only
  migrated records can fall back to unlinked legacy `manual-{id}` operations.
  Explicit/legacy compatibility requires the same user, manual kind, no provider
  VPS, operation time at/after host creation, deterministic node/key agreement
  and consistent host node linkage. Invalid explicit links fail closed.
- With empty linkage and host.created_at > legacy op.created_at, that legacy
  operation is history: resolve no current operation and create a new generation
  operation/node. Operation 1 and owner `vps4cfeae-8e2ee` remain untouched.
- Legacy records have no generation evidence for same-second historical reuse;
  timestamps cannot retroactively disambiguate that old data. All post-fix INSERTs
  are protected by fresh keys and never use unlinked legacy fallback.
- The resolver is repeated under the acceptance transaction's write lock; the
  full host snapshot (including install_key), expected operation key/node/state,
  reclaim provenance, live claimants, quota and encrypted token checks remain.

The old write sequence was owner/reservation -> CreateOperation -> provisionVars
-> ValidateExtraVars -> SSH credential preparation -> MarkInstallingIfActive
-> SetManualHostNode -> worker. Early setup errors could leak a slot and leave
no host linkage. A crash between MarkInstallingIfActive and SetManualHostNode
can leave an installing operation with empty host links. On restart,
`resumeInstallingOps` passed manual operations to `resumeInstallOp`, whose
provider_vps_id check fails (manual operations deliberately have none), and
marked them failed. This explains a concrete source path to failed + empty
links, but **does not prove** which original failure/release occurred on prod.
A normal InstallProfile step failure happens after linkage and alone cannot
explain empty links. Original logs are required to establish that history.

## Safe reclaim and atomic accept

`control.ReauthorizeManualOperation` uses a SQLite transaction, acquiring a
write lock before reading the authoritative evidence. It checks:

1. The saved host still equals the snapshot whose credentials were validated,
   belongs to the session user, and is not the system user's host.
2. For retry: exact operation ID, user, kind=manual_install, generation key
   (or validated compatible legacy key), deterministic node ID, selected persisted
   profile and empty provider_vps_id.
   Host links must be empty or already point to this same node/operation.
3. The operation state has not changed since preparation and is failed or an
   orphaned installing operation. A running worker is rejected by the handler.
4. No other manual host claims the node or same host:port, no other operation
   claims the node, and no nondeleted cloud resource claims the node.
5. Existing active binding belongs to the same user and is not deleting.
   Legacy recovery is allowed **only** with an existing failed operation,
   system legacy owner, not deleting, recoverable nonempty secret/hash,
   no reservation, matching release provenance (when recorded), and owner
   creation within the saved host/operation lifetime.
6. Old releases have no provenance column. Their narrow compatibility branch
   uses the exact persisted failed-operation identity and the above evidence;
   it is not an invitation to claim arbitrary legacy/system nodes. New releases
   record `released_user_id`; repeated release preserves the original value.
7. Current database user is enabled. Quota uses distinct identities across
   nonlegacy owners, nondeleted cloud resources and nonfailed/nondeleted create
   operations, so a slot is counted exactly once. A conflicting reservation is
   refused, never replaced. Any error rolls back all writes.
8. The encrypted token prepared before acceptance must still match the stored
   ciphertext and hash inside the transaction. Recoverable tokens are reused
   unchanged; existing hashes without a recoverable copy fail closed. Only a
   new binding or same-user binding with no issued hash gets a new token.

The registry checks its candidates for the same node or host IP. A released
node present in candidates is refused regardless of apparent health; a same-ID
active own node must match the saved host IP. `nodeClaimMu` serializes acceptance
through worker dispatch with `/register` authorization through candidate insertion,
including secret issuance. Lock order is nodeClaimMu -> state mu, not the reverse.
SQLite checks handle other store writers. Deployment assumes the existing single
registry process owns the state file; this lock is not distributed across replicas.

Profile vars, defaults, protected vars, injected-var limits, saved host address,
port/user and SSH credentials are validated **before** acceptance. Secret
generation/encryption is in memory until the transaction commits. No prestart
validation or failed transaction can leave a new owner or slot behind. Once
accepted, a worker failure retains its one slot for retry, as before. Restart
marks interrupted manual work failed/retryable without auto-running SSH steps;
it no longer treats it as a provider operation. Host linkage is already durable.

## Error contract and diagnostics

- Owner/evidence/live-claim mismatch: 403 `owner_mismatch`.
- Quota: 403 `quota_exceeded`; disabled user: 403 `user_disabled`.
- State/worker conflict: 409 `in_progress`; missing saved host: 404 `not_found`.
- Invalid vars/host: 400 `validation_failed`; bad SSH secret: 400 `ssh_unavailable`.
- SQL or secret preparation failure: 500 `internal`, generic client message.
- Missing master key: existing 503 `secrets_unavailable`.
- Missing store: existing session guard 503 `control_unavailable`, with real
  initialization diagnostics. Install-handler/store errors no longer call
  writeControlUnavailable. Host lookup and profile selection also log their real
  errors. Diagnostics reuse quote redaction, control-character stripping,
  bounded messages and SQLite error codes; never log credentials or vars.

## Compile-only verification

Run from the repository root (Go commands execute in `registry/`; there is no
root Go module). No `go test`, race run, integration/E2E check, or live execution:

```bash
gofmt -w registry/internal/control/control.go registry/internal/control/manual_hosts.go registry/internal/control/manual_install.go registry/internal/server/cabinet_manual.go
(cd registry && go vet ./internal/control ./internal/server)
(cd registry && go build -mod=readonly ./internal/control ./internal/server)
(cd registry && go build -mod=readonly -o /tmp/opencode/sharedd-registry-owner-fix .)
```

Verification: **partially_verified**. Targeted gofmt, vet, package build and
registry binary build passed. Compile-only artifact:
`/tmp/opencode/sharedd-registry-owner-fix` (not executed or deployed).
Runtime, transactional and concurrency behavior remain untested by user
instruction; an independent reviewer agent is unavailable in this session.

## Leader deployment and recovery handoff (not executed)

1. Review the focused patch and the existing dirty feature work separately.
   The registry's new control/provision packages are untracked in this checkout;
   do not package only this fix while omitting their required base implementation.
2. Build the registry with the approved release process/version. Independently
   verify artifact identity/version before publishing. Do not change agents,
   SSH installers, provider credentials or node secrets as part of this fix.
3. In an approved deployment window, back up control.db consistently (SQLite
   backup, or stopped service including WAL/SHM), registry state/config and the
   existing master.key. Preserve master.key exactly: recovery requires it.
4. Replace **only** the approved registry artifact and restart under operator
   control. Open adds/backfills install_key (and retains the prior additive
   released_user_id migration); no destructive schema rewrite or manual owner
   UPDATE is needed. Review migration/startup logs.
5. Reconfirm the read-only evidence above, available user-4 quota and no
   live claimant. Do not delete/recreate host 1, release another node, rotate a
   secret, or use the paid VPS-create/retry endpoint to bypass a refusal.
6. With explicit operator authorization, user 4 may retry **the saved host 1**
   via POST /api/me/servers/1/install with valid session/CSRF and appropriate
   vars. This action WILL start the normal manual SSH worker; it is deliberately
   not performed by this agent. This is a fresh operation: normal request/default
   profile selection applies, rather than inheriting stale operation 1's profile.
7. Check that host 1 links to a **new** operation with its generation idem and
   a new node, owner user 4/legacy=0 and exactly one user-4 reservation. Operation
   1, its legacy owner and secret remain unchanged. Repeated installs reuse the
   new operation/node (running/done state rules still apply). Follow registration
   status; done still requires registry confirmation. No provider purchase occurs.
8. If 403 is returned, investigate failed evidence/quota/live claimant rather
   than forcing ownership. If 500, use the exact manual.install.* operation tag
   and SQLite code in the redacted logs; do not diagnose a restart from HTTP alone.
9. Rollback, if required, restores the prior approved registry artifact. The
   additive column is compatible with the prior implementation; do not restore
   an old DB over newer operation changes without a separate recovery decision.

## Source-reviewed delete/recreate flow (not executed)

`handleMeServerDelete` deletes only the host row and releases its linked node
when present; it does not delete operations. `CreateManualHost` subsequently
assigns a fresh key regardless of PK/time reuse. A new unlinked record cannot
fall back to `manual-{id}`. Its generation idem selects a new operation/node;
atomic acceptance persists linkage. Repeats resolve that explicit linkage and
reuse the same identity/profile. A host deleted/recreated during preparation
fails snapshot equality inside acceptance because install_key differs. No
provider client is introduced: the handler still calls `provision.New(..., nil,
...)` and only dispatches the existing manual SSH worker on an accepted request.
