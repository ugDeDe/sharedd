# Stage 06 — Cloud install (create→install→pool) (as implemented)

Scope: install the node agent onto an **already-created** cloud resource,
completing the manual loop `create → install → pool`. This is the narrow
install slice of `03-cabinet-api.md` §4 (“install intent”) and the state/worker
model of `04-storage-and-execution.md` §5, reusing existing primitives. It adds
no new tables, no new dependencies, and no blueprint engine.

## 1. Files created / changed

Created:

| File | Role |
|---|---|
| `registry/internal/control/cloud_install.go` | Store layer: `AuthorizeCloudInstall` (atomic owner/state/quota/pending acceptance) + `SetCloudResourceNode` (CAS node binding after claim). |
| `registry/internal/control/cloud_install_test.go` | Unit tests: happy/retry reuse, quota, owner isolation, not-ready, node CAS. |
| `registry/internal/server/cabinet_cloud_install_test.go` | HTTP-level lifecycle/guards/isolation/leak/replay tests. |
| `docs/specs/hosting-blueprints/implementation-stage-06-cloud-install.md` | This document. |

Changed:

| File | Change |
|---|---|
| `registry/internal/server/cabinet_cloud.go` | New route `POST /api/me/cloud-resources/{id}/install`, `handleCloudInstall`, `cloudInstallPassword`, `writeCloudInstallErr`; imports `io`, `provision`, `sshx`. Shared per-resource mutation guard `cloudGuardAcquire`/`cloudGuardRelease` (F1). |
| `registry/internal/server/run_main_impl.go` | `cloudMu`/`cloudBusy` comment widened from change-ip-only to the shared install/delete/change-ip guard (no schema change). |

Not touched: `control.go` schema, `manual_install.go`, `provision/`, `nodehost`,
`/register`, `manual_hosts`, `cabinet_manual.go`, and all stage 01–05 files.

## 2. Route contract

`POST /api/me/cloud-resources/{id}/install` — session + CSRF, `Idempotency-Key`
required. Body `{profile_id?, vars?}`. Success `200` with the existing
`operationJSON` shape (`kind = "cloud_install"`).

| Condition | Result |
|---|---|
| Unknown/foreign `{id}` | 404 `not_found` |
| Missing `Idempotency-Key` | 400 `idempotency_key_required` |
| `state != ready` | 409 `not_ready` |
| No sealed root password in `secrets_enc` | 409 `ssh_unavailable` |
| Worker already running for this resource | 409 `in_progress` |
| `label` empty or contains spaces | 400 `validation_failed` |
| Beget connection | 501 `not_implemented` |
| Quota / disabled / identity conflict (from acceptance) | 403 `quota_exceeded` / `user_disabled` / `owner_mismatch` |
| Bad profile / vars | 400 `validation_failed`, `profile_unavailable`, or 404 `not_found` |

Nodehost install path (only provider accepted): decrypt the sealed
`{password}` from `secrets_enc`, `selectableProfileVersion` (text),
`MergeProfileVars` + `provisionBaseVars`, inject `vars["label"] = resource.Label`
and `vars["node_secret"] = secret`, `AuthorizeCloudInstall`, then run
`provision.New(r.control, nil, r.provisionConfig()).InstallProfile` on the shared
`provisionWorkers` pool keyed by **operation id** (the same namespace as the
beget/manual paths). `Config.Exec` is the already-injectable `r.sshExec`, so
tests supply a fake executor without new seams.

The operation is keyed by the resource: `idem_key = cloud_install:<resourceID>`
(`control.CloudInstallIdem`). `node_id` is **not** pre-created: the pending
secret is written by the acceptance transaction and claimed on first
`/register` (`ClaimPendingNodeSecret`, generic — it fills `operations.node_id`
and no-ops `manual_hosts`). `InstallProfile.Confirm` requires a non-empty
`operation.node_id`, then calls `SetCloudResourceNode(id, nodeID, ready)` to bind
the resource. A failed worker launch (key taken) marks the operation `failed`
with `worker_busy`; a worker panic is recovered by the existing
`cloudRecover` (`worker_panic`).

