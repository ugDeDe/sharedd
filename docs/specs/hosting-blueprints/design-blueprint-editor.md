# Blueprint editor — Phase 1 design / frozen-backend handoff

**Status: partially_verified (source inspection only).** Docs only; no app edits,
builds, tests, browser/provider/SSH operations, or delegation in this task.
This document is the sole new artifact; `DESIGN.md` and backend stay unchanged.

## 1. Scope and evidence

Design an operator's full-width workspace, reached from cabinet **«Мои профили»**,
not a larger modal and not a new application. Keep vanilla HTML/CSS/TypeScript,
DOM blocks + inline SVG control wires; no framework, graph package or npm build.
Reuse existing cabinet tokens/components. Saving a design never runs it.

Read: root `DESIGN.md`; `05-blueprint-profiles.md` §1–§5 and templates §6–§7;
`docs/specs/cabinet.md`; `design-cabinet-hosting.md` and
`specs/cabinet-hosting-ui.md`; current cabinet source; actual
`blueprint_api.go`, `blueprint_runs_api.go`, `profiles_user_api.go`,
`profiles_admin_api.go`, management handlers in `cabinet_cloud.go`, and
`internal/blueprint/{model,registry,validate}.go`. **Actual handlers/DTOs win over
older proposed contracts and stale stage comments.**

Partial-work inspection: dirty cabinet files contain hosting UI plus the old
`profileEditor()` text modal; `api.ts` still has text-only profile payload types.
No standalone graph-editor module/view or graph/run API wrapper was found in
`ui/pages/cabinet/`. Preserve existing work; do not assume cancellation reverted it.
Impeccable skill loaded; local context script/PRODUCT.md absent and access to its
global reference files denied. No context files were created outside this scope.

## 2. Integration locations (future implementation, not edits in this task)

| Location | Bounded integration |
|---|---|
| `ui/pages/cabinet/index.html` | Keep `#view-profiles`, `#profiles-create`, `#profiles-body`, `#profiles-published`; add a sibling editor view and run-detail view inside existing `main`. Retain desktop/mobile Profiles nav, theme, session shell. |
| `ui/pages/cabinet/main.ts` | Extend `route()`/hash parsing **before** ordinary Profiles dispatch; integrate `renderProfiles`, `profileEditor`, `saveProfile`, `copyPublishedProfile`, `removeProfile`, delegated `[data-pedit]`/`[data-pcopy]` handlers. Keep text modal for confirmed text versions. Reuse modal/toast services. |
| `ui/pages/cabinet/blueprint-editor.ts` (proposed new module) | Own editor state, DOM/SVG, inspector, undo, validation, run detail. Small entry/leave/poll integration; do not grow the already-large `main.ts` with the editor body. |
| `ui/pages/cabinet/api.ts` | Extend DTOs from actual JSON; add blocks/validate/numeric-version/run wrappers using existing `req`, CSRF and `newIdemKey`. Preserve `diagnostics` on 422 errors (currently discarded). Expand management PUT with `profile_version_id` and `inputs`. |
| `ui/pages/cabinet/page.css` | Scoped `.blueprint-*` geometry; `.wrap` full-width modifier **only while editor/run view is open**. Reuse imports/components, no global token changes. |
| `ui/pages/cabinet/node-detail.ts` | Replace permanently disabled Profile radio/help in `managementHTML()` and submit handler with the supported form in §10. No changes to unrelated statistics/actions. |
| `ui/lib/dom.ts`, `ui/lib/vars.ts`, `ui/tokens.css`, `ui/components/*.css` | Reuse `esc`, focus-safe rendering, vars helpers, tokens and controls; not a second design system. |

Client hash routes to add: `/cabinet#/profiles/editor/new`,
`#/profiles/editor/{profile_id}?version={positive_integer}` and
`#/profiles/runs/{run_id}`. These are **client views**, not new HTTP endpoints.
Profiles nav stays active. Parse query separately (existing `currentView()` does
not handle every query form). Back returns to `#/profiles` and its invoking row.
Never directly edit generated `registry/internal/webassets/cabinet.html`.

