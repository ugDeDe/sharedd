# Spec — Cabinet hosting UI (per-component)

Companion to [`../design-cabinet-hosting.md`](../design-cabinet-hosting.md).
Vanilla TS + DOM string templates (no framework), following `main.ts` /
`node-detail.ts` / `api.ts` conventions. Every classname below is either an
**existing** component class or a **new layout-only** class listed in §20.
Russian UI copy. No new tokens, no new buttons/fields/badges.

Legend: **GET/POST/…** = HTTP method; `→` = success shape; error codes are the
machine `error` string from the existing `controlAPIError` envelope
(`{"error":"…","message":"…"}`, 03 §2).

---

## 0. Data layer contract (`api.ts` additions)

`req<T>(path, method, body?)` already: sets `X-CSRF-Token` on non-GET, `401→/signin`,
throws `Error & {code,status,reason}`. Extend it with an optional options arg so
command routes can pass extra headers:

```ts
async function req<T>(path, method = "GET", body?, opts?: { headers?: Record<string,string> }): Promise<T>
```

New request header constants:

- `Idempotency-Key: <uuid>` — required on **create/replace/delete/change-ip/install**.
  Generate once per logical attempt with the existing `newIdemKey()`; **reuse the
  same key on retry of the same intent**, regenerate only when the user changes
  the command (03 §2.1).
- `X-Confirm-Action: delete` — required on `DELETE cloud-resource` only.

### 0.1 Types

```ts
export type Provider = "nodehost" | "beget";

export interface HostingCapabilities { create: boolean; change_ip: boolean; delete: boolean; }

export interface HostingBalance {
  amount: string | null; currency: string | null; scope: string;
  freshness: "fresh" | "stale" | "unavailable";
  checked_at: string | null; last_attempt_at: string | null;
  error: string | null;          // safe code: provider_error|provider_timeout|credentials_unavailable|…
}

export interface HostingConnection {
  id: number; provider: Provider; label: string;
  revision: number; credential_revision: number; credentials_configured: boolean;
  verified_at: string | null;
  capabilities: HostingCapabilities;
  balance: HostingBalance;
}

// POST/PATCH body (credentials are write-only; omission keeps bundle, object replaces it):
export interface HostingConnectionInput {
  provider?: Provider;            // create only; immutable afterwards (03 §3)
  label?: string;                 // 1..100
  credentials?: Record<string, string>;   // nodehost:{api_key}; beget:{cloud_login,cloud_password,hosting_api_login?,hosting_api_password?}
}

export interface CloudResource {
  id: number; connection_id: number; provider: Provider; label: string;
  state: string;                  // planned|creating|ready|deleting|deleted|error (+ extended, shown verbatim)
  revision: number; external_id?: string | null;
  ip?: string | null; current_ip?: string | null;   // read both defensively (spec §4 vs stage-05)
  node_id?: string | null;
  replaces_resource_id?: number | null; replaced_by_resource_id?: number | null;
  active_operation_id?: number | null;
}

export interface NodehostConfiguration {
  name: string; hostname?: string;
  cpu: string; cores: number; ram: number; disk: number; diskType: string;
  os: string; location: string; period: string; networkSpeed: string;
}

export interface ManagementPolicy {
  mode: "manual" | "profile"; profile_version_id: number | null;
  inputs: Record<string, unknown>; process_current_ban: boolean;
  revision: number; current_resource_id: number | null; active_run_id: number | null;
}

export interface CommandAck { operation_id: number; state: string; resource_id: number | null; }
```

### 0.2 Functions

| Function | Request | Success → | Notable errors |
|---|---|---|---|
| `hostingConnections()` | `GET /api/me/hosting-connections` | 200 `{connections:HostingConnection[]}` | 401, 503 `control_unavailable` |
| `createHostingConnection(body)` | `POST /api/me/hosting-connections` | 201 `HostingConnection` | 400 `validation_failed`, 403 `csrf_invalid` |
| `updateHostingConnection(id, body)` | `PATCH /api/me/hosting-connections/{id}` | 200 `HostingConnection` | 404, 409 `revision_conflict`, 409 `connection_busy`, 400 |
| `deleteHostingConnection(id)` | `DELETE /api/me/hosting-connections/{id}` | 204 | 404, 409 `connection_in_use` |
| `verifyHostingConnection(id, code?)` | `POST …/{id}/verify` body `{code?}` | 200 `{verified,checked_at,capabilities}` | 422 `provider_auth_failed`/`provider_2fa_required`, 504 `provider_timeout`, 429 |
| `refreshHostingBalance(id)` | `POST …/{id}/balance/refresh` body `{}` | 200 `HostingConnection` (never 500) | 404 |
| `hostingCatalog(id)` | `GET …/{id}/catalog` | 200 catalog | **501 `not_implemented`** |
| `cloudResources()` | `GET /api/me/cloud-resources` | 200 `{resources:CloudResource[]}` | 401, 503 |
| `cloudResource(id)` | `GET /api/me/cloud-resources/{id}` | 200 `CloudResource` | 404 |
| `createCloudResource(body, key)` | `POST /api/me/cloud-resources` + `Idempotency-Key` | **202** `CommandAck` | 400 `validation_failed`/`idempotency_key_required`, 409 `idempotency_conflict`/`resource_busy`, 501 |
| `deleteCloudResource(id, key)` | `DELETE …/{id}` + `Idempotency-Key` + `X-Confirm-Action: delete`, no body | **202** `CommandAck` | 400 `confirmation_required`/`idempotency_key_required`, 409 `resource_busy`, 501 |
| `changeCloudIP(id, key, subnetId?)` | `POST …/{id}/change-ip` + `Idempotency-Key` body `{subnet_id?}` | **200** `CloudResource` (impl; spec 202) | 409 `resource_busy`, 501, 404 |
| `replaceCloudResource(id, key, body)` | `POST …/{id}/replace` + `Idempotency-Key` body `{confirm:true,expected_revision,configuration,installation_profile_version_id?,vars?}` | **202** `CommandAck` (new id) | 400, 409 `resource_busy`/`revision_conflict`, 501 |
| `installCloudResource(id, key, body)` | `POST …/{id}/install` + `Idempotency-Key` body `{profile_id?,vars?}` | **200** operation (`kind:"cloud_install"`) | 404, 400 `idempotency_key_required`/`validation_failed`/`profile_unavailable`, 409 `not_ready`/`ssh_unavailable`/`in_progress`, 403 `quota_exceeded`/`user_disabled`/`owner_mismatch`, 501 |
| `estimateCloudResource(body)` | `POST /api/me/cloud-resources/estimate` | 200 estimate | **501 `not_implemented`** (stage-05) |
| `cloudIPOptions(id)` | `GET …/{id}/ip-options` | 200 `{options,…}` | **501 `not_implemented`** |
| `nodeManagement(id)` | `GET /api/me/nodes/{id}/management` | 200 `ManagementPolicy` | 404 |
| `putNodeManagement(id, body)` | `PUT /api/me/nodes/{id}/management` | 200 `ManagementPolicy` | 409 `revision_conflict`, 422 `profile_incompatible`, 404 |

