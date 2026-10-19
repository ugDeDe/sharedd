# Blueprint editor — Phase 3 real-browser design QA

**Verdict: FAIL ❌ — return to frontend through leader.**
**Verification status: partially_verified.** Real Chromium interaction and visual
inspection of the generated cabinet bundle, with in-browser API mocks only. This
is **not** backend E2E, deployment approval, security certification, or permission
to execute a workflow. Review date: 2026-10-17.

## 1. Scope, isolation and evidence

- Read `design-blueprint-editor.md`, `implementation-blueprint-editor.md`, root
  `DESIGN.md`, all `ui/pages/cabinet/blueprint-*.ts`, `blueprint.css`, relevant
  current `index.html`, routing/profile/refresh code in `main.ts`, and management
  integration in `node-detail.ts`. Registry declarations and existing DOM/tokens
  were read to make mocks and explain observed behavior.
- Ran the existing generated `registry/internal/webassets/cabinet.html`; **no
  app, generated asset, Go, design-system, or other reviewer's files were edited**.
  Only this report was added in the repository.
- Static preview: `python3 -m http.server 8765 --bind 127.0.0.1 --directory
  registry/internal/webassets`, after checking directories and that the port was
  unused. Log: `/tmp/opencode/blueprint-qa-static.log`. Owned PID: `233407`.
- Chrome DevTools could not find its default stable executable. **agent-browser
  successfully launched Chrome**, session `blueprint-qa`; QA was not abandoned or
  substituted with source-only inspection.
- The browser was restricted to `127.0.0.1` / `localhost`. On the initial local
  directory-listing document, the harness fetched the generated cabinet HTML,
  installed a same-origin `fetch` mock, then rendered that HTML with
  `document.open/write/close`. App scripts/styles were unchanged. Hash navigation
  exercised the real client routes without reloading away the mock.
- Impeccable skill loaded. Context script reported missing `PRODUCT.md`; command
  reference reads were denied by environment permissions. No context files were
  created outside the report-only boundary, and no unavailable Impeccable
  subcommands are claimed as executed. QA used the supplied frozen design spec
  and existing `DESIGN.md`.

### Mock contract and limits

`GET /api/me` returned a synthetic user with quota 10. Initial
`GET /api/me/profiles` returned `{enabled:true,max:10,profiles:[],published:[]}`.
`GET /api/me/blueprint-blocks` returned `{schema_version:1,blocks:[...]}` with all
18 declarations manually transcribed from the current registry: exact input and
output names/types, control outputs, effects, retry class, provider capabilities,
nullable empty params/port arrays and enum/timeout declarations. This was not a
generic JSON-Schema or invented success/error-port registry.

The harness also mocked the cabinet's session-refresh reads (nodes, operations,
Beget link, authkey), install defaults, hosting connections and empty resources.
A synthetic Nodehost connection and port 8443 were available as fixture data;
the blocked Run button prevented the instance form from actually consuming them.
No credentials or real account data were used. A fake local Secure CSRF cookie
allowed the actual request wrapper to run; this does not test real auth/CSRF.

All `/api/*` fetches were intercepted and recorded in browser memory before any
network access. Undefined API paths returned a synthetic 404; external origins
were rejected. Authoring validation deliberately returned `200
{valid:true,diagnostics:[]}`: **this tests client behavior, not graph validity**.
Mock create/append returned metadata, retained an in-memory immutable version,
and served its numeric GET read-back. Mock PUT checked `expected_version` against
the stored ordinal. PKs were deliberately different from ordinals.

No automatic mutation driver/retry was used. Each observed authoring request
followed an explicit click on the dirty-leave dialog's Save action. Six non-GET
requests total: three validations, two profile creates, one version append.
**Zero run-create or management-write requests.** No real backend, external host,
provider, paid, SSH, or Go test/vet/build calls were made.

For two deliberate local graph operations, the harness supplied a **one-shot
affirmative `window.confirm` answer**, recorded its message and restored the
original function. These were connection replacement and node deletion. This
tests the resulting graph interaction, **not native confirm-dialog focus or its
default button**. It did not bypass consent or open the execution form.

## 2. Blocking and accessibility findings

### BP-QA-01 — HIGH / P1: delegated panel matching swallows editor actions

**Observed in the real browser, both pointer and keyboard activation.**

