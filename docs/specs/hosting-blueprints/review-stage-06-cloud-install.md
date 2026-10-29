# Review — Stage 06 cloud install

Read-only review of `registry/internal/control/cloud_install.go` (+test),
`registry/internal/server/cabinet_cloud.go` (install route + helpers),
`cabinet_cloud_install_test.go`, against `03-cabinet-api.md` §2/§4, §2.1,
`04-storage-and-execution.md` §5, and the stage-05 precedents.

## Verdict

**BLOCK** — 1 HIGH (concurrency: install vs delete/change-ip not mutually
excluded). Everything else is verified or a documented deviation.
Downgrade condition: if the stage-05 **L3** envelope ("resource-mutation
exclusion deferred") is taken to cover install too, F1 falls to MEDIUM and the
verdict is **WARNING**; the spec text currently promises the opposite
(`03 §2.1`: «один активный mutation на resource»).

| Severity | Count | Status |
|---|---|---|
| CRITICAL | 0 | pass |
| HIGH     | 1 | block |
| MEDIUM   | 0 | — |
| LOW/INFO | 2 | info |

## NEW findings

### F1 — HIGH — install does not exclude delete / change-ip on the same resource

Evidence:

- `cloud_resources2` has no `installing` member
  (`control/cloud_resources.go:22-27`), and `AuthorizeCloudInstall`
  (`control/cloud_install.go:32-156`) never writes `cloud_resources2.state` —
  the resource stays `ready` for the whole install.
- `handleCloudInstall` serializes only on `nodeClaimMu`
  (`server/cabinet_cloud.go:628`), released as soon as
  `provisionWorkers().run` starts the goroutine.
- `handleCloudDelete` (`cabinet_cloud.go:415-468`) and `handleCloudChangeIP`
  (`472-582`) take neither `nodeClaimMu` nor `cloudBusy`; both gate only on
  `state==ready` (delete also allows `error`), and their workers CAS
  `ready→deleting` (`874-880`) / `ready→ready` (`576`).
- `cloudBusy` is per-resource but only exists for change-ip
  (`run_main_impl.go:111-114`); install never touches it.

Consequence: while an install worker is SSHing to the VPS, a DELETE on the same
resource still passes its `ready` guard and the provider `Delete` runs under the
install; a change-ip can repoint the IP the install captured in `res.IP` at
handler time. Because the state machine cannot express "install in flight",
neither command can observe it.

Minimal fix (existing primitives, no new state): make the per-resource guard
shared. Either

- (a) verify `runningOp(<install op>)` inside `handleCloudDelete` /
  `handleCloudChangeIP` (`OperationByIdemKey(control.CloudInstallIdem(res.ID))`
  → `provisionWorkers().runningOp(op.ID)` → `409 resource_busy`), and
  symmetrically reject install while a `cloud_delete` op is non-terminal; or
- (b) gate install/delete/change-ip on one `cloudBusy`-style map keyed by
  `res.ID`.

Design fix for a later stage: add `installing` to the resource state set and
transition `ready→installing→ready|error` in the install worker, so the existing
`state==ready` guards do the exclusion (docs already list `installing` in
`04 §5.1`).

Acceptance tests (fail today, pass after fix):

- `TestCloudInstallVsDeleteBusy`: block the install worker via the fake
  executor; `DELETE` the same resource with `X-Confirm-Action: delete` +
  `Idempotency-Key` → expect `409 resource_busy`, resource still `ready`,
  provider `Delete` not called; release → install reaches `done`.
- `TestCloudInstallVsChangeIPBusy`: same shape → `409 resource_busy`, provider
  `ChangeIP` not called.

### F2 — LOW — install trusts the denormalized `resource.Provider`, not the connection

`handleCloudInstall` gates beget on `res.Provider != nodehost`
(`cabinet_cloud.go:602`) and never loads the owning connection
(`HostingConnectionOwned`) nor a provider client. A resource whose connection
was deleted, or whose row disagrees with its connection, still installs. Impact
is low (ownership is enforced by `cloudOwned`, and install only needs the sealed
password + host), so no fix is required at this stage — flagging for symmetry
with create/delete/change-ip, which do load the connection.

### F3 — INFO — `Idempotency-Key` required but the value is unused

`handleCloudInstall` calls `cloudIdemKey` and discards it (`cabinet_cloud.go:594`);
dedup is resource-scoped (`cloud_install:<id>`). Two distinct keys while idle
both drive a reinstall; no canonical body hash is stored. This matches
`implementation-stage-06 §3` (documented), so it is a deviation, not a defect.

## Verified (matches spec / prior patterns)

- **Ownership 404.** `cloudOwned` → `CloudResourceOwned` → `ErrNotFound` → 404;
  foreign `{id}` pinned by `TestCloudInstallForeignAndBeget`.
- **CSRF.** Install is mounted inside `requireCSRF` (`cabinet_cloud.go:226`);
  `requireMasterKey` is checked first (`589`).
- **Idempotency-Key required.** `cloudIdemKey` → 400
  `idempotency_key_required`; pinned. **Replay no second worker:** op lookup +
  `runningOp(op.ID)` returns 409 `in_progress` before any second `run`
  (`642-645`); pinned by `TestCloudInstallReplayNoSecondWorker` (exactly one
  `cloud_install` op). Same/different key both serialized by `nodeClaimMu`.
- **Resource state guard.** `state != ready` → 409 `not_ready` (`606-609`);
  pinned. `SetCloudResourceNode` CAS is `... WHERE state='ready'`, so a
  delete-that-won returns `ErrRevisionConflict` and `Confirm` fails rather than
  binding a dead node (`cloud_install.go:161-184`).
- **Sealed SSH password.** Decrypted only in `cloudInstallPassword`
  (`cabinet_cloud.go:731-746`) and used only as `sshx.Auth{Password}` +
  `Secrets: []string{password}` (masked by `provision.applyProfile`/`MaskSecrets`,
  `provision.go:437-477`). Not logged (`writeCloudInstallErr` logs only sentinel
  errors; no `%v` of password anywhere) and not in any DTO
  (`SecretsEnc json:"-"`, `cloud_resources.go:48`; `operationJSON` has no secret
  field). Pinned by `TestCloudInstallLifecycle` (secret in steps, absent from
  response).
- **Pending secret + quota == AuthorizeManualOperation semantics.** Byte-for-byte
  parallel to `AuthorizeManualOperation`: write-lock via self-UPDATE, owner
  `ErrNotFound`, `disabled→ErrUserDisabled`, quota counts
  `node_owners + pending WHERE operation_id<>op.ID` (no double count on retry),
  reinstall clears `quota_reservations`/`node_owners` and re-reserves pending
  (`cloud_install.go:107-149`). Pinned by
  `TestAuthorizeCloudInstallQuotaAndOwnerAndState`.
- **Claim via generic helper, no `/register` edits.** `ClaimPendingNodeSecret`
  matches by `secret_hash`, sets `operations.node_id` by `operation_id`, and its
  `manual_hosts` update is a no-op for cloud (`manual_install.go:269-274`).
  `users_control.go` and `manual_install.go` are unmodified (git status).
- **Worker guard keyed by `op.ID`.** `run(op.ID, …)`; `run=false` →
  `UpdateOperationState(failed,"worker_busy")` + 409 (`722-724`); panic recovered
  by `defer r.cloudRecover(op.ID)` (`702`). Matches stage-05 H1/L2.
- **Confirm requires non-empty `node_id`.** `got.NodeID == ""` →
  `"node did not register"` → op `failed`, no false `done`
  (`cloud_install.go:710-719`).
- **Label validation.** Non-empty, no spaces (`610-613`); authoritative
  `label+"-"` prefix stays in the claim — documented deviation.
- **Profile errors mapped.** `writeProfileSelectErr`: 404 `not_found`,
  403 `user_profiles_disabled`, else 400 `profile_unavailable` (`663-667`).
- **beget 501 documented** and pinned.
- **No secret leakage in logs.** `logProfileControlErr` receives only selection
  errors; no password path logs.

## Test evidence

```
$ go test -race -count=2 -run 'TestCloudInstall|TestAuthorizeCloudInstall|TestCloudResourceNode' ./internal/server ./internal/control
ok  sharedd/registry/internal/server   12.7s
ok  sharedd/registry/internal/control   5.9s
```

- The given regex does **not** match `TestSetCloudResourceNodeCAS` (name is
  `TestSetCloudResourceNodeCAS`, not `TestCloudResourceNode`); run separately:
  `go test -race -count=2 -run 'TestSetCloudResourceNodeCAS' ./internal/control`
  → `ok`.
- Verbose: server `TestCloudInstallLifecycle` / `ReplayNoSecondWorker` /
  `Guards` / `ForeignAndBeget` and control `TestAuthorizeCloudInstallHappy` /
  `QuotaAndOwnerAndState` all PASS.

## Documented deviations confirmed (not findings)

beget 501; in-process worker (no resume-on-boot, resource stays `ready`);
root-password-only SSH; label-prefix deferred to claim; graph mode 422 deferred;
resource-scoped idempotency (no body hash, replay-after-`done` = reinstall);
no route-level `confirm`. `implementation-stage-06-cloud-install.md` §3 matches
the code. Reinstall clearing `node_owners`/quota before the worker runs is the
same semantics as the manual path (`manual_install.go:120-131`), not a new delta.

---

# Follow-up review — F1 closure (re-review)

Scope: `implementation-stage-06-cloud-install.md` §7, `cabinet_cloud.go`
(shared guard + install/delete/change-ip), `run_main_impl.go` (comment),
`cabinet_cloud_install_test.go`. Original review above is preserved.

## Verdict

**PASS** — F1 is closed. The shared per-resource guard does exclude
install/delete/change-ip on the same resource; every path releases it; no
self-deadlock on install replay; existing replay semantics preserved.

| Severity | Count | Status |
|---|---|---|
| CRITICAL | 0 | pass |
| HIGH     | 0 | pass (F1 closed) |
| MEDIUM   | 0 | — |
| LOW/INFO | 2 | info (F2/F3 unchanged) |

## F1 closure evidence

- **Shared primitive.** `cloudGuardAcquire`/`cloudGuardRelease`
  (`cabinet_cloud.go:488-505`) is the single `cloudMu`/`cloudBusy` map keyed by
  `res.ID` (`run_main_impl.go:111-115`). install (`689`), delete (`459`), and
  change-ip (`568`) all use it.
- **Mutual exclusion, both directions.**
  - install holds → delete `459` returns `409 resource_busy` before
    `CreateOperation`/provider; change-ip `568` marks the op failed and returns
    `409` before `cloudProviderFor`/`ChangeIP`; install `689` returns `409` and
    creates no operation.
  - Pinned by `TestCloudInstallVsDeleteBusy` (provider `Delete` count 0, resource
    stays `ready`), `TestCloudInstallVsChangeIPBusy` (`ChangeIP` count 0),
    `TestCloudInstallRejectedWhileGuardHeld` (no `cloud_install` op).
- **Release on all paths, single release.**
  - install: `guarded` flag + deferred release (`693-698`) covers every early
    return (profile/vars/secret/accept); success sets `guarded=false` (`768`) and
    the worker releases via `defer r.cloudGuardRelease(resourceID)` (`744`);
    `run=false` keeps `guarded=true` → handler releases (`764-766`). Panic runs
    both worker defers (guard release then `cloudRecover`).
  - delete: `guarded` + defer (`463-468`); success hands off (`478`) and the
    worker releases (`829`), or `launchCloudDelete` releases on `worker_busy`
    (`833`).
  - change-ip: synchronous `defer` (`573`).
- **No self-deadlock on install replay.** The `runningOp(op.ID)` check
  (`668-671`) returns `409 in_progress` *before* `cloudGuardAcquire` (`689`), and
  the resource-scoped replay never acquires its own guard. `provisionWorkers.run`
  is non-blocking (`provision_worker.go:46-65`), so the handler returns and
  releases `nodeClaimMu` before the worker (or the agent's later `/register`)
  needs it — no lock-order cycle (`nodeClaimMu → cloudMu` only).
- **Replay semantics preserved.** Same install key while running → `in_progress`
  409 (unchanged); replay after `done` → reinstall (unchanged); delete/change-ip
  idem-key replays short-circuit (`451`, `546`) without touching the guard.

## Residual (documented, not blocking)

- Guard is in-process and not crash-durable; a registry restart drops it (§3/§7;
  durable `installing` member deferred). Same class as the stage-05 worker stance.
- change-ip marks a failed `cloud_change_ip` op when it loses the guard race —
  observable but harmless and consistent with its existing failure path.
- F2 (LOW) and F3 (INFO) from the original review remain open, unchanged.

## Test evidence

```
$ go vet ./...                                             # clean
$ go test -race -count=2 -run 'TestCloud' ./internal/server
ok  sharedd/registry/internal/server   72.261s
```
