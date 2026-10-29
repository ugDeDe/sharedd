# Editable standard variables — Phase 3 focused source QA (dev)

## Current verdict: PASS — focused B1/B2 source gate

Verification status: **partially_verified** — source inspection and generated-asset
check on the initial review; source inspection only on this focused re-QA.
Both original P1 findings are closed in source. No remaining blocker found within
this re-QA scope. Neither the original findings nor their fixes were browser-tested.
This is not a full visual/accessibility or runtime security certification.

## Focused re-QA of the corrections

Read `implementation-editable-vars-ui.md:146–206` and the current shared editor,
cabinet main/API, manual-install backend, operation response mapper and authorized
profile selection/list paths. No code changes or new checks/tests were executed;
only this QA document was updated. Earlier build results below are historical,
not claimed as rerun results for the corrections.

### B1 — CLOSED / source PASS: stored-profile lock agrees across UI and backend

- `cabinet_manual.go:354–364` now takes `existing.ProfileID` whenever
  `existing.ID != 0`, including stored zero. Only a first installation without
  an existing operation uses the submitted choice. Caller omission or a different
  submitted ID cannot replace the profile of an existing manual operation.
- `cabinet_manual.go:75–93` attaches the owned operation to the server response.
  `cabinet_beget.go:285–305` emits `profile_id` without `omitempty`, so explicit
  zero remains distinguishable from unavailable metadata. Cabinet API
  `Operation.profile_id?: number` (`api.ts:77–79`) accommodates missing metadata
  without interpreting it as zero.
- `main.ts:504–520` does not paint substitute retry variables while loading.
  It resolves the stored ID against authorized published/own metadata, renders
  only that profile and leaves the selector disabled with the lock explanation.
  The readiness marker is set only after resolution and editor rendering.
- `main.ts:549–564` submits the stored operation profile ID on retry, not the
  selector choice. Its readiness check is independent of selector `disabled`,
  so a successfully resolved locked editor can submit normally.
- Explicit stored **zero** resolves `sharedd-node` metadata in the UI
  (`main.ts:513`) and the backend's `control.DefaultProfileName` in
  `selectableProfileVersion` (`cabinet_beget.go:457–478`;
  `control/profiles.go:252` defines the same name). Both then use that profile's
  latest schema/version rather than a substitute profile.
- **Absent operation/profile ID**, negative/noninteger ID or unavailable profile
  leaves readiness unset and blocks retry (`main.ts:511–514,539–554`). Explicit
  zero is not rejected as absent. The current backend supplies zero explicitly;
  an older/missing metadata response fails closed, not into standard inheritance.
- Unpublished own profiles are blocked when user profiles are disabled, matching
  backend eligibility (`main.ts:514`; `cabinet_beget.go:469–476`). Missing/deleted
  or unauthorized profile metadata likewise does not produce fallback variables.
  Initial-install profile selection and switch warnings remain intact.

| Case | UI behavior | Manual backend behavior | Source result |
|---|---|---|---|
| Existing operation with positive stored ID A | Resolve A, lock selector, send A | Use stored A regardless of submitted ID | PASS |
| Existing operation with stored ID 0 | Resolve/lock actual `sharedd-node`, send 0 | Use stored 0; resolve default by the same name | PASS |
| Retry metadata ID absent | No substitute schema; readiness absent; no submit | Existing stored ID remains authoritative if called outside UI | PASS / fail-closed UI |
| Stored profile unavailable/ineligible | Show loading failure; no ready editor/submit | Selection rejects missing/unauthorized/disabled-own profile | PASS |
| No existing operation, initial install | Normal profile choice | Use request ID; absent request ID means standard | PASS |

This locks the **profile ID**, not a historical version snapshot. Both paths use
current profile metadata/latest version; changes between loading and submission
remain a runtime timing boundary, not a newly claimed immutable snapshot.

### B2 — CLOSED / source PASS: trimmed sensitive-name classification

- `vars.ts:34–35` now classifies conventional sensitive names with
  `v.name.trim()`, matching validation (`:41–45`) and profile/install key
  serialization (`:72,243–248`). Matching remains case-sensitive; schema
  `secret:true` remains independently authoritative.
- Initial paint (`:135–145`), name/secret-checkbox input handling (`:196–205`)
  and reset (`:171–176`) all use the same `isSecretVar` classifier. Padded
  `password`, `authkey` and `proxy_secret` therefore default to password inputs;
  only explicit reveal switches to text, updating pressed state/label. Reset and
  name edits restore masking and the unpressed reveal state.
- Confirmation trims schema-secret names and override keys before classification
  (`main.ts:1170–1177`), so bullets are used consistently, including empty secret
  overrides. User values are not trimmed; omission/explicit-empty semantics are
  unchanged (`vars.ts:171–179,243–248`).
- Protected-name checks also trim on initial paint and metadata arrival
  (`vars.ts:135,215–219`), as already done during name edits (`:199–204`).
  Protected defaults are not rendered, reveal is hidden, and async metadata does
  not populate their values. Protected-key validation remains in place.
