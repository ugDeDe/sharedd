# Production lifecycle — VPS READY; exact-profile installation FAILED

**Status: deployment/create verified; installation FAILED before registration; pool readiness NOT achieved.** Final installation verification: **2026-10-18T21:37:49Z** (UTC). Exactly one subsequent authorized install request used profile **6**, immutable version PK **26**, on resource **9**. No replacement, ban, deletion, manual repair, or install retry was performed. The earlier deployment resumed after the isolated runner could not access the leader's socket, using explicitly authorized command-local `SSHPASS`/`sshpass -e` without writing the SSH credential into files or handoff artifacts.

## Build and deployment

The required fix document, including the profile-version-PK follow-up, and the current create/auth/CSRF/catalog contracts were read directly. The coherent local worktree was built **once**, with no Go test suite:

```bash
# Working directory: /home/usr/Desktop/shareDDproxy/sharedd/registry
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=readonly -trimpath -o /tmp/opencode/sharedd-registry-livefix .
```

| Check | Observed result |
| --- | --- |
| Target | `root@registrar.ddproxy.xyz` |
| Service / binary | `sharedd-registry` / `/usr/local/bin/sharedd-registry` |
| Exact local and staged `--version` | `sharedd-registry` |
| Binary format | Static Linux x86-64 ELF |
| Local, staged, installed/running SHA256 | `896538cf60ad77d0255837e6d8afb034237367d386e20a45befc6b8134d1616a` |
| Previous binary SHA256 | `ffcc3449c9e5f6194469210e79a3e49d80a7e7a6f730ffc85808ebe63298dbae` |
| Backup created before stop | `/var/backups/sharedd-lifecycle-20261018T212246Z` |
| Backup ownership / directory mode | root / `0700`, verified |
| Local deployment script | `/tmp/opencode/sharedd-live-deploy.py` |
| Remote deployment script / manifest | `/root/sharedd-live-deploy.py` / `/root/sharedd-lifecycle-deploy.json` |
| Config path / preserved mode | `/etc/sharedd/registry.toml` / `0660` |
| State / master key | `/var/lib/sharedd/registry_state.json` / `/var/lib/sharedd/master.key` |
| SQLite databases | `/var/lib/sharedd/registry.db`, `/var/lib/sharedd/control.db` |
| Health smoke test | `GET http://127.0.0.1:8080/healthz` → **200**, including final check |

Before stopping, the old binary, config, JSON state, and master key were copied into the protected backup, and both SQLite databases received online backup-API snapshots passing `PRAGMA integrity_check`. Raw database/WAL/SHM files were also copied. After the short service stop, another consistent SQLite snapshot set and refreshed config/state/key copies were taken. Missing SHM files after clean stop were explicitly recorded, not silently ignored. The manifest lists every copy.

The new executable was uploaded to `/usr/local/bin/.sharedd-registry-livefix`, hash/version checked, fsynced, and renamed over the service executable on the same filesystem, preserving binary ownership/mode. The service was started; its running `/proc/<pid>/exe` hash was checked. Deployment-through-verification took 6.96 seconds; exact outage duration was not separately instrumented. Config permissions and the master key were verified unchanged. No rollback was needed.

## Actual migrated state, quota, and startup behavior

- `PRAGMA table_info(operations)` now contains `provider_op_id`. Actual columns: `id`, `user_id`, `node_id`, `kind`, `state`, `idem_key`, `provider_vps_id`, `region`, `config_id`, `request_json`, `last_error`, `created_at`, `updated_at`, `profile_id`, `provider_op_id`. Migration was not inferred from file size.
- Old resource **7** is `error`, external ID empty; old operation **24** is `failed`, both provider handles empty, safe error **`provider_reconcile_required`**. No new create was used to reconcile it and no history row was deleted. It remains an explicitly unresolved old obligation.
- Startup journal inspection found one pending-secret cleanup message and no `panic`/`fatal` matches. Successful cloud reconciliation emits no dedicated log line on this path; durable state above is the startup-resume evidence. Restart of a paid in-flight create was not deliberately exercised.
- `ddugul`, user **4**, enabled, max nodes **3**. Cabinet `GET /api/me`: **`used_nodes: 0`** before and after creation. SQL live reservations **0**, pending credentials **0**, including the final verification. Uninstalled VPS creation does not occupy a pool slot.
- All **6** existing deleted-resource history rows were preserved, checked against pre-create IDs.

## Real preflight and quote