## 3. Layout, hierarchy and existing tokens

Night-time operator checking a potentially paid workflow needs stable, legible
controls, not a presentation canvas: preserve the existing dark-default/light
toggle and restrained Azure accent.

- Full available width: remove the cabinet's 1280px cap only for this workspace;
  20px outer gutters, 12px at ≤640px. No new server page/mount required.
- Top: «Мои профили» back link; h1 «Редактор графа»; profile name, provider,
  numeric version, «Есть изменения»/«Сохранено» status. Actions: «Проверить»,
  **«Сохранить версию»** (one primary), secondary «Запустить…» when supported.
- Below: persistent safety/gate notice; toolbar «Добавить блок», «Шаблоны»,
  «Отменить», «Повторить», «Показать всё», zoom −/percentage/+; entry selectors
  «Первичное создание» and «Наблюдение». These select `entries`, not extra nodes.
- ≥1200px: palette 220px | flexible canvas | inspector 320px. Use `min-width:0`
  on center; 14px gutters, 18px panel padding. Canvas fills remaining viewport
  height with accessible independent scrolling for long panels; page may scroll.
- 721–1199px: palette becomes a button-opened drawer; inspector is 300px or
  explicitly opened, never squeezed over the canvas. ≤720px: «Схема / Блоки /
  Свойства / Проверка» tabs, one panel at a time; default **Blocks list** on first
  narrow-screen visit. Same editing/connect controls, not a read-only mobile UI.
  Use existing `.nd-tabs` keyboard pattern; keep above mobile navigation/safe area.
- Collapsible bottom diagnostics strip: summary + ordered findings, not a toast
  that disappears. Run view reuses the center for a **read-only** graph/list.

Surfaces: `--bg` canvas, `--surf` panels/nodes, `--surf-2` headers/inputs,
`--line/--line-2` dividers. Text: `--fg` important copy, `--fg-2` metadata;
do not use low-contrast `--fg-3` for port names, form help/errors or placeholders.
Use existing `--sans`/`--mono`, 17px h1, 13.5px body/node title, 12.5px labels;
machine IDs/refs mono, wrapping permitted in inspector. `--gap`, `--pad`,
`--r-sm/--r-md`, `--ctl-h` remain authoritative. No new palette/font/shadows.
No decorative glass, gradient, oversized headings, side-stripe node accents.
Essential borders, wires and focus must reach 3:1; text 4.5:1 in both themes.
Existing subtle divider tokens alone are insufficient for interactive port edges.

## 4. Palette, blocks and wire representation

Fetch `GET /api/me/blueprint-blocks` → `{schema_version,blocks}` once on entry.
Normalize nullable port/param arrays to empty lists for rendering.
Palette uses only returned types compatible with the chosen provider; searchable
Russian title + exact mono type + effect badge. Group for scanning:
«Сервер» (create/ready/distinct_ip/delete/absent), «Установка и допуск»
(install.text/pool.admit), «Политика» (event.ip_ban/policy.guard/observe.arm/
terminal.stop), «Переключение» (resource.fence/resource.switch), «IP Beget» (`ip.*`).
Effect labels: `read` «чтение», `mutation` «изменение», `purchase` «платное действие»,
`control` «завершение». Badge text carries meaning, not hue alone.

Each palette item has **«Добавить»**; optional dragging is an enhancement.
Add near canvas center/selection, assign unique stable ID, select and focus its
node heading. Empty draft: «Добавьте блок или выберите шаблон. Сохранение станет
доступно после проверки схемы». Local incomplete drafts are allowed, not server
versions. Display counts: nodes /128, connections /256, run-input declarations /32.

HTML nodes: about 240px wide, content height, 12px inner gap; title, exact type,
ID, effect, entry chips and diagnostics count. Selected outline uses `--acc`,
keyboard focus uses existing 2px focus outline with offset (selection ≠ focus).
No raw editable HTML/source inside nodes. Header is drag handle; fields/buttons
do not start dragging. Initial/observe badges can share a node if schema permits.

