# Profile create/edit diagnostics and editor fixes (dev)

## Evidence and exact cause

This is a source diagnosis, **not a reproduction of the reported live failure**.
No production/cloud calls, DB writes, profile executions, deployments, restarts,
or test suites were performed. The pre-existing dirty tree was retained.
No real failed-request response, process initialization log, or database schema
from the affected process was supplied/observed. Therefore the specific runtime
cause of its `control_unavailable` remains unproven; do not infer restart,
missing DB, stale browser cache, a foreign-key failure, or a protected default.

Confirmed defects in the inspected source:

1. `panelReq` and cabinet `req` constructed errors with
   `fallback || data.message`, discarding a real server message whenever the
   code was `control_unavailable`. They now prefer `data.message`, retaining
   response status and code. Unavailable messages/logs no longer invent a
   restart diagnosis.
2. `profileAdminView` swallowed failures of `ProfileVersion` and
   `CountProfileVersions`, returning a success object with absent steps/vars or
   zero versions. Both admin and owner lists used this view. It now returns an
   error, logged with the read operation, instead of presenting corrupted data
   as an empty editor. Only a genuinely missing latest version (`ErrNotFound`)
   is treated as an empty draft. Published-version read failures in the cabinet
   list now also propagate.
3. Admin editor always sent `steps`/`vars_schema`, even for a name/description
   edit. That invoked `AddProfileVersion` and unpublished the profile. It now
   sends version fields only when steps or the normalized vars table changed.
4. Empty descriptions were silently ignored on PUT. Optional pointer-backed
   descriptions distinguish omission from `""`: omission keeps metadata;
   `""` clears it. Both admin and owner handlers use this semantics.
   An explicitly empty admin PUT name now reaches name validation rather than
   being silently treated as omission.
5. Admin steps-only updates erased vars; vars-only updates were ignored. Admin
   updates now preserve an omitted version field from the latest version and
   append for either field. Vars validation occurs before admin metadata writes;
   create validates vars before inserting a draft. Creation trims names and
   maps duplicate profile names to `409 duplicate_name`, not a username message.

## Exact error call chains

### Admin

`POST/PUT /panel/api/admin/profiles[/id]`
→ `handleAdminCreateProfile/handleAdminUpdateProfile`
→ `adminUsersGuard`:

| Condition | Result |
|---|---|
| Panel Bearer rejected | `401 unauthorized` |
| Users disabled | `503 users_disabled` |
| `r.control == nil` | `503 control_unavailable`; request operation logged, recorded Open cause logged if available |
| Store exists | Decode/validate → metadata/version/publish operations |

After the guard, arbitrary DB errors **did not become `control_unavailable` in
the inspected admin handlers**. They went to `writeControlErr` → `500 internal`
with `control db error`, without operation-specific logging. The patch adds
redacted diagnostics at list, lookup, rename, description, version, publish,
delete, cleanup, and response-view boundaries.

`control.Open` → `sql.Open` → schema Exec → column/uniqueness migrations →
legacy-user initialization. Any error causes `openControl` to record
`r.controlErr`, leave the store nil, and return; that is one possible source of
the guard response, not evidence that it happened in the affected process.

### Cabinet

`/api/me/profiles` → `requireSession` → `cabinetGuard` → cookie lookup →
`SessionUser` → (writes only) `requireCSRF` → owner/profile handlers.
The nil-store guard returns `503 control_unavailable`. A non-not-found,
non-expired `SessionUser` error still returns `503 control_unavailable` (existing
auth fail-closed status), but its actual underlying error and route operation
are now logged before that response; it must not be mistaken for a nil store.
Expired/unknown sessions remain `401`; disabled accounts remain `403`; CSRF
rejection remains `403 csrf_invalid`.

Previously owner create's `CountUserProfiles`, and owner list's
`ProfilesByOwner`/`PublishedProfiles`, sent arbitrary read errors to
`writeControlUnavailable`, which logged initialization state rather than the
actual query failure. Those three paths now use operation-tagged
`writeControlErr`, returning `500 internal` for unknown DB failures rather than
mislabeling them as absent control storage. Owner metadata/version errors already
used `writeControlErr`; they now include the failing operation.

