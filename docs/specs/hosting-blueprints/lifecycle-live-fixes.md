# Bounded lifecycle live fixes — 2026-10-19

## Scope and evidence

Application code only. No production access, provider purchase, delegation,
memory writes, commit, profile edit, or broad test/vet loop was performed.
Prior local/untracked work was preserved.

The following production facts were supplied by the leader, **not independently
queried during this change**:

- Deployed control schema lacks `operations.provider_op_id`; local unfinished
  code already has the additive migration and partial read-only resume support.
- Resource **7**, last operation **24**, is pending/creating with an empty
  external ID, creation timestamp **1792219058**, older than a day.
- Captured provider inventory has **0 VPS**. Operation
  `cmvc0ycis13xll1sdcf53msbg` succeeded for resource
  `cmvc0ye0p13xml1sdmfg7rlcs`, which is now gone. Other failed operations have
  `error` as a **string**, not the object expected by the local decoder.
- `ddugul`: max nodes **3**, live reservations **0**, pending credentials **2**
  from failed installations of deleted resources.
- User-owned installation profile **6**, version-row PK **26**, is named
  **mtproxyl-bin khinkal**. It is not the previously failing default scripts.
  The user profile was not modified.
- Follow-up leader evidence: the frontend sends `profile_version_id`, but the
  old cloud-install body accepted only `profile_id` and silently ignored the
  version field. Failed operations **25/28** therefore executed the default
  four-step telemt profile instead of the user's MTProxyL profile. This
  supersedes the earlier, incorrect no-version-pin guidance in this report.

## Fixes / safety decisions

1. **Provider errors:** custom decoding accepts string/object/null; errors are
   converted to fixed local codes. Known insufficient-funds/balance messages
   map to `insufficient_funds`; unknown text/codes map to `provider_error`.
   Nothing is made safe by merely truncating arbitrary provider text.
   Invalid JSON/schema/oversize responses terminate polling immediately.
2. **Create/resume:** mark submission before Create, persist the provider
   operation handle before the first poll, and retain the instance handle before
   waiting for readiness. Handle-persistence failure stops rather than claiming
   crash durability. Restart uses only Operation/Server reads. Missing handles
   become visible `error`/`failed` with `provider_reconcile_required`; no Create
   is retried. Existing additive startup migration supplies an empty handle for
   old rows, not a guessed link to an account-wide operation.
3. **Readiness:** require an ACTIVE/RUNNING server, parseable IP and durably
   encrypted password before ready. A 404/server schema failure is terminal;
   readiness/password gaps have the existing bounded create deadline. Known
   instance IDs survive failure for subsequent provider-backed deletion.
4. **Quota:** usage is live reservations only. No pending/install/order pool
   quota check precedes registration (including legacy Beget acceptance).
   Both admission paths check current limits transactionally, including an
   existing owner's recovery. Personal-secret recovery no longer bypasses
   admission. Startup removes stale reservations and seeds actual candidates
   in one transaction; ownership and working candidate slots survive.
   Reinstall also preserves ownership and live slots. Bearer, ownership,
   disabled-user and pending-label checks are retained.
5. **Cleanup/deletion:** preserve existing startup/periodic orphan-pending
   cleanup and confirmed deletion revocation. A missing worker is not billing
   proof: local deletion is blocked with 409 `provider_reconcile_required`
   unless the plan was unsubmitted or the purchase definitively refused.
   No new inventory matching/automatic paid retry was introduced.
6. **Exact installation selection (critical follow-up):** recognize explicit
   `profile_version_id`, look up `ProfileVersionByNumericID`, enforce the same
   own/published visibility and user-profile feature gate as existing selection,
   and reject a mismatching optional parent `profile_id`. Invalid versions
   never fall back. Graph/non-text versions are rejected with 422 before SSH
   password decryption, pending-secret preparation, operation creation or SSH.
   Pass the accepted immutable row ID through `InstallRequest.ProfileVersionID`
   into `applyProfile`; its lookup cannot reload latest or a different parent.
   Cloud legacy parent-ID selection still resolves latest at acceptance, then
   pins that row for its worker. Manual/Beget callers remain unchanged when the
   new internal field is zero. No UI or production profile was changed.

