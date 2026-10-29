# Editable standard variables — Phase 1 (dev)

## Scope and evidence

Small reuse-first specification; no redesign, application edits, tests, commits,
or live installation. Keep `DESIGN.md`, tokens, shared fields, modals, and layout.
Sources: `ui/lib/vars.ts`, `ui/components/vars.css`, panel
`views/profiles.ts`, cabinet `main.ts`/`api.ts`, and
`docs/specs/fix-profile-control-errors.md` (§ provisionVars precedence).
`registry/internal/server/cabinet_beget.go` confirms the precedence below.

Current profile create/edit forms already contain steps textareas. Installation
forms differ: manual install/retry reuses the schema editor; the VPS wizard
renders declared values and extra rows separately. Neither should require users
to add the ordinary standard keys manually. This is not evidence of a reproduced
browser visibility failure.

## Required precedence; backend dependency, not a UI-only promise

| Key | Current behavior | Required installation behavior |
|---|---|---|
| `proxy_port` | Server overwrites schema/request with `[shared_proxy].port` | Explicit installation override > profile default > registry default |
| `proxy_sni` | Server overwrites schema/request with `[shared_proxy].tls_domain` | Explicit installation override > profile default > registry default |
| `proxy_secret` | Present merged key wins, including explicit empty; otherwise registry secret | Preserve installation override > profile default > registry default |
| `ad_tag`, valid `ad_tag_*`, declared/custom ordinary keys | Request > non-empty schema default; no ad-tag injection | Preserve; no invented default or alias |
| `node_id`, `node_secret` | Server-issued identity overwrites caller values | Protected, never user-overridable |
| `registry_url` | Non-empty `panel.public_url` overwrites input; empty config currently leaves copied input | Protected; reject caller/schema override, including when config is empty |

Making port/SNI editable without changing server precedence would silently discard
the user's input. Leader/backend must resolve that dependency before claiming the
feature works. Preserve identity/auth/ownership/secret recovery; do not turn the
existing empty-public-url loophole into an identity override feature. This spec
does not define a new endpoint or authorize changing registry policy.

Always show by `proxy_port`:
> «Порт установки должен совпадать с [shared_proxy].port регистратора.
> Изменение здесь не меняет настройки регистратора или допуск ноды к мастерству.»

The proxy port is **not** the server's SSH port. A mismatch may make a node
ineligible; never promise that changing installation variables changes eligibility,
health policy, shared machine configuration, or the agent's port enforcement.

## Installation editors (VPS wizard and manual install/retry)

- Use existing `.field`, `.vars-editor`, `.vars-row`, `.input-group`, `.in-btn`,
  `.btn`, `.help`, `.banner`; extend the shared vars helper minimally for an
  installation-value mode rather than build a new editor/library.
- Under «Переменные установки», always show, in this order: `proxy_port`,
  `proxy_sni`, `proxy_secret`, **separate** `ad_tag`, then remaining declared keys
  and custom rows. The standard/default profile and an empty schema still show
  these four. Merge by exact key: a declaration enriches the standard row, not a
  duplicate. `ad_tag_*` remains a separate ordinary key, not an alias of `ad_tag`.
- Standard/declared names are fixed in installation mode; edit values only.
  Retain «Добавить переменную» for custom names/values and existing validation.
  Do not offer to remove a standard field. Installing does not edit the profile.
- Label values «Значение для этой установки», not «значение по умолчанию».
  Profile editors separately edit «Значение по умолчанию профиля».
- Identify inheritance with text: «Из профиля» / «Из регистратора» /
  «Для этой установки». A small existing ghost button «Сбросить переопределение»
  restores inheritance. Untouched/reset fields are omitted from request vars;
  explicit empty overrides remain present where the server allows them. Do not
  serialize inherited blanks as overrides or drop intentional empty strings.
  Invalid empty port is a validation error, not silent fallback.
- Show actual registry defaults only if an authorized server response supplies
  them. Do not hardcode `443`, a sample SNI/secret, or treat schema defaults as
  registry values. Existing profile payloads expose `vars_schema`, not a proven
  installation-defaults metadata contract. Until supported, show
  «Значение регистратора недоступно в интерфейсе; применяется сервером» and
  still permit an explicit override. Distinguish unavailable from known empty.
- Registry proxy-secret values may be revealed only if the authorized contract
  explicitly permits returning them; otherwise show «Задаётся регистратором».
  Never fetch privileged panel configuration from a cabinet session.
- `proxy_secret` is always password/reveal, regardless of schema `secret` flag.
  Also mask declared/custom secret fields and conventionally sensitive keys.
  Reuse the existing eye control; label it «Показать значение <key>» /
  «Скрыть значение <key>» with an announced pressed state. Confirmation and
  operation output stay masked; no secret values in logs, errors, or persistence.
- Preserve unsent edits through wizard back/forward. Profile switching must not
  silently submit stale profile-specific values; clearly reset those values on
  change, warning before discarding edited values. Loading/error messages must
  not masquerade as an empty schema; preserve edits on save/launch failure.

## Profile editors (panel and cabinet, create and edit)

- Keep the existing wide modal and order: name, description, **steps**, variables,
  action footer. Steps are always expanded, not in an advanced accordion/tab.
  Preserve the shared 260px minimum height and set `rows=12` in both editors.
  Use the existing monospace textarea, visible «Шаги (shell-команды)» label,
  associated syntax/64 KiB helper, and normal keyboard editing.
- Show the same four ordinary standard rows plus declared/custom variables;
  permit editing their profile defaults. Do not auto-persist synthetic rows on
  opening an existing profile or turn unchanged metadata edits into new versions.
  Retain current versioning/publication rules and existing owner permissions;
  do not add rename/description capabilities as incidental scope.
- Replace misleading blanket advice that all proxy fields are protected. Explain
  the target ordinary-variable precedence only once the backend supports it;
  before then disclose the current port/SNI overwrite limitation.

## Protected reference and accessibility

Separate a compact read-only «Системные переменные» reference from editable rows:
`node_id` — «Выдаётся сервером»; `node_secret` — «Выдаётся сервером; значение
недоступно»; `registry_url` — «Задаётся регистратором». Show actual non-secret
values only if authorized and available. **Never return `node_secret` in defaults
metadata, render its value (even in a hidden/password field), or add reveal/copy
for it.** No editable fields for these keys. Reject protected custom/declaration
names with an inline error; legacy declarations get a visible diagnostic, not
silent rewriting of stored profiles. Server enforcement is required too.

All fields have visible labels and unique associations; help/errors use
`aria-describedby`, invalid fields use `aria-invalid`, invalid submit focuses the
first affected field. Add/remove/reveal/reset work via Tab/Enter/Space; removal
returns focus to a neighboring row or Add. Reuse modal focus trap/Escape/return
focus and polite save/loading announcements. Steps remain reachable at small
heights with the existing scrolling body/footer; 320px reflows variable rows to
one column without page overflow. Long shell lines scroll inside the textarea,
not the page. Preserve contrast and focus tokens in both themes.

## Handoff / verification

Acceptance: both installation surfaces show all four standard fields exactly
once with declared/custom keys; profile defaults and installation overrides are
distinct; real supplied defaults are identifiable; secrets are masked/revealable
only where permitted; steps are prominent in all create/edit forms; protected
identity cannot be supplied; the shared-port warning is visible.

Verification status: **partially_verified** — source/spec cross-check only.
No browser QA, tests, build, runtime precedence change, or API metadata capability
verification performed. Backend precedence and protected-key enforcement are
explicit implementation dependencies, not delivered behavior.