**Control**: one labelled receiving `exec` affordance; output buttons strictly
from `ports.control`. Edge JSON is exactly `{from,port,to}`: there is no target
port key, edge ID, data type or arbitrary failure edge. Ordinary output `ok`;
`policy.guard` **only** `active`/`stop`; `ip.order`, `ip.attach`,
`ip.switch_advertised` `ok`/`fallback`. `observe.arm` and `terminal.stop` have
**no** control outputs. Do not invent `error`, `success`, `retry` or an arm loop.

SVG behind HTML shares one world transform; cubic control wires, 2px stroke,
arrowhead and visible output label. Default `--fg-2`, selection `--acc`, invalid
`--bad` plus error text; no animated flowing particles. SVG is `aria-hidden`;
equivalent visible **Connections** list supplies select/remove buttons and names
«{from} · {port} → {to} · exec». Larger transparent pointer hit stroke is optional.
**Data is not a second edge array**: show typed input/output names on the node,
edit references in inspector (§6); no fake draggable data wires in this slice.

Node positions/pan/zoom are **UI-only memory**, separate from strict blueprint
JSON. No `position`, `ui`, `viewport`, `label`, `disabled` or unknown keys in API
payload. On reload use deterministic left-to-right layout by control dependency,
separate initial/observe lanes, stable node-ID tie-break; shared nodes appear once.
No server layout persistence, autosave or new layout endpoint.

## 5. Pointer, keyboard and focus contract

- Drag header moves node; drag empty canvas / Space+drag pans; pointer capture
  prevents lost release. Single drag = one undo item. No drag needed to operate.
- Wheel pans; toolbar zoom 25–200%, pointer-centered modified wheel only when
  canvas intentionally focused. Do not capture browser Ctrl/Cmd zoom or page
  scrolling outside it. «Показать всё» fits graph; focus/selection can center an
  offscreen node. No inertial movement; reduced motion is immediate.
- Keyboard alternatives: palette Add; node-list selection; inspector position
  controls «Левее/Правее/Выше/Ниже» or x/y numeric fields (UI-only). Canvas arrow
  keys pan **only with canvas focus**; selected node arrows move by 10 world px,
  Shift+arrow 50; toolbar provides all zoom/pan-reset operations.
- Connect: select source output with Enter/Space → announce «Выберите следующий
  блок» and highlight eligible targets; choose target node/exec with Enter,
  Escape cancels and returns focus to source. Inspector equivalent: output
  select + target select + «Соединить»; source/target can be anywhere offscreen.
  One target per output; replacing it requires explicit replace, not silent loss.
  Reject self/cyclic/fan-out connections locally, then server checks dominance.
- Delete/Backspace removes selected edge/node **only outside editable controls**;
  toolbar button is equivalent. Node removal dialog names affected control edges,
  data bindings and entry roles; cancel is default. Confirm removes those refs,
  leaves required inputs/entries visibly unset; never silently rewires a graph.
  Return focus to next node/list or Add when empty.
- Ctrl/Cmd+Z and Shift+Ctrl/Cmd+Z undo/redo local graph/param/binding/entry/layout
  edits; buttons always available. Native text undo wins while typing. No undo
  of save, run, policy enable/cancel, provider actions. No unmodified global
  single-letter shortcuts or `role=application`/keyboard trap.
- DOM order remains stable: named palette, canvas region, node list, inspector,
  diagnostics; heading focus on route entry, not every fetch. Selected node
  opens inspector without stealing typing focus. Escape closes drawers/dialogs,
  cancels connection mode first. Dialogs trap focus/restore invoker; tabs rove.
  Minimum hit area 24px, prefer 34px controls/44px on touch even for tiny ports.

## 6. Inspector: strict params, typed bindings and declarations

