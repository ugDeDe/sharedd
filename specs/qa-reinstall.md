# QA — manual-server REINSTALL flow (Phase 3 gate)

Scope: `ui/pages/cabinet/main.ts` reinstall variant only. No live render (cabinet needs session); code/UX review.
Register: product (app UI). Skill: impeccable (critique + audit).

## Verdict: PASS ✅ (conditional — fix HIGH before ship)

Core requirement met: `done` → secondary «Переустановить», fresh-install modal variant with **enabled** profile dropdown; `failed` → «Повторить установку» locked-profile. No CRITICAL.

## Checklist

1. **PASS** — `main.ts:387` `mode = failed?retry : done?reinstall : install`; `:388` labels; `:389` reinstall is `btn sm` (not primary). `openInstall` `isRetry=false` for reinstall → fresh branch: `:537-541` builds full option list, `:542 sel.disabled=false`. Retry keeps locked single option `:530` + status «сменить его нельзя» `:531`.
2. **PASS** — destructive warning `:498-499` `banner bad`: «перезапишет текущую конфигурацию прокси … и перезапустит прокси. Выберите профиль.» Accurate (agent apply = stop → patch → restart), cabinet tone, `host` escaped.
3. **PASS (note)** — install=primary `:389`; reinstall=secondary `:389`; delete=danger `:413`; **retry = neutral `btn sm` (secondary), not primary** `:389`, unchanged from prior. On `done`/`failed` cards no button is primary — acceptable, pre-existing.
4. **PASS** — warning is visible text (not color-only) `:499`; modal focus/inert/trap unchanged (`openModal` `:80-99`, first foot btn «Отмена» focused); new button has text label; classes are all existing (`banner bad`, `btn sm`, `field`, `help`, `modal-wide`). No drift.
5. **PASS** — button gated by `!installing && meta.key!=="deleted"` `:386`; `installing` covers installing/pending/sent/confirmed `:368-369`; `deleted` excluded `:372`. `new`/`checked` still primary install — no regression.
6. **PASS** — `.srv .node-side{flex-wrap:wrap}` `cabinet/page.css:208`; label length equals pre-existing retry label → no new overflow.

## Findings

### HIGH — reinstall dropdown mislabels profile (silent wrong outcome)
`main.ts:537` first option `value="0">Стандартный профиль` is auto-selected; `doInstall:567` sends `Number(sel.value)||0`. Backend `cabinet_manual.go:376-379`: `effectiveProfile := existing.ProfileID; if body.ProfileID != 0 {…}` → **0 means "keep the existing profile"**, not "standard".
Consequences on a `done` server currently running a custom profile:
- select shows «Стандартный профиль» while the reinstall actually **keeps** the custom profile (misleading on a destructive action);
- the owner **cannot revert to the standard profile** — choosing «Стандартный профиль» sends 0 → keep custom. Same mismatch in the vars editor: `:553` renders decls of profile 0, not the current profile.
Fix (design/backend decision): for reinstall, preselect the server's current profile (`s.operation?.profile_id`) and render its vars (`:520`,`:551-553`); and make the standard option carry the standard profile's **real id** (or add an explicit «Оставить текущий профиль» option distinct from standard) so a chosen profile is always honored. For `install` mode, 0=standard is correct — do not change that path.

### LOW — retry has no primary emphasis on a `failed` card
`main.ts:389` retry renders `btn sm` alongside «Проверить доступ» (`:411`) of equal weight; the card's recovery action doesn't stand out. Pre-existing, not a regression.

### LOW — destructive banner has no icon
`:499` `banner bad` with text only; `.banner.bad svg` accent (`misc.css:14-15`) unused. Text carries meaning (pass on a11y), and other `banner bad` uses (`:313`) also omit the icon — consistent.

Verification: `verified` (code review only; no browser render available). Backend reinstall path confirmed by `manual_servers_test.go:545-704`.