## 3. Agreed deviations

- **beget install → 501 `not_implemented`.** Only `nodehost` connections are
  installable here; beget keeps the legacy `/api/me/vps` + `/api/me/servers`
  flows. The two contours are not unified at this stage.
- **Worker is in-process, not crash-durable.** Install runs on the shared
  `provisionWorkers` pool keyed by operation id. A registry restart leaves the
  operation visibly `installing` (and the resource `ready`); there is no
  resume-on-boot. This matches the stage-05 worker stance and is surfaced
  honestly (no silent success).
- **Install requires the nodehost root password captured at create.** The
  sealed `{password}` written by `cloudCreateDo` is the only SSH credential;
  a resource with an empty `secrets_enc` is rejected 409 `ssh_unavailable`
  rather than guessing a key.
- **Label must match the VPS hostname prefix.** Claim-on-first-register requires
  `node_id` to start with `label + "-"` (`ClaimPendingNodeSecret`). The route
  validates only that the label is non-empty and space-free (400
  `validation_failed`); the authoritative substring check stays in the claim,
  so a label that does not become the hostname prefix makes the install fail at
  `Confirm` (operation `failed`, no resource `node_id`). The label is reused
  verbatim from the resource created in stage-05.
- **Graph profile mode still 422 / deferred.** `profile_versions.format` does
  not exist yet (stage-05 deviation), so `selectableProfileVersion` can only
  return text profiles; the install path performs no separate graph rejection.
  This is the same deferral recorded for `PUT .../management` (profile →
  422).
- **`Idempotency-Key` is required but deduplication is resource-scoped.** The
  03 §2.1 user-scoped key namespace is not replicated verbatim because the
  install operation is already unique per resource (`cloud_install:<resourceID>`).
  A replay while the worker is running returns 409 `in_progress` and starts no
  second worker; a fresh request after `done` re-runs the same operation
  (reinstall), matching the manual-install reinstall semantics. No canonical
  body-hash conflict is stored (the resource-scoped key makes a distinct-body
  conflict impossible to distinguish from reinstall).
- **No route-level `confirm:true`.** Install is a non-purchase command; the
  mandatory `Idempotency-Key` is the only contract gate (03 §2.1 lists confirm
  for purchase/delete commands).
- **Per-resource mutation exclusion is in-process, not state-backed.** The
  install/delete/change-ip guard is the shared `cloudMu`/`cloudBusy` map keyed
  by `resource id`; it is lost on registry restart, so it is not crash-durable.
  A durable `installing` member on `cloud_resources2` (transition
  `ready→installing→ready|error` in the install worker, letting the existing
  `state==ready` guards do the exclusion across restarts) is deferred to a
  later stage — `04 §5.1` already lists `installing` in the state set.

## 4. Test matrix (actual results, all green)

`cd registry && go test -race ./...` — all packages `ok` (incl.
`internal/control` 52.7s, `internal/server` 168.2s).

Control (`internal/control`):

| Test | What it pins |
|---|---|
| `TestAuthorizeCloudInstallHappy` | Acceptance yields a `cloud_install` operation in `installing` with empty `node_id`; lookup by `cloud_install:<id>`; pending secret stored with the resource label; retry reuses the same operation. |
| `TestAuthorizeCloudInstallQuotaAndOwnerAndState` | No-slot user → `ErrQuotaExceeded`; foreign user → `ErrNotFound`; planned resource → `ErrCreateInProgress`; empty secret → `ErrOwnerMismatch`. |
| `TestSetCloudResourceNodeCAS` | Node binding bumps revision; wrong expected state → `ErrRevisionConflict`; unknown id → `ErrNotFound`. |

Server (`internal/server`):