Inspector tabs/sections: «Блок», «Входные данные», «Связи». No selection → graph
settings: name (1–100 chars), description metadata, provider, initial/observe
node selects, run-input declaration table `{name,type,required}` (max 32).
Names/IDs match `^[a-z][a-z0-9_]{0,63}$`, node IDs exclude `input`/`run`.
Keep node IDs read-only after add; removal/re-add avoids broken hidden refs.
Input rename/removal must update affected refs atomically or show them unset.
Changing provider retains incompatible existing blocks with diagnostics; never
silently deletes blocks or changes the workflow to make it executable.

Use returned `params_schema` **array of `{name,type,required,enum?}`**, not a JSON
Schema form renderer. Enum → native select; integer → numeric input; other
declared strings → text. No params → «Нет параметров» and serialize `{}`.
`purpose`: initial/replacement; `family`: IP_V4; terminal `reason`: policy_inactive.
Timeout starter values: ready/admit/absent 600s, install/switch_advertised 1200s.
Positive integer editing is a UI safety rule; actual registry exposes no min/max
and current validator only checks integer (do **not** claim server max 86400).
Retry section only for `retry_class:"safe-read"`: optional `{max_attempts:1..5,
backoff_seconds:1..300}`; otherwise «Повтор блока запрещён». Do not promise the
engine scheduler implements exactly this authored retry schedule.

For every `ports.inputs` entry, show label + exact type and a source selector:
«Вход запуска» → declared `input.name`; «Выход блока» → compatible `node.port`;
«Текущее состояние» → `run.current_server` only on observe-safe paths;
«Константа» only for string/finite number/bool. Serialize **exactly one**
`{ref:...}` or `{literal:...}` into `node.in[name]`. Native checkbox for bool,
number for numeric; no coercion or expressions/interpolated shell/eval/URLs.
Do not offer literal config/server/operation/ssh_target/secretref or secrets
hidden inside string constants. No password/key editor in the graph.

Offer compatible types as candidates, not a guarantee of branch availability;
server diagnoses inactive-path dependencies. `run.policy_revision` and
`run.episode_watermark` are accepted by validator but **not resolved by the
current engine**; do not offer them for new bindings; preserve loaded values
with execution-blocking warning. No arbitrary server/secret handle input form:
there is no credential/opaque-handle picker API for it in this slice.

## 7. Validate, version-save and conflict handling

| Intent | Actual request / result |
|---|---|
| Validate structure | `POST /api/me/profiles/validate` `{format:"graph",blueprint}` → **200** `{valid,diagnostics}` including `valid:false`; no effects/storage. Optional `inputs` validates an instance too; omit it for authoring-only check (not `{}`). |
| Create private graph | `POST /api/me/profiles` `{name,description,format:"graph",blueprint}` → **201 profile metadata**, not a version DTO. No `steps`, `published`, run inputs/secret values. |
| Append version | `PUT /api/me/profiles/{id}` `{format:"graph",blueprint,expected_version:N,name?,description?}` → **200 profile metadata**; send explicit loaded **numeric version** N every time, never its PK. |
| Read immutable version | `GET /api/me/profiles/{id}/versions/{positive_number}` → `{profile_id,version,profile_version_id,format,steps?,blueprint?,vars_schema?}`. `latest` is not accepted. |

Store draft, loaded version baseline and validation result separately. Editing
invalidates validation status immediately; validate the exact snapshot being
saved, disable mutation while request is pending, ignore obsolete validate
responses. Server validates again on save. Local incomplete/invalid graphs cannot
be saved as server drafts. Graph payload **and full request envelope** must fit
256KiB UTF-8; 128 nodes/256 combined control+node-output refs; not JS char count.

Diagnostics `{node_id?,port?,code,message}` are blocking findings (no severity
field). Render escaped Russian summary + safe original detail/code. Selecting
one reveals/centers/focuses node or field; global finding focuses graph settings.
Server valid means «Схема корректна», **not** «Готов к выполнению»: runtime gates,
ownership, connection/config/text-version availability remain separate.