> Deviations to handle defensively (stage-04/05): catalog = 501; `balance/refresh`
> returns the **Connection**, not a bare `Balance`; `change-ip` = 200 not 202;
> `estimate`/`ip-options` = 501. UI must degrade, not break, on each.

---

## 1. Routing & segments

`#/beget` → `view-beget` (VETS key `beget`, label «Хостинги»). Inside:

```html
<section class="view" id="view-beget" hidden>
  <nav class="nd-tabs" id="hosting-tabs" role="tablist" aria-label="Разделы хостингов">
    <button type="button" role="tab" id="htab-conns"     data-htab="conns"     aria-controls="hpanel-conns"     aria-selected="true"  tabindex="0">Подключения</button>
    <button type="button" role="tab" id="htab-resources" data-htab="resources" aria-controls="hpanel-resources" aria-selected="false" tabindex="-1">Ресурсы</button>
  </nav>
  <div class="nd-panel" id="hpanel-conns"     role="tabpanel" aria-labelledby="htab-conns"     tabindex="-1" data-hpanel="conns">…</div>
  <div class="nd-panel" id="hpanel-resources" role="tabpanel" aria-labelledby="htab-resources" tabindex="-1" data-hpanel="resources" hidden>…</div>
  <div class="sr-only" id="hosting-live" aria-live="polite" aria-atomic="true"></div>
</section>
```

- Active segment in the hash: `#/beget?tab=conns|resources` via `history.replaceState`
  (same technique as `node-detail.ts` `writeTabHash`). Default `conns`.
- Tab click/keys: same delegated handler pattern as `node-detail.ts`
  (click + ArrowLeft/Right/Home/End, roving tabindex).
- On entering the view: `loadConnections()`; on switching to `resources`:
  `loadResources()`. Polling hook (see §8) runs every 5 s while the view is
  visible, wrapped in `renderSection` so focus/scroll is preserved.

---

## 2. Connections list — `#hosting-conns`

**Container.** `<div class="node-list" id="hosting-conns" role="list">` inside
`<section class="card">` with `.card-head` (h2 «Подключения» · `.hint` · primary
«Добавить подключение») and a `.card-note` (secrets + 60 s balance min-interval).

**States**

| State | Render |
|---|---|
| loading (first) | `<div class="skeleton skel-block"></div>` inside the list |
| empty | `.empty-state` (icons via `EMPTY_SVG`-style rect icon) + copy §5 design doc |
| error (5xx / `control_unavailable`) | `.banner.bad[role="alert"]` + «Повторить» button (`isRetryableLoad` pattern); 4xx — без авто-повтора |
| loaded | one `.node-card.hc` per connection |

**Card DOM**

```html
<article class="node-card hc" role="listitem" data-conn="{id}" aria-labelledby="hc-t-{id}">
  <div class="node-main">
    <div class="node-top">
      <span class="node-id" id="hc-t-{id}">{label}</span>
      <span class="badge mono">{provider}</span>
      {verified_at ? <span class="badge ok">подключён</span>
                    : <span class="badge">не проверен</span>}
    </div>
    <div class="node-sub">
      {capability badges} <span class="dim">баланс</span>
      {balance: fresh → <span class="badge ok">актуален · {age}</span><span class="mono">{amount} {currency}</span>  …}
    </div>
    {error.code ? <div class="node-problem" title="{errorCode}">{ruReason}</div> : ""}
  </div>
  <div class="node-side">
    <button type="button" class="btn sm" data-hcbalance="{id}">Обновить баланс</button>
    <button type="button" class="btn sm" data-hcverify="{id}">Проверить</button>
    <button type="button" class="btn sm ghost" data-hcedit="{id}">Изменить</button>
    <button type="button" class="btn sm danger" data-hcdel="{id}">Удалить</button>
  </div>
  <div class="node-more"> … kv details … </div>
</article>
```

**Capability badges.** `.badge ok` «создание»/«смена IP»/«удаление» when true;
`.badge` (neutral) with `title="провайдер не поддерживает"` when false.