Reproduce on `#/profiles/editor/new?starter=nodehost`:

1. Search `terminal.stop`, click its enabled **Добавить**.
2. Click **Проверить**, **Сохранить версию**, **+**, **Показать всё**.
3. Select Beget in the template control and click **Применить как черновик**.
4. Click a node in the visible Blocks list.

Actual: node count remains 18; no validation/save request; zoom stays `100%` and
world stays `translate(0px, 0px) scale(1)` before any pan; template/provider remain
Nodehost; list selection does not change the inspector. No error or useful
feedback. After saving by the separate leave-dialog path and reopening a clean
saved Nodehost graph, **Запустить… is enabled but opens no review dialog**.

After keyboard deletion, the enabled **Отменить** button and Ctrl+Z both failed:
17 nodes and unset initial entry remained, with no undo announcement. The shortcut
dispatches the same swallowed button action. This makes a destructive local edit
unrecoverable through the advertised undo controls.

**Cause confirmed against source:** `Editor.panels()` sets `data-panel` on the
editor root (`blueprint-editor.ts:330–337`). `Editor.click()` first calls
`t.closest("[data-panel]")` (`:514–521`), which finds that **root** for ordinary
controls, treats it as a tab click and returns before the actual action handlers.
Browser probe: Add's closest matching element ID was `view-blueprint-editor`.
Real tab buttons still work because they match themselves. Canvas/inspector
controls with their own earlier event handlers can still work; this is not a
blanket claim that every child control is dead.

**Required correction:** distinguish tab-action elements from the root's
panel-state attribute; restrict tab dispatch to the actual tab controls. Retest
Add, template apply, Validate, Save, Run review, list selection, edge remove,
diagnostic selection, undo/redo, zoom/fit and conflict actions—not just one button.
Do not treat the alternate leave-dialog Save as a fix for normal Save.

### BP-QA-02 — HIGH / P1: background refresh redirects keyboard port focus

Reproduce at desktop width on a loaded Nodehost graph:

1. Focus `ready_initial`'s **exec** button (Tab navigation or focus it for a
   deterministic browser test).
2. Leave it focused through one ordinary five-second cabinet refresh.
3. Activate with Enter while connecting a source output.

Actual: focus moves to **create_initial · exec**, the first matching exec
control. A single in-page timed test (6.1s, no intermediate automation command)
captured:

| | Focus accessible name | Captured request count |
|---|---|---:|
| Before | `Соединить с ready_initial · exec` | 543 |
| After | `Соединить с create_initial · exec` | 548 |

Intervening requests were only `/api/me`, `/api/me/nodes`, `/api/me/operations`,
`/api/me/beget/link`, `/api/me/authkey`. Slower keyboard connection attempts
therefore targeted the source itself and announced a cycle error rather than
connecting the intended node. A quick batched keyboard connection succeeded,
so timing matters; the keyboard path is not reliably usable.

**Source correlation:** `ui/lib/dom.ts:28–46` identifies focused elements by their
first `data-*` attribute and restores focus with a global `document.querySelector`
even when repainting an unrelated section. Exec uses repeated empty `data-exec`;
outputs repeat `data-output="ok"` (`blueprint-canvas.ts:128–129`). Cabinet refresh
continues rendering other sections (`main.ts:2492–2518`, `:2710–2722`).

**Required correction:** unrelated repaints must leave a still-connected focused
element alone; when a focused control really must be recreated, its identity must
include the owning node and port. Verify keyboard connections with a pause across
multiple normal refreshes and ensure the active target never changes. This is an
interaction/focus regression, not grounds to remove background safety reads.

### BP-QA-03 — MEDIUM / P2: light-theme text contrast below WCAG AA

Measured from **computed browser styles**, compositing alpha backgrounds over the
actual solid ancestor surfaces; WCAG sRGB relative-luminance calculation:

| Visible text | Foreground / effective background | Ratio | Required |
|---|---|---:|---:|
| Inactive editor tab **Блоки** | `rgb(141,151,168)` / `rgb(238,240,246)` | **2.59:1** | 4.5:1 |
| Node **платное действие** badge | `rgb(182,116,11)` / `rgb(246.24,238.32,225.72)` | **3.33:1** | 4.5:1 |