## Exact live-driver installation body

```http
POST /api/me/cloud-resources/{new_ready_resource_id}/install
Content-Type: application/json
Idempotency-Key: <fixed install-attempt key>
X-CSRF-Token: <ddugul CSRF token>
Cookie: <ddugul session and matching CSRF cookie>

{"profile_version_id":26,"vars":{}}
```

Use **`profile_version_id: 26`** to pin the user's exact version, NOT
`profile_id: 26`. Also accepted:

```json
{"profile_id":6,"profile_version_id":26,"vars":{}}
```

Both IDs must agree; mismatch is **400 `profile_mismatch`**. Parent-only legacy
`{"profile_id":6,"vars":{}}` still selects that profile's latest visible version.
An explicit zero/negative/null/non-integer version is **422 `profile_invalid`**;
an unknown/private foreign version is **404 `not_found`**; graph/non-text is
**422 `profile_incompatible`**. None falls back to a default. Supply explicit
`vars` only if needed; `{}` inherits the selected version's schema defaults.
Success is HTTP 200 with the existing operation DTO; poll
`GET /api/me/operations` for `done` and verify the corresponding live node.

## Files changed in this pass

Production Go:

- `registry/internal/nodehost/types.go`
- `registry/internal/nodehost/client.go`
- `registry/internal/nodehost/methods.go`
- `registry/internal/control/nodes.go`
- `registry/internal/control/manual_install.go`
- `registry/internal/control/cloud_install.go`
- `registry/internal/control/cloud_resources.go`
- `registry/internal/control/operations.go`
- `registry/internal/server/users_control.go`
- `registry/internal/server/api_auth.go`
- `registry/internal/server/cabinet_cloud.go`
- `registry/internal/server/cabinet_beget.go`
- `registry/internal/provision/provision.go` (exact-version follow-up only)

Checks and expectation updates:

- `registry/internal/nodehost/client_test.go`
- `registry/internal/control/control_test.go`
- `registry/internal/control/cloud_install_test.go`
- `registry/internal/control/manual_install_reinstall_test.go`
- `registry/internal/control/pending_release_test.go`
- `registry/internal/server/cabinet_cloud_test.go`
- `registry/internal/server/cabinet_cloud_resume_test.go`
- `registry/internal/server/cabinet_cloud_install_test.go` (one follow-up test)
- `registry/internal/server/manual_servers_test.go`
- `registry/internal/provision/provision_test.go`

Documentation:

- `docs/api-contract.md`
- `docs/specs/hosting-blueprints/quota-active-only.md`
- `docs/specs/hosting-blueprints/lifecycle-live-fixes.md`

Pre-existing local migration/startup changes in
`registry/internal/control/control.go` and
`registry/internal/server/run_main_impl.go` remain required deployment inputs;
they were inspected/preserved, not replaced in this pass.

## Initial bounded verification (once)

Working directory: `/home/usr/Desktop/shareDDproxy/sharedd/registry`.
Changed Go files were formatted with `gofmt -w` before verification.

```bash
go test -vet=off -race -count=1 -run '^(TestOperationFailedCarriesSanitizedCode|TestUsedNodesCountsManualPending|TestCloudCreateResumeReadOnlySafety)$' ./internal/nodehost ./internal/control ./internal/server
go build -mod=readonly ./internal/nodehost ./internal/control ./internal/server ./internal/provision
```

Results: **PASS** (nodehost 1.023s, control 3.438s, server 10.856s); build PASS.
Commands ran through `rtk proxy`; no additional test or vet suite was run.
Root `git diff --check` also passed before this documentation update.

The three selected checks cover string/object/null errors and redaction;
pending=0/live=1, recovery limits, concurrent claims and startup reconciliation;
old-schema migration plus handle persistence across reopen; missing handles,
read-only success resume, terminal schema/string failures, missing IP/password,
404, and blocked deletion of uncertain creates. Other existing expectation
updates and provider-backed cleanup paths were not separately executed.

## Exact-version follow-up verification (one test, one build)

Only these files were touched for this follow-up:

- `registry/internal/server/cabinet_cloud.go`
- `registry/internal/provision/provision.go`
- `registry/internal/server/cabinet_cloud_install_test.go`
- `docs/api-contract.md`
- `docs/specs/hosting-blueprints/lifecycle-live-fixes.md`

Working directory: `/home/usr/Desktop/shareDDproxy/sharedd/registry`.
The three changed Go files were formatted with `gofmt -w`.

```bash
go test -vet=off -race -count=1 -run '^TestCloudInstallPinsExplicitProfileVersion$' ./internal/server
go build -mod=readonly ./internal/provision ./internal/server
```

Both commands ran **once** through `rtk proxy`: test **PASS (2.774s)**, build
**PASS**. `git diff --check` passed. No previous tests, full suites, vet loops,
live VPS calls, UI changes, production edits or unrelated refactors were made
for this follow-up.

The single injected-executor regression test selects an older non-default
MTProxyL-style text version with a distinct version-row PK while a newer
version exists. It verifies the exact old steps **and schema defaults**, one
custom step rather than the default four, successful local registration,
matching optional parent ID, and legacy parent-only latest selection. Invalid
zero/null/type, nonexistent/private foreign, mismatched-parent, and graph
selections are rejected with unreadable SSH credentials present, without an
install operation or any SSH command. This catches both silent default
substitution in the handler and latest-version substitution in `applyProfile`.

## Gaps / live next step

**Verification status: `partially_verified`** — focused application checks passed;
the requested one-VPS live run was not executed by this code-only task.

- Deploy the coherent local build (including the existing migration/startup
  files). The currently deployed old binary cannot gain this behavior by itself.
- Old resource 7 cannot recover an absent operation handle automatically.
  The driver's supplied inventory/history evidence must be reconciled against
  that owned resource before clearing its obligation. The app intentionally
  blocks local deletion meanwhile; do not treat `failed` here as proof of no bill.
- Under the ddugul session, confirm `GET /api/me` shows `used_nodes: 0` when
  there are no pool candidates, the two deleted-resource pending credentials
  have been cleaned, and version PK 26 belongs to visible profile 6. It need not
  remain latest: the explicit version request now pins it exactly.
- **The one paid VPS now exists: resource 9. Do not create another VPS.** The
  live continuation in `prod-live-lifecycle.md` supersedes the earlier create
  instruction. The expected-ID follow-up below resolves the pre-registration
  identity contract; verify the separately noted installer flag before retry.
  The next separately authorized install retry must target resource **9** with
  exact version **26**, never buy a replacement. Check
  `GET /api/me/operations`, `GET /api/me/nodes`, and `GET /api/me`: confirmed
  registration should yield one pool node and `used_nodes: 1` of max 3.
  Stop on failure; do not automatically issue a second paid create.
- No user profile or machine shared-secret configuration needs modification.
  No frontend/generated assets or node-agent sources were changed in this pass.

Skills applied: coding-standards, golang-patterns, security-review. Security
review was local to these trust/credential/billing boundaries, not a new audit.

## Standard-variable aliases — live resource 9 follow-up

Read `prod-live-lifecycle.md`, especially its final install-only continuation.
Its supplied evidence: SSH check passed on resource **9**; exact profile **6** /
version PK **26** was selected; operation **31** failed step 0 after **23,885 ms**
with `--port: не хватает значения`. Profile arguments use `port`/`sni`/`secret`,
but its schema declares only `ad_tag` and the resolver injected only `proxy_*`.
No proxy service/config or node-agent binary was installed. These facts were
read from the devops artifact; this follow-up made no production calls.

### Minimal compatibility behavior

- Normalize `port → proxy_port`, `sni → proxy_sni`, `secret → proxy_secret` in
  the existing schema/request merge. Explicit request > nonempty schema default
  > registry default works across both names. Conflicting explicit synonyms
  fail validation rather than choosing by map iteration or silently overriding.
- The common `provisionBaseVars` injects both spellings with the same resolved,
  validated values. Cloud and manual installs already use this helper; Beget
  uses it through `provisionVars`. No saved profile/version/template is edited.
  No alias or synthetic default is added for `ad_tag` or unrelated variables.
