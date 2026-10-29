# design-qa-report.md — Phase 3 Design QA (dev)

**Verdict: FAIL** (fix required — a11y focus-management, polling focus reset, and
mutation disabled-state are explicit Definition-of-Done items that are unmet).

Reviewed against `DESIGN.md` (§3, §4, §5, §6, §7, §8, §9, §10, §12) and
`docs/specs/cabinet.md` (§1–§3, «Сквозные»). Sources inspected:
`ui/pages/signin/*`, `ui/pages/cabinet/*`, `ui/pages/panel/{index.html,main.ts,page.css,views/users.ts}`,
`ui/components/{nodecard,modal,topbar}.css`, shared `ui/{tokens,base}.css`,
`ui/components/{card,form,badge,table,misc}.css`.
No browser server was started; source + static review was sufficient. Application code was not edited.

---

## What passes (verified)

| Item | Evidence |
|---|---|
| **Generated assets fresh (not stale)** | `bash scripts/build_ui.sh -check` → exit 0, all six pages «без изменений». `webassets.go` embeds `signin.html`/`cabinet.html`. |
| **Token reuse / no ad-hoc colors** | No raw hex/rgb in new page sources; only `#fff`/rgba remain in pre-existing `topbar.css` brand-mark. Radii all via `--r-*`; the only raw `border-radius` are `50%` circles (dots/step counter) in `cabinet/page.css:37,61`. |
| **Component reuse** | signin/cabinet import the shared `components/*`; `.node-card`/`.modal` extracted into shared components and reused by panel + cabinet. No new button/field/badge components. |
| **Secrets — one-time authkey reveal** | Self-rotate correctly absent: hint only (`cabinet/main.ts:197`, matches implementation-summary / DESIGN §11.1). Admin rotate shows key once in `.secret-reveal`, `esc()`-ed (`users.ts:186-202`); `closeModal()` does `modal-body.replaceChildren()` (`cabinet/main.ts:74`, `users.ts:57`) → key leaves DOM. Prefix-only in cabinet (`cabinet/main.ts:190`). |
| **Secrets — Beget password** | Cleared after successful `PUT` (`cabinet/main.ts:654`); never echoed; SSH shown only as «SSH-ключ зарегистрирован» (`:245`). |
| **idem_key fixed per attempt** | Generated once when leaving «Переменные» (`cabinet/main.ts:502`) or in `submit()` if unset (`:512`), never regenerated on retry; reset only after success (`submitted()` `:310`). `api.ts:101` UUID. |
| **unknown operation → manual review, no retry** | `OP_META.unknown` = «неизвестно · ручной разбор» `.badge.special` (`main.ts:32`); `cancelCell` returns no button for `unknown` (`:201-204`); explanatory `.card-note` shown (`:231-234`). No «повторить создание» anywhere. |
| **States present (mostly)** | signin idle/submitting/error (`main.ts:71-88,102-127`); cabinet loading skeletons (`index.html:59,82,95`), empty-states (`main.ts:144,213,354,377`), error banners (`:260`, `users.ts:119`). |
| **Copy register** | Operator register preserved, no marketing/emoji; button verbs infinitive; errors «что + что делать»; em-dash for unknowns. |
| **CSRF / auth** | `X-CSRF-Token` on non-GET (`api.ts:65`); `401 → /signin` (`api.ts:72`). |
| **XSS** | All interpolations `esc()`-ed across cabinet/users/signin; no `console.log`. |
| **Responsive** | `.table-cards`→cards ≤720 (`table.css:33`); metrics 2→1 col ≤640/≤400 (`card.css:45,52`); wizard steps horizontal-scroll (`page.css:25`); mobile-nav ≤720 (`topbar.css:100`). |

---

## Findings (required fixes)

### HIGH

**H1 — Modals do not trap focus (DESIGN §10.3 mandatory).**
`cabinet/main.ts:54-80`, `users.ts:38-63`. Dialog has `role="dialog" aria-modal="true"`, focuses the
first button and restores focus on close — but `Tab`/`Shift+Tab` escapes to the page behind, which
`aria-modal="true"` promises it cannot. Required: trap Tab within `.modal-box` (cycle first↔last
focusable), and mark the page behind inert/`aria-hidden` while open.

### MEDIUM

**M1 — Polling re-renders the wizard every 5 s and drops input focus (DESIGN §10.8).**
`cabinet/main.ts:578` (`if (currentView() === "create") wizard.render();` inside `refresh()`),
`:315-338` (`render()` does `wiz-body.innerHTML = this.stepHTML()`). While an operator types into
«Конфигурация» or profile vars, the 5 s poll rebuilds the inputs and steals focus/caret. Required:
re-render the wizard only when its state actually changed (dirty flag), or update in place without
replacing focused inputs.

**M2 — Duplicate event listeners on wizard step controls.**
`cabinet/main.ts:508` `refreshFoot()` calls `:472 bindStep()`; `bindStep` re-adds listeners to the
*persistent* `[data-var]` (`:490-491`) and `wiz-confirm` (`:496`) nodes on every `refreshFoot()`
(i.e. on every var keystroke / confirm toggle). Listeners accumulate → `collectVars`/`refreshFoot`
run N times per event. Required: bind step-body listeners once (in `render()`), not in `refreshFoot()`.

**M3 — Mutation buttons not disabled in flight (cabinet.md §3 «кнопка disabled на время»; DESIGN §12).**
`users.ts`: `createUser` `:142-151`, `editQuota` `:166-172`, `revokeKey` `:212-217`, `releaseNode`
`:231-237`, `resetPassword` `:251-257` fire the request without disabling the action button; modal
stays open until success, so double-submits are possible. Required: disable the acting button (and
show a busy label) until the request settles.

**M4 — Panel toast has no `aria-live` (DESIGN §10.7).**
`ui/pages/panel/index.html:241` `<div id="toast"></div>`. Cabinet has it (`cabinet/index.html:146`);
the users view (in scope) shows its mutation results only through this unannounced toast. Required:
add `aria-live="polite"` to the panel toast.

### LOW

**L1 — Quota edit uses a modal instead of the spec's inline editor (cabinet.md:287-288).**
`users.ts:156-174` opens `.modal`; the spec describes click-to-edit `input[type=number]` inline.
Functionally equivalent, but deviates from the agreed UX. Either accept and update the spec, or
implement inline.

**L2 — Inline style colour in the operations table (DESIGN §5.2 «никаких инлайновых цветов»).**
`cabinet/main.ts:219` `style="background:var(--acc-soft)"` for the new-op highlight. Move to a
`.row-highlight` class in `cabinet/page.css`.

**L3 — Card-head H2 override off the fixed type scale (DESIGN §3 = 13.5px).**
`signin/page.css:17` sets `.auth-card .card-head h2 { font-size: 16px }` — a size not in the scale.
Revert to the component size, or add the size to the documented scale.

**L4 — Inconsistent nav label for the users view.**
`panel/index.html:33` desktop nav = «Пользователи»; `:237` mobile nav span = «Люди». Spec
(cabinet.md §3) says «Пользователи». Make the mobile label match.

**L5 — Tabs lack roving tabindex / arrow-key navigation.**
`signin/index.html:42-43` + `main.ts:35-43`. Both `role="tab"` buttons stay in the tab order and the
APG arrow-key behaviour is absent (only Enter/Space works). Operable, so WCAG 2.1.1 passes, but the
tablist pattern is incomplete. Optional: roving `tabindex` + Left/Right handling.

**L6 — Status badge wrapped in an unlabeled toggle button (DESIGN §10.5).**
`users.ts:87`: `<button data-toggle title="...">${badge}</button>` — accessible name is the badge
text («активен»), which does not convey the toggle action. Add `aria-label`/`aria-pressed`.

**L7 — Signin inline alert is a bespoke component instead of reusing `.banner` (DESIGN §1/§5.1).**
`signin/page.css:29-34` defines `.auth-alert` with `--bad-soft/--info-soft`; cabinet.md §1 allowed a
`.login-err`-style inline error, so this is sanctioned, but reusing `.banner.bad/.info` would remove
a near-duplicate. Optional.

**L8 — Dev brief flagged «автоповтор»/«пересоздаёт»; both are spec-mandated negations, not violations (spec conflict).**
- `cabinet/main.ts:233` ops note: «Автоповтор платного заказа **запрещён**» — verbatim from
  cabinet.md §2.4, but DESIGN §8 lists «автоповтор» among banned formulations.
- `cabinet/main.ts:109` dc2 title: «отказ здоровья **не** пересоздаёт VPS» — verbatim from
  DESIGN §6.2, while §8 bans «пересоздать».
Both are negated (they forbid/deny recreation), so intent (§8) holds. Recommend resolving the
doc conflict: use DESIGN §6.3 wording «Повторный платный заказ запрещён» in the ops note, and
explicitly allow the §6.2 phrase in §8. No code defect.

**L9 — Possible double quarantine badge.**
When `status==="quarantined"` **and** `quarantine` present, the card shows both the status badge
«карантин Globalping» (`main.ts:23`) and the `quar` badge «карантин» (`:117`). Cosmetic; emit one.

---

## Required fixes before PASS
1. H1 — trap focus in modals (`cabinet/main.ts`, `users.ts`).
2. M1 — stop wizard re-render stealing focus on the 5 s poll.
3. M2 — bind wizard step listeners once.
4. M3 — disable mutation buttons in flight (`users.ts`).
5. M4 — `aria-live="polite"` on panel `#toast`.
6. L1–L4 — spec-listed items (inline quota editor or spec update; no inline colour; type scale;
   nav label).