Success: refresh own metadata, GET the numeric saved version and capture returned
`profile_version_id`; new runs use that immutable PK. Keep the loaded baseline
until read-back succeeds. Saving graph appends immutable content and resets
published; rename/description-only PUT does not append. Treat profile metadata
name and `blueprint.name` as separate: updating graph name changes content.

409 `revision_conflict`: keep draft/history, show «Профиль изменён в другом окне»;
actions «Загрузить актуальную» (confirm discard), «Сохранить отдельной копией».
Never silently replace expected_version/retry or force overwrite. The optional
backend check is not an atomic compare-and-append transaction; frontend must
not advertise an exclusive lock/multi-writer guarantee. After network ambiguity,
reload metadata/version and compare saved content before any new POST/PUT;
authoring endpoints have **no idempotency support**, so no automatic resend.
Dirty back/hash/browser navigation → «Сохранить / Не сохранять / Остаться»;
browser unload uses native warning. No localStorage persistence of graphs/inputs.

## 8. MyProfiles, templates and frozen API differences

Keep own/published lists and counts `enabled/max`; add «Создать граф» beside the
existing text create action. Format badge appears only after version is known;
unknown ≠ text, missing `steps` ≠ empty text. Own row's `versions` count resolves
latest numeric version in current contiguous append-only storage, then GET it;
if unavailable show retry/version chooser, never open a graph in shell editor.
Published rows lack **both format and version/count**: provide an explicit
numeric version opener (blank until entered). No fabricated `latest`/versions
list route, guessed global version PK, or destructive copy of unknown format.

| Backend fact (source) | Required UI decision |
|---|---|
| List and POST/PUT profile DTOs omit format, blueprint and version PK (`profileAdminJSON`); published DTO is only id/name/description/vars_schema | Hydrate numeric version as above; after save re-read; never infer format from source length. All legacy install selectors in `main.ts` must exclude resolved graphs; unresolved entries are not executable text choices. |
| `/profiles/{id}/copy` always uses `AddProfileVersion(...Steps...)` (text) | **Never call it for graph/unknown.** Graph copy = read exact selected visible version → local copy/new name → graph POST on explicit Save. Copy no run/policy/connection credentials. Legacy copy only when latest text format is positively known. |
| Admin handlers remain text-authoring; user graph publish is not implemented | No cabinet Publish or promise that admin publishing is available. Own valid private graphs are allowed by run/management owner checks; do not require publication for those. |
| DELETE checks pinned policies/nonterminal runs | Confirm delete by name; 204 removes row, 409 `profile_in_use` keeps it and links to node management/known run. Historical terminal run may remain readable after its graph was deleted; say «Версия удалена» with checkpoints still shown. |
| `GET /api/me/profile-runs` and `GET /api/me/operations/{id}` are **not mounted**; operation list lacks `run_id` | No invented global run history or operation-detail fetch. Run access uses acknowledged ID/URL or explicit «Открыть запуск по ID» form; optional session-known IDs labelled «Открытые в этой сессии», not all runs. |
| Management GET omits `active_run_id` | Show «Не предоставляется API», not «Нет запусков»; do not discover runs by scanning IDs or depend on absent field. |
| Catalog returns 501; estimate/ip-options are not mounted in current cloud mux | Manual config fields; no quote, tariff picker or fake estimate response. 404/501 disables unavailable capability, not all authoring. |
| Validator accepts Beget/IP registry types; engine rejects **all `ip.*` and Beget graph create** | Editing/validate/save allowed; **Run and Profile enable disabled for Beget**. Capability badges are not executable guarantees. No silent Nodehost substitution or replacement fallback. |

Templates are local editor starters from
`registry/internal/blueprint/testdata/nodehost_replacement.json` and
`beget_ip_or_replacement.json` (same graphs as 05 §6/§7); future frontend bundles
their content, never fetches these filesystem paths or an invented templates API.
Buttons: «Nodehost · замена VPS», «Beget · IPv4 или замена VPS», «Пустая схема».
Preview provider/effects/entry paths; replace dirty graph only after confirmation.
Apply creates **only local draft**; validate before server save, no run/mode change.