Tabs are enabled navigation, not exempt disabled text. The purchase badge is
10.5px/640-weight, not large text; it communicates a meaningful spending effect.
Reference screenshot: desktop inspector light. For comparison, sampled node data
text was 6.12:1 and the Nodehost notice 4.67:1 in light theme.

**Required correction:** use contrast-safe foregrounds for these roles in the
scoped blueprint surface while preserving the cabinet system. Recheck active,
inactive, hover and focused states in both themes. No full-page WCAG or axe
certification is claimed from these samples.

## 3. Interaction results

| Check | Real-browser result |
|---|---|
| Profiles entry and full-width editor | Loaded through the actual Profiles template link. 1440px viewport, workspace width 1400px / 20px outer gutters; three-column canvas and inspector. |
| Local fixture starters | Nodehost: 18 nodes / 16 control wires. Beget: 25 nodes / 25 control edges. Merely opening either generated no mutation. Toolbar Apply itself **fails BP-QA-01**. |
| Registry and rendering | HTML nodes + decorative SVG wires visible; sampled guard outputs `active/stop`; arm/stop have no outgoing controls. Null declarations rendered without JS errors. Not an exhaustive registry-compatibility certification. |
| Add block | Search filters palette; enabled Add did not add a node: **FAIL**. |
| Select / typed inspector | Clicking the canvas node header opened its real inspector. `server.create` showed purpose enum, number connection refs and config ref; config had no literal editor; purchase retry forbidden. List selection **fails BP-QA-01**. |
| Keyboard movement | Repeated Right, Right, Shift+Down moved selected initial node from `(40,40)` to `(60,90)`; header focus persisted in this test. |
| Pointer movement | Actual mouse down/move/up changed the saved fixture's initial node from `(40,40)` to `(90,70)`. Layout-only movement did not dirty saved graph content. |
| Pan | Actual empty-canvas mouse drag changed world transform from `(0,0)` to `(-50,-50)` at scale 1. |
| Zoom / Fit | Both clicked; no effect: **FAIL BP-QA-01**. Range limits / wheel zoom were not validated. |
| Keyboard connect | Enter on output enters connection mode and announces the target instruction. Quick focus/Enter to another exec with one-shot explicit replace approval changed `create_initial → ready_initial` to `create_initial → install_initial` in the live Connections list. Slow path **fails BP-QA-02**. |
| Delete / undo | Delete on selected node, with one-shot approval, listed affected edge, `ready_initial.operation` binding and initial entry. Node count 18→17, entry and related connections cleared. Button undo and Ctrl+Z **both failed BP-QA-01**. This edited draft was discarded, not saved. |
| Native dirty leave | Opened actual native HTML dialog with Stay / Discard / Save. Save and later Discard completed through this separate handler. Browser-unload/OS-native confirm accessibility not certified. |
| Validate / Save toolbar | Clicks sent no API calls: **FAIL BP-QA-01**. Success transport was separately exercised through dirty-leave Save; details below. |
| Nodehost explicit run review | Saved clean graph had enabled Run, but click opened no dialog. Consent/form/submission **blocked, not verified**. Source copy is not substituted for a runtime PASS. |
| Beget gate | Full design-only warning visible before and **after mocked successful validation/save/read-back**. Saved v1 / PK11201 remained Run-disabled; no run request. |
| Mobile 390×844 | Light theme, first-entry panel `blocks`, tabs reflowed, no page horizontal overflow. Tab click to Properties and Left-arrow roving back to Blocks worked. Palette precedes the long existing-node list; list/add actions still fail BP-QA-01. |
| Narrow 320×800 | Saved Beget header/toolbar reflowed, document overflow 0px; Run stayed disabled. Not a full 320px form/diagnostics audit. |
| Themes / focus / console | Actual theme toggle and screenshots in both themes. Canvas selection and keyboard header focus visibly distinct. Browser page-errors collection empty before and after; functional failures did not throw exceptions. |

Mobile polish note (not a new AA minimum-target failure): palette Add buttons
measured 28px tall versus the preferred 44px touch target in the design contract.
They exceed the 24px minimum but should be enlarged with the interaction fixes.
At 390px the safety text and toolbars occupy nearly the first screen; the tab row
starts around y770. Scrolling makes it reachable; this is not a claim that the
fixed mobile nav makes the editor inaccessible.

### Captured authoring requests — alternate leave-dialog Save path

These are **observed mock transport facts**, not proof that normal Save works:

1. Nodehost starter → explicit leave-dialog Save:
   - `POST /api/me/profiles/validate` keys exactly `format,blueprint`; **no inputs**.
   - `POST /api/me/profiles` keys `name,description,format,blueprint`.
   - Read-back `/api/me/profiles/21/versions/1`; displayed ordinal **1**, PK **11101**.
2. Reopen ordinal 1, change graph name, explicit leave-dialog Save:
   - validation as above; `PUT /api/me/profiles/21` added **`expected_version:1`**,
     not 11101. Read-back `/api/me/profiles/21/versions/2`; PK **11102**.
   - Profile metadata name and graph content name stayed separate.
3. Beget starter → explicit leave-dialog Save:
   - validation and graph POST only; `/api/me/profiles/22/versions/1`, PK **11201**.
   - Runtime warning persisted and Run remained disabled despite valid/save success.

All observed graph envelopes contained only
`schema_version,name,provider,inputs,entries,nodes,edges`; nodes had no UI position,
pan, zoom or viewport keys. No literal bindings at all occurred in the tested
fixtures, hence no secret literals; this is **not** an arbitrary-input secret
scanner audit. Authoring had the synthetic CSRF header, no idempotency header,
no steps mixed into graph requests, no run/policy mutation or silent replay.
Only numeric version paths 21/1, 21/2, 22/1 occurred—no `/latest` or PK-as-ordinal
path. Browser storage contained only `sharedd.theme`; session storage was empty.

## 4. Screenshots

All captured from the real local browser, outside the repository:

| File | Evidence |
|---|---|
| `/tmp/opencode/blueprint-qa-desktop-initial-dark.png` | 1440×1000, initial Nodehost graph, full-width workspace and control wires. |
| `/tmp/opencode/blueprint-qa-desktop-delete-undo-fail.png` | 1440×1000, deleted initial node remains absent after both undo attempts; enabled Undo and unset initial entry visible. |
| `/tmp/opencode/blueprint-qa-desktop-inspector-light.png` | 1440×1000, saved numeric version/PK, selected node, typed refs and light-theme contrast samples. |
| `/tmp/opencode/blueprint-qa-mobile-beget-light.png` | 390×844, Beget draft safety warning and disabled Run. |
| `/tmp/opencode/blueprint-qa-mobile-beget-blocks-light.png` | 390×844 after scrolling, selected Blocks tab, reflowed palette and mobile navigation. |
| `/tmp/opencode/blueprint-qa-mobile-beget-saved-light.png` | 390×844, saved v1 / PK11201 still design-only with Run disabled. |

Static images document appearance, not the full interaction sequence. Request
captures and DOM measurements above supply the behavioral evidence.

## 5. Unverified boundary and next gate

- No real backend validator, persistence/CAS concurrency, session/CSRF, provider,
  price, purchase, SSH, admission, deployment, or retry semantics were exercised.
- Nodehost review/unchecked-consent, config/text-version PK selection and actual
  submit remain **not_verified** because the normal Run entry is broken. Do not
  infer safety coverage from zero run requests or from available source strings.
- Run detail/polling/cancel/resume and node management were source-read but not
  browser exercised in this pass. No backend E2E claims for them.
- No 409/422/network-ambiguity, registry-load error, profile-limit/disabled,
  published-profile, complete screen-reader, OS dialog, touch-device, reduced
  motion, browser-zoom or comprehensive contrast audit. Viewport emulation is not
  physical mobile/touch verification.
- Strict TypeScript/build success comes from the supplied Phase 2 handoff, **not
  a rerun by this QA agent**. No application code changed; no Go commands or
  external package/browser installs were run.

**Required before reconsidering PASS:** fix BP-QA-01 and BP-QA-02, meet BP-QA-03
contrast thresholds, then rerun the broken controls plus slow keyboard connect,
mobile complete list editing, normal toolbar validate/save/CAS and explicit
Nodehost review/consent against mocks. Keep Beget gated. Backend/provider testing
requires a separately authorized scope, not this preview.

**Cleanup completed:** browser session closed. Only owned static-server PID
233407 was terminated; connection check to 127.0.0.1:8765 returned 111 (refused).
No preview process or polling session from this QA remains running.

---

# Follow-up browser retest of the fixes (2026-10-17)