## Verification
- Generated assets: `verified` (build_ui.sh -check exit 0).
- Source/static design review: `verified`.
- Rendered-browser a11y pass (contrast/focus at runtime): `not_verified` (no server started —
  recommend `/impeccable audit` + keyboard pass after fixes).

---

# Re-check — Phase 3 Design QA (fix round)

**Verdict: PASS** (source-verified). All prior code-level findings H1, M1–M4, L1–L7, L9 are
resolved in the updated sources; the L8 spec conflict is resolved in the docs. The only
standing item is the rendered-browser a11y pass (contrast/focus at runtime), which cannot run
without a server — see «Remaining not_verified» below. Application code was not edited in this
re-check; documentation only.

Sources re-inspected: `ui/pages/cabinet/{main.ts,page.css,index.html,api.ts}`,
`ui/pages/panel/{views/users.ts,index.html,page.css,main.ts}`,
`ui/pages/signin/{index.html,main.ts,page.css}`.

## Per-finding status

| # | Status | Evidence |
|---|---|---|
| **H1** | ✅ PASS | `cabinet/main.ts:56-104` + `users.ts:40-87`: `openModal` sets `inert` on every `document.body` child except `#modal`/`#toast`; `closeModal` clears it. One `document` keydown traps `Tab`/`Shift+Tab` cycling first↔last focusable in `.modal-box`; Escape closes; focus restored to initiator. Verified `#modal`/`#toast` are direct body children (cabinet/index.html:146-154, panel/index.html:241-249) so the modal is not itself inerted. |
| **M1** | ✅ PASS | `cabinet/main.ts:611-615`: `refresh()` calls `wizard.render()` only when `document.activeElement` is outside `#wiz-body, #wiz-foot`. Typing in «Конфигурация»/vars no longer loses focus/caret on the 5 s poll. |
| **M2** | ✅ PASS | `cabinet/main.ts`: `bindStepBody()` (line 361, once per `render()`) vs `bindFoot()` (line 362 in `render()` **and** 541 `refreshFoot()`). `refreshFoot` replaces `#wiz-foot` innerHTML before rebinding, so foot listeners do not accumulate; `[data-var]`/`wiz-confirm` (persistent nodes) are bound exactly once. |
| **M3** | ✅ PASS | `users.ts`: `createUser` (185), `saveQuota` (201), `revokeKey` (245), `releaseNode` (268), `resetPassword` (291) set `btn.disabled=true` + busy label and restore in `.catch`. No double-submit. (`rotateKey`/`toggleDisabled` were outside the original M3 scope — see observation.) |
| **M4** | ✅ PASS | `panel/index.html:241` `<div id="toast" aria-live="polite">`. |
| **L1** | ✅ PASS | `users.ts:93-103` `quotaCell` renders `input[type=number]` + «Сохранить»/«Отмена» (`PATCH`); modal+`editQuota` removed (grep clean). Styles `.q-edit` at `panel/page.css:313-314`. |
| **L2** | ✅ PASS | `cabinet/main.ts:243` uses class `row-highlight`; rule at `cabinet/page.css:84`. No inline `background:` colour remains (grep clean). |
| **L3** | ✅ PASS | `signin/page.css`: `.auth-card .card-head h2 { font-size:16px }` override removed; no `16px` left in the file. h2 falls back to the 13.5px scale. |
| **L4** | ✅ PASS | `panel/index.html:237` mobile nav span = «Пользователи» (matches desktop nav line 33). No «Люди» anywhere. |
| **L5** | ✅ PASS | `signin/index.html:42-43` roving `tabindex="0"/"-1"`; `main.ts:44-46` `showTab` sets `tabIndex`; `main.ts:53-61` ArrowLeft/ArrowRight APG handler on `.tabrow`. |
| **L6** | ✅ PASS | `users.ts:122` `aria-label="<Заблокировать\|Разблокировать> пользователя <login>"` + `aria-pressed`. Accessible name conveys the action. |
| **L7** | ✅ PASS | `signin/index.html:46` uses shared `.banner bad` + `role="alert"` (hidden); `main.ts` `alertBox` toggles `.bad`/`.info`; `signin/page.css:30-31` only adds margins + `[hidden]`. No bespoke `.auth-alert` rule remains. |
| **L8** | ✅ RESOLVED (docs) | Conflict removed in the specs. `DESIGN.md` §8 now scopes the ban to **affirmative** claims («пересоздаст», «автовосстановление», «автоповтор разрешён») and explicitly **permits** the negated invariant phrases («отказ здоровья не пересоздаёт VPS» §6.2; «Повторный платный заказ запрещён» §6.3). `docs/specs/cabinet.md` §2.2 («Повторный платный POST запрещён») and §2.4 («Повторный платный заказ запрещён») now use the single authoritative §6.3 wording. Matches shipped copy. No app code touched. |
| **L9** | ✅ PASS | `cabinet/main.ts:141`: separate `.badge.warn «карантин»` rendered only when `n.status !== "quarantined"`; otherwise the status badge «карантин Globalping» stands alone. |

## No-regression checklist (re-verified)

- **Component reuse:** `cabinet/page.css`/`panel/page.css` import the shared `components/*`
  (incl. `nodecard.css`, `modal.css`); `signin/page.css` imports `glass/topbar/card/badge/form/misc`.
  No new button/field/badge components introduced by the fix round.
- **States:** signin idle/submitting/error; cabinet loading skeletons, empty-states, error
  banners; panel table skeleton/empty/error+retry. Unchanged by fixes.
- **Copy:** operator register, no emoji/marketing; button verbs infinitive; em-dash for unknowns.
- **a11y:** modal focus-trap + inert (H1), tablist roving tabindex (L5), toggle `aria-label` (L6),
  toast `aria-live` (M4) all present; `role="dialog" aria-modal="true"`, `role="alert"` intact.
- **Secrets:** full authkey only in one-time `.secret-reveal` (rotate), wiped by
  `closeModal().replaceChildren()`; Beget password cleared after `PUT` (`cabinet/main.ts:691`);
  prefix-only display; SSH shown as fact only.
- **idem_key:** generated once on entering «Подтверждение» (`main.ts:535`) or lazily in
  `submit()` (545), never regenerated on retry, reset in `submitted()` (334). Unchanged.
- **unknown-op manual review:** `OP_META.unknown` = «неизвестно · ручной разбор» `.badge.special`;
  `cancelCell` emits no button for `unknown`; explanatory note shown (257). No «повторить» path.

## Remaining not_verified

- **Rendered-browser a11y pass — `not_verified` (no server).** Runtime contrast ratios and the
  keyboard/focus behaviour of the modal trap (`inert` + Tab cycling) were verified by source
  review only. Recommend `/impeccable audit` + a keyboard pass on a running registry once a
  server/browser is available.
- **Observation (non-blocking, out of original scope):** `users.ts` `rotateKey` (POST) and
  `toggleDisabled` (PATCH) do not disable their trigger button while in flight, so a fast
  double-click could issue two requests. Not part of finding M3; flag for a future pass if
  desired.

## Files changed (this re-check)

- `DESIGN.md` — §8 scope clarified (affirmative ban vs permitted negations).
- `docs/specs/cabinet.md` — §2.2 and §2.4 wording aligned to DESIGN §6.3.
- `docs/specs/design-qa-report.md` — this re-check section.

No application code changed; no commit made.

---

# Round 3 — Phase 3 Design QA: приглашения + «Разрешить общий секрет»

**Verdict: FAIL** (3 spec violations require fixes: 1 backend↔frontend payload mismatch,
1 broken `hidden` cascade affecting 7 elements, 1 ordering requirement). Sources inspected:
`ui/pages/panel/views/users.ts`, `ui/pages/panel/{index.html,page.css}`,
`ui/pages/signin/{index.html,main.ts,page.css}`, shared `ui/{base,tokens}.css` +
`ui/components/{misc,modal,badge,table,form,card}.css`, generated
`registry/internal/webassets/panel.html` (+`signin.html` freshness), backend
`registry/internal/server/{invites_admin_api,users_admin_api,cabinet_api}.go` and
`registry/internal/control/{invites,control}.go`. Reviewed against
`docs/specs/invitations-legacy-ui.md` (authoritative) and
`docs/specs/invitations-migration-contract.md` §2–§3. Application code not edited; no commit.

`bash scripts/build_ui.sh -check` → all five pages «без изменений» ✅.

## What passes (verified)