Persistent Beget badge/banner and disabled-action explanation, including template:
**«Только проектирование. Смена IP Beget не выполняется: блоки ip.* не реализованы;
проверка primary IP не закрыта. Создание VPS через граф Beget также недоступно.»**
Do not invent promote-primary endpoint/port. No amount or successful validation
can remove this gate. «Открыть прежнее создание Beget» may link `#/create`, but
is a separate manual flow, not execution of the selected blueprint.

## 9. Explicit run creation and run view

«Запустить…» opens a distinct review form for a **saved**, resolved Nodehost
graph PK (dirty draft must save first). Never POST by selecting a template,
saving, validating, loading a page, refreshing balance or receiving a ban badge.
Keep both actions visually/semantically separate.

Run-instance fields (not stored in blueprint):

| Input | Control and actual value |
|---|---|
| `connection_id:number` | Owned matching-provider connection select from existing hosting list; show label/ID, verified/capability and balance freshness/scope. No credential entry here. |
| `configuration_ref:config` | **Current engine takes an inline Nodehost config object**, not `cfg_81`/stored snapshot ID. Structured fields `name`, `cpu`, `cores`, `ram`, `disk`, `diskType`, `os`, `location`, optional `period`, `networkSpeed`; no hostname (old frontend type contains it, actual CreateRequest does not). Required text name ≤100, CPU/diskType/OS/location ≤64; cores/RAM/disk positive integers; RAM/disk labelled GB. Period blank or 1d/3d/1w/1m/3m/6m/1y; optional speed ≤64. Manual values, no invented provider presets/prices. |
| `installation_profile_version_id:number` | Choose visible text profile and **numeric version**, GET it, require `format:text`, submit its `profile_version_id` PK. Use its `vars_schema` for non-secret install vars; never send profile ID/version ordinal as PK. |
| Other primitive declarations | Required/optional labelled string/number/bool controls; optional unset omitted. No arbitrary JSON/opaque handle or literal secret input bypass. |

Secret install vars: no literal values in graph, templates, URL, logs, exports or
session cache. This slice offers no secret override; retain server-configured
inheritance. If a required secret has no supported server source, block launch
with explanation rather than a password-in-JSON workaround. Standard proxy port
comes from `installDefaults`; no made-up 443 or per-node override of registry port.

Default entry `initial`, no resource_id; optional labelled deployment `node_id`
is a durable deployment identity, **not** a server label. Preserve it and the
same request body/key across uncertain retries; do not blank it or change key to
evade `deployment_unresolved`. Existing-node observe uses §10 policy input
snapshot, not an arbitrary initial/run selector with editable runtime handles.

Confirmation copy:
«Сохранение профиля не запускает его и не включает автоматические расходы.
Этот запуск разрешает перечисленные платные действия в подключении Nodehost.
Цена API не предоставлена. Ошибка или неизвестный результат не разрешают
повторную покупку». Explicit unchecked consent and «Создать VPS и запустить».
Review lists reachable purchase/mutation blocks and conditional branches; the
VPS-specific button applies to the create template, otherwise «Запустить граф».
Balance is informative, never spending authorization. Initial completion does
not promise automatic policy creation/activation: current `observe.arm` only
updates an existing policy resource; policy enable is a separate action.

`POST /api/me/profile-runs` body `{profile_version_id,entry,inputs,vars?,
node_id?,resource_id?,confirm:true}` + CSRF + `Idempotency-Key` → **202**
`{operation_id,run_id,state,resource_id}`. Show «Запуск принят», navigate to known
run route; no «сервер готов». Same logical intent keeps same key/body; changed
intent needs fresh review/consent, never automatic new purchase after unknown.