| Test | What it pins |
|---|---|
| `TestCloudInstallLifecycle` | Happy install with injected fake executor + emulated `/register` claim → operation `done`, `resource.node_id` set to `pool-node-*`; `node_secret` injected into steps and absent from the HTTP response. |
| `TestCloudInstallReplayNoSecondWorker` | Replay while the worker holds the operation → 409 `in_progress`; exactly one `cloud_install` operation. |
| `TestCloudInstallGuards` | not-ready → 409 `not_ready`; ready without sealed password → 409 `ssh_unavailable`; missing key → 400. |
| `TestCloudInstallForeignAndBeget` | Foreign resource → 404; beget ready resource → 501 `not_implemented`. |
| `TestCloudInstallVsDeleteBusy` | Install worker blocked on SSH; `DELETE` same resource → 409 `resource_busy`, resource stays `ready`, provider `Delete` not called (F1). |
| `TestCloudInstallVsChangeIPBusy` | Install worker blocked; `change-ip` same resource → 409 `resource_busy`, provider `ChangeIP` not called (F1). |
| `TestCloudInstallRejectedWhileGuardHeld` | Guard held (simulated in-flight delete/change-ip); install → 409 `resource_busy`, no `cloud_install` operation created (F1, reverse direction). |

## 5. Explicitly NOT done

- Blueprint / graph profile engine; `profile_versions.format`; graph-profile
  install (and its 422).
- UI for cloud install.
- Beget new install lifecycle under `/api/me/cloud-resources` (legacy flow only).
- Durable worker restart recovery (no resume-on-boot; `installing` stays visible).
- Route-level user-scoped idempotency/body-hash storage (see deviations).
- No pinned install snapshot on the resource: the profile is chosen per request
  and stored only as `operations.profile_id`.

## 6. Status

`partially_verified` — `go vet ./...`, `go build -mod=readonly ./...`,
`go test -race ./...` (all packages), `gofmt -l .` (empty) and
`git diff --check` are clean. `golangci-lint`/`staticcheck` are not installed in
this environment, so lint was not run.

## 7. Fixes after review (F1)

`review-stage-06-cloud-install.md` F1 (HIGH): install did not exclude
delete / change-ip on the same resource — `handleCloudDelete` and
`handleCloudChangeIP` gated only on `state==ready`, so a `DELETE` could destroy
the VPS mid-install and a `change-ip` could repoint the IP the install captured.

Fix (existing primitive — no new resource state): the change-ip-only
`cloudMu`/`cloudBusy` map became the **shared per-resource mutation guard**
(`cloudGuardAcquire`/`cloudGuardRelease`, keyed by `resource id`). install,
delete, and change-ip all acquire it and release on completion:

- install acquires before acceptance (`AuthorizeCloudInstall`) and holds the
  guard through the worker (`defer` inside the worker closure); any error path
  releases it.
- delete acquires before `CreateOperation` and holds it through the worker
  (`launchCloudDelete` releases in the worker, and on `worker_busy`).
- change-ip keeps its synchronous acquire/release (now via the helpers).
- Any of the three on a resource whose guard is held → `409 resource_busy`
  without calling the provider (install also creates no operation).

Newly created guards are released on every error path (`guarded` flag + worker
`defer`), so a rejected command cannot leak a busy slot. `runningOp(op.ID)`
still returns `409 in_progress` for a replay of the *same* install operation,
unchanged.

**Deviation (unchanged):** the guard is in-process and not crash-durable — a
registry restart loses it. A durable `installing` member on `cloud_resources2`
(worker transition `ready→installing→ready|error`, so the existing `state==ready`
guards exclude across restarts) is the later-stage fix; see §3.

Verification: `go vet ./...`, `go build -mod=readonly ./...`,
`go test -race -count=2 -run 'TestCloud' ./internal/server`, `gofmt -l .` (empty),
`git diff --check` — all clean.