- The effective port must equal actual `[shared_proxy].port`. Wrong alias or
  canonical overrides/defaults are rejected, not coerced. There is no new
  hardcoded port and the registry port is never changed by install variables.
- Values stay raw until existing `RenderStep` shell-quotes them **once**.
  Both `secret` and `proxy_secret` are masked without requiring a schema secret
  flag; echoed shell-quoted secrets are also masked, before output truncation.
- Preflight checks only referenced required standard variables: secret names,
  `registry_url` and `node_id` must be nonempty strings before any SSH. Explicit
  empty secrets still suppress defaults; if actually required by the steps,
  they fail instead of being silently replaced. `registry_url` remains protected
  and server-configured; an empty public URL cannot run as an empty argument.
  Optional/custom variable empty behavior is otherwise unchanged.

### Previous node_id blocker — resolved by expected-ID follow-up below

The live artifact explicitly reports **unreached `{{ node_id }}` references in
version 26**. At the alias-only checkpoint these failed preflight because no
full ID was prepared. The follow-up below now supplies a stable, server-issued
**expected** ID before SSH, without inventing an owner or a successful register.
The old missing-ID diagnosis is historical, not the current cloud contract.
Keep/reuse paid VPS **9**; no new purchase is required for template variables.

### Exact files touched for this follow-up

- `registry/internal/control/profiles.go` — cross-name merge and validation.
- `registry/internal/server/cabinet_beget.go` — common alias injection and
  shared-port equality check.
- `registry/internal/provision/provision.go` — required-standard preflight and
  raw/shell-quoted masking for either secret name.
- `registry/internal/server/cabinet_cloud_install_test.go` — one focused test.
- `registry/internal/server/cabinet_api_test.go` — shared test fixture now has
  a nonempty synthetic registry URL and client secret for required references.
- `docs/api-contract.md` — alias/required-reference contract.
- `docs/specs/hosting-blueprints/lifecycle-live-fixes.md` — this handoff.

### Executed verification (once each)

Working directory: `/home/usr/Desktop/shareDDproxy/sharedd/registry`. Changed Go
files were formatted with `gofmt -w`.

```bash
go test -vet=off -race -count=1 -run '^TestProvisionStandardAliasesRenderMaskAndSharedPort$' ./internal/server
go build -mod=readonly ./internal/server
```

Both ran once through `rtk proxy`: **test PASS (1.598s), build PASS**. The one
test uses configured port **9443**, checks nonempty aliases through the actual
renderer/injected executor, exactly-once shell quoting, cross-name schema and
explicit-value precedence, input immutability, mismatch refusal, each secret
name's masking independently, and missing secret/URL/node-ID preflight refusal.
No other test, full suite or vet was run. No UI, user-profile, production,
provider, VPS, delegation, memory, commit or release action was performed.

Verification: **`partially_verified`** overall; the focused application checks
passed, but no live install retry was executed. See the follow-up below for the
resolved identity contract and separate installer compatibility note.

## Expected node identity — implemented, not a fake registration

The leader supplied version 26's exact node-install command:

```text
curl -fsSL https://raw.githubusercontent.com/ugDeDe/sharedd/main/scripts/install_node_web.sh | bash -s -- --registry={{ registry_url }} --registry-token={{ node_secret }} --node-secret={{ node_secret }} --name={{ node_id }}
```

### Actual fixed contract

- `AuthorizeCloudInstall` generates one valid full ID from the validated
  resource label (1..10 NAME characters) and five cryptographically generated
  lowercase hex characters (a valid subset of the lowercase-alphanumeric suffix).
  It persists this as `operations.request_json.expected_node_id`, preserving
  other JSON metadata. No new schema column/migration is necessary.
- Metadata, the existing pending credential, install attempt state, and the
  resource's `last_operation_id` commit in **one SQLite write transaction**.
  Retries and DB reopen preserve the same ID; acceptance rejects attempts to
  replace an existing pending token/ciphertext. Old failed installs such as
  operation 31 receive the metadata on their next accepted retry, reusing their
  recoverable pending token rather than rotating it.