Run detail polls `GET /api/me/profile-runs/{id}` every 5s while visible (single
request at a time); stop on leave/terminal, manual refresh after stale/error.
Header: run/operation IDs, pinned profile ID+version PK, entry, run revision,
policy revision, resource link `#/resources/{id}`, node link if returned.
No made-up creation time, total cost, refund, branch history or progress percent:
DTO supplies none. Show cost «Не предоставлена API» and last-update time locally.
Run DTO has version **PK**, not version ordinal: render cached verified graph
only when its PK matches. Otherwise let user choose a numeric profile version
and compare the returned PK; no PK-as-version guess or invented PK read route.
Until resolved, checkpoint list is the complete usable fallback.

States: pending/running/reconciling → `.badge.info`; waiting_event/waiting_retry/
paused → `.badge.warn`; needs_attention → `.badge.special`; failed → `.badge.bad`;
succeeded → `.badge.ok`; cancelled neutral. Always Russian label + machine code
in details. `waiting.type/after_episode_id`, `next_wake_at` are optional;
timestamps are Unix seconds. No checkpoints → «Шаги ещё не зафиксированы».

Checkpoint list/table: block node ID, invocation, state, attempt, error,
`next_attempt_at`; escaped allowlisted output handles and returned `__port` if
present. Highlight only evidenced branch; unvisited ≠ successful/skipped.
States ready/claimed/dispatched/confirmed/rejected/unknown/waiting distinct;
unknown means «Результат неизвестен — сверка/разбор», not generic retry.
No raw JSON dump of arbitrary output/vars/secrets. If pinned graph unavailable,
retain journal and controls supported by evidence, graph unavailable note.

- Cancel: confirm «Остановить дальнейшие шаги? Отправленные действия не
  отменяются; сервер и расходы могут остаться». POST `…/{id}/cancel`
  `{expected_revision}` + fresh logical command key → 202; render
  `cancel_requested` until detail confirms cancellation. No refund/delete claim.
- Resume selected checkpoint: POST `…/{id}/resume`
  `{expected_revision,checkpoint_id,decision}` + key. `reconcile` offered only
  claimed/dispatched/unknown; `retry_safe` conservatively only known
  `effects:read` **and** `retry_class:safe-read`, never confirmed checkpoint;
  `acknowledge` means «Отметить просмотр», stays needs_attention, not recovery.
  Do not add Retry purchase/SSH, force success, restart graph or fallback create.
- 409 revision/unsafe_resume: keep journal, re-GET and require another deliberate
  choice. No automatic revised command/new key. Resume/cancel 202 is acceptance.

## 10. Node management: explicit Profile inputs, not implicit activation

At existing node «Управление», GET `/api/me/nodes/{id}/management`; radio
«Ручной / Профильный». Profile expands saved graph profile/version selection,
resolved PK, provider/gate note and declared inputs from §9. Use existing node's
current resource from policy; no user-supplied server handle or resource mutation
hidden in form. Missing required linkage/config/text version blocks enable.
Manual mode hides inputs/checkbox; does not delete resources.
Policy PUT has no `vars` field. Do not offer policy installation-var overrides
or promise inheritance of an initial run's overrides: current successor creation
uses empty `VarsJSON`. If safe execution requires those values, block enable
with explanation; never silently substitute a different installation setup.

«Обработать текущий эпизод блокировки» is **unchecked by default** on new
activation; existing persisted true is displayed honestly. It requires its own
explanation/consent, not a preselected “recommended” option. Historical episodes
are not replayed by default. Beget/unsupported runtime refs block Profile enable.

Profile confirmation must say: «Профильный режим разрешает действия выбранной
версии при новых блокировках, включая платную замену VPS, если она есть в схеме.
Сохранение самого профиля этого не делает». Current-ban opt-in can act now;
never show the old unconditional “ban cannot trigger purchases” copy **inside
an explicitly enabled Profile policy**. Manual mode retains no-auto-purchase copy.
Do not imply full independent GP/metrics/TCP admission: current engine uses a
bounded registry-readiness check, not all normative 05 postconditions.