**Verdict: PASS ✅ — all five previously broken areas verified fixed in the real
browser.** Verification status: partially_verified (same boundary as above: mocked
same-origin API, no real backend/provider/Go — this remains not a backend E2E,
deployment approval, security certification, or permission to execute a workflow).

## Scope, isolation, and evidence for this retest

- Read the prior FAIL report above, `implementation-blueprint-editor.md`
  §Follow-up, and `review-blueprint-editor.md` H1 before testing. Retest covered
  **only** the previously broken paths plus the explicitly listed regressions.
- `bash scripts/build_ui.sh` ran **once** at the start (allowed; the shared
  `ui/lib/dom.ts` change also regenerates `panel.html` — checked "без изменений"
  against the already-regenerated follow-up bundle, so no stale webassets and no
  hand-edits). No other app file was touched; the only repository edit is this
  section.
- Static preview: `python3 -m http.server 8812 --bind 127.0.0.1 --directory
  registry/internal/webassets`; harness opened the directory listing, fetched the
  generated `cabinet.html`, installed a same-origin `fetch` mock, then
  `document.open/write/close` — identical technique to the first pass. App
  scripts/styles unchanged; hash navigation used the real client router.
- **agent-browser** Chrome, session `bp2` (desktop) plus an isolated namespace
  session attached to a self-launched headless Chrome
  (`--window-size=390,844 --remote-debugging-port=9333`, own PID, later killed)
  for the true 390px mobile measurements. Both restricted to 127.0.0.1/localhost.
  Light theme (`sharedd.theme=light`) for all contrast and mobile checks.
- Fake `__Host-sharedd_csrf` cookie set/deleted via `document.cookie` on the
  trustworthy 127.0.0.1 origin; the real `api.ts` request wrapper ran, including
  its local no-cookie 403 path. This does not test real auth/CSRF.
- Mocks: full 18-block registry transcribed from the current
  `registry/internal/blueprint/registry.go`; synthetic user/profiles/session/
  install-defaults/hosting-connection (create-capable)/text-profile fixtures;
  validate always `200 {valid:true}`; create/append with numeric-ordinal
  `expected_version` CAS; run POST with three controllable behaviors —
  `lost` (record then reject the promise: server-accepted-but-response-lost),
  `csrf403` (`{error:"csrf_invalid"}` 403), `ok` (dedupe on identical key+body,
  `idempotency_conflict` 409 otherwise). Native `window.confirm` was overridden
  with a **counted, recorded** approval for the three deliberate destructive
  local operations (template replace ×2, node delete) — same boundary as the
  first pass; CDP native-dialog handling was flaky in this environment, and no
  consent or run was ever auto-created: every run POST followed an explicit
  submit click in the review dialog.

### Run/mutation tally

`POST /api/me/profiles/validate` ×6, graph `POST`/`PUT /api/me/profiles` ×5,
run `POST /api/me/profile-runs` ×6 total across both cycles (3 lost + 3
replayed-with-original-key; two cleared by ack, one cycle ended deliberately
still locked). **Zero fresh-key run POSTs. Zero real backend/provider/SSH/Go
calls.** Console page-error collection was empty in both sessions.

## Per-item results

### 1. Editor action dispatch (BP-QA-01) — PASS ✅

Root `data-panel` no longer participates in dispatch: probe showed neither the
Add button nor the editor root matches
`.blueprint-tabs button[role="tab"][data-bp-tab]`. All retested controls fired:

| Action | Observed result |
|---|---|
| Palette Add (`terminal.stop`) | 18 → **19** nodes |
| Apply Beget template | confirm (recorded) → **25** nodes / 50 edges |
| Проверить | `POST /api/me/profiles/validate` fired |
| Сохранить версию (normal toolbar) | `POST /api/me/profiles` + numeric `GET …/41/versions/1`; header showed **версия 1 · PK 4101**; later `PUT` with `expected_version` → v2/v3 read-backs |
| Delete (Delete on focused node handle) | confirm listing edges/bindings → **18 → 17** |
| Отменить button and Ctrl+Z | both restored **17 → 18**, node back |
| Zoom + / Показать всё | `100% → 125%`, world `translate(55px,29px) scale(1.25)`; fit `25%`, `translate(10px,10px) scale(0.25)` |
| Blocks-list / canvas node selection | inspector opened `create_initial · server.create` with typed refs |
| Запустить… on clean saved Nodehost | review dialog **opened** (was dead before); consent checkbox required; Beget saved graph stayed Run-disabled |