- This closes the padded-name masking defect; no new identity-secret exposure,
  secret-returning metadata capability or persistence was found in these fixes.

**Remaining verification:** browser entry/reveal/reset, retry payload capture,
zero/absent metadata behavior and effective defaults at execution are still
unverified. No tests, production actions, commits or application edits performed.

---

## Initial QA record — historical FAIL, superseded by the source PASS above

Reviewed `editable-standard-vars.md`, `implementation-standard-vars.md`,
`implementation-editable-vars-ui.md`, `DESIGN.md`, shared vars editor/CSS,
cabinet main/API, panel profile editor, and backend merge, provisioning,
metadata, profile-save and error-mapping paths. Branch checked: `dev`.
Impeccable loaded; audit/product guidance read. Its context lookup reported
missing `PRODUCT.md`; no unrelated context/design files were added for this
focused review. No application changes, tests, commits, deployment, service
starts, paid requests, SSH, profile writes or DB mutations performed.

## Original blockers (historical; both now closed)

### B1 — P1: manual retry displays the wrong effective profile/defaults

- **UI:** `ui/pages/cabinet/main.ts:484–523` opens install and retry with the
  same selector, initially `0` (standard profile). It paints standard declarations
  at line 523 and permits selecting another profile at lines 517–521.
  Lines 533–548 collect overrides and send that displayed `profile_id`.
- **Backend:** `registry/internal/server/cabinet_manual.go:354–364` replaces
  the submitted profile ID with `existing.ProfileID` whenever an existing
  operation has a nonzero stored profile. Merge/default selection therefore uses
  the original profile, not necessarily the profile shown in the retry editor.
- **Source trace:** first install with profile A, whose `proxy_sni` default is
  `a.example.org`; retry opens displaying the standard profile, or select B with
  `b.example.org`. Leave SNI untouched or reset it. The UI omits the key correctly,
  but the server inherits A's SNI. Declared/custom defaults and secret flags can
  similarly disagree. This violates accurate inheritance/source labeling; the
  generic success message does not disclose the mismatch.
- **Required correction:** preserve the backend's original-profile retry policy.
  Resolve and show the authorized effective stored profile before painting retry
  variables, and disallow a misleading profile switch. If its metadata cannot be
  loaded, show an explicit unavailable state rather than presenting standard or
  unrelated defaults as effective. Leader should resolve any metadata dependency;
  do not silently change retry policy or expose identity secrets.
- **Acceptance:** retry after installation with A displays A's schema/defaults;
  untouched/reset fields inherit A, not the displayed defaults of another profile.
  Profile choice, explanation and actual server behavior agree.

### B2 — P1: masking uses raw names while accepted/submitted names are trimmed

- **Editor:** `ui/lib/vars.ts:34–46` checks conventional sensitivity against
  untrimmed `v.name`, but name validation trims it. Painting at lines 135–145 and
  input handling at lines 196–205 consequently allow a valid, sensitive custom
  key to use a visible text input.
- **Serialization:** profile serialization trims names at line 72; install
  collection trims them at line 246. Thus `password ` or ` authkey` is accepted
  as `password`/`authkey` and submitted under that sensitive key, although
  `isSecretVar` sees the padded name and returns false when `secret` is unset.
- **Source trace:** add a custom installation row, enter `password ` as its name,
  then type a value. No name error is shown, the value input remains `type=text`,
  and the request key becomes `password`. The profile editor has the same issue
  when the secret checkbox is off. Wizard confirmation masks the trimmed key
  (`main.ts:1154–1161`), but that does not repair plaintext entry/re-display.
- **Required correction:** apply the same trimmed, case-sensitive key identity
  to conventional sensitivity classification as validation and serialization.
  Keep schema `secret:true` authoritative too; do not introduce lowercase aliases.
  Ensure initial paint, name edits and reset all use the consistent classification.
- **Acceptance:** accepted whitespace-padded `password`/`authkey` names stay
  password inputs with the existing reveal/pressed-state control, and confirmation
  remains masked. Exact protected names, including padded input, remain rejected.
- **Boundary:** this is ordinary sensitive-variable masking, not evidence of a
  server-issued `node_secret` exposure. No identity-secret rendering was found.

## Initial requirements source review (historical line references)