**Balance** rendering — table in §3.2 of the design doc. `amount===null` with
`fresh` → «сумма не указана»; never `0`.

**Interactions**

- Click a button → delegate on `document` (patterns of `main.ts`), look up the
  connection in the in-memory `CONNS` array by `data-…` id.
- `.node-more` toggle: `data-hctoggle` button (or clicking `.node-card.hc`),
  toggles `.open`; store open ids in a `Set` so 5 s re-render keeps them
  (same as `OPEN_OPS`).

**Fetch.** `GET /api/me/hosting-connections` → 200. On 503/5xx: banner + retry;
on 401: `req` already redirects.

---

## 3. Add / Edit connection modal

Reuse `openModal(title, body, actions)` from `main.ts`. Body is a `<form class="…">`
with `novalidate`; fields depend on the selected provider.

### 3.1 Provider select

```html
<div class="field">
  <label for="hc-provider">Провайдер</label>
  <select id="hc-provider" name="provider">
    <option value="nodehost">Nodehost</option>
    <option value="beget">Beget</option>
  </select>
  <span class="help">Провайдер подключения изменить нельзя.</span>
</div>
```

Edit mode: `select` disabled with `title="провайдер неизменен"`; helper line shows
«Провайдер: {provider}».

### 3.2 nodehost body

```html
<div class="field">
  <label for="hc-label">Название</label>
  <input id="hc-label" type="text" maxlength="100" autocomplete="off" required>
</div>
<div class="field">
  <label for="hc-api-key">API-ключ</label>
  <div class="input-group">
    <input id="hc-api-key" type="password" autocomplete="new-password">
    <button type="button" class="in-btn" data-reveal="hc-api-key" aria-label="Показать ключ" title="Показать ключ">…eye svg…</button>
  </div>
  <span class="help">Write-only: не отображается после сохранения. Оставьте пустым при правке, чтобы не менять.</span>
</div>
```

### 3.3 beget body

```html
<div class="srv-grid">
  <div class="field"><label for="hc-cloud-login">Логин облака</label><input id="hc-cloud-login" class="mono" autocomplete="off"></div>
  <div class="field"><label for="hc-cloud-pass">Пароль облака</label>
    <div class="input-group"><input id="hc-cloud-pass" type="password" autocomplete="new-password"><button type="button" class="in-btn" data-reveal="hc-cloud-pass" aria-label="Показать пароль">…</button></div>
  </div>
  <div class="field"><label for="hc-hosting-login">Логин хостинга (API)</label><input id="hc-hosting-login" class="mono" autocomplete="off"><span class="help">Необязательно; если указан — заполните и пароль.</span></div>
  <div class="field"><label for="hc-hosting-pass">Пароль хостинга (API)</label>
    <div class="input-group"><input id="hc-hosting-pass" type="password" autocomplete="new-password"><button type="button" class="in-btn" data-reveal="hc-hosting-pass" aria-label="Показать пароль">…</button></div>
  </div>
</div>
```

Validation before submit (client-side, mirrors 03 §3):

- **nodehost:** `api_key` non-empty on create; on edit may be omitted.
- **beget:** `cloud_login`+`cloud_password` both required (pair); `hosting_api_login`/
  `hosting_api_password` — **either both or neither** (all-or-none). On edit any
  omitted whole-pair = keep existing bundle; a provided object replaces it fully.
- Invalid → `.banner.bad[role="alert"]` at the form bottom, focus first invalid
  field, `aria-describedby` points to the banner.

**Submit** → `createHostingConnection(body)` (201) or `updateHostingConnection(id,
{expected_revision: conn.revision, label?, credentials?})` (200). On success:
close modal, clear password fields, toast «Подключение добавлено» /
«Подключение сохранено», `loadConnections()`.