- Cloud templates receive the full ID in **both `node_id` and `label`** so
  either installer spelling produces that same identity. The resource's display
  label is unchanged. User request/schema `node_id` remains forbidden.
- Preparing an ID writes **no `node_owners` or `quota_reservations` row**.
  `operations.node_id` remains empty until the real pending-token `/register`
  claim. The claim must match the persisted full ID, not merely its label prefix;
  a same-label/different-suffix claim fails without consuming the credential.
  Existing owner, disabled-user, bearer and transactional quota checks remain.
- For already-claimed resources, acceptance reuses the actual owned ID and
  exact stored token after checking ownership/hash. It preserves the live owner
  and reservation, but clears the new attempt's `operations.node_id` and issues
  a pending claim for the **same** token. `Confirm` therefore cannot falsely
  succeed just because an earlier attempt once registered that node. A fresh
  real claim fills the full ID again without taking a second quota slot.
- Expected-ID preparation does not bypass the required-variable preflight or
  modify profile 6/version 26. No production/provider/VPS action was performed.

### Installer source verification and separate flag compatibility

Both checked-in installers accept full NAME-HASH in `--name` and preserve it:

- `scripts/install_node_web.sh`: full-ID validation in lines 88–93, direct
  `NODE_ID="$NODE_NAME"` in lines 217–219, persisted through its normal install path.
- `scripts/install_node.sh`: validation in lines 106–110, direct assignment and
  state-file persistence in lines 383–388.

**Separate observed issue, not an identity blocker:** the checked-in
`install_node_web.sh` argument parser (lines 69–85) accepts `--registry-token`
but **does not accept `--node-secret`** and rejects unknown flags. The supplied
command contains both. The registry build cannot change the script fetched from
GitHub `main`; verify/fix that served installer's flag compatibility before the
live retry. This pass did not silently rewrite the user's script/profile, edit
installer files, publish artifacts, or claim that the remote installer supports
that flag. Full-ID binding is implemented and checked independently of this.

### Exact files touched in this follow-up

- `registry/internal/control/cloud_install.go` — expected-ID generation,
  metadata lookup, atomic preparation, same-token/owned-identity reuse.
- `registry/internal/control/manual_install.go` — strict cloud expected-ID
  claim and existing-owner token check; no manufactured registration.
- `registry/internal/server/cabinet_cloud.go` — strict label validation and
  protected full-ID injection before profile preflight/SSH.
- `registry/internal/server/cabinet_cloud_install_test.go` — one exact regression.
- `registry/internal/control/cloud_install_test.go` — existing zero-quota
  expectation uses the prepared ID instead of an arbitrary suffix (not run).
- `docs/api-contract.md` — actual expected-ID versus registration contract.
- `docs/specs/hosting-blueprints/lifecycle-live-fixes.md` — this handoff.

### Verification (one exact test, one server build)

Working directory: `/home/usr/Desktop/shareDDproxy/sharedd/registry`.
Changed Go files were formatted with `gofmt -w`.

```bash
go test -vet=off -race -count=1 -run '^TestCloudInstallExpectedIdentityStableUntilRealClaim$' ./internal/server
go build -mod=readonly ./internal/server
```

Both ran **once** through `rtk proxy`: **test PASS (2.955s), server build PASS**.
The test injects the exact supplied command into the fake executor (it does
**not** execute curl/bash/SSH). It checks protected caller-ID rejection, valid
expected-ID/token injection, metadata/pending persistence across DB reopen,
zero owners/quota before real registration, rollback on attempted token change,
same-label wrong-ID refusal, real authenticated `/register`, full operation ID
and one reservation afterward, and no false `done` when a claimed-node reinstall
omits its new claim. Repeated real claims reuse identity/token and quota stays 1.
No other test/full suite/vet was run and no production/profile/UI/installer or
provider changes, delegation, memory writes, commits or releases were performed.

Status: **`partially_verified`** overall: expected-ID application behavior is
checked; live installation remains unexecuted here. The next live action, once
the served installer compatibility is settled, is an explicitly authorized
retry on **the same paid resource 9** with **`{"profile_version_id":26,"vars":{}}`**,
not a new VPS purchase or a fabricated registration.