- Acting through a newly inserted SHA256-hashed ddugul session; no password/hash changes to the user account. Session TTL **2 hours**, expires **2026-10-18T23:25:04Z**. Raw session token exists only in the protected session file listed below. The harness derives the source-defined CSRF hash in memory and sends `sharedd_session`, `__Host-sharedd_csrf`, and `X-CSRF-Token` to the local cabinet APIs.
- Own connection **1**, provider **Nodehost**, label **`test`**. Provider account is genuinely live: **`api.test: false`**. Credentials were decrypted only in production memory using AES-GCM with the configured hex master key and nonce-prefixed base64 ciphertext. Direct provider calls were **GET only**.
- Own profile **6**, **`mtproxyl-bin khinkal`**, `created_by: 4`; latest immutable version-row PK remains **26**, version number **1**, format **`text`**, selectable through the enabled cabinet profile API. No default-profile substitution or profile edit occurred.
- Provider inventory before create: **0**. Live balance returned `0.46000000000000013` USD (approximately **$0.46**).
- The app's real DE-filtered CPU catalog, disk/OS options, limits, and available network were used. Minimum selection: **1 core, 1 GB RAM, 10 GB disk, 1 Gbps, DE, 1 day**; CPU **AMD Ryzen 9 5950X**, disk type **Storage1**, actual OS catalog name **Debian 12**.
- `POST /api/me/cloud-resources/estimate` returned HTTP **200**, **$0.09 USD**, `period: 1d`, `period_days: 1`, scope `nodehost_api_pricing`, checked at **2026-10-18T21:25:05Z**. No top-up was attempted.

## Exactly one cabinet create and READY evidence

| Field | Actual result |
| --- | --- |
| Create submitted | **2026-10-18T21:25:25Z** |
| Name / resource label | `e2e182125` (9 ASCII characters) |
| Mutation | `POST /api/me/cloud-resources` → **202** |
| Number of create POST attempts | **1**, no replay or second purchase |
| Resource ID / owner | **9** / user **4** |
| App operation ID / kind / state | **30** / `cloud_create` / **confirmed** |
| Persisted provider operation ID | `cmvec3vmj05nhzcsdhgbr1wbd` |
| Real provider operation GET | Matching ID, **succeeded**, matching resource ID |
| Provider VPS ID | `cmvec3x3l05nizcsdqm0ivg0j` |
| Resource state / provider server state | **ready / ACTIVE** |
| IP | **191.44.108.190**, parseable and matching provider GET |
| Stored root password | Encrypted secret present and successfully decrypted/nonempty; not printed or copied into the ledger/report |
| READY verified | **2026-10-18T21:28:04Z**, about 159 seconds after submission |
| Inventory after | Exactly **1 ACTIVE VPS**, matching the new provider ID |
| Auto-renew | **false** |
| Provider expiry | **2026-10-19T21:25:27.248Z** |
| Provider balance after | `0.3700000000000001` USD (approximately **$0.37**) |
| Actual charged amount | **Not returned by Create/operation API**; $0.09 is the quote, not asserted as a charge receipt. Balance movement is recorded separately. |
| Creation-phase management | Manual create only at this checkpoint; the later explicitly authorized install attempt is documented below |

The stable unique create idempotency key, reference, configuration/body hash, and initially unset resource/operation ID slots were saved before sending. The accepted IDs and both provider handles were then persisted in the ledger. The harness refuses a new purchase once submission has been attempted and can recover the same accepted operation by its stored key. Polling ran independently in the background, normally every 12 seconds with a 590-second budget, reading cabinet resource/operations and comparing the persisted provider operation against real provider GETs. Request audit entries contain only timestamp, scope, method, path, and HTTP status—no headers, credentials, passwords, or raw provider response dumps.

## Protected handoff and next task

All these production files were verified root-owned, mode **0600**:

- Session: `/root/sharedd-lifecycle-session.json` — **path only; never publish contents**.
- Ledger: `/root/sharedd-lifecycle-ledger.json`.
- Driver: `/root/sharedd-lifecycle-harness.py`.
- Sanitized progress log: `/root/sharedd-lifecycle-progress.log`.

Exact next **read-only** command on the production host:

```bash
python3 /root/sharedd-lifecycle-harness.py status
```

The subsequent authorized installation task sent the following exact cabinet request **once**. This is now a reproduction record, **not an instruction to retry while the defect remains**:

```http
POST /api/me/cloud-resources/9/install
Content-Type: application/json
Idempotency-Key: <persist one stable installation-attempt key>
Cookie: <read protected session file in memory; source-defined cookies>
X-CSRF-Token: <derive in memory from session token>

{"profile_version_id":26,"vars":{}}
```

Never use `profile_id:26`, omit the explicit version, or substitute the old default profile. Revalidate the short-lived session expiry before that task. No replacement, ban, or deletion has been sent.