Submit `PUT /api/me/nodes/{id}/management`
`{mode:"profile",profile_version_id,inputs,process_current_ban,expected_revision}`
→ **200 management DTO**; manual `{mode:"manual",expected_revision}`.
No Idempotency-Key on policy PUT; CAS revision from GET. 409 keeps choices and
requires refresh/reconfirmation; 422 diagnostics focus profile/input. No implicit
POST run when toggling a radio or saving editor content. If offering explicit
observe POST after enable, inputs must exactly match policy snapshot; actual
create handler validates required inputs **before** its policy-default fallback,
so do not omit them expecting inheritance. Do not send observe vars overrides.

## 11. Required loading, empty, error and navigation states

| Area / failure | Required treatment |
|---|---|
| Registry/version first load | Stable skeleton + `aria-busy`, «Загрузка блоков/версии…»; no edit/save until known. Registry error keeps existing draft visible, disables new unknown blocks, retry GET only. |
| No compatible blocks / no connections / no text version | Explain cause; clear search / «Добавить подключение» link `#/beget` / numeric text-version picker respectively. Never fill with invented defaults. |
| Nothing selected / no wires / no diagnostics | Inspector graph settings; «Связей пока нет» + connect affordance; «Проверка ещё не выполнена» (not “valid”). |
| 200 valid:false; 422 invalid_blueprint/profile_invalid/profile_incompatible | Persistent diagnostics; retain all edits; focus explicit field/finding after user validation/save, never on background polling. |
| 400 validation_failed / unsupported_format / steps_and_blueprint_conflict | Inline error, no retry loop; never send both formats. Oversized authoring body currently maps to 400, not promised 413. |
| 403 user_profiles_disabled / profile_limit_reached | Keep read/view; explain flag/limit; disable create/copy/save as applicable; backend authoritative. No paid retry suggestion. |
| 409 duplicate_name / profile_in_use / deployment_unresolved | Focus name / retain profile with management link / retain known run-resource identity; do not bypass with new ID or new purchase. |
| 401 / 403 csrf_invalid | Existing signin/session-expired handling; no silent mutation replay after reauth. No secrets in URL/storage recovery. |
| 404 version/run/ownership | «Версия/запуск недоступны» + back/numeric chooser; no owner disclosure or “try next ID”. |
| 5xx/network on reads | Keep last data + stale banner/time + «Повторить»; never replace with empty success. Mutation ambiguity keeps draft/intent and offers read/reconcile, not resend. |
| No known runs | «Откройте запуск по ID или после явного запуска профиля»; no claim this account has no runs. |
| Narrow viewport / browser zoom | All forms/diagnostics reflow at 320px, long IDs wrap. Only canvas is spatial/scrollable; Blocks+Connections list remains a complete non-spatial equivalent. |

Every form uses explicit labels, errors via `aria-describedby`/`aria-invalid`;
one polite live region announces add/connect/delete/undo/validate/save/run changes,
not pointer coordinates or every identical poll. Use escaped text (`esc` or DOM
text), semantic buttons/links, no pointer-only menus. Poll only run/status regions
in-place; never rebuild editor/inspector, drop selection, move focus or reset a
caret. Dirty-leave guard covers nav links, browser Back and session logout too.

## 12. Acceptance / handoff boundary

- Full-width client editor from MyProfiles; current text editor/hosting work
  preserved; no framework/backend/generated-HTML hand edits.
- Registry-faithful blocks/ports, HTML nodes/SVG control wires, typed inspector,
  pointer **and** keyboard add/move/connect/remove, local undo, list equivalent.
- Exact JSON only; local layout separate; validate/save/PK and version-CAS
  distinctions explicit; conflict/ambiguous-save never silently overwritten.
- Local templates; Beget draft gate always visible; secret literals absent;
  saving/validation never spends, explicit run/policy consent is truthful.
- Run detail uses only mounted routes/returned facts; unknown/cancel/resume and
  management inputs respect pinned identities and safe-action boundaries.

**Verification limit:** source/contract cross-check and artifact read-back only;
not live design QA, implementation approval, security review or WCAG certification.
Frontend implementation, keyboard/touch/contrast/responsive checks and normal
repository verification belong to a separately authorized next phase. None were
run or claimed here.