| Item | Evidence |
|---|---|
| **One-time invite_url reveal + cleanup** | Success modal renders `readonly` input, `esc()`-ed (`users.ts:446`, quote-safe `dom.ts:5-7`); `closeModal` does `modal-body/foot.replaceChildren()` (`users.ts:93-94`) → URL leaves DOM on Close/Escape/backdrop; closure dies with the listeners; no re-reveal after reload (list has prefix only). No secret in storage (only panel token in `state.ts:26`, theme on signin), toasts (`showToast` gets static text), `title`, or `aria-label`. |
| **Real clipboard semantics** | «Скопировано» + polite toast only inside `writeText().then()` (`users.ts:463-468`); rejection shows exact spec fallback «Не удалось скопировать. Выделите ссылку и скопируйте вручную» (`:449,470`) — no rotateKey-style false success; re-copy allowed while open (1.5 s label reset, `:468`); non-secure context → select-for-manual-copy (`:473-476`). |
| **Create-invite modal** | Exact title/caption; TTL select 24/72/168/720 within contract 1..720, default 72 preselected (`users.ts:403-408`); no email/role/quota; submit «Создание…» + disabled (`:418`); error stays inside modal, input preserved (`:409,429-431`). |
| **Unknown create outcome** | `err.status === undefined` → exact text «Результат создания неизвестен. Проверьте список приглашений» + list refresh, no auto-POST (`users.ts:423-428`). |
| **Statuses / badges / columns** | Активно `.badge.ok`, Использовано `.badge.info`, Истекло/Отозвано neutral `.badge` (`users.ts:337-342`); columns Приглашение (prefix, not token)/Создано/Действует до/Статус/Действия (`index.html:216`); dates absolute via `Intl.DateTimeFormat("ru-RU", …timeZoneName:"short")` (`users.ts:31-33`). |
| **Revoke flow** | Confirm texts exact (`users.ts:480-481`); «Отмена» gets initial focus (`fillModal` focuses first foot button, `:84`); danger → «Отзыв…» disabled (`:486`); status updated only after server ack, row not deleted (`:487-495`); already-used/expired explained («созданный аккаунт не изменён»), never claims account revoked; backend revoke idempotent 204 (`invites_admin_api.go:116-117`). |
| **List states** | Skeleton first-load only in section (`index.html:217`); empty text exact (`users.ts:358-360`); load error «Не удалось загрузить приглашения» + «Повторить», no fake empty (`:349-354`); refresh error keeps old list + stale banner (`:391-393`); first-load error hides mutations (`:388`, modulo H-INV2 below). |
| **OFF confirmation** | Modal «Отключить общий секрет?» with full machine-route enumeration, no-disconnect statement, counter banner or unknown text, «До отключения обновите агенты…» (`users.ts:595-598`); «Отмена» initial focus; Escape/backdrop/cancel send nothing — checkbox restored to ON until confirm (`:637-640`); double-submit blocked («Сохранение…» disabled, `:603`). |
| **persisted / 5xx / unknown** | `persisted === false` → «Изменение не сохранено постоянно…» (`users.ts:575-578`); HTTP error → «Не удалось сохранить режим. Изменение не подтверждено» (`:585`); lost response → «Результат сохранения неизвестен…» + re-read truth, no fictional rollback, no auto-retry (`:584,557-564`); backend is fail-closed 5xx, `persisted:true` hardcoded (`invites_admin_api.go:155-161`). |
| **affected_nodes** | `null` → «Число затронутых нод неизвестно…» (`users.ts:509-510`); `>0` → banner + N + «Данные на {as_of}» (`:511-513`); 0 → banner suppressed (deliberate; spec sentence degenerates at N=0 — observation O1). Counter sourced only from server (`LegacySecret`), never from user counts. |
| **Load/read states (legacy)** | Skeleton + disabled control while loading (`index.html:238`, `users.ts:522-524,536-539`); read error never substitutes OFF — control stays hidden, «Не удалось загрузить режим совместимости» + «Повторить» (`users.ts:543-553`). |
| **Signin invite-only gating** | Register tab without token → exact gate text + «Войти» (`signin/index.html:74-77`); no manual token input, no bypass. |
| **?invite handling** | Read once into memory, URL cleaned synchronously via `history.replaceState` before any navigation (`signin/main.ts:199-207`); not a form field, only inside POST body `{username,password,invite}` (`:159-160` = contract §2); no storage. |
| **Reason banners** | All five codes `invite_expired/used/revoked/invalid/required` map to the exact spec texts (`signin/main.ts:57-63` = spec §4 table); backend emits exactly these codes with 403 (`users_admin_api.go:53-62`); terminal reasons lock the form, keep «Войти» (`:70-74`). |
| **No false failure mapping** | Invite reasons checked before generic validation (`:177-179`); 409 «Логин уже занят» keeps the invite for retry (`:181`); network/503 → «Нет связи…»/«Сервис временно недоступен», never read as expiry/revocation; 429 exact text (backend sends no wait — `rate_limited` only, `cabinet_api.go:335`); no auto-retry on register. |
| **Success flow** | Exact text; passwords cleared; INVITE/INVITE_DEAD nulled; URL re-cleaned; switch to Вход with only username filled (`signin/main.ts:164-174`); active session redirects to /cabinet without consuming (`:211-213`). |
| **a11y (majority)** | Real buttons; labelled TTL select; checkbox label wraps input (whole label clickable), `:focus-visible` ring on `.box` (`form.css`), `.legacy-check{min-height:24px}` (`panel/page.css:328`); sm buttons 28px, `.in-btn` 26px; modal `role="dialog" aria-modal="true"`, Tab/Shift+Tab trap, Escape, focus return to initiator, `fillModal` preserves return point (`users.ts:57-113`); `inert` on background; errors `role="alert"` (`inv-create-err`, `inv-copy-err`, `legacy-err`, `auth-alert`, `reg-invite-alert`); copy/save success via `#toast aria-live="polite"` (`panel/index.html:279`); roving tabindex + arrows on signin tabs (`signin/main.ts:44-97`); skeleton shimmer has `prefers-reduced-motion` kill (`misc.css`). |
| **Mobile 320/720** | `.table-cards` → cards ≤720 with `data-label` on every invites cell; actions row `.u-acts .btn{width:100%}` (`panel/page.css:315`); modal width 100%−40px (280px @320); `word-break:break-all` on URL field; no page-level horizontal scroll at 320 (auth wrap 400px max, `.table-wrap` internal). |
| **Design system** | Zero new colors — all `var()` tokens; `--gap:14`, `--pad:18`, `--ctl-h:34`, body/h2 13.5px/640 match spec §2 (`tokens.css:73-75`, `base.css:38-41`); high-contrast warnings forced to `--fg` (`panel/page.css:330`, `signin/page.css:37`); reuse of `.card/.table-cards/.field/.check/.btn/.badge/.banner/.skeleton/.secret-reveal`/panel modal only; Russian operator register; section order Users → Invitations → Compatibility without new routes. |
| **Prior-round fixes intact** | Modal focus trap + inert (H1) and toast `aria-live` (M4) present in current sources — no regression. |

## Findings (required fixes)

### HIGH

**H-INV1 — `expires_at` unit mismatch: backend sends unix seconds, frontend parses as ISO → «действует до 21.01.1970» in the creation modal.**
`invites_admin_api.go:53,63` serializes `CreateInvite`'s `expiresAt int64` = `now.Add(ttl).Unix()`
(`control/invites.go:44,56`) as a JSON **number**. Frontend declares it `expires_at: string` and
formats with `fmtISO` (`users.ts:419`, `:444`): `new Date(1791884400)` → **milliseconds** →
Jan 1970; `isNaN(+d)` is false, so the fallback never fires and a wrong 1970 date is shown to the
admin (esbuild does not type-check, so this ships silently).
Required fix (frontend, one line): type the field `expires_at: number` and render
`fmtDate(r.expires_at)` (same unix-seconds formatter the list already uses, `users.ts:33,365-366`).
(Backend could alternatively emit RFC3339, but frontend-only is the minimal change and keeps the
list/create formatters consistent.)

**H-INV2 — Broken `hidden` semantics: `.banner`, `.btn`, `.legacy-row` set `display` in author CSS, which overrides the UA `[hidden]{display:none}` rule. Seven elements are always visible / their toggles are no-ops.**
The repo itself documents this trap (`signin/page.css:29`: «класс .banner задаёт display и
перебивает UA-правило») and patches it on signin/cabinet — but the panel additions ship no
`[hidden]` guards: `grep '\[hidden\]' registry/internal/webassets/panel.html` → 0 matches for
these selectors, while `misc.css:.banner{display:flex}`, `form.css:.btn{display:inline-flex}`,
`panel/page.css:326:.legacy-row{display:flex}`. Affected, with consequences:
- `#inv-copy-err` (`users.ts:449`, toggled `:467,470,474`) — «Не удалось скопировать…» is
  **permanently visible in the success modal**, telling the admin copying failed when it succeeded;
  subsequent toggle does nothing. False failure messaging.
- `#inv-create-err` (`users.ts:409,417,427,431`) — empty red banner visible inside the create modal
  from first paint; real errors visually merge with it.
- `.legacy-row` / `#legacy-row` (`index.html:231`, toggled `users.ts:521,537,547`) — checkbox row
  visible during «Загрузка…» **and after read error**, rendered unchecked = visually substituting
  OFF, which spec §5 forbids («не подставлять OFF»); the enabled unchecked checkbox also invites
  toggling against no state.
- `#legacy-err`, `#legacy-warn` (`index.html:239-240`) — empty bordered banners visible from page
  load; read-error text appears inside an already-visible box, warn text appears for everyone.
- `#invites-stale` (`index.html:220`, toggled `users.ts:380,392`) — «Список мог устареть…» visible
  from first paint; a false staleness claim is always on screen.
- `#invites-create` (`.btn primary`, `index.html:212`, hidden on first-load error `users.ts:388`) —
  button stays visible and clickable when mutations must be unavailable (spec §3).
Required fix (panel `page.css`, one rule):
`#invites-stale[hidden], #legacy-err[hidden], #legacy-warn[hidden], #invites-create[hidden], .legacy-row[hidden], #inv-create-err[hidden], #inv-copy-err[hidden] { display: none; }`
(mirrors the sanctioned pattern in `signin/page.css:31,38,41` and `cabinet/page.css:46,85`).
Re-run `build_ui.sh` after — CI fails on stale assets.

### MEDIUM

