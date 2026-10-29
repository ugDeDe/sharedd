# Editable standard variables — Phase 2 UI handoff (dev)

Inputs read: `DESIGN.md`, `editable-standard-vars.md`,
`implementation-standard-vars.md` (authoritative backend contract), and
`fix-profile-control-errors.md`, all under `docs/specs/` except DESIGN.
Existing dirty tree preserved. No commits/pushes, tests, lint, deployment,
service starts/restarts, paid requests, SSH installations or profile/DB writes.

## Files changed by this task

- `ui/lib/vars.ts`: shared profile-default / install-override editor, metadata
  type, standard rows, protected-name diagnostics, masking, reset/collection.
- `ui/components/vars.css`: existing token-based grids extended for installation
  values, small-screen reflow and wrapping reset controls; hidden eye controls
  explicitly stay hidden despite the input-group display rule.
- `ui/pages/panel/views/profiles.ts`: shared help and validation, associated
  steps helper; existing metadata-only version guard retained.
- `ui/pages/cabinet/main.ts`: shared editor on manual install/retry and VPS
  variables step, schema/default loading states, override snapshots, steps
  rows=12, unchanged-profile no-op and persistent wizard launch error.
- `ui/pages/cabinet/api.ts`: authorized session GET `/api/me/install-defaults`.
- `registry/internal/webassets/panel.html` and `cabinet.html`: regenerated only
  through `scripts/build_ui.sh`. Other generated pages reported unchanged.
- `docs/specs/implementation-editable-vars-ui.md`: this handoff.

Nuxt UI adoption: not applicable. DESIGN.md and AGENTS.md require the existing
vanilla HTML/CSS/TypeScript + Go/esbuild pipeline; no framework migration or
dependencies were introduced.

## Behavior and serialization

All four profile create/edit and both installation surfaces display standard
rows in order: `proxy_port`, `proxy_sni`, `proxy_secret`, separate `ad_tag`, then
declared/custom rows. Standard keys enrich existing declarations rather than
appearing twice. `ad_tag_*` and legacy `port` remain separate ordinary names.

Profile forms edit profile defaults, not registry settings. Both retain the
wide modal, visible expanded shell steps (`rows=12`, existing 260px minimum),
monospace/no-wrap commands and syntax/64 KiB helper. Blank synthetic standard
rows are excluded from saved schema unless deliberately populated/annotated.
Existing declaration order and unchanged baseline are preserved for comparison;
opening a profile does not rewrite its schema. Default values are not trimmed:
whitespace is significant under the backend contract.

Admin name/description-only saves omit `steps` and `vars_schema`; therefore they
do not append/unpublish a version. Cabinet does not gain metadata-edit capability:
name and description are read-only on edit; unchanged steps/vars close with a
no-change message and no PUT. Real cabinet version edits still send both version
fields, preserving the existing owner endpoint behavior.

Installation names are fixed for standards and declarations; only custom rows
can be removed/renamed. Sources are text, not color: «Из профиля», «Из регистратора»,
«Для этой установки» (or no default for ordinary undeclared keys). Nonempty
profile defaults take priority when displaying inherited values. The session
metadata GET is requested once per page's installation UI, including when both
surfaces are used. Actual port/SNI arrive from that response; no hardcoded proxy
defaults. Load failure is explicitly unavailable, not known-empty. Metadata
completion updates only inherited values/placeholders and never submitted vars.

`collectInstallVars` serializes only rows marked as explicit overrides:

| Interaction | Request vars |
|---|---|
| Open with inherited registry/profile values; leave untouched | `{}` |
| Edit port to a valid string | `{ "proxy_port": "<entered value>" }` |
| Clear an entered/inherited secret or ad_tag by editing | Key present with `""` |
| Click «Сбросить переопределение» | Key omitted, even if displayed inheritance is blank |
| Custom ordinary key with deliberately empty value | Key present with `""` |
| Explicit blank/invalid port | Inline validation; submission blocked |
| Explicit blank SNI | Inline validation; submission blocked |

Backend retains full SNI, name and size validation. Both surfaces show the exact
shared-port warning beside `proxy_port`; it is not the SSH port and does not
change registry configuration or master eligibility.

## Errors, security and state

Source security review before implementation used the backend whitelist/auth
contract and existing session/CSRF request wrapper. Cabinet never fetches panel
configuration. Registry proxy-secret plaintext is never requested or synthesized:
only `proxy_secret_configured` controls its placeholder. Reveal applies only to
entered/authorized profile values. `proxy_secret` is masked regardless of schema
flag; declared secret and conventional `authkey`/`password` fields are masked too.
Confirmation masks those keys, including explicitly empty secrets. No new secret
logging, local/session storage, identity fetches, or output rendering changes.

Protected keys `node_id`, `node_secret`, `registry_url` have a read-only reference
without values. Legacy declarations produce visible inline diagnostics; their
values are not rendered, and version/install submission is blocked until corrected.
Custom protected names are rejected with inline error and disabled/cleared value.
Unchanged legacy admin metadata saves do not rewrite the stored schema.