| Requirement | Evidence / result |
|---|---|
| Four ordinary standard fields, separate `ad_tag` | `vars.ts:29,98–102,138–146`: standard order is `proxy_port`, `proxy_sni`, `proxy_secret`, `ad_tag`; declarations enrich the standard row. `ad_tag_*` and legacy `port` are not aliases. Both profile editors and installation surfaces call this helper. Values are editable; standard/declared installation names are fixed. |
| Untouched/reset omission and explicit empty secret | `vars.ts:171–179,187–195,243–248`: only `data-override=true` rows are collected; reset clears that marker. Empty string values are not filtered out. Cabinet manual install and wizard send this result (`main.ts:545–548,1208–1210,1273–1279`). |
| Backend precedence agrees with ordinary override contract | `control/profiles.go:467–490` copies present request keys first and adds only nonempty absent-key schema defaults. `cabinet_beget.go:540–549` injects proxy defaults only for absent keys. Explicit `proxy_secret:""` and ordinary `ad_tag:""` survive; no ad-tag default is injected. |
| Empty/invalid port and SNI rejected, not fallback | Client checks explicit port and blank SNI at `vars.ts:262–265`; server applies full port/domain validation at `control/profiles.go:504–535`. Profile nonempty defaults are validated at lines 446–460 even when a request would override them. |
| All three install paths use the boundary | VPS: `cabinet_beget.go:384,415`; operation retry: `:934,941`; manual install/retry: `cabinet_manual.go:364,403`. Protected/schema validation precedes quota/paid/install effects. B1 remains a profile-selection mismatch, not a merge-precedence failure. |
| Protected identity enforcement | `control/profiles.go:451–453,477–479,493–500` rejects exact `node_id`, `node_secret`, `registry_url` declarations/request keys, including blanks. Defensive rejection at `cabinet_beget.go:527–531`; `registry_url` always comes from config at line 540, even when empty. |
| No identity-secret value in this editor/metadata | Protected defaults are blanked, value/description controls disabled and reveal hidden (`vars.ts:135–146`); reference at line 153 contains names/explanations only. Custom protected input clears/disables value at lines 199–204. Session metadata whitelist is port/SNI/secret-configured boolean only (`cabinet_api.go:335–348`), mounted with `requireSession` at line 306. No cabinet panel-config request was found in the inspected API/main paths. |
| Secret handling for exact ordinary names | Exact `proxy_secret` is masked regardless of schema flag; declared `secret:true` values are masked (`vars.ts:34–35,144–145`). Registry-secret plaintext is absent from metadata; only placeholder state is provided. Confirmation masks secret overrides (`main.ts:1154–1161`); runner masks identity/proxy/conventional secrets (`provision/provision.go:431,466–488`). B2 is the padded-name exception. |
| Expanded steps in panel/cabinet create and edit | Shared create/edit template always includes steps before variables: panel `profiles.ts:104–113`, cabinet `main.ts:849–859`. Both set `rows=12`, `wrap=off`, monospace and associated syntax/64 KiB help. Shared `modal.css:21–28` supplies wide modal, 260px textarea minimum and scrolling body/fixed footer. |
| Metadata-only admin save does not append/unpublish | `vars.ts:211,229–240` excludes blank synthetic rows and preserves the original baseline/order when unchanged. Panel `profiles.ts:135–146` compares canonical schema/steps and omits both version fields on metadata-only changes. Backend `profiles_admin_api.go:198–228` renames/updates description independently and appends only when a version field is supplied. |
| Cabinet edit name/message agreement | `main.ts:851–853` makes name/description read-only on edit; unchanged steps/vars close with the no-change message and no PUT (`:881–890`). Real edits send both version fields, matching `profiles_user_api.go:110–123`. No unsupported rename claim or false new-version message found in these paths. |
| Admin rename/duplicate message agreement | Admin UI sends `name` and labels rename as metadata (`profiles.ts:96–100,141–154`). Backend calls `RenameProfile` (`profiles_admin_api.go:200–203`); `control/profiles.go:200–218` preserves versions/publication. `users_admin_api.go:54–55` maps profile collision to `duplicate_name` with a profile-specific message, not a username error; panel maps it consistently (`profiles.ts:157–158`). |
| Port/SSH distinction | Exact shared-port warning at `vars.ts:32,149`, beside proxy port in the shared editor. No claim that an installation override changes registry port settings or node eligibility. |
| State preservation and source a11y hooks | Wizard snapshots retain override markers/empty values (`main.ts:1230–1243`); profile switches warn/reset. Manual loading errors retain inputs and block submit (`:503–540`); save/install failures leave edits intact. Shared fields have unique label associations, described hints, invalid states, reveal pressed state and reset/remove focus handling. These are source checks, not keyboard/browser passes. |

## Initial permitted check (not rerun during focused re-QA)

Executed from repository root:

```sh
bash scripts/build_ui.sh -check
```

**PASS:** cabinet, dashboard, links, panel, signin and stats HTML all reported
unchanged. This establishes generated-asset freshness/esbuild compilation only;
it is not full TypeScript checking, a runtime request check or a test suite.

## Browser/runtime still unverified

- Actual payload capture for untouched, reset and explicit-empty values in manual
  install/retry and VPS back/forward; backend effective defaults at execution.
- Both themes' contrast, 320px reflow, small-height modal scrolling, textarea
  overflow, touch targets, Tab/Enter/Space, focus trap/Escape/return focus, reveal
  and reset announcements.
- Real loading/error timing, profile switching and preserved edits after failure;
  metadata-only version count/publication state and duplicate-name responses in
  an authorized dev session.
- No browser opened and no tests/type-check command run. The focused source gate
  now passes; full browser/runtime Phase 3 release approval remains pending.

Return to leader: B1/B2 source gate PASS, overall **partially_verified**;
browser/runtime follow-up remains, not deployment approval.