**M-INV1 — «Новые сверху» not implemented: list renders server ASC order.**
Spec §3: «Новые сверху». Backend returns `ORDER BY i.id ASC` (`control/invites.go:65`);
`renderInvites` maps `INVITES` as-is (`users.ts:363`). Required fix: sort descending before render —
`INVITES = [...(data.invites || [])].sort((a, b) => b.id - a.id);` (`users.ts:378`) — or flip the
backend `ORDER BY`; frontend-only keeps the contract list untouched.

**M-INV2 — Explicit ON has no «Сохранение…» indicator (spec §5: «явное включение… отправляет
сохранение, „Сохранение…", контрол заблокирован»).**
`saveLegacy(true)` (`users.ts:642`) only disables the checkbox (`:570`); OFF shows «Сохранение…» on
the modal button, ON shows nothing — the operator gets silent feedback for a state-changing action.
Required fix: while saving, show «Сохранение…» adjacent to the row (e.g. swap
`#legacy-state-badge` content to `<span class="badge">Сохранение…</span>` and restore from
`applyLegacy`, or add a `role="status"` span) — same element works for both ON and OFF paths.

**M-INV3 — Modal is not viewport-height-bound; body does not scroll (spec §2: «модалка ограничена
viewport, её тело прокручивается, действия доступны»).**
`modal.css` sets no `max-height`/`overflow` on `.modal-box`/`.modal-body`; at 320×568 the tallest
dialogs (Приглашение создано with error, Отключить общий секрет?) fit, but at 200% zoom or short
landscape viewports the foot buttons are unreachable (acceptance criterion #5 includes 200% zoom).
Required fix: `.modal-box { max-height: calc(100dvh - 40px); }` + `.modal-body { overflow-y: auto; }`
(shared `modal.css`; guards `dvh` with an `vh` fallback line). Improves pre-existing flows too;
does not alter visuals at normal sizes.

### LOW

**L-INV1 — Success announced as `role="alert"` (spec §6: успех — polite status).**
`alertBox("Аккаунт создан…", true)` reuses `#auth-alert` which is hard-coded `role="alert"`
(`signin/index.html:46`). Required fix: toggle `role` in `alertBox` — `"alert"` for errors,
`"status"` for `info` success.

**L-INV2 — Create-invite error after Escape is silently lost.**
Escape during «Создание…» closes the modal and `closeModal` detaches `#inv-create-err`
(`users.ts:93`); the later `.catch` writes to the detached node (`:430-431`) — user sees nothing.
Required fix: in the catch, if `$("modal")` lacks `.show`, report via `showToast(e.message, true)`
instead of the detached banner.

**L-INV3 — Register lost-response message doesn't offer «войти» (spec §4: «предложить вход, если
аккаунт мог уже создаться»).**
`signin/main.ts:190`. Required fix: «Нет связи с регистратором. Повторите позже. Если аккаунт уже
создан — войдите.»

**L-INV4 — Hardening: `persisted === false` should be `persisted !== true`.**
Backend always returns `persisted:true` (`invites_admin_api.go:161`) and 5xx on failure, so the
current check is dead code per contract — but a future envelope that omits the field would yield a
green success. `users.ts:575`. One-character-class fix; not blocking.

### Observations (non-blocking)

- **O1** — `affected_nodes: 0` hides the warn banner entirely (`users.ts:514`); spec's banner
  sentence degenerates at N=0, and the standing card-note still carries the caution, so this is a
  defensible reading of «Проверены N>0, N=0 и неизвестный N».
- **O2** — Escape during in-flight OFF save closes the modal while the request continues; the
  change was already confirmed, the outcome still arrives via toast + re-read, and the checkbox
  stays disabled until settle — no mutation violation.
- **O3** — Success modal re-opens (with the one-time link) if the creation response lands after the
  user escaped; correct for a one-time secret, worth a comment if touched.
- **O4** — `#reg-invite-alert` uses `display:flex` with an explicit `[hidden]` guard — correct;
  signin has **no** H-INV2-class defects (verified: `.auth-form[hidden]`, `.reg-gate[hidden]`,
  `.banner[hidden]`, `.reg-invite-note` has no author `display`).

## Backend↔frontend payload cross-check (contract §2–§3)

| Endpoint | Frontend | Backend | Verdict |
|---|---|---|---|
| `POST /panel/api/admin/invites` | `{ttl_hours}` → `{id, invite_url, expires_at, prefix}` | same (`invites_admin_api.go:38-65`); 201 | ✅ names; ❌ `expires_at` unit → H-INV1 |
| `GET /panel/api/admin/invites` | `{invites:[{id,prefix,created_at,expires_at,status,used_by,used_by_username}]}` | same, 4 error envelope `{error,message}` (`:87-100`) | ✅ |
| `DELETE /panel/api/admin/invites/{id}` | 204 handled (`users.ts:45`) | 204 idempotent (`:116-117`) | ✅ |
| `POST /api/auth/register` | `{username,password,invite}` | same (`cabinet_api.go:338-344`) | ✅ |
| invite error codes | `invite_required/invalid/expired/used/revoked` @403 | identical strings @403 (`users_admin_api.go:53-62`) | ✅ |
| 409/429/503 | «Логин уже занят»/429 text/«Сервис временно недоступен» | `duplicate` @409, `rate_limited` @429 (no wait signal), `users_disabled`/503 | ✅ |
| `GET /panel/api/admin/legacy-secret` | `{enabled, affected_nodes:number\|null, as_of:RFC3339}` | same (`invites_admin_api.go:124-139`) | ✅ |
| `POST /panel/api/admin/legacy-secret` | `{enabled}` → `{enabled, persisted}` | `{enabled, persisted:true}`, fail-closed 5xx (`:144-162`) | ✅ (see L-INV4) |

## Required fixes before PASS

1. **H-INV1** — `fmtDate(r.expires_at)` + `number` type (`users.ts:419,444`).
2. **H-INV2** — add the seven `[hidden]` display guards to `ui/pages/panel/page.css`, then
   regenerate webassets (`bash scripts/build_ui.sh`).
3. **M-INV1** — sort invites newest-first before render (`users.ts:378`).
4. **M-INV2** — visible «Сохранение…» state for the legacy toggle (ON and OFF paths).
5. **M-INV3** — modal `max-height` + scrollable body in `ui/components/modal.css`.

## Verification

- Generated assets: `verified` (`build_ui.sh -check` exit 0, all pages unchanged).
- Source/static design review incl. backend payload cross-check: `verified`.
- Rendered-browser pass (contrast pairs at runtime, keyboard walk, 320/720/zoom screenshots):
  `not_verified` (no server started — same limitation as prior rounds; H-INV2 is proven
  statically from the generated bundle's CSS, which is authoritative for runtime).

---

# Round 4 — Phase 3 Design QA RE-CHECK (fix round 3)

**Verdict: PASS** (source + built-bundle verified). Every Round-3 finding (H-INV1, H-INV2,
M-INV1..3, L-INV1..4) is resolved in the updated sources, and the generated `panel.html`/`signin.html`
are in sync (`build_ui.sh -check` → exit 0, all six pages «без изменений»). No regression found in the
sweep below. The only standing item is the rendered-browser a11y pass, which cannot run without a
live registry server. Application code was **not** edited in this re-check; no commit made.

Sources re-inspected: `ui/pages/panel/views/users.ts`, `ui/pages/panel/page.css`,
`ui/components/modal.css`, `ui/pages/signin/main.ts`, `ui/pages/panel/index.html`,
and the generated `registry/internal/webassets/{panel,signin}.html`.

## Per-finding status

| # | Verdict | Evidence |
|---|---|---|
| **H-INV1** | ✅ PASS | `users.ts:16` `AdminInvite.expires_at: number`; create response typed `expires_at: number` (`:420`); `showCreatedInvite` renders `fmtDate(r.expires_at)` (`:448`). `fmtDate = (unixSec) => DTF.format(new Date(unixSec*1000))` (`:33`) — same unix-seconds formatter as the list (`:365-366`). No `new Date(number)` ISO misuse remains. |
| **H-INV2** | ✅ PASS | `panel/page.css:336` ships global `[hidden] { display: none; }` as the **last** rule of the file (after all `@import`s). Verified in the built `panel.html`: `.btn{display:inline-flex}` @23002, `.banner{display:flex}` @37844, `.legacy-row{display:flex}` @59654, global `[hidden]{display:none}` @59856 — i.e. the guard is source-later than every rule it must override. Specificity is equal (0,1,0) for `.banner`/`.btn`/`.legacy-row`, so later wins; the compound selectors `.banner.bad`/`.banner.warn`/`.btn.primary` set **no** `display` property, so they do not out-rank the guard. The 7 elements (`#invites-stale`, `#invites-create` `.btn primary`, `#legacy-err`, `#legacy-warn`, `#legacy-row`/`.legacy-row`, and the JS-built `#inv-create-err`/`#inv-copy-err` `banner bad`) now genuinely hide. |
| **M-INV1** | ✅ PASS | `users.ts:379` `INVITES = [...(data.invites || [])].sort((a, b) => b.id - a.id)` — «Новые сверху» enforced client-side, server ASC untouched. |
| **M-INV2** | ✅ PASS | `users.ts:577` `saveLegacy` sets `#legacy-state-badge` ← `<span class="badge">Сохранение…</span>` on the common path, so **both** ON (`:648`) and confirmed OFF (`:610`) show it; `applyLegacy` restores the real state from server (`:529-531`). |
| **M-INV3** | ✅ PASS | `modal.css:16` `.modal-box{ max-height: calc(100vh - 40px); max-height: calc(100dvh - 40px); }` (vh fallback + dvh override); `:19` `.modal-body{ overflow-y:auto; min-height:0 }`; `:18` `h2{flex:none}`, `:20` `.modal-foot{flex:none}` — actions stay reachable at 200% zoom / short landscape. |
| **L-INV1** | ✅ PASS | `signin/main.ts:31` `el.setAttribute("role", info ? "status" : "alert")` — success = polite status, errors = alert (`:167` passes `info=true`). |
| **L-INV2** | ✅ PASS | `users.ts:431` catch: `if ($("modal").classList.contains("show")) { errBox.textContent = msg; errBox.hidden=false; } else { showToast(msg, true); }` — Escape-closed case now reported via toast, not a detached node; lost-response still refreshes the list (`:429`). |
| **L-INV3** | ✅ PASS | `signin/main.ts:192` «Нет связи с регистратором. Повторите позже. Если аккаунт уже создан — войдите.» |
| **L-INV4** | ✅ PASS | `users.ts:581` `if (r.persisted !== true)` — missing field now treated as failure, not green success. |

## Regression sweep (re-verified)

| Item | Verdict | Evidence |
|---|---|---|
| One-time reveal cleanup | ✅ | `closeModal` `$("modal-body").replaceChildren()` + `$("modal-foot").replaceChildren()` (`users.ts:93-94`); URL only in modal DOM, wiped on Close/Escape/backdrop. |
| Invite reason banners vs backend 403 codes | ✅ | Frontend `INVITE_BANNERS` keys `invite_expired/used/revoked/invalid/required` (`main.ts:60-64`) ≡ backend `users_admin_api.go:54-62` (all `403`, same strings). |
| OFF confirm semantics | ✅ | `confirmLegacyOff` (`:598`); checkbox forced back to `checked = true` until confirm (`:645`); Escape/backdrop/cancel send nothing; confirm calls `saveLegacy(false)` (`:610`) with «Сохранение…» disabled button. |
| affected_nodes 0 / null | ✅ | `legacyWarnText` (`:513-519`): `null` → «Число затронутых нод неизвестно…»; `>0` → banner + N + «Данные на …»; `0` → `""` → `warn.hidden = true`. |
| signin gating + `?invite` cleanup | ✅ | `renderRegister` gate/terminal/form (`main.ts:67-82`); `?invite` read once (`:203`), URL cleaned via `replaceState` before any nav (`:207`); success re-cleans (`:173`); active session doesn't consume (`:213-214`). |
| a11y | ✅ | Modal focus-trap + `inert` (`users.ts:60-113`); `aria-live="polite"` toast `panel/index.html:279`; `role="alert"` on error banners, `role="status"` on success; roving tabindex + arrows on signin tabs. Unchanged. |
| No new colors | ✅ | `grep` for hex/rgb/hsl in all four changed files → none; all `var()` tokens. |
| Secrets never persisted | ✅ | `INVITE` memory-only (no storage/cookie/URL after read); `grep localStorage\|sessionStorage\|document.cookie` in changed files → none; rotate key stays in one-time `.secret-reveal`, wiped by `closeModal`. |
| Generated assets fresh | ✅ | `bash scripts/build_ui.sh -check` → exit 0, all six pages «без изменений» (bundles authoritative for runtime). |
| No console/debug | ✅ | `grep console\.` in changed files → none. |

## Remaining not_verified

- **Rendered-browser a11y pass — `not_verified` (no server).** Runtime contrast ratios, the modal
  `inert` + Tab-cycling behaviour, and 200%/320 px modal scroll were verified by static source +
  built-bundle CSS review only; the bundle CSS is authoritative for the `[hidden]` cascade. Recommend
  `/impeccable audit` + a keyboard/zoom pass on a running registry when a server/browser is available.

## Files changed (this re-check)

- `docs/specs/design-qa-report.md` — this Round 4 re-check section only.

No application code changed; no commit made.

---

# Round 5 — addendum §6/§7 (переменные профиля, waiting_vps/retry, свои профили, приглашения N, masters_per_domain)

**Verdict: PASS** (no blocker, no critical defect). The single most important
check — **no affordance anywhere can create a second paid order** — is satisfied
and verified end-to-end. Two MEDIUM deviations and several LOW observations are
recorded below; none are release-blocking because each fails safe (idempotent
`idem_key`, server-side refusal, or clear error text) with no wrong data and no
double charge.

Reviewed against `docs/specs/addendum-profiles-vps.md` (§1–§8),
`docs/specs/implementation-summary-addendum.md`, `docs/specs/implementation-summary-ui.md`
(last two rounds), `docs/api-contract.md` (§8), `DESIGN.md`. Sources inspected:
`ui/lib/vars.ts`, `ui/components/vars.css`,
`ui/pages/panel/views/{profiles.ts,users.ts,settings.ts}`,
`ui/pages/panel/{index.html,page.css,state.ts}`,
`ui/pages/cabinet/{main.ts,index.html,api.ts,page.css}` and backend
`registry/internal/{control/profiles.go,control/invites.go,provision/provision.go,server/{profiles_admin_api.go,profiles_user_api.go,cabinet_beget.go,invites_admin_api.go,users_control.go}}`.
Static source + built-bundle review; no dev server started. Application code was not edited; no commit.

## Per-item findings

### (1) Variables editor (admin + user) vs real `vars_schema` and server validation — **PASS**

- **Shape matches.** Client `ProfileVar`/`parseVarsSchema`/`serializeVarsSchema`
  (`ui/lib/vars.ts:12-68`) emit exactly `{"vars":[{name,default,description,secret}]}`
  ≡ `control.ProfileVar` (`registry/internal/control/profiles.go:408-413`). Empty
  rows dropped (`vars.ts:59`), empty set → `""` (`:67`), matching server `omitempty`
  round-trip.
- **Client never promises a name the server rejects.** Client forbidden set
  (`ui/lib/vars.ts:22`) ≡ server `forbiddenVarNames` (`profiles.go:419-422`); both
  use `ansible_*` prefix via lowercase (`vars.ts:30` ↔ `profiles.go:426`); both use
  the same regex (`vars.ts:19` ↔ `profiles.go:415`). No name is accepted by the
  client that `ValidateProfileVarsSchema` (`profiles.go:451-465`) or
  `ValidateExtraVars` (`:491-516`) would reject — the client can only over-reject.
- **Server errors shown verbatim.** `profiles.ts:143-148` (admin) and
  `cabinet/main.ts:419-427` (user) put `e.message` into `#p-err`/`#mp-err`
  unchanged; the HTTP path preserves text: `controlAPIError` writes
  `{"error":code,"message":msg}` (`users_control.go:129-133`) and both clients read
  `data.message || data.error` (`users.ts:50`, `cabinet/api.ts:106`). Playbook
  sandbox (`playbook_rejected`) and `validation_failed` texts are therefore shown
  as-is. Note `firstVarNameError` (`vars.ts:140-146`) pre-empts the forbidden/regex
  cases before the round-trip, so the *server's* wrapper text for those specific
  cases is only reachable if the client check is bypassed — this is a UX bonus, not
  a promise mismatch.
- **Secret values not rendered plaintext (partial — see L2).** The wizard masks
  declared secret values on the confirm step (`cabinet/main.ts:693-700`, `•••`) and
  uses `type="password"` for declared secret inputs (`:678`); run output is masked
  server-side (`provision.MaskSecrets`, `provision.go:374-381`). The profile
  *editor's* `default` field for a `secret` var is a plain input (`vars.ts:76`) —
  LOW L2.

### (2) Wizard variables step + request shape — **PASS**

- Defaults pre-filled: `val = st.vars[d.name] === undefined ? (d.default ?? "") : …`
  (`cabinet/main.ts:672`). Overrides collected on `change`/`input` (`:785-788`).
- Extra vars via `#wiz-xadd` (`:790-796`), validated by `varNameHint`
  (`varsValid`, `:741-745`) → Next-disabled on forbidden names.
- Confirm step masks declared secrets (`:693-700`) and disables submit until the
  confirm checkbox is ticked (`footHTML` `:726`, `confirmHTML` `:717`).
- **Request shape is an exact match.** UI body
  `{region, configuration_id, profile_id, vars, confirm, idem_key}`
  (`cabinet/api.ts:70-77`, sent at `:840-846`) ≡ backend struct
  (`cabinet_beget.go:242-249`). No field-name mismatch.

### (3) waiting_vps / retry / no-second-order — **PASS on the money invariant; PARTIAL on retry-gating (M1)**

- **No second paid order — verified.** `createVps` is called from exactly one
  place, the wizard submit (`cabinet/main.ts:840`; grep of `ui/` confirms no other
  caller). `idem_key` is generated once per attempt (`:827`, `:837`) and reused;
  backend `POST /api/me/vps` returns the existing operation for a repeated key
  (`cabinet_beget.go:264-270`) and `RetryInstall` never calls `CreateVps`
  (`provision.go:510-545`). The retry button explicitly states it does not buy a
  new VPS (`cabinet/main.ts:271,550,552,987`). No "recreate" affordance exists on
  nodes/operations. **This check passes.**
- **`waiting_vps` distinct + non-terminal.** `OP_META` badge `.warn` «ждём
  готовности VPS» (`cabinet/main.ts:34`); the ops note and wizard banner state it
  is non-terminal, the slot is held, and no new order is possible
  (`:270-272,549-550`). Retry is offered for `waiting_vps` and is valid (backend
  allows it — it is not in the refuse switch, `cabinet_beget.go:558-568`).
- **M1 (MEDIUM) — retry shown for `failed` ops that have no VPS.** `actionCell`
  (`cabinet/main.ts:233-239`) renders «Повторить установку» for **every**
  `state === "failed"`, but `operationJSON` exposes no `provider_vps_id`/`has_vps`
  (`cabinet_beget.go:209-218`), so the client cannot distinguish a failed install
  (VPS exists → retry valid) from a `pending→cancelled` failure or a
  runner-unavailable / pre-create validation failure (no VPS → retry invalid,
  `cabinet_beget.go:554-557` returns `no_vps`). The user gets a clean toast
  (`retryErrText`, `cabinet/main.ts:278-285`) and no wrong state — but requirement
  (3) "retry-install appears only where valid" is not fully met.
  **Required fix (one of):** add a boolean (`has_vps`/`retryable`) to
  `operationJSON` and gate `actionCell` on it; or exclude never-created failed ops
  from the `failed` retry set client-side once such a discriminator exists.

### (4) Own unpublished profile — **PASS**

- Labelled «мой профиль»: `cabinet/main.ts:654` badge shown when
  `p.published === false`; own drafts are `enabled`/selectable (no `disabled`
  attribute) — `:652`. `allProfiles()` dedups published-first (`:627-635`).
- Versions shown: user profile list «версий: N» (`:356`) from `OwnProfile.versions`;
  admin table column «Версий» (`profiles.ts:33`); backend supplies it
  (`profileAdminJSON.Versions`, `profiles_admin_api.go:30-31,42-44`).

### (5) masters_per_domain control — **PASS**

- Range 1..8 on the input (`settings.ts:131`) and client-side `1..8` guard
  (`:373-377`) ≡ server `maxMastersPerDomain = 8` (`deps_config.go:18`,
  `config.go:621`), server validates 1..8 (`config_editor.go:240-242`,
  `config.go:387`).
- Hint names it explicitly and disowns `[srmd] max_nodes_per_domain`
  (`settings.ts:132-133`); DNS-rebuild warning that links don't change
  (`:135`); inline error box `#s-sel-err` (`:136`) filled from the server message
  (`:379`). Load reads effective `c.selection.masters_per_domain` (`:251`).

### (6) invites «Число регистраций» — **PASS**

- Field 1..1000 default 1 (`users.ts:414-416`), client guard 1..1000 (`:427-431`)
  ≡ server `inviteMaxUsesMin/Max` (`invites_admin_api.go:22-24,58-63`).
- «N/M использовано» column (`users.ts:370`).
- `exhausted` is a distinct badge (`.warn` «Исчерпано», `:341`) vs `used`
  `.info`, `expired`/`revoked` neutral (`:339-345`), matching backend
  `Invite.Status` (`control/invites.go:32-35`). Revoke distinguishes exhausted
  (`users.ts:509`).

### (7) a11y / mobile / register / console — **PASS with LOW notes**

- **Modal trap + focus return:** `inert` on background siblings + Tab/Shift-Tab
  cycling + Escape + `lastFocused` restore (`cabinet/main.ts:56-108`,
  `users.ts:58-115`). `role="dialog" aria-modal="true"` on `.modal-box`
  (`cabinet/index.html:165`).
- **role=status/alert:** error banners `role="alert"`
  (`cabinet/main.ts:391`, `profiles.ts:103`, `panel/index.html:239`); toasts
  `aria-live="polite"` (`cabinet/index.html:162`, `panel/index.html:295`).
- **Targets ≥24px:** `.btn.sm` height 28px (`form.css:19`), `.btn.icon.sm` 28px
  (`:21`). LOW L5: the `.vars-secret` `.check` label wraps a 17px box
  (`form.css:59`) — the app-wide pre-existing `.check` pattern, not a new regression.
- **Mobile 320/720:** vars grid collapses to 2 columns at ≤720px
  (`vars.css:28-31`); wizard steps scroll horizontally (`cabinet/page.css:26-30`);
  `@media (max-width:640px)` stacks the wizard footer (`:88-91`).
- **No new colors:** `vars.css` uses existing tokens only (`--fg-3`, `--surf-3`,
  `--line`, `--acc`, `--acc-soft`, `--bad`).
- **Russian operator register:** all new copy is Russian.
- **LOW L1 — `console.warn` remains:** `ui/pages/panel/main.ts:183`. Pre-existing
  (present in `HEAD`, not introduced this round), but item (7) says no `console.*`;
  the prior sweeps only grepped `console.log`. Recommend an `eslint no-console`-style
  guard or removing it.

### (8) Webassets freshness — **PASS**

- `bash scripts/build_ui.sh -check` → **exit 0**, all six pages «без изменений».
  `ui/components/vars.css` is imported by both `panel/page.css:9` and
  `cabinet/page.css:9`; `ui/lib/vars.ts` is bundled by uigen/esbuild.

## Regression sweep

| Item | Verdict | Evidence |
|---|---|---|
| No second paid order | ✅ | Single `createVps` call site; `idem_key` fixed per attempt; `retry-install` never calls `CreateVps` (`provision.go:510-545`). |
| `[hidden]` global guard still effective | ✅ | `panel/page.css:337` `[hidden]{display:none}` as last rule (Round-3 H-INV2 fix intact); `#profiles-create`/`.banner` toggles work. |
| Unique-profile `duplicate` mapping | ✅ | `profiles.ts:145`; server returns `duplicate` via `writeControlErr` (`users_admin_api.go:45-46`). |
| Info-leak: `profile_id` in ops | ✅ | `operationJSON` includes `profile_id` (`cabinet_beget.go:214`); retry from the ops table sends `{}` and backend falls back to stored `op.ProfileID` (`:580-583`) — works without client context. |
| Busy-state on mutation buttons | ✅ | Retry buttons disabled + «Повтор…» (`cabinet/main.ts:562,992`); profile save/delete disabled (`:414,437`). |
| No new colors / tokens | ✅ | grep for hex/rgb/hsl in changed UI files → none. |
| Secrets: user Beget password cleared | ✅ | `cabinet/main.ts:1029`. |

## Remaining not_verified

- **Rendered-browser a11y + contrast pass — `not_verified` (no server).** Runtime
  contrast of tokens, the `inert`+Tab behavior at 320px/200% zoom, and the
  `.vars-row` grid at exactly 320px were reviewed by source/bundle only.
- **Live Beget catalog price field — `not_verified`.** `GetConfigurations`
  transport is mocked in tests; `price:null` handling is correct in code
  (`cabinet/main.ts:892-894`, `cabinet_beget.go:194-204`) but the real API shape is
  unconfirmed (documented risk).

## Findings summary (this round)

| # | Severity | Item | Finding | Fix |
|---|---|---|---|---|
| M1 | Medium | (3) | Retry button shown for `failed` ops with no VPS (cancelled-pending, runner-unavailable); backend `operationJSON` lacks `provider_vps_id`/`has_vps` | Expose `has_vps`/`retryable` in `operationJSON`, gate `actionCell` (`cabinet/main.ts:236-237`) |
| M2 | Medium | (3) | After a non-`done` result, returning to `#/create` reopens the stale confirm step with the same in-memory `idem_key`; only a page reload yields a fresh order (`leave()` nulls `result` but not `step`/`idemKey`, `cabinet/main.ts:580-584`) | Reset wizard to step 0 (or offer an explicit new-order action) for non-`done` results; fails safe today |
| L1 | Low | (7) | `console.warn` present (`ui/pages/panel/main.ts:183`), pre-existing | Remove or gate; add a no-console check |
| L2 | Low | (1) | `secret`-flagged var `default` rendered plaintext in profile editor (`ui/lib/vars.ts:76`) | Use `type=password` + reveal for secret rows, or document defaults must not hold real secrets |
| L3 | Low | (1)/(4) | Edit always sends `playbook` (`profiles.ts:126`) → description-only edit bumps a version and resets `published` | Send `playbook` only when the textarea changed |
| L4 | Low | (2) | Confirm step masks only *declared* secret vars; an *extra* var named `authkey`/`password`/`node_secret` renders plaintext (`cabinet/main.ts:693-699`) | Extend the masked set with the convention keys |
| L5 | Low | (7) | `.vars-secret` `.check` label target may be <24px (app-wide pre-existing pattern) | Add `min-height:24px` to `.check` if tightening WCAG 2.2 2.5.8 |
| L6 | Doc | — | `implementation-summary-ui.md` §«Деградации» says `profile_id` is not serialized in `GET /api/me/operations`; it now is (`cabinet_beget.go:214`) | Update the doc note |

## Files changed (this round)

- `docs/specs/design-qa-report.md` — this Round 5 section only.

No application code changed; no commit made.

---

# Round 6 — Phase 3 Design QA: широкий редактор профиля (модалка)

**Verdict: PASS** (source + built-bundle verified). The widening fix is
correct, scoped, token-neutral, and leaks to no narrow modal. No application
code touched; no commit.

Scope (branch `dev`): `ui/components/modal.css`, `ui/components/vars.css`,
`ui/pages/panel/views/profiles.ts`, `ui/pages/panel/views/users.ts`,
`ui/pages/cabinet/main.ts`, plus `registry/internal/webassets/{panel,cabinet,links}.html`
freshness. Verified against `docs/specs/implementation-summary-ui.md` §«Раунд —
UI fix: широкий редактор профиля» (`:437-476`) and prior rounds in this file.

## Per-requirement findings

### (1) The widened modal actually helps editing — **PASS**

| # | Check | Result |
|---|---|---|
| 1a | Wide box `max-width: min(1100px, 96vw)` | ✅ `modal.css:21`. Present in built `panel.html`/`cabinet.html` (`1100px, 96vw`). |
| 1b | Textarea tall enough | ✅ `.profile-editor textarea { min-height:260px }` `modal.css:23-26`; `260px` present in built bundles. |
| 1c | Monospace | ✅ textarea carries `class="mono"` — `profiles.ts:96`, `cabinet/main.ts:388`. |
| 1d | `wrap="off"` + horizontal scroll, no word-break | ✅ attribute on both textareas; CSS `white-space:pre; overflow:auto; overflow-wrap:normal` (`modal.css:25`). |
| 1e | Vars columns usable, inputs full width | ✅ desc column widened to `minmax(140px,1.6fr)`; inputs `width:100%; min-width:0` (`vars.css:11,14-20`). `minmax(140px` present in built bundle. |
| 1f | Body scrolls, footer stays pinned | ✅ `.modal-body{overflow-y:auto;min-height:0}` + `.modal-foot{flex:none}` with `max-height:calc(100dvh - 40px)` (`modal.css:16,27-28`). |

### (2) No regression (Escape / Tab trap / inert / scroll / close-reopen / focus return) — **PASS**

- Focus/inert/Escape/Tab logic is **untouched** by this round — it lives in
  `users.ts:60-116` (panel) and `cabinet/main.ts:57-109`; the diff only adds a
  `wide` parameter and a `classList.toggle`. `openModal` still stores
  `lastFocused`, `closeModal` still restores it and clears `inert`.
- `setPageInert` still skips `#modal`/`#toast`; both are direct body children
  (`panel/index.html:297`, `cabinet/index.html:164`), so the dialog subtree is
  never inerted.
- The class toggle is **explicit-boolean** on every open
  (`fillModal` `users.ts:74`, cabinet `main.ts:70`), so no state carries over.

### (3) Responsive at 320px / 720px — **PASS (by layout reasoning)**

- `width:100%` on `.modal-box` (`modal.css:12`) clamps the box to the grid cell
  (viewport − 40px padding) even where `96vw` exceeds it, so the box never
  overflows the viewport at 320px or 720px → **no horizontal PAGE scroll**.
- Vars grid: `≤720px → 1fr 1fr` (`vars.css:31-34`); `≤480px → 1fr` one column
  (`vars.css:36-39`, incl. 320px). The `480px` block (`vars-row/.vars-row-x`)
  is present in the built `panel.html`.
- Textarea's own horizontal overflow is confined to the element (`overflow:auto`),
  not the page.

### (4) No new colors/tokens, Russian register, no `console.*` — **PASS**

- `rg` for `#hex` / `rgb(a)` / `hsl` / `oklch` in `modal.css` + `vars.css` → **none**;
  only existing `var(--*)` tokens.
- Copy/comments Russian; no user-facing English.
- `rg console.(log|debug|warn|error|info)` across the five changed files → **none**
  (the pre-existing `panel/main.ts:183` `console.warn` from Round 5 L1 is outside
  this round's diff).

### (5) The wide class cannot leak to narrow modals — **PASS**

Exhaustive `openModal`/`fillModal` audit (16 `openModal(` tokens incl. the two
definitions). Only **two** callers pass `wide === true`, and both are the profile
editor:

| Caller | wide | Modal |
|---|---|---|
| `profiles.ts:91-108` | `true` | admin profile editor |
| `cabinet/main.ts:382-399` | `true` | user profile editor |
| `users.ts`: createUser(194), rotateKey(254), revokeKey(274), releaseNode(290), resetPassword(313), createInvite(405), revokeInvite(498), confirmLegacyOff(615) | default `false` | narrow ✅ |
| `users.ts:460` `fillModal` (showCreatedInvite) | default `false` | narrow ✅ |
| `profiles.ts:159` removeProfile | default `false` | narrow ✅ |
| `cabinet/main.ts`: removeProfile(434), confirmDeleteNode(952), retryInstall(995) | default `false` | narrow ✅ |

Because the toggle passes the boolean explicitly, opening any narrow dialog
after a wide editor **removes** `modal-wide`. `closeModal` not clearing the
class is invisible (`.modal { display:none }`), and no path opens a modal
without going through these two functions. `links.html` uses its own
`#qr-modal` (never `openModal`) → never widened.

## Webassets freshness — **PASS**

`bash scripts/build_ui.sh -check` → **exit 0**, all six bundles «без изменений»
(`cabinet`, `dashboard`, `links`, `panel`, `signin`, `stats`). Built bundles
contain the shipped rules: `modal-wide` + `1100px, 96vw`, `260px`,
`wrap="off"` (`panel.html`+`cabinet.html`), `minmax(140px` and the `@media
(max-width:480px)` vars single-column block. No stale generated files.

## Observations (non-blocking)

| # | Sev | Item | Note |
|---|---|---|---|
| O1 | Low | `ui/pages/links/page.css:33` page-local **unscoped** `.modal-box { max-width:340px; align-items:center; text-align:center }` | Links never opens a wide modal, and `.modal-box.modal-wide` (0,2,0) out-ranks `.modal-box` (0,1,0), so no bug today. Latent trap: a future wide modal on links would inherit `text-align:center`. If tightening, scope to `#qr-modal .modal-box`. Not part of this round. |
| O2 | Low | `modal.css` `.profile-editor textarea` rules ship into `links.html` via the shared import | Inert (no `.profile-editor` on links); a few hundred bytes, acceptable. |
| O3 | Low | `cabinet/main.ts:66` `openModal` types `onClick: () => void` but several handlers read `ev.currentTarget` | Pre-existing loose typing; runtime passes the event (listener attached directly), works. |

## Remaining `not_verified` (needs a real browser)

- **Rendered width / scrolling in a live DOM — `not_verified`.** Actual
  `min(1100px,96vw)` px, textarea 260px height, and horizontal scroll inside the
  textarea were confirmed by source + built CSS only.
- **Tab-focus order inside the widened editor — `not_verified`.** The trap code
  is unchanged, but the concrete first↔last cycle across the taller textarea +
  vars table + «глаз» reveal buttons needs a browser.
- **No page horizontal scroll at exactly 320px and 720px — `not_verified`.**
  Confirmed by layout reasoning (`width:100%` clamp + single-column vars), not
  by rendered measurement.

## Files changed (this round)

- `docs/specs/design-qa-report.md` — this Round 6 section only.

No application code changed; no commit.

---

# Round 7 — Phase 3 Design QA: «Ключ доступа» + «Свои серверы» (manual servers)

**Verdict: FAIL** (source + backend-wiring + built-bundle verified). Two blockers,
both on user-facing paths of the «Свои серверы» section; everything else passes.

Branch `dev`. Spec sources: `docs/specs/addendum-manual-servers.md` §1/§3/§4,
`docs/specs/implementation-summary-manual-servers.md`,
`docs/specs/implementation-summary-ui.md` (latest rounds),
`docs/api-contract.md` §2.1/§2.2. Authoritative code:
`ui/pages/cabinet/{index.html,main.ts,api.ts,page.css}`,
`ui/components/{modal,form,vars}.css`,
`registry/internal/server/{cabinet_manual.go,cabinet_api.go}`,
`registry/internal/machineapi/security.go`, `registry/internal/control/manual_hosts.go`.

## Blockers (must fix → back to Phase 2)

### B1 — Create-form validation surfaces **English** backend messages in a Russian UI
**Severity: Medium/High (register + primary first-run error path).**

`POST /api/me/servers` errors are shown verbatim:
- `ui/pages/cabinet/main.ts:513` — `submitServerForm` catch → `showErr(e.message …)`.
- `ui/pages/cabinet/main.ts:445` — `checkServer` catch → `showToast(e.message, true)`.
- `ui/pages/cabinet/main.ts:389` — `installErrText` `default: return e.message`.

But the backend messages are English:
- `registry/internal/machineapi/security.go:66` — `"ip must be an IPv4 address"`.
- `registry/internal/machineapi/security.go:70` — `"ip must be public and routable"`.
- `registry/internal/server/cabinet_manual.go:115` — `"label is required (1..64 chars)"`.
- `:122` `"host must not be the registry's own IP"`; `:128` port; `:136` `ssh_user`;
  `:141` exactly-one-secret; `:147` key > 16 KiB; `:150` invalid key.
- `registry/internal/server/cabinet_beget.go:489` `"user profiles are disabled"`,
  `:496` `"profile is not available"` (reached from `installErrText` default).

Reproduction: enter a private IP (or port `70000`, or no secret) → the Russian
form shows `ip must be public and routable` (etc.). The UI already localises
*install* codes (`installErrText`, `main.ts:381-391`) but not the create path, so
this is an **inconsistency inside the same section**, not just a backend wording
nit. `addendum-manual-servers.md` §4.2 requires «400 с внятной причиной» — the
reason exists but is not in the UI's language; `implementation-summary-ui.md`
§«Состояния»/«Верификация» claim Russian register.

**Exact fix (either, prefer 1):**
1. Localise at the source: return Russian `message` strings from
   `normalizeManualHost` (`cabinet_manual.go:112-158`), `validatePublicIPv4`
   call site, and `writeProfileSelectErr` (`cabinet_beget.go:487-497`); the
   machine-API callers of `ValidatePublicIPv4` (agent `/register`, health) keep
   their English code by mapping at the server layer, not in the shared helper.
2. Or add a client map in `submitServerForm`: on `e.code === "validation_failed"`
   translate the known reasons, and pre-check `host`/`port` client-side so the
   common cases never round-trip.

### B2 — No guard against adding the **same host twice** → a second manual quota slot
**Severity: Medium (requirement (3), "most important").**

The «Подключить свой сервер» form (`ui/pages/cabinet/index.html:97-145`) submits
`POST /api/me/servers` with no dedupe, and neither layer blocks a repeat:
- `registry/internal/control/manual_hosts.go:36-48` `CreateManualHost` — plain
  `INSERT`, no `UNIQUE(user_id, host, port)` (schema
  `registry/internal/control/control.go` has none on `manual_hosts`).
- `registry/internal/server/cabinet_manual.go:181-210` `handleMeServerCreate` —
  `normalizeManualHost` validates the host is public IPv4 but never checks the
  user's existing rows.
- Install keys the node on the record id, not the host:
  `cabinet_manual.go:313` `idem = fmt.Sprintf("manual-%d", host.ID)` →
  `nodeIDForIdem(idem)` (`:360`) → a distinct `node_id` per record → each record
  reserves its own quota slot (`AuthorizeNode`, `:364`).

Consequence: adding `203.0.113.10:22` a second time and pressing «Установить»
consumes a **second** quota slot for one physical host. This is the exact state
requirement (3) forbids. (It creates no paid order — manual install makes zero
provider calls — so the *paid* half of (3) is clean; the *manual-slot* half is not.)

**Exact fix (either):**
1. UI guard (cheap, immediate): in `submitServerForm` before the request —
   `if (SERVERS.some(s => s.host === host && s.port === (port || 22)))` →
   `showErr("Этот адрес уже подключён. Откройте его в списке или удалите прежнюю запись.")`.
2. Or durable backend guard: `UNIQUE(user_id, host, port)` on `manual_hosts`
   (+ map the violation to `409`/`validation_failed` with a clear message), and
   have `handleMeServerCreate` surface it; the UI shows it through the B1 fix.

## Per-requirement findings (everything above the blockers)

### (1) Key section — **PASS**

| # | Check | Result |
|---|---|---|
| 1a | One-time reveal, full key shown only in modal | ✅ `cabinet_api.go:531-539` returns `{authkey}` once; UI renders it in the `.secret-reveal` modal (`main.ts:272-282`). |
| 1b | Real clipboard semantics (no false success) | ✅ `copyText` `main.ts:224-244` — `navigator.clipboard.writeText` in a secure context, honest `Не удалось` + toast on failure; legacy path reports its own result. |
| 1c | Reissue invalidates old key immediately | ✅ note `main.ts:260-261` and reveal `:276`; matches `RotateAuthkey` (revokes all). |
| 1d | Revoke stops new registrations, existing nodes keep working | ✅ note `main.ts:261` + confirm modal `:290`; matches `RevokeAuthkeys` (touches no `node_owners`). |
| 1e | **ONE key covers ALL VPS** and copy is exact/not misleading | ✅ banner `main.ts:255` «Один ключ покрывает **все** ваши VPS. Раздельных ключей на сервер нет.» + reveal `:277`. Matches §1 model (key per user). |
| 1f | Confirm dialog for revoke | ✅ `confirmRevokeAuthkey` `main.ts:288-301`, danger button, busy label. |
| 1g | Prefix-only otherwise | ✅ `renderAuthkey` `main.ts:248` `${prefix}…`; `GET` payload `{prefix,created_at,active}` matches `authkeyJSON` (`cabinet_api.go:512-516`). |

### (2) Servers section — **PASS except B1**

| # | Check | Result |
|---|---|---|
| 2a | Error surfacing / reason codes | **FAIL → B1** (codes map correctly; message language does not). |
| 2b | Secret field cleared after submit | ✅ `clearServerSecret` `main.ts:477-481` resets `sv-pass`, `sv-key`, `sv-key-file`; called on success `:507`. (Retained on 400 — desirable.) |
| 2c | Key via textarea **AND** file picker | ✅ `index.html:137-138`; `FileReader` → textarea `main.ts:521-528`; file-input `aria-label` present; help states ≤16 KiB (matches `maxManualKeyBytes`). |
| 2d | Auth-type radiogroup semantics | ✅ role/aria present (`index.html:120-123`, `setAuthType` `main.ts:466-475`) — but keyboard behaviour incomplete → L1. |
| 2e | List states new/checked/installing/done/error | ✅ `serverState` `main.ts:308-318` → `новый/проверен/устанавливается/установлен/ошибка [установки]`; badges reuse existing classes. |
| 2f | «Проверить доступ» | ✅ `data-scheck` `main.ts:350`, disabled while installing; result toast from `{verified,last_error}` (`cabinet_manual.go:290`). |
| 2g | Install / retry labelled, nothing implies buy/create-again | ✅ «Установить» / «Повторить установку» (`main.ts:328`); card-note `index.html:147`; install modal banner `main.ts:396` — all state no new server/charge; matches `idem_key=manual-<id>`. |
| 2h | Delete with confirm | ✅ `confirmDeleteServer` `main.ts:448-461`, danger button, busy label; text matches backend (record removed, slot freed, pool node kept). |
| 2i | Operation output truncated + secrets masked | ✅ `.srv-out` `max-height:160px; overflow:auto` (`page.css:113-115`), wizard `<pre>` `max-height:240px` (`main.ts:851`). Masking server-side: `Secrets: []string{secretForMask}` (`cabinet_manual.go:429`) + `MaskSecrets`/`secretValues` (`provision.go:431,489`); nothing raw is echoed by the UI. |

### (3) No second paid server / second manual slot — **PAID half PASS, MANUAL half FAIL → B2**

- Paid path is single: `api.createVps` has exactly one caller, the wizard
  `main.ts:1150`. `retryInstall` (`:867`,`:1309`) reuses the created VPS
  (`has_vps` gating, `actionCell` `:539`). No label/flow implies buying again.
- Manual path reuses per **record** but not per **host** → B2.

### (4) a11y / responsive / register / colors — **PASS except B1 + L1/L2/L3**

- Modal trap / focus return / `role=alert`/`aria-checked`/`aria-live` — present
  (`main.ts:96-110,61-94`; `index.html:232`; `sv-err`/`mp-err` `role="alert"`).
- Targets: `.btn.sm` 28px, `.in-btn` 26px, `.seg` buttons ~29px ≥24px ✅.
  `.check .box` 17px row <24px → L3 (pre-existing, Round 5 L5 still open).
- 320/720: `.srv-grid` `auto-fit minmax(220px,1fr)` → one column at 320px;
  `@media (max-width:640px)` stacks wizard foot; `.vars-row` collapses at
  720/480px. No new breakpoint gaps seen in source.
- **No new colors**: `page.css` adds only layout on existing tokens; no hex/rgb
  literals in this round's CSS.
- **Russian register**: blocked only by B1.
- **No `console.*`** in `ui/pages/cabinet/**` or `ui/components/**` ✅.

### (5) Webassets freshness — **PASS**

`bash scripts/build_ui.sh -check` → **exit 0**, all six pages «без изменений»
(`cabinet`, `dashboard`, `links`, `panel`, `signin`, `stats`). No stale generated
files.

## Low findings (non-blocking)

| # | Sev | Item | Finding | Exact fix |
|---|---|---|---|---|
| L1 | Low | (2d) | `#sv-authtype` uses `role=radio`/`aria-checked` but both buttons stay tabbable and Arrow keys do nothing — diverges from APG and from the Round-4 signin tab fix (`signin/main.ts` roving tabindex) | Either add roving tabindex + `ArrowLeft/ArrowRight` in `setAuthType`/`initServerForm` (`main.ts:466-475,517-529`), or replace `.seg` with native `<input type="radio">`. |
| L2 | Low | (2f)/(1b) | Focus drop on re-render: `closeModal()` restores focus, then `loadServers()` replaces the trigger node (`main.ts:424-427`) → focus falls to body; same for the 5-s `renderAuthkey` (`:1230`) replacing «Перевыпустить» | After a list-replacing mutation, move focus to a stable element (e.g. the section heading) or re-focus by matching `data-sid`. |
| L3 | Low | (4) | `.check` label target height ~19-20px (<24px) for the wizard confirm box `#wiz-confirm` (`form.css:56-62`) — app-wide, carried from Round 5 L5 | Add `min-height:24px` to `.check` (and `.check .box` stays 17px inside). |
| L4 | Low | (2e) | `serverState` maps operation state `deleted` → «ошибка установки» (bad) (`main.ts:313`) — inaccurate label for a deleted op | Map `deleted` to a neutral «удалено»/no-op badge, or fall through to the `last_error`/`new` branch. |
| L5 | Low | (2b) | `setAuthType` leaves the inactive secret field populated in the DOM (password survives an auth-type switch, and vice-versa) | Clear the non-active field in `setAuthType` (`main.ts:473-474`) or on switch; secrets are AES-GCM at rest, so this is DOM hygiene only. |

## Verified vs not

- **Pass** by source + backend + built bundle: authkey model/copy, create-form
  field set, install/retry semantics, delete/confirm, secret clearing, output
  truncation + server-side masking, no paid duplicate path, webassets freshness,
  no `console.*`, no new colors, payload/reason-code **codes**.
- **Fail**: B1 (English user-facing validation), B2 (duplicate host → second manual slot).
- **`not_verified` (needs a live browser):** rendered contrast of tokens on the new
  servers card, the Tab cycle through `#sv-authtype` + key textarea + file input,
  and no page horizontal scroll at exactly 320px. Confirmed by source/bundle only.

## Files changed (this round)

- `docs/specs/design-qa-report.md` — this Round 7 section only.

No application code changed; no commit.