### Constraints, validation, default profile, and versions

| Failure | Response/diagnostic |
|---|---|
| Bad JSON / missing name or steps | `400 validation_failed` |
| Explicit empty steps / >64 KiB | `400 steps_rejected`, original validation message |
| Invalid vars JSON/name | `400 validation_failed`, original validation message; quoted variable names redacted in log |
| Duplicate create/rename name | `409 duplicate_name` |
| Other SQLite constraint in a tagged profile operation | `409 constraint_conflict`; actual SQLite extended code + redacted underlying diagnostic in log |
| Missing profile/version required for publish | `404 not_found` |
| Unknown DB/schema/scan/transaction error | `500 internal`, no raw SQL detail in HTTP response; underlying diagnostic in log |

There is **no default protected-profile guard** in the actual
`control/profiles.go` or profile handlers. `created_by=0` identifies system/admin
profiles but the current schema has **no foreign key on profiles.created_by**.
Admin creation with owner 0 is therefore not itself evidence of a foreign-key
error. `ErrLegacyProtected` protects the legacy **user**, not `sharedd-node`.
`EnsureDefaultProfile` only seeds an absent profile at startup; it does not
prohibit admin rename, edit, unpublish, or delete. No such guard was removed or
invented by this patch.

Versions still use the existing historical SQL column `playbook` for the
public `steps` field. `AddProfileVersion` validates, checks the profile, begins a
transaction, computes max(version)+1, inserts, unpublishes, commits. A constraint
or SQL failure there is distinguishable from metadata failure via
`admin.profiles.update.version` / `user.profiles.update.version` and SQLite code.
No DB migration was guessed or applied.

Other `users_control.go` translations were also inspected: node-owner lookup,
authkey lookup, and non-domain `AuthorizeNode` failures on `/register` call
`writeControlUnavailable`. Owner/quota/bad-authkey refusals have their explicit
403 codes. These registration paths are not invoked by profile CRUD and were
not changed. The shared unavailable-state log now distinguishes a nil store
from an initialized store with a failed request, instead of asserting restart
or nil state for every 503. Provisioning/install endpoints likewise are not
profile create/edit; no paid or SSH path was invoked/changed.

## Editor availability

The actual admin editor already unconditionally rendered editable `p-name`,
`p-desc`, `p-steps`, and `p-vars` for **both creation and editing**. Creation was
already accessible on list failure. No feature flag or default-profile branch
hid steps. The modal helper supplies `modal-wide`; shared CSS supplies a
260px-minimum steps textarea, scrolling body, and reachable action footer.
The patch additionally sets native `rows=12` and focuses the name field.
No redesign or new dependency was added. The generated panel is rebuilt.

Cabinet `profileEditor` was separately inspected: steps and vars are also
unconditional there; its name remains read-only on edit, and its existing save
path always sends steps/vars (not rename/description). These are owner-editor
limitations, **not the admin editor**, and were not expanded in this fix.
Shared request-message preservation and safe profile-view reads cover both UIs.

The reported inability to see/use the textarea was **not reproduced**. Source
and generated-asset availability do not prove what the affected browser served
or rendered. No cache/restart explanation is asserted.

## provisionVars precedence (inspected, unchanged)

Callers first `MergeProfileVars(schema, requestVars)`: request values beat schema
defaults; missing declared keys receive non-empty defaults. Names/sizes are
validated. `provisionVars` then copies that map and injects server fields:

| Field | Actual precedence / overrideability |
|---|---|
| `proxy_port` | Always overwritten by `[shared_proxy].port`; schema/request override does not survive |
| `proxy_sni` | Always overwritten by `[shared_proxy].tls_domain`; schema/request override does not survive |
| `proxy_secret` | Existing map key wins, including an explicitly empty value and a schema default; otherwise server default comes from shared_proxy.users (sole entry, or lexicographically first username; empty if none) |
| `ad_tag` and valid `ad_tag_*` keys | No built-in injection/filter/rewrite found; ordinary variables. Request beats schema default; without either, no value is invented |
| `node_id` | Always overwritten by the precreated server node ID |
| `node_secret` | Server-issued/recovered secret overwrites input on success; failure aborts rather than using a caller identity |
| `registry_url` | Non-empty panel.public_url overwrites input; if public_url is empty the copied input remains (existing behavior, not changed) |

There are no ad-tag aliases automatically derived by this function. These
variables only affect commands that reference them. Overriding a profile
variable does not override the registry's actual machine configuration/health
policy. No change to `provisionVars`, `node_id`, `node_secret`, Bearer auth,
ownership, secret rotation, or security identity overrideability was made.
The old comment saying all proxy fields were overrideable was not accurate for
port/SNI; admin editor help now matches the code.

## Redaction/security review

Authentication/CSRF/ownership gates retained. Queries remain parameterized;
steps and vars are never included in logs or HTTP database diagnostics.
Diagnostics carry a constant operation (or matched route pattern), actual
error text, type/SQLite code. Quoted text is redacted, control characters are
removed, messages bounded to 1024 bytes. SQLite errors contain schema/code
diagnostics, not bound profile values. No request bodies, cookies, headers,
passwords, node secrets, or user variable values are logged. Raw internal DB
messages remain server-side; domain validation messages retain existing HTTP
status/message. No external review service received project data.

## Changed files

- `registry/internal/control/profiles.go`
- `registry/internal/server/profiles_admin_api.go`
- `registry/internal/server/profiles_user_api.go`
- `registry/internal/server/users_admin_api.go`
- `registry/internal/server/users_control.go`
- `registry/internal/server/cabinet_api.go`
- `ui/pages/panel/views/profiles.ts`
- `ui/pages/panel/views/users.ts`
- `ui/pages/cabinet/api.ts`
- `registry/internal/webassets/panel.html` (generated)
- `registry/internal/webassets/cabinet.html` (generated)
- `docs/api-contract.md`
- `docs/specs/fix-profile-control-errors.md`

## Allowed verification

No tests added or executed, as explicitly requested. Run from the stated dirs:

```sh
# registry/
gofmt -w internal/control/profiles.go internal/server/profiles_admin_api.go internal/server/profiles_user_api.go internal/server/users_admin_api.go internal/server/users_control.go internal/server/cabinet_api.go
go vet ./internal/control ./internal/server
go build -mod=readonly ./...

# repository root
bash scripts/build_ui.sh
bash scripts/build_ui.sh -check

# registry/ again after embedding regenerated HTML
go build -mod=readonly ./...
```

Initial targeted vet/build and UI build/check passed. Final post-edit targeted
vet, UI build/check, and post-regeneration registry build also passed. UI build
is esbuild compilation, not a full
TypeScript typecheck or browser functional check.

Verification status: **partially_verified** — permitted compile/static checks
pass, but the reported failing request and browser steps-editor failure were
not reproduced. No test suites run by request.

## Remaining uncertainty / leader handoff

- Actual runtime `control_unavailable` cause is not captured from a failing
  process in this session. New diagnostics make the next naturally occurring
  failure distinguishable without mutating profiles for testing.
- For diagnosis, leader needs the failed request's route/status/code/message
  and redacted `control operation=...` / Open log from that same process, plus
  confirmation of the served build. Do not request tokens or profile secrets.
- Combined metadata/version updates remain multiple DB transactions; an SQL
  failure after a rename can leave metadata changed. Creation cleanup is
  best-effort and now logged. No speculative transactional rewrite was made.
- Existing stored vars are normalized by the browser editor on version changes;
  no migration or rewrite of stored/live profiles was performed.
- **Return to leader before deployment.** No commit, push, restart, service
  change, production profile mutation, or deployment authorized/performed.