### 2. Slow keyboard connect across refreshes (BP-QA-02) — PASS ✅

Single in-page eval (11.5 s continuous, **zero intermediate automation
commands**), Nodehost draft, connect mode armed from `ready_initial · ok`:

| | Focus | Requests |
|---|---|---:|
| Before wait | `bp-port-admit_initial-exec` · «Соединить с admit_initial · exec» | 189 |
| After ≥2 refreshes (16 reads: me/nodes/operations/beget/authkey) | **same element object**, same id/label | 205 |

`document.activeElement === destBtn` held (`true`) — the element survived and
was never even refocused, let alone jumped to the first node. Enter connected
the intended pair: connections list gained `ready_initial · ok → admit_initial ·
exec` (replace confirm recorded). Port buttons now carry unique
`bp-port-{node}-exec` / `bp-port-{node}-out-{port}` IDs.

### 3. Contrast + mobile Add (BP-QA-03) — PASS ✅

Light theme, computed styles composited over actual ancestor surfaces:

| Text | fg / bg | Ratio | Required |
|---|---|---:|---:|
| Inactive tab «Блоки» | `rgb(87,98,122)` / `rgb(238,240,246)` | **5.37:1** | 4.5 |
| Active tab «Схема» | `rgb(16,23,37)` / `rgb(238,240,246)` | **15.73:1** | 4.5 |
| Canvas «платное действие» badge (10.5px) | `rgb(16,23,37)` / `rgb(238,240,246)` | **15.73:1** | 4.5 |

Mobile at a true 390×844 viewport: palette Add buttons measured **44px** tall
(`min-height: 44px` computed, was 28px); no horizontal overflow; tab colors
identical at mobile width. Not a full-page axe/WCAG certification — these are
the previously failing roles only.

### 4. H1 — uncertain paid intent survives a rejected replay — PASS ✅

Full acceptance trace, executed twice (second cycle used the server-side
`csrf_invalid` 403 instead of the local no-cookie 403):

1. Clean saved Nodehost v3 (PK 4103) / v1 (PK 4201) → review → form filled
   (connection #7, config fields, text version 31101) + consent → submit with
   response lost. Result: `POST /api/me/profile-runs` **recorded** (key
   `8d99d273…` / `94296e19…`, body `profile_version_id:4103/4201`), promise
   rejected → error «Результат запроса неизвестен…», retry visible, fields +
   fresh submit **disabled**.
2. CSRF cookie deleted → same-key replay → **local 403 before any fetch**
   (0 requests left the page) → still fully locked, ambiguous wording kept.
   Server-403 variant: same lock, error shown, no new key.
3. Close review → reopen → lock notice «Сохранены исходные body и
   Idempotency-Key…», fields/submit disabled, retry visible. `requestSubmit()`
   fresh attempt sent **nothing**; the only key ever transmitted remained the
   original.
4. CSRF restored → retry → exactly **one** POST, byte-identical body and
   identical key to the original → mock ack `{run_id:501}` → dialog closed,
   hash `#/profiles/runs/501`, uncertainty cleared.

### 5. Beget stays design-only — PASS ✅

Saved Beget graph (25 nodes) kept «Запустить…» `disabled` before and after
mocked validate/save; no run entry point. Combined with H1-locked fresh-submit
refusal, no unexpected paid POST occurred at any point.

## Unverified boundary of this retest

- Mock boundaries as listed: no real validator, CAS concurrency, session/CSRF,
  provider, purchase, SSH, admission, retry, or run-detail/polling/cancel
  behavior; the run ack destination (`#/profiles/runs/501`) was not followed
  into run-detail rendering. `confirm`-dialog native focus/default button still
  not certified (recorded override used).
- Dark-theme contrast, 320px, touch hardware, reduced motion, screen readers,
  non-listed controls (edge remove via button, diagnostics, conflict actions)
  were not re-run this pass; nothing observed suggests regression there, and
  the dispatch fix is structural (single shared selector).
- Strict TS/build claims come from the frontend handoff, not rerun here beyond
  the one sanctioned `build_ui.sh`.

**Cleanup completed:** both browser sessions closed; owned static server
(8812) and owned headless Chrome (9333) terminated; both ports refused
afterwards. No preview or polling process from this QA remains.