**Errors inside the modal** (do **not** close): 400 `validation_failed` → banner
with `message`; 409 `revision_conflict` → banner «Настройки изменились; обновите
данные» + button «Обновить» (reloads the connection into the form, keeping the
user's typed label/credentials out — only revision is refreshed); 409
`connection_busy` → banner «По подключению есть незавершённая команда…».

**a11y:** heading is the modal title (`aria-labelledby="modal-title"` already);
fields labelled; `autocomplete="new-password"` on secrets; reveal button
`aria-label`.

---

## 4. Verify connection (with optional 2FA)

Trigger: `data-hcverify={id}`.

Modal body:

```html
<div class="field">
  <label for="hc-code">Код подтверждения (2FA)</label>
  <input id="hc-code" type="text" class="mono" inputmode="numeric" autocomplete="one-time-code" maxlength="8">
  <span class="help">Необязательно: введите, если провайдер запросил код. Код не сохраняется.</span>
</div>
<div class="banner bad" role="alert" id="hc-verify-err" hidden></div>
```

Full flow to open: first `POST …/{id}/verify` **without** code (body `{}`).

- 200 `{verified:true,checked_at,capabilities}` → close modal, toast
  «Подключение проверено», update card (`verified_at`, capability badges).
- 422 `provider_2fa_required` (or `reason==="code_required"`, mirroring existing
  Beget handling) → **keep the modal open**, reveal/focus the code field
  (`borderColor=var(--bad)`), banner «Провайдер требует код подтверждения (2FA)».
  Re-submit sends `{code}`. Code is **never persisted** (03 §3).
- 422 `provider_auth_failed` → banner with server `message` (do not close).
- 504 `provider_timeout` → banner «Провайдер не ответил вовремя» + re-enable button.
- 429 `rate_limited` → banner, disable submit until «Повторить» after `Retry-After`
  (no auto-retry, like the Beget ratelimit handling in `main.ts`).
- Success/failure toggles `[disabled]` on the submit button while in-flight
  (text «Проверка…»).

**Keyboard:** Enter submits; Escape closes (modal handler); focus returns to
`data-hcverify` button.

---

## 5. Refresh balance

Trigger `data-hcbalance={id}` (inline, no modal).

- Button `disabled`, text «Обновление…».
- `POST …/{id}/balance/refresh` body `{}` → 200 `HostingConnection`
  (**never 500** on provider error).
- Update the card from the returned `balance` (freshness/amount/error) — the
  endpoint applies the 60 s min-interval server-side (stage-04): a too-soon
  refresh returns the stored snapshot with **no external call**; UI still shows
  the returned values (this is expected, not an error).
- 404 → toast «Подключение не найдено», `loadConnections()`.
- On success: toast «Баланс обновлён» (only when values changed) and refresh the
  `aria-live` region «Баланс {label}: {amount} {currency}, {freshness}».

---

## 6. Delete connection

Trigger `data-hcdel={id}` → confirm modal (explicit object name):

> Title «Удалить подключение „{label}“?»
> Body «Подключение будет удалено. Если на нём есть ресурсы, операции или
> незавершённые эффекты, удаление отклоняется (409). Секреты удаляются вместе
> с подключением.»

Actions: «Отмена» (ghost) / «Удалить» (danger).

`DELETE /api/me/hosting-connections/{id}`:

- 204 → close, toast «Подключение удалено», `loadConnections()`.
- 409 `connection_in_use` → keep modal conceptually closed, show toast/banner
  «Подключение используется ресурсом или операцией — удалить нельзя», reload list.
- 404 → toast «Подключение не найдено», reload list.
- 403 `csrf_invalid` → `req` throws `csrfError` → user re-signs in.

---

## 7. Provider catalog (degraded)

There is **no UI control that hard-depends** on catalog (stage-04: 501 stub).
When the resource-create form needs provider options (location/OS/diskType…),
attempt `GET …/{id}/catalog` lazily:

- 200 → fill `<select>`s from `catalog` (allowlisted provider data only).
- **501 `not_implemented`** → render the fields as free-text/known presets and a
  `.banner.info`: «Каталог провайдера пока недоступен — заполните параметры
  вручную». Never blocks creation.
- 404/503 → same degraded state + retry link.

---

## 8. Resources list — `#cloud-res-list`

**Container.** `<div class="node-list" id="cloud-res-list" role="list">` inside
`<section class="card">` (h2 «Ресурсы» · `.hint` · primary «Создать ресурс»),
plus `.card-note` with the no-auto-recreate invariant (design doc §5).

**States:** skeleton / `.empty-state` / `.banner.bad`+retry / loaded. If **all**
connections are beget → show the 501 `.banner.info` with `#/create` link instead
of hiding the create button (design doc §5 «Beget-облако»); the create button is
still visible but, on submit with a beget connection, surfaces 501.

**Card DOM** as in design doc §3.3.

**State-dependent actions** (`.node-side`):

| state | buttons shown |
|---|---|
| `planned`/`creating` | disabled «Создаётся…» (no mutating command) |
| `ready` | «Установить профиль» `data-crinstall`, «Сменить IP» `data-crip`, «Заменить» `data-crreplace`, «Удалить» `data-crdel` |
| `deleting` | disabled «Удаляется…» |
| `deleted` | none (только `.node-more` с деталями) |
| `error` | «Заменить» + «Удалить» (+ `last_error` в `.node-problem`) |

If operations are globally busy (`active_operation_id` non-null or an operation
for this resource is `pending`/`installing`/`deleting`) — all mutating buttons on
that card are `disabled` with `title="по ресурсу уже выполняется команда"`.

**Resource linkage** (`.node-more` / `.node-sub`): `node_id` → link
`<a class="node-open mono" href="#/nodes/{node_id}">{node_id}</a>`; `replaces`
→ link to the replaced resource card (`data-res` scroll-into-view + highlight
`data-res` briefly using the existing `.row-highlight` class); `replaced_by` →
«заменён на #{id}».

**Fetch:** `GET /api/me/cloud-resources` → 200 `{resources}` (impl unpaginated;
`next_cursor` optional/ignored). Poll every 5 s while the view is active via
`renderSection` (focus-safe).

---

## 9. Create resource (nodehost) + operation polling

Form in a `.card` (hidden by default; toggled by «Создать ресурс»), not a modal —
the parameter set is long (03 §4). Structure and fields:

```html
<form class="card-body" id="cr-form" novalidate>
  <div class="banner bad" id="cr-err" role="alert" hidden></div>
  <div class="srv-grid">
    <div class="field"><label for="cr-conn">Подключение</label>
      <select id="cr-conn" required></select>   <!-- only nodehost connections with capabilities.create -->
      <span class="help">Только проверенные подключения Nodehost с поддержкой создания.</span></div>
    <div class="field"><label for="cr-name">Имя ресурса</label><input id="cr-name" class="mono" autocomplete="off" required></div>
    <div class="field"><label for="cr-hostname">Hostname</label><input id="cr-hostname" class="mono" autocomplete="off"></div>
    <div class="field"><label for="cr-cpu">CPU</label><input id="cr-cpu" class="mono" autocomplete="off" placeholder="AMD Ryzen 9 7950X"></div>
    <div class="field"><label for="cr-cores">Ядра</label><input id="cr-cores" type="number" min="1" inputmode="numeric"></div>
    <div class="field"><label for="cr-ram">ОЗУ, ГБ</label><input id="cr-ram" type="number" min="1" inputmode="numeric"></div>
    <div class="field"><label for="cr-disk">Диск, ГБ</label><input id="cr-disk" type="number" min="1" inputmode="numeric"></div>
    <div class="field"><label for="cr-disktype">Тип диска</label><input id="cr-disktype" class="mono" placeholder="SSD NVMe"></div>
    <div class="field"><label for="cr-os">ОС</label><input id="cr-os" class="mono" placeholder="ubuntu-24.04"></div>
    <div class="field"><label for="cr-loc">Локация</label><input id="cr-loc" class="mono" placeholder="DE"></div>
    <div class="field"><label for="cr-period">Период</label><input id="cr-period" class="mono" placeholder="1m"></div>
    <div class="field"><label for="cr-net">Скорость сети</label><input id="cr-net" class="mono" placeholder="1gbps"></div>
  </div>
  <div class="banner info" id="cr-estimate" hidden></div>   <!-- hidden: estimate 501 -->
  <div class="card-foot" style="margin:14px calc(var(--pad)*-1) calc(var(--pad)*-1)">
    <button type="button" class="btn ghost" id="cr-cancel">Отмена</button>
    <button type="submit" class="btn primary" id="cr-submit">Создать ресурс</button>
  </div>
</form>
```

**Parameters** map 1:1 to 03 §4 nodehost fields/units (`ram`/`disk` numbers,
provider units per 02). Password is **not** a field (provider password is captured
server-side). `configuration.name`/`hostname` come from the name/hostname fields.

**Submit (the paid command):**

1. Client-side validate required fields → else banner + focus.
2. Generate one `Idempotency-Key` (`newIdemKey()`) **when the form is opened /
   attempt begins**; reuse on retry of the same body.
3. `createCloudResource({ connection_id, label, configuration, management:{mode:"manual"} }, key)`.
   (`management` is sent as `manual` per 03 §4; stage-05 ignores it — documented
   deviation. Sending it is the forward-compatible contract.)
4. **202 `{operation_id,state:"pending",resource_id}`** → do **not** claim success:
   - switch to a progress view: `.banner.info[aria-live]` «Ресурс {label}:
     принят, создаётся…» + `.kv` with operation `#{id}`;
   - poll `GET /api/me/cloud-resources/{resource_id}` (+ the operations list entry
     `operation_id` for steps) every 3 s until state ∈ {`ready`,`error`,`deleted`}
     or 3 min timeout.
   - `ready` → toast «Ресурс готов», `aria-live` announcement, reload resources,
     enable «Установить профиль».
   - `error` → `.banner.bad` with `last_error` + invariant note; keep the resource
     card visible; the only retry path is a **new create** (explicit user action).
   - timeout → banner «Создание идёт дольше обычного — статус по-прежнему виден в
     разделе „Ресурсы“»; no false failure.
5. Errors (before 202): 400 `validation_failed` → banner; 409
   `idempotency_conflict` → banner «Запрос с этим ключом уже был с другим телом»
   + reload; 409 `resource_busy` → banner «По ресурсу уже выполняется команда»;
   403 `quota_exceeded` → banner «Квота исчерпана…»; 501 `not_implemented`
   (beget connection selected) → banner Beget-облако §5 with `#/create` link.

**a11y:** progress region is the shared `#hosting-live` (polite); submit toggles
`.btn[disabled]` with text «Создание…»; focus stays on submit.

---

## 10. Estimate (degraded)

`POST /api/me/cloud-resources/estimate` is **not implemented (501)** at stage-05.
UI: no estimate is shown by default; `#cr-estimate` stays hidden. If a future
button triggers it and returns 501 `not_implemented` → keep hidden + one toast
«Оценка стоимости пока недоступна». Never fabricate a price. On 200, fill
`#cr-estimate` with `amount currency period` and a `.hint` «предварительная
оценка, резерв цены не выполняется».

---

## 11. Delete resource

Trigger `data-crdel={id}` → confirm modal:

> Title «Удалить ресурс „{label}“?»
> Body «Ресурс будет отмечен на удаление у провайдера; освобождение места ждёт
> подтверждения. Бан или ошибка установки удаление не запускают автоматически.»

`DELETE /api/me/cloud-resources/{id}` with `Idempotency-Key` (fresh) **and**
`X-Confirm-Action: delete`, **no body**.

- 202 `{operation_id,state,resource_id}` → toast «Удаление запущено», poll until
  `deleted` (reuse §9 polling); `aria-live` «Ресурс {label}: удаляется…».
- 400 `confirmation_required` → shouldn't happen (we always send the header) →
  toast «Подтвердите действие».
- 400 `idempotency_key_required` → toast «Повторите: не сгенерирован ключ запроса».
- 409 `resource_busy` → toast «По ресурсу уже выполняется команда», reload.
- 404 → toast «Ресурс не найден», reload.
- 501 → Beget banner.

---

## 12. Change IP

Trigger `data-crip={id}` (available only when `state==ready`).

Modal:

```html
<div class="field">
  <label for="cr-subnet">Подсеть (необязательно)</label>
  <input id="cr-subnet" type="number" inputmode="numeric" min="0">
  <span class="help">Оставьте пустым для адреса провайдера по умолчанию. Смена IP не создаёт новый сервер.</span>
</div>
<div class="banner bad" id="cr-ip-err" role="alert" hidden></div>
```

Optional subnet options: try `GET …/{id}/ip-options`; on 200 render a `<select>`;
on **501/404** keep the free `<input type="number">` (degraded, honest help).

Actions: «Отмена» / primary «Сменить IP».

`changeCloudIP(id, key, subnetId?)`:

- **spec says 202; impl returns 200 `CloudResource`** — handle both: if 202 ack,
  poll resource until IP changes / operation done; if 200, update card from the
  returned resource.
- 409 `resource_busy` → banner «По ресурсу уже выполняется команда».
- 409 (spec) / 501 → banner per table.
- Success: toast «IP изменён», update `.node-sub` IP, `aria-live` announce.
- Note: no auto-create fallback (stage-05) — failure leaves the resource as-is;
  copy must not imply replacement.

---

## 13. Install profile onto a resource

Trigger `data-crinstall={id}` (only `state==ready`). Reuse the existing install
UI building blocks (`renderVarsEditor`, `collectInstallVars`, `installErrText`
patterns from `main.ts`) — profile `<select>` + vars editor.

Modal (wide): choose profile + vars.

- Profile list: `GET /api/me/profiles` (existing `myProfiles`); only `format:text`
  versions are selectable (graph → 422, stage-06).
- Vars: `renderVarsEditor(decls, snapshot, onChange)`; secrets handled by existing
  viewer; label is injected server-side from the resource label (install requires
  the VPS hostname prefix to equal the label — stage-06 deviation).

Submit (non-purchase; `Idempotency-Key` required, no `confirm`):

`installCloudResource(id, key, {profile_id, vars})` → **200** operation
(`kind:"cloud_install"`).

| Condition | HTTP/error | UI |
|---|---|---|
| not ready | 409 `not_ready` | banner «Сервер ещё не готов (нет RUNNING). Установка начнётся после готовности.» |
| no sealed root password | 409 `ssh_unavailable` | banner «Нет доступа по SSH (пароль root недоступен). Пересоздайте ресурс.» (no fake retry) |
| worker running | 409 `in_progress` | banner «Для этого ресурса уже выполняется команда.» |
| beget | 501 `not_implemented` | Beget banner |
| quota/disabled/identity | 403 `quota_exceeded`/`user_disabled`/`owner_mismatch` | banner with mapped copy |
| missing key | 400 `idempotency_key_required` | regenerate key, toast |
| bad profile/vars | 400 `validation_failed`/`profile_unavailable` or 404 | banner + focus field |

Success: close modal, toast «Установка запущена», link «К операциям»
(`#/operations`), poll operation (in the operations list already refreshed by
`refresh()`); steps shown via the existing `stepsHTML`/`op-step` table.

**Label/SSH gate:** the resource label must become the VPS hostname prefix; if the
install fails at `Confirm`, the operation is `failed` with no `node_id`. Copy for
that failure must say the install failed (not that a server was created) and
point to the resource card.

---

## 14. Replace resource

Trigger `data-crreplace={id}`. Replace is a **new paid command** in the same
connection (03 §4); it does **not** reuse the old resource.

Modal: reuse the create form fields (configuration) pre-filled from the current
resource where known, plus optional install profile/vars. Body includes
`configuration` (stage-05 requires it — no pinned snapshot yet).

Submit: `replaceCloudResource(id, key, {confirm:true, expected_revision:res.revision,
configuration, installation_profile_version_id?, vars?})`.

- **202** ack with the **new** resource id → toast «Замена запущена», poll new
  resource (§9); old resource shows `.badge` «заменён на #{new}», new shows
  `.badge.info` «заменяет #{old}».
- 409 `resource_busy`/`revision_conflict` → banner accordingly (revision →
  reload card and re-open the modal with fresh revision).
- 501 → Beget banner.

Confirm copy must state a new server is purchased and this is the user's explicit
action (invariant §8).

---

## 15. Beget managed-cloud (501) state

Any nodehost-only command against a **beget** connection returns 501
`not_implemented` (stage-05/06). UI never hides the concept; it labels it:

- In «Ресурсы» when a beget connection exists, show a persistent `.banner.info`
  (design doc §5 copy) with `<a href="#/create">Создать сервер</a>`.
- If a create/replace/change-ip/install is attempted and returns 501, render the
  same banner at the top of the form/modal with the same link. No retry loop.

---

## 16. Node detail — «Управление» panel

Add to `node-detail.ts` `TABS`/panels (new tab `management`, label «Управление»,
between `checks` and `journal` — order fixed by product: `overview, metrics,
checks, management, journal, actions`). Fetch `GET /api/me/nodes/{id}/management`
alongside the detail (or lazily on tab open).

```html
<div class="nd-grid card-body">
  <div class="kv"><span class="kv-k">Режим управления</span><span class="kv-v">{manual→«ручной» | profile→«профильный»}</span></div>
  <div class="kv"><span class="kv-k">Текущий ресурс</span><span class="kv-v">
     <a class="node-open mono" href="#/resources/{current_resource_id}">#{id}</a> | «—»</span></div>
  <div class="kv"><span class="kv-k">Активный запуск</span><span class="kv-v mono">{active_run_id ?? «—»}</span></div>
</div>
<div class="field" style="margin-top:12px">
  <label id="mg-mode-lbl">Режим</label>
  <div class="seg" id="mg-mode" role="radiogroup" aria-labelledby="mg-mode-lbl">
    <button type="button" role="radio" data-mode="manual" aria-checked="true">Ручной</button>
    <button type="button" role="radio" data-mode="profile" aria-checked="false" disabled title="Профильный режим ещё недоступен">Профильный</button>
  </div>
  <span class="help">Профильный режим (автоматическое управление граф-профилем) пока недоступен — движок графов не подключён.</span>
</div>
<div class="field" style="margin-top:12px">
  <label class="check"><input type="checkbox" id="mg-ban"><span class="box"></span>
    «Реагировать на текущую блокировку»</label>
  <span class="help">По умолчанию выключено. Включайте осознанно: это разрешает обработку только текущего эпизода блокировки, не прошлых.</span>
</div>
<div class="card-foot"><button type="button" class="btn primary" id="mg-save">Сохранить режим</button></div>
```

- **Default** (no row, GET returns virtual `mode:"manual", revision:1`) → manual
  selected, save button enabled.
- Selecting «Профильный» visually selects but **keeps the save disabled** and, if
  clicked, shows `.banner.info` «Профильный режим ещё недоступен». (403 in the
  field is `disabled`; this mirrors the post-implementation truth without a doomed
  request.)
- Save manual: `putNodeManagement(id, {mode:"manual", process_current_ban, expected_revision: policy.revision})`.
  - 200 → toast «Режим сохранён», update revision from response.
  - 409 `revision_conflict` → banner «Настройки изменились; обновите данные» +
    «Обновить» (re-GET management).
- If the API ever returns profile-capable data, the «Профильный» radio becomes
  enabled and the help line is removed — forward-compatible.

`process_current_ban` copy must never imply auto-purchase; it is a policy consent
for the current episode only (03 §5, 04 §4).

---

## 17. Node detail — manual action entry points

The `actions` panel (currently a placeholder `actionsHTML`) becomes real. It links
the node's management policy to resource commands:

```html
<section class="card">
  <div class="card-head"><h2>Действия</h2><span class="hint">ручное управление ресурсом</span></div>
  <div class="card-body nd-grid">
    <div class="kv"><span class="kv-k">Текущий ресурс</span><span class="kv-v">{link | «—»}</span></div>
    <div class="kv"><span class="kv-k">Подключение</span><span class="kv-v">{provider} · {label} | «—»</span></div>
  </div>
  <div class="card-body srv-actions">
    <a class="btn" href="#/beget?tab=resources">Создать сервер…</a>
    <button type="button" class="btn" data-res-action="install" {disabled if no resource}>Установить профиль</button>
    <button type="button" class="btn" data-res-action="change-ip" {disabled}>Сменить IP</button>
    <button type="button" class="btn" data-res-action="replace" {disabled}>Заменить</button>
    <button type="button" class="btn danger" data-res-action="delete" {disabled}>Удалить ресурс</button>
  </div>
  <div class="banner info" role="note">…ban/disposition copy (design doc §5)…</div>
</section>
```

- Buttons operate on `current_resource_id`; disabled when absent, when the resource
  is not `ready` (except «Заменить»/«Удалить» on `error`), or when the resource is
  busy.
- These are **entry points**: they reuse §11–§14 modals. `data-res-action` handlers
  resolve the resource by `current_resource_id` from the resource list (fetched on
  demand if needed) and open the same modal by resource id.
- `#/resources/{id}` is a new deep-link route that opens the «Ресурсы» tab and
  scrolls to / highlights the card (uses `.row-highlight`).

---

## 18. Node detail — ban / disposition, active operation, resource linkage

1. **Ban/disposition** — informational `.banner.info[role="note"]` (copy above). It
   is derived from node events (`ip_blocked`, `node_terminated`, `ban_lifted`) and
   `quarantine`/`status` already returned by `GET …/detail`; **no API change**. It
   must explicitly state that ban does **not** trigger a purchase/replacement.
2. **Active operation** — if `active_operation_id` (resource) or the resource is in
   `creating`/`deleting`/operation `installing`, render a `.kv` block with the
   operation `#id`, state badge (`OP_META`), and a link «К операциям» (`#/operations`
   + `HIGHLIGHT_OP` to focus it, existing pattern). Progress steps come from the
   operations list already polled every 5 s.
3. **Resource linkage** — show `node_id → resource → connection` chain: a `.kv`
   with resource `#id` (link to `#/resources/{id}`) and its connection label; on the
   resource side, `node_id` links back to `#/nodes/{node_id}`.

---

## 19. Rendering conventions (must match existing code)

- Build HTML with template strings + `esc()` for every interpolated value
  (`ui/lib/dom.ts`). Never inject raw provider strings.
- Machine values (ids, IPs, external ids, keys) → `.mono`; labels → plain.
- Re-render via `renderSection(el, poll, () => { … })` for anything on the 5 s
  poll, so focus/scroll/caret survive.
- Delegate clicks/keydown on `document` (in-memory lookup by `data-*`), because
  `innerHTML` re-render kills element listeners — same as `main.ts`/`node-detail.ts`.
- Toasts via `showToast(text, isErr)`; modals via `openModal`/`closeModal`.
- `isRetryableLoad(e)` (5xx / `status===0` / `control_unavailable`) drives the
  banner+«Повторить» auto-retry; 4xx never auto-retries.

---

## 20. New classnames (page.css only, layout-only)

Added to `ui/pages/cabinet/page.css` (not `components/`), per `DESIGN.md` §5.2:

| Class | Purpose | CSS (layout only) |
|---|---|---|
| `.node-card.hc` | connection row modifier | `cursor: default;` |
| `.node-card.cr` | resource row modifier | `cursor: default;` |
| `.hc .node-side`, `.cr .node-side` | action cluster wraps | `flex-wrap: wrap; justify-content: flex-end;` (mirrors `.srv .node-side`) |
| `#hosting-tabs` | segment wrapper | reuse `.nd-tabs` as-is |
| `.cr-progress` | progress banner slot inside resource card | `margin: 8px 0 0;` |
| `.hc-balance` | balance cluster in `.node-sub` | `display:inline-flex; align-items:center; gap:6px; font-variant-numeric: tabular-nums;` |

`@media (max-width:720px)`: `.hc .node-side, .cr .node-side { justify-content: flex-start; }`
(the existing block already does this for `.srv .node-side`).

No new color, radius, shadow, or typography rule.

---

## 21. Accessibility matrix

| Element | Requirement |
|---|---|
| Segments (`hosting-tabs`) | `role="tablist"`/`role="tab"`, `aria-selected`, `aria-controls`, roving `tabindex`, Arrow/Home/End; panels `role="tabpanel"` `tabindex="-1"` |
| Connection/resource rows | `role="listitem"` inside `role="list"` (`.node-list`), `aria-labelledby` → the label span |
| Status badges | text present (not colour-only); `title` carries the machine code, never shown as visible code |
| Balance freshness | badge text + age; `aria-live` announce after refresh |
| Mutating buttons | `type="button"`; in-flight `disabled` + text change; icon-only buttons `aria-label`+`title` |
| Forms | `<label for>`; errors `.banner.bad role="alert"` + `aria-describedby`; focus first invalid; secrets `autocomplete="new-password"`; 2FA `autocomplete="one-time-code"` `inputmode="numeric"` |
| Modals | existing focus trap + Escape + focus return (`openModal`/`closeModal`) |
| Progress | shared `#hosting-live` `aria-live="polite" aria-atomic="true"` for operation state changes |
| Polling | `renderSection` preserves focus/scroll; never steal focus |
| Ban note | `.banner.info role="note"` (non-alert) |

---

## 22. Error / status handling matrix (exact)

| HTTP | `error` | Where | UI treatment |
|---|---|---|---|
| 400 | `validation_failed` | forms | `.banner.bad[role="alert"]` with `message`; focus first invalid |
| 400 | `confirmation_required` | delete resource | toast/«Подтвердите действие» (shouldn't occur; header always sent) |
| 400 | `idempotency_key_required` | commands | regenerate key, toast, retry enabled |
| 401 | `unauthorized` | all | `req` → `/signin` |
| 403 | `csrf_invalid` | commands | `req` throws `csrfError` → re-sign-in hint |
| 403 | `user_disabled` / `quota_exceeded` | create/install | mapped banner copy |
| 404 | `not_found` | any object | toast «… не найден», reload list |
| 409 | `revision_conflict` | PATCH conn / PUT mgmt / replace | banner + «Обновить» (re-GET, keep form) |
| 409 | `resource_busy` | delete/change-ip/replace/install | banner/toast «По ресурсу уже выполняется команда» |
| 409 | `connection_in_use` | DELETE connection | toast «используется — удалить нельзя», reload |
| 409 | `connection_busy` | PATCH connection | banner «незавершённая команда — смена данных отложена» |
| 409 | `idempotency_conflict` | commands | banner «тот же ключ, другое тело» + reload |
| 409 | `not_ready` | install | banner «Сервер ещё не готов…» |
| 409 | `ssh_unavailable` | install | banner «Нет доступа по SSH…» |
| 409 | `in_progress` | install | banner «уже выполняется» |
| 422 | `provider_auth_failed` | verify | keep modal, banner with server `message` |
| 422 | `provider_2fa_required` | verify | reveal+focus code field, banner |
| 422 | `profile_incompatible` | mgmt profile / graph | banner «Профильный режим недоступен» |
| 422 | `capability_unavailable` | any command | banner «Провайдер не поддерживает действие» |
| 429 | `rate_limited` | verify/commands | banner + disable, respect `Retry-After`, no auto-retry |
| 501 | `not_implemented` | beget / catalog / estimate / ip-options | Beget banner or degraded field (never a raw error) |
| 502 | `provider_error` | sync reads/verify | banner + «Повторить» |
| 503 | `control_unavailable` | any | banner «Хранилище недоступно…» + retry (isRetryableLoad) |
| 504 | `provider_timeout` | verify/reads | banner «Провайдер не ответил вовремя» + retry |

`202` is **never** rendered as done — always transitions to polling (§9).
Provider `last_error` codes after a 202 are shown in the resource `.node-problem`
and operation steps; `insufficient_funds` → pause copy (no «повторите покупку»),
`unknown` → reconcile copy (03 §7).

---

## 23. QA gate & build

Implementation QA (mandatory, `DESIGN.md` §12 + `INSTRUCTIONS.md`):

```
/impeccable critique   /impeccable audit   /impeccable polish
```

Build (mandatory, `AGENTS.md` «UI Generation»):

```
bash scripts/build_ui.sh      # regenerates registry/internal/webassets/*.html
git add registry/internal/webassets/*.html
```

CI runs `bash scripts/build_ui.sh -check` and fails on stale assets; editing
`registry/internal/webassets/*.html` by hand is forbidden. No Node/npm in the
build.

**Definition of Done for this spec:** every area has loading/empty/error (+ stale
for balance); 202→poll; secrets never rendered; invariant §8 present on the
resources screen, delete/replace confirms, and the ban note; ban shown as
informational with explicit "no auto-purchase"; segments and modals are fully
keyboard/screen-reader reachable; `mark verified` only after `build_ui.sh` and the
QA commands pass.