## Rollback and verification limits

For an application rollback, stop the service, protect the **current** data/ledger, atomically restore only the saved binary with its ownership/mode, start, and verify health/running identity. Database restore is **only** for diagnosed data/migration damage and must reconcile resource **9** and its provider handles first: blindly restoring the pre-deploy database would lose the new paid-resource obligation. Consistent `.stopped.sqlite` backups and raw companions are available in the protected backup directory; no database was restored in this run.

Creation-phase verification: one static build, exact binary identity/hashes, backup integrity, deployment health, actual migration/state/quota, authenticated profile/connection/catalog/quote preflight, one real create, persisted provider-handle matching, ACTIVE/IP/password readiness, inventory/history preservation, and protected-file permission checks. Python harness syntax compiled successfully. The installation outcome below supersedes the original create-only handoff. No Go tests, full audit, application source edits, commits, memory operations, or delegation. Replacement, ban, deletion/refund, and crash-restart durability remain untested.

## Authorized install-only continuation — FAILED, diagnosed, no repair

### Acceptance and bounded execution

- Target remained resource **9**, label **`e2e182125`** (9 ASCII alphanumeric characters), IP **191.44.108.190**, provider VPS **`cmvec3x3l05nizcsdqm0ivg0j`**. No old VPS was accessed.
- Revalidated profile **6** ownership by ddugul/user **4**, immutable version **26**, format `text`, selectable through the actual cabinet session. The profile text was not changed: SHA256 **`e25492b27dac6cf814c6315c52ac96ccf4eae704601f3d4b447975a362cecc2b`** before submission and after diagnosis.
- At **2026-10-18T21:31:50Z**, actual `POST /api/me/cloud-resources/9/check` returned **200**, `ssh_ok: true`, using the protected ddugul session and source-defined CSRF headers/cookies. `used_nodes` was **0/3** before installation.
- `installation.installationIdemKey`, the exact request body **`{"profile_version_id":26,"vars":{}}`**, the profile hash and submission guard were persisted in `/root/sharedd-lifecycle-ledger.json` **before the first send**. No default profile substitution occurred.
- At **2026-10-18T21:34:29Z**, exactly **one** `POST /api/me/cloud-resources/9/install` returned HTTP **200**, app operation **31**, profile ID **6**. Audit metadata confirms one install POST; no replay/reinstall occurred.
- The independent background QA observer had a persisted **710-second** deadline from first submission; it did not restart its timer. The app failed well before this limit. At **21:35:00Z** the observer recorded terminal `failed` after **30.4 seconds** total observation.
- A QA-only pre-submit column-name mistake (`steps` rather than SQLite's historical `playbook`) was corrected before any install POST. It created no application operation and caused no VPS mutation. The issue is retained under `installation.qa_driver_issues`; this was not an install retry.

### Actual step statuses

The exact custom profile contains **six** steps, not the default profile. Only the first step began:

| Zero-based step index | Actual state | Evidence |
| --- | --- | --- |
| **0** — MTProxyL manager / telemt binary installation | **error** | **23,885 ms**, shell exit **1** |
| **1** — custom network tuning | Not reached | No operation-step row |
| **2** | Not reached | No operation-step row |
| **3** | Not reached | No operation-step row |
| **4** — `install_node_web.sh` | Not reached | No node-agent installation occurred |
| **5** | Not reached | No operation-step row |

Sanitized app error header:

```text
шаг 1/6 "Установка MTProxyL (manager, telemt binary)": код выхода 1
```

The failed-step tail shows the bootstrap manager installation succeeded, followed by the decisive error:

```text
✓ MTProxyL установлен
Запуск: mtproxyl
[✗] --port: не хватает значения
```

### Confirmed root cause: unresolved profile variable names

The actual immutable profile calls the installer with these template arguments:

```text
--port {{ port }} --sni {{ sni }} --secret {{ secret }} --ad-tag {{ ad_tag }}
```

Its `vars_schema` defines **only `ad_tag`**, with a nonempty default. The exact authorized `vars: {}` body therefore supplies no `port`, `sni`, or `secret`. The registry's actual shared proxy port is **443**, but `provisionBaseVars` injects **`proxy_port`, `proxy_sni`, `proxy_secret`**, not their unprefixed counterparts. `RenderStep` resolves an unspecified template variable to a shell-quoted empty string. The first command therefore reaches the installer with **`--port ''`** (and empty `sni`/`secret` values), which the installer rejects as missing.

Evidence locations, read-only:

- `registry/internal/server/cabinet_cloud.go` — exact version selection, `MergeProfileVars`, `provisionBaseVars`, and later `label`/`node_secret` injection.
- `registry/internal/server/cabinet_beget.go:565-589` — only the `proxy_*` shared proxy names are populated.
- `registry/internal/control/steps.go:103-113` — missing template variables become `''`.
- Production `profile_versions.id=26` — historical `playbook` column and schema metadata, read in memory without editing.

This is **not** a confirmed missing-package, binary-download, registry-claim, or authentication failure. The app SSH check and direct TEST SSH both passed. The first observed fatal error is the empty port. The node-agent download and claim path were never reached.

Additional **unreached** contract risk: the profile also references **`{{ node_id }}`**, while this cloud acceptance path injects `label` and `node_secret`, not a pre-generated node ID. Do not claim that fixing only the port will complete installation; the backend developer must reconcile/preflight all referenced variable names while preserving profile/version pinning. No compatibility fix, profile change, supplied override, or manual install was applied here.

### Independent TEST VPS and pool-readiness truth

Only **191.44.108.190** was accessed by SSH. Its sealed root password was decrypted in production memory and provided to an SSH subprocess through command-local `SSHPASS`, never printed or persisted in a file. The registrar lacked `sshpass`, so an existing QA helper binary was copied to `/root/sharedd-qa-sshpass` (root-only executable `0700`); no package installation or repair was performed on the TEST VPS.

| Check | Observed result |
| --- | --- |
| TEST SSH | Passed |
| OS | Debian GNU/Linux **12** (bookworm) |
| MTProxyL manager CLI | `/usr/local/bin/mtproxyl` exists |
| Downloaded bootstrap script | `/root/mtproxyl-install.sh` exists |
| `telemt` executable | Not on PATH; `/usr/local/bin/telemt` absent |
| Node-agent executable/version | **Not installed**; `/usr/local/bin/sharedd-node-agent` absent |
| Proxy / agent services | No matching `mtprox*`, `telemt`, or `sharedd` service units |
| MTProxyL proxy configs | `/opt/mtproxyl/mtproxy/config.toml` and `telemt.toml` absent |
| Agent config | `/etc/sharedd/node.toml` absent |
| Independent registrar → TEST TCP **443** | **Failed** |
| TEST loopback `http://127.0.0.1:9090/metrics` | Unreachable (`URLError`); no proxy config/service installed |
| GP | **No registered candidate or measurement** to verify |
| Registration / resource node ID | **Not registered / empty** |
| Cabinet own nodes | **0** |
| Final quota / live reservations | **0/3 / 0** |
| Pending node credentials | **1**, from this accepted but failed install; does **not** consume quota |
| Pool readiness | **NOT achieved**; resource `ready` still means cloud VPS readiness only |

`bash`, `curl`, `wget`, `python3`, `sysctl`, and `ss` are present on TEST. `git`, `jq`, and Docker are absent, but no evidence ties those missing tools to the observed failure. They must not be presented as its root cause.

The desired registration transition to one reservation/`used_nodes: 1` was **not reached**, so it is not claimed as tested. No GP, metrics, or TCP success was inferred from SSH accessibility or cloud `ready`.

### Exact reproduction and protected handoff

The already-executed reproduction is the cabinet request above: resource **9**, immutable version **26**, empty vars, accepted operation **31**, followed by first-step empty-port failure. Correct the backend template compatibility/preflight contract before any authorized retry; do not mutate the user's profile or silently install by hand. The QA driver deliberately refuses another install send after its persisted submission flag is set. A failed installation can otherwise be rerun by the app, so the header alone is not a safe reason to replay it.

Production files (no secret contents in this report):

- `/root/sharedd-lifecycle-session.json` — existing session, `0600`, expires **2026-10-18T23:25:04Z**.
- `/root/sharedd-lifecycle-ledger.json` — `0600`, includes `installation`, stable key, app IDs, sanitized progress, diagnosis, and final verification.
- `/root/sharedd-install-qa.py` — `0600`, install-only observer; no automatic repair/retry.
- `/root/sharedd-install-progress.log` — `0600`, short structured step/quota progress.
- `/root/sharedd-install-diagnosis.json` — `0600`, sanitized failed-step and TEST SSH state evidence.

Next safe command **on the registrar**, read-only against the app:

```bash
python3 /root/sharedd-install-qa.py status
```

Final result at **2026-10-18T21:37:49Z**: operation **31 failed**, resource **9 retained**, no node ID, no pool readiness, profile hash unchanged. No new VPS purchase, delete, replacement, ban, manual install/repair, application source edit, build, test suite, commit, or delegation occurred in this install-only continuation.