Initial manual profile-list load errors retain the editor and offer retry; install waits
for that list rather than treating failure as an empty schema. Values typed while
loading survive successful loading/retry. Selected profile changes warn before
discarding overrides. VPS back/forward keeps row snapshots, including explicit
empty/custom values; profile switches clear snapshots/overrides/confirmation.
Save/install/launch errors retain edits. Wizard launch messages survive polling;
400/503 messages are no longer replaced with blanket region/service advice.
Existing request helpers prefer server messages; no new restart diagnosis.

## Source critique → audit → polish

⚠️ DEGRADED: single-context (no sub-agent tool exposed; compile/build-only scope).
Source-only provisional critique: **3/4**, not a live design-QA score. Reused
token-based controls, distinct default/override labels and explicit source/reset
semantics; no redesign. Source audit covered label associations, inline
`aria-invalid`/described errors, focus on variable validation, reveal pressed
state, protected values, modal reuse and responsive grids. Polish addressed
wrapping reset controls, hidden reveal specificity, persistent errors, metadata
arrival without clobbering input and single-binding wizard footer actions.

Full Impeccable/browser gate is **pending**, not PASS: no detector/browser/a11y
execution or independent reviewer was run under the compile/build-only scope.
Impeccable context reported missing PRODUCT.md; no unrelated root context files
were added. Designer should inspect live layout/contrast, 320px and small-height
modal scrolling, keyboard/reveal/reset, profile switching, error preservation,
and omitted-versus-empty payloads during Phase 3 with appropriate authorization.

## Permitted verification and return to leader

Passed from repository root:

```sh
bash scripts/build_ui.sh
bash scripts/build_ui.sh -check
git diff --check
```

Passed after final regeneration from `registry/`:

```sh
go build -mod=readonly ./...
```

Source search found no `console.log` in changed UI files and no privileged panel
config request in cabinet. Esbuild compilation is not full TypeScript typechecking;
no installed `tsc` was found and no tooling/config was added. No tests or lint run
by explicit request. Verification: **partially_verified** (compile/static only).
To reproduce allowed checks, use the exact commands above.

Implementation complete — return to **leader** for routing to **designer Phase 3
design QA**, not deployment. Existing operation-list retry confirmation remains
unchanged; the manual install/retry editor and VPS wizard use the new helper.

## Focused P1 corrections — qa-editable-vars.md B1/B2 (dev)

Both source-confirmed P1 findings are **fixed in source**. This supersedes the
manual-retry behavior described above; the original QA report remains historical,
not a runtime approval. Existing work preserved; no tests, commits or push.

### B1 — original manual-operation profile

- `registry/internal/server/cabinet_manual.go`: every existing operation overrides
  the submitted profile with its stored `ProfileID`, including legacy zero
  (standard-profile lookup). First installation still honors the submitted choice.
- `ui/pages/cabinet/api.ts`: optional `Operation.profile_id` models the existing
  server response without assuming older/missing metadata means profile zero.
- `ui/pages/cabinet/main.ts`: manual retry resolves `server.operation.profile_id`
  against the authorized published/own profile metadata. It displays only that
  profile in a disabled selector, explains the lock, and paints its schema/defaults.
  Explicit zero resolves the actual `sharedd-node` metadata; missing operation ID,
  missing profile or disabled unpublished own profile blocks the editor/submission.
- Loading/error states never paint a substitute retry schema or invented defaults.
  The readiness gate is independent of selector `disabled`, so a loaded locked
  retry can submit; submission uses the operation ID, not a selectable replacement.
  Initial installs retain profile selection, switch warning and pending edits.

### B2 — canonical sensitive names

- `ui/lib/vars.ts`: `isSecretVar` trims names exactly like validation and
  serialization, without lowercasing. Padded `password`, `authkey`, `proxy_secret`
  and schema `secret:true` entries use password inputs on paint, name edits and
  reset; the existing explicit reveal/pressed-state control remains available.
- Protected-name checks on paint and asynchronous registry-default arrival also
  use trimmed names, preventing a padded protected declaration from displaying
  its value; existing protected-name rejection remains intact.
- `ui/pages/cabinet/main.ts`: confirmation trims both schema-secret names and
  override keys for classification, rendering bullets rather than secret values.
  Profile-load errors are fixed text; reviewed variable-validation errors report
  names/constraints, not submitted values. No new secret-bearing error output,
  logging, storage, endpoint or auth behavior was introduced.

### Verification and remaining boundary

Executed successfully after the corrections:

```sh
# repository root
bash scripts/build_ui.sh
bash scripts/build_ui.sh -check
# registry/
rtk go build -mod=readonly ./...
```

Only `registry/internal/webassets/cabinet.html` and `panel.html` regenerated;
dashboard, links, signin and stats were unchanged. Source search found no
`console.log` in UI TypeScript. Focused source review covered locked-profile
readiness/error handling, inheritance omission, canonical masking, confirmation
and protected values. No dependencies, CSS or Nuxt UI migration (not this stack).

Verification: **partially_verified** — UI compilation/freshness and registry build
passed; no tests, lint, full TypeScript check, browser/runtime checks or independent
QA were run. No new live critique score or full polish-gate PASS is claimed.
Follow-up QA remains required for actual retry payload/default agreement, padded
secret entry/reveal/reset and confirmation/error rendering in an authorized session.
