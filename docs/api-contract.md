# api-contract.md — Пул sharedd: пользователи, authkey, квоты, DC2 health

## Lifecycle live fixes (2026-10-19; supersedes historical quota wording below)

- `used_nodes` counts **only** `quota_reservations` for nodes in the pool.
  Pending secrets, creates, installs and durable `node_owners` do not consume
  slots. Manual/cloud install and legacy Beget purchase acceptance do not check
  pool quota; paid-operation idempotency, cooldown and daily limits remain.
- `/register` atomically acquires a reservation and checks the current DB limit,
  including pending claims and recovery with an existing owner's personal
  secret/authkey. Already-reserved nodes do not take a second slot. Ownership,
  disabled-user, bearer and label checks still apply. Reinstall acceptance does
  not erase the old owner or release a working candidate's reservation.
- Startup atomically reconciles reservations to the persisted candidate set,
  removing stale reservations as well as seeding live ones. Orphan/deleted
  failed-install pending secrets are revoked; retryable live targets retain theirs.
- Cloud create persists `operations.provider_op_id` before polling; startup
  migration adds that column with an empty default to old databases. Restart
  resume issues only `Operation`/`Server` reads, never another paid Create.
  Missing handles produce resource `error`, operation `failed`, and safe
  `last_error: "provider_reconcile_required"`, not endless `creating`.
- Provider operation errors accept string/object/null. Only fixed local codes
  survive decoding, including known insufficient-funds messages; raw provider
  messages and credential-like error codes are discarded. Definitive JSON/schema
  failures stop polling immediately. A successful operation alone is not ready:
  an ACTIVE/RUNNING server, a valid IP and a durably sealed password are required.
- `DELETE /api/me/cloud-resources/{id}` returns **409
  `provider_reconcile_required`** for an uncertain paid outcome even without a
  worker or external ID. Local deletion requires an unsubmitted plan or a
  definitive purchase refusal; existing-instance deletion remains provider-backed.
- `POST /api/me/cloud-resources/{id}/reconcile` (session + CSRF, no body, no
  `Idempotency-Key`) resolves the above dead end read-only against the
  nodehost inventory (all pages). Responses:
  `200 {"resolved":"absent"}` — no provider instance exists; the resource is
  CAS-moved to `state=deleted` and its pending install reservation is released
  (deletion becomes possible/implicit);
  `200 {"resolved":"found","external_id":"srv_x"}` — the instance exists and
  is this resource's own `external_id`; stuck `planned/creating/error` states
  are CAS-promoted to `ready` with the inventory IP;
  `409 {"error":"provider_reconcile_required","message":"N unaccounted servers
  in provider inventory"}` — ambiguous (empty `external_id` but unclaimed
  servers exist); nothing is mutated and no server is silently claimed;
  `409 resource_busy` — `ready`/`deleting` state or a live worker;
  `501 not_implemented` — beget; `404` — foreign or already-deleted resource.

Exact current text-profile install request (not a blueprint run):

```http
POST /api/me/cloud-resources/{resource_id}/install
Content-Type: application/json
Idempotency-Key: <one fixed key for this install attempt>
X-CSRF-Token: <session CSRF token>
Cookie: <ddugul session and matching CSRF cookie>

{"profile_id":6,"vars":{}}
```

Profile versioning is gone: a profile has exactly one content row, edited in
place. The install API selects the **profile ID**.

| Install body | Selection |
|---|---|
| `{"profile_id":6,"vars":{}}` | Single content row of profile 6 |
| `{"profile_version_id":26,"vars":{}}` | Legacy alias: only used when `profile_id` is absent; resolves to its profile and then uses that profile's current content |
| `{"vars":{}}` / `{}` | Existing operation's profile, or the legacy default if none |

An explicit zero/negative/null/non-integer `profile_version_id` is 422
`profile_invalid`; unknown or foreign unpublished profiles are 404 `not_found`.
A graph/non-text profile is 422 `profile_incompatible` before secret
preparation/decryption or SSH. Own-unpublished/published visibility and
user-profile feature gates still apply. `vars` uses the current row's
schema/defaults; `{}` inherits them and registry defaults. Success remains HTTP
200 with the installation operation DTO (`profile_id` identifies the profile);
`done` requires registration. Provisioning reloads the profile's current row at
acceptance and pins it for that worker.

Cloud installation also atomically persists a server-generated full NAME-HASH
in `operations.request_json.expected_node_id` before SSH (valid resource label
1..10 characters plus five lowercase alphanumeric characters). Templates get
that ID through protected `node_id` and `label`; resource display label is
unchanged. This is an **expected identity**, not registration: no owner/quota
row is created, `operations.node_id` stays empty, and `Confirm` still requires
a real `/register` using the pending token and the exact expected ID. A different
same-label ID is refused. Retries reuse the persisted expected ID and credential;
already-claimed installs reuse the actual owned ID/token, preserve the live
reservation, and reset the attempt's `operations.node_id` until a fresh claim.
Expected-ID metadata, pending credential, attempt state and resource operation
link commit together. No new client-editable identity field is accepted.
See `specs/hosting-blueprints/lifecycle-live-fixes.md` for verification and gaps.

Manual-install identity update: `POST /api/me/servers/{id}/install` uses a
persisted, internal `manual_hosts.install_key` plus host ID for new operation
idempotency. The key is not accepted or exposed in host JSON. Repeated installs
reuse the same record's operation/node; delete/recreate gets a fresh identity,
even with numeric ID and timestamp reuse. Valid same-lifetime explicit/legacy
linkage is retained; older unlinked legacy operations remain history. Errors
remain 403 `owner_mismatch` for evidence/owner conflicts and 500 `internal` for
SQL/preparation failures, not 503. No provider purchase occurs. Migration and
compatibility constraints: `docs/specs/fix-manual-install-owner.md`.

Ветка: `dev`. Milestone 1 (авторизованный мультиарендный пул). Phase 2/3
(Beget-провижининг, несколько мастеров на домен) — отдельные спеки после
этого milestone; здесь только зарезервированные места в схеме.

Источники правды:
- README/AGENTS.md репозитория (инварианты пула, machine API).
- `registry/internal/state`, `registry/internal/history` — существующие хранилища.

## 1. Контрольная БД (control store)

Новый пакет `registry/internal/control`, SQLite (modernc.org/sqlite, как
`internal/history`). Файл: `[database] control_db` в registry.toml, по
умолчанию рядом с state-файлом (`control.db`). **Fail-closed**: если БД не
открылась — панельные user-API и любая регистрация, требующая владельца,
отвечают 503; пул на legacy-режиме (см. §4) продолжает работу, только если
open вообще удался, иначе регистрация новых нод запрещена. Логи — единожды.

Отказ отдаётся как `503 control_unavailable` (машинный код неизменен) с
сообщением «Хранилище пользователей недоступно — обратитесь к администратору
для проверки журнала регистратора». Рестарт не предполагается без фактов.
Причина Open пишется в журнал с редактированием quoted-значений; повторный
лог состояния отсутствующего store ограничен разом в минуту. Профильные
операции отдельно логируют operation и underlying-ошибку/SQLite code.
Frontend сохраняет HTTP status/code и показывает server message приоритетно.

Таблицы (миграции встроены в Open, `CREATE TABLE IF NOT EXISTS`):

```sql
users(id INTEGER PK, username TEXT UNIQUE NOT NULL, password_hash TEXT NOT NULL,
      role TEXT NOT NULL CHECK(role IN ('admin','user')), max_nodes INTEGER NOT NULL DEFAULT 0,
      disabled INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL)
sessions(token_hash TEXT PK, user_id INTEGER NOT NULL, expires_at INTEGER NOT NULL)
authkeys(key_hash TEXT PK, user_id INTEGER NOT NULL, prefix TEXT NOT NULL,
         created_at INTEGER NOT NULL, revoked_at INTEGER)
node_owners(node_id TEXT PK, user_id INTEGER NOT NULL, registered_at INTEGER NOT NULL,
            legacy INTEGER NOT NULL DEFAULT 0)   -- сохраняется навсегда (prune/expiry не удаляют)
quota_reservations(node_id TEXT UNIQUE, user_id INTEGER NOT NULL, created_at INTEGER NOT NULL)
```

Резерв места: `password_hash` — bcrypt или аргон2id (выбор разработчика из
stdlib-совместимых зависимостей; md5/sha1/plain запрещены). `key_hash` —
sha256 authkey. Показ ключа — один раз в ответе rotate/create; хранится
только хеш + короткий префикс (`sda_XXXXXXXX…`) для UI.

## 2. Панельные API (Bearer `panel.token`, существующий `panelAuthorized`)

| Метод/путь | Тело | Ответ |
|---|---|---|
| `POST /panel/api/admin/users` | `{username, password, role?, max_nodes?}` | 200 `{id, username, role, max_nodes}` / 400 / 409 |
| `GET /panel/api/admin/users` | — | `{users:[{id, username, role, max_nodes, used_nodes, disabled}]}` |
| `PATCH /panel/api/admin/users/{id}` | `{max_nodes?, disabled?, password?}` | 200 обновлённый объект |
| `DELETE /panel/api/admin/users/{id}` | — | 204 (полное удаление; см. правила ниже) |
| `POST /panel/api/admin/users/{id}/authkey/rotate` | — | 200 `{authkey}` (полный ключ, один раз) |
| `DELETE /panel/api/admin/users/{id}/authkey` | — | 204 (все ключи пользователя revoked) |
| `POST /panel/api/admin/users/{id}/release-node` | `{node_id}` | 200 (владелец node_id → legacy, место освобождается; требуется когда нода удалена физически) |

Правила: username `[a-z0-9_.-]{3,32}`; пароль ≥ 10 символов; `max_nodes` 0..10000;
нельзя заблокировать/разжаловать последнего активного админа; смена
`max_nodes` вниз не удаляет существующие ноды, но блокирует новые регистрации
сверх лимита.

`DELETE /panel/api/admin/users/{id}` — полное удаление пользователя.
Отказ (400, русское сообщение): системный `legacy`
(`legacy_protected`, «Системного пользователя legacy удалить нельзя.») и
последний активный админ (`last_admin`, «Нельзя удалить последнего активного
администратора.»). Иначе: его ноды перепривязываются к системному `legacy`
(остаются в пуле, физически не удаляются, место в квоте освобождается — как
`release-node`); снимаются его `quota_reservations`; удаляются его
authkeys/сессии/профили (с версиями)/свои серверы (`manual_hosts`)/beget-связки/
операции/облачные ресурсы/pending-секреты. Возврат — 204. Неизвестный id — 404.

`POST /panel/api/admin/users` без явного `max_nodes` (0) выдаёт новому
пользователю `[users] default_max_nodes` (0 = прежнее поведение, квота 0).

Использование мест: `used_nodes = COUNT(quota_reservations WHERE user_id=?)` —
только ноды, находящиеся в пуле (активные резервы). Pending-секреты установок
места не занимают. `node_owners` —
отдельная вечная карта владения (`node_id → user_id`), в квоту НЕ входит и не
показывается в `used_nodes`: prune/expiry/terminate/замена по IP снимают резерв
(`ReleaseQuota`), поэтому «0 активных нод, но N/N» исключён.
Pending-бронь освобождается вместе с объектом: удаление cloud-ресурса (phantom
local или подтверждённое провайдером) снимает pending его `cloud_install`
операции, удаление manual-хоста — pending его `manual_install` операции.
Startup + периодический reconcile удаляет брони только тех failed/cancelled/
deleted установок, чьи ресурс/хост уже отсутствуют; pending retryable-установки
на живом ресурсе/хосте сохраняется.
Транзакционный захват места при регистрации (см. §3) идёт через
`quota_reservations` с `UNIQUE(node_id)`: сначала INSERT reservation, затем
SELECT count; если count > max_nodes → ROLLBACK + 403 `quota_exceeded`.
Повторная регистрация своей ноды (node_id уже в node_owners) — 200, резерв
восстанавливается идемпотентно, второе место не занимается; если нода была вне
пула, возврат разрешён только при свободном месте. Бан/terminate освобождает
место, и повторный `/register` после восстановления снова занимает его.

### 2.1 Кабинет пользователя (session-cookie, не panel.token)

Аутентификация: `POST /api/auth/register` (создаёт пользователя role=user,
max_nodes=0 по умолчанию — админ выдаёт квоту), `POST /api/auth/login`,
`POST /api/auth/logout`, `GET /api/auth/me`. Cookie `sharedd_session`
(HttpOnly, Secure, SameSite=Lax, из таблицы sessions M1), CSRF double-submit
на мутирующих запросах.

| Метод/путь | Назначение |
|---|---|
| `POST /api/auth/register` | `{username,password}` → 200 (создать; rate-limit по IP) |
| `POST /api/auth/login` / `logout` | сессия |
| `GET /api/me` | `{username, role, max_nodes, used_nodes}` |
| `GET /api/me/install-defaults` | session-only, read-only `{proxy_port:number, proxy_sni:string, proxy_secret_configured:boolean}`; registry defaults only, no plaintext secrets or identity metadata; `Cache-Control: no-store` |
| `GET /api/me/nodes` | свои ноды: `{node_id, ip, status, quarantine?, master_domains?, availability, dc2_ok}` |
| `GET /api/me/authkey` | `{prefix, created_at, active}` (полный ключ — только при rotate) |
| `POST /api/me/authkey/rotate` | отзывает ВСЕ ключи пользователя и выдаёт новый → `{authkey}` (полный ключ ОДИН раз; один ключ на все VPS пользователя) |
| `DELETE /api/me/authkey` | 204; все ключи отозваны (живые ноды продолжают работать, новые регистрации с ключом — нет) |
| `GET /api/me/servers` | свои серверы: `{servers:[{id,label,host,port,ssh_user,auth_type,created_at,verified_at?,last_error?,node_id?,operation?}]}` (секреты не отдаются) |
| `POST /api/me/servers` | 201; `{label,host,port?,ssh_user?,password?\|private_key?}` — свой сервер без провайдера (см. валидацию ниже) |
| `POST /api/me/servers/{id}/install` | `{profile_id?, vars?}` → операция `manual_install` (одно место в квоте на сервер, предсозданная нода, шаги по SSH; retry — та же нода, тех же операций, ноль провайдерских вызовов) |
| `POST /api/me/servers/{id}/check` | (опц.) проверка SSH-доступа/версии ОС без установки → `{verified, last_error}`, результат в `verified_at`/`last_error` |
| `DELETE /api/me/servers/{id}` | 204; удаляет запись и освобождает место (нода остаётся в пуле) |
| `GET /api/me/profiles` | свои локальные + published-профили для выбора |
| `GET /api/me/beget/regions` | регионы/тарифы из каталога (после connect) |
| `PUT /api/me/beget/link` | `{login,password,code?}` → проверка кредов (`code` — необязательный 2FA/verification код), регистрация SSH-ключа |
| `GET /api/me/beget/link` | `{verified, last_error}` (секреты не отдаются) |
| `POST /api/me/vps` | `{region, configuration_id, profile_id, vars, confirm:true, idem_key}` → операция (в ответе — `profile_id`, `has_vps`; готовность VPS ждётся ограниченно; таймаут → `waiting_vps`) |
| `GET /api/me/operations` | `{id, kind, state, node_id?, profile_id, has_vps, last_error, created_at}` (вкл. `unknown`/`waiting_vps`; `profile_id` — выбранный при создании профиль, `0` у legacy-записей) |
| `POST /api/me/operations/{id}/cancel` | отменить незавершённую (не платную после sent) |
| `POST /api/me/operations/{id}/retry-install` | повтор установки на существующий VPS (`{profile_id?, vars?}` → операция с `has_vps`; профиль берётся из сохранённого `operations.profile_id`, `profile_id?` — fallback только для legacy; новых `CreateVps` нет; отказ при нет VPS / активна / `done`) |

Manual-install recovery: `POST /api/me/servers/{id}/install` may restore a
released binding only for the same persisted failed `manual_install`, session
user, `manual-{host_id}` key, deterministic node identity and saved host.
Empty host `node_id`/`operation_id` are restored atomically with ownership,
one quota reservation and `installing`; conflicting links are never overwritten.
Foreign owners, generic legacy nodes and live claimants are refused with
**403 `owner_mismatch`**; quota refusal is **403 `quota_exceeded`**; concurrent
state changes are **409 `in_progress`**. Real SQL/setup failures are
**500 `internal`**, with redacted `manual.install.*` server diagnostics, not
503 initialization/restart guesses. An absent store still fails closed with
503 at the session guard. Recoverable node secrets are reused and checked
against their stored hash; unreadable/mismatched copies are not silently rotated.
Manual work interrupted on restart becomes failed/retryable without automatic
SSH execution or any provider call. See `specs/fix-manual-install-owner.md`.

`has_vps` — `true`, если у операции есть созданный VPS (непустой `operations.provider_vps_id`). Кабинет по нему скрывает «повторить установку» для операций, у которых VPS не создавался. Сам `provider_vps_id` — провайдер-внутренний id, наружу не отдаётся.

`PUT /api/me/beget/link` при отказе провайдера возвращает 400 `{"error":"beget_auth_failed","message":"<конкретное сообщение>","reason":"<код>"}`. `reason` — машиночитаемая причина, по которой UI подсвечивает поле «Код (2FA)». HTTP-статус провайдера проверяется РАНЬШЕ кода: при троттлинге Beget отдаёт `429` (иногда `403`) с ПУСТЫМ телом и без `code`.

| `reason` | Когда | Действие UI |
| --- | --- | --- |
| `rate_limited` | HTTP `429`/`403` (в т.ч. пустое тело) ИЛИ код содержит `RATE`/`LIMIT`/`CAPTCHA`/`TOO_MANY` | показать текст про ~30 минут ожидания и **выключить кнопку** (без авто-повтора и повторных запросов — они продлевают блокировку) |
| `bad_credentials` | HTTP `200` + `INCORRECT_CREDENTIALS` (провайдер отдаёт строкой в `error`) | показать сообщение про логин (не e-mail), API-доступ и отключение 2FA |
| `code_required` | код содержит `CODE`/`TOTP`/`2FA`/`VERIFY` | сфокусировать/подсветить поле «Код (2FA)» |
| `provider_error` | иначе | показать сырой код провайдера |

`message` для `rate_limited`: «Beget временно ограничил попытки входа для этого логина. Подождите примерно 30 минут и попробуйте один раз; частые повторы продлевают блокировку.»

Провайдер-код/сообщение и статус логируются сервером (`beget auth failed: login=u*** status=… code=… message=…`); пароль не логируется, логин маскируется.

### 2.2 Свои серверы (addendum-manual-servers.md §2)

Пользователь подключает уже существующий хост: `IP` + пароль **или** свой
SSH-ключ. Провайдер не участвует, платных вызовов нет. Секрет (пароль/ключ)
шифруется AES-GCM (`master.key`) и наружу не отдаётся ни одним эндпоинтом; в
`operations.last_error`/выводе прогона маскируется.

Валидация `POST /api/me/servers` (400 `{"error":"validation_failed","message":"<причина>"}`;
`message` — **на русском**, машиночитаемый `error`-код не меняется):

| Поле | Правило |
| --- | --- |
| `label` | непустой, ≤ 64 символов |
| `host` | публичный IPv4 (запрещены loopback/private/link-local и IP самого регистратора из `panel.public_url`) |
| `port` | 1..65535, дефолт 22 (явный `0` — ошибка; отсутствие поля → дефолт) |
| `ssh_user` | `[a-z_][a-z0-9_-]{0,31}`, дефолт `root` |
| `password` \| `private_key` | ровно ОДНО; `private_key` — валидный PEM/OpenSSH приватный ключ ≤ 16 KiB |

**Один сервер = одно место.** На `manual_hosts` действует
`UNIQUE(user_id, host, port)`: тот же `host:port` нельзя подключить дважды.
Повтор → 409 `{"error":"duplicate_host","message":"Этот адрес уже подключён. …"}`
(второй слот квоты и вторая личность ноды НЕ создаются). UI также блокирует
повтор до отправки. Миграция существующих БД безопасна: дубли схлопываются
(сохраняется запись с операцией, иначе самая ранняя), лишние ноды/резервы
снимаются, затем создаётся уникальный индекс.

`install` резервирует одно место в квоте на сервер (`AuthorizeNode`), создаёт
предсозданную личность ноды (`node_id` + `node_secret`) и запускает выбранный
профиль тем же `provision.Executor`, что и Beget. `done` ставится ТОЛЬКО после
подтверждённой регистрации ноды (verify-шаг профиля через `GET /api/node/status`
+ пост-проверка реестра); первый упавший шаг прерывает профиль. Повтор
`install` — та же операция (`idem_key = manual-<id>`) и та же нода, ноль
провайдерских вызовов. Коды: 400 `validation_failed`, 403 `quota_exceeded`,
400 `ssh_unavailable`, 409 `already_done`/`in_progress`, 409 `duplicate_host`
(повтор `host:port` в `POST /api/me/servers`), 404 `not_found`
(чужая/неизвестная запись существование не подтверждаем). Сообщения `message`
на русском; профиль-селект отдаёт 403 `user_profiles_disabled` / 400
`profile_unavailable` / 404 `not_found` также с русским `message`.


Админские user-эндпоинты — §2 (panel.token). Владелец-проверки обязательны.

## 3. Machine API — изменения

### POST /register (тело расширяется)

```json
{ "node_id": "...", "ip": "...", "authkey": "sda_...", "node_type": "..." }
```

Порядок авторизации:
1. Bearer — общий `[security] node_token` остаётся первым барьером (как сейчас).
2. `authkey` в теле: хеш ищется в authkeys; не найден/revoked → 403 `bad_authkey`.
   Пользователь disabled → 403 `user_disabled`.
3. Владелец: если node_id уже в node_owners → владелец должен совпасть
   (иначе 403 `owner_mismatch`); иначе привязываем пользователя и захватываем
   место по §2.

Исключение — «Разрешить общий секрет» = ON (§7.3): валидный общий токен БЕЗ
`authkey` регистрирует НОВУЮ ноду, привязывая её к системному `legacy` в
пределах `[users] legacy_max_nodes` (переполнение → 403 `quota_exceeded`, без
вставки). Уже привязанная к реальному пользователю нода по-прежнему требует
`authkey` (403 `bad_authkey`), legacy-нода — как раньше. Это восстанавливает
полную legacy-совместимость при ON (новые агенты без `authkey`).
Неизвестный Bearer (протухший `node_secret`) при ON отклоняется **401
`stale_node_secret`** — агент сбрасывает секрет и перерегистрируется общим
токеном; произвольные строки как валидные НЕ принимаются.

### POST /heartbeat, /report, /retire, GET /config

Дополнительно: если запрашивающий node_id не найден среди кандидатов,
регистратор сверяет node_owners; нода, удалённая из state, но не
освобождённая владельцем, отвечает как сейчас (410/terminate-логика) —
поведения пула не меняем. В поток отчёта HealthReport добавляются поля (см.
`docs/specs/dc2-health.md`), регулятор treats их как замену `metrics_ok`
(существующие fail/recover-защёлки и путь в quarantine — без изменений).

## 4. Совместимость / миграция

- Все ноды, зарегистрированные ДО включения user-режима, привязываются к
  системному пользователю `legacy` (role `user`, max_nodes = большой,
  создаётся миграцией, нельзя удалить через API).
- Конфиг: новая секция

```toml
[users]
enabled = true              # false = прежнее поведение, machine API не требует authkey
legacy_max_nodes = 100000
default_max_nodes = 0       # квота нового пользователя при регистрации по приглашению и admin POST без max_nodes; 0 = прежнее поведение (квота 0). Валидируется 0..1000000
allow_shared_secret = true  # только бутстрап durable-переключателя (см. §7); nil = true
```

При `users.enabled=false` контроллер БД может не открываться; все новые
панельные user-API отвечают 503 `users_disabled`.

## 5. Формат ошибок

`{"error":"<code>"}` + человекочитаемое сообщение, HTTP-статусы: 400 валидация,
401 нет/неверный Bearer, 403 authkey/owner/quota/invite, 404 нет объекта, 409
конфликт (дубль username / дубль имени профиля `duplicate_name`), 503
`control_unavailable` (control-БД недоступна или регистратор перезапускается,
см. §1) / `users_disabled`.

## 6. Phase 2/3 (НЕ реализуется в M1, резервируем имена)

- `beget_links(user_id, ...)` — подключения личных Beget-аккаунтов.
- `operations(id, user_id, kind, state, idem_key UNIQUE, provider_vps_id, profile_id, ...)`
  — durable-журнал платных операций с состоянием `unknown` при потере ответа;
  `profile_id` — профиль установки, выбранный при создании (`0` = не задан/legacy).
- `Assignments map[string][]string` (домен → список нод) для
  `masters_per_domain > 1` — отдельная спека + миграция state.

## 7. Приглашения, node_secret, «Разрешить общий секрет»

Реализация docs/specs/invitations-migration-contract.md (см. также
docs/specs/implementation-summary-invitations.md). Схема control.db
расширена (миграция внутри `control.Open`, включая ensureColumn для
существующих node_owners):

```sql
invites(id INTEGER PK, token_hash TEXT UNIQUE, prefix TEXT, created_by INTEGER,
        created_at INT, expires_at INT, used_by INT, used_at INT, revoked_at INT)
settings(key TEXT PK, value TEXT)   -- legacy_shared_secret = "on"|"off"
-- node_owners += node_secret_hash TEXT NOT NULL DEFAULT '',
--               node_secret_prefix TEXT NOT NULL DEFAULT '',
--               node_secret_enc TEXT NOT NULL DEFAULT ''   -- AES-GCM(master.key) копия (DEF-A)
```

### 7.1 Приглашения (Bearer `panel.token`)

| Метод/путь | Тело | Ответ |
|---|---|---|
| `POST /panel/api/admin/invites` | `{ttl_hours?}` (default 72, 1..720) | 201 `{id, invite_url, expires_at, prefix}`; полный токен виден ОДИН раз в `invite_url` |
| `GET /panel/api/admin/invites` | — | `{invites:[{id, prefix, created_at, expires_at, status, used_by, used_by_username}]}`; status `active\|used\|revoked\|expired` |
| `DELETE /panel/api/admin/invites/{id}` | — | 204 идемпотентно; использованное (used) приглашение терминально и отзывом не меняется |

`invite_url` = `panel.public_url` + `/signin?invite=<token>` (без public_url —
относительный путь). Саморегистрация: `POST /api/auth/register
{username,password,invite}` — открытой регистрации больше нет. Валидация
полей и резерв приглашения атомарны в одной транзакции с созданием
пользователя (условный UPDATE: `used_by IS NULL AND revoked_at IS NULL AND
expires_at>now`); ошибка валидации полей приглашение НЕ расходует. Коды
отказа (403): `invite_required` / `invite_invalid` / `invite_expired` /
`invite_used` / `invite_revoked`.

### 7.2 node_secret

Выдаётся в JSON-ответе успешного `POST /register`
(`{"node_secret":"..."}` — 32B urlsafe, полный секрет только там и у агента);
в `node_owners` хранится sha256 + префикс (источник истины аутентификации,
constant-time сравнение хеша) и AES-GCM копия `node_secret_enc` (master.key),
чтобы registry-initiated `retry-install` мог отрендерить реальный токен, а не
`''` (DEF-A). Агент хранит секрет в `/var/lib/sharedd/node_secret` (0600) и
отправляет как `Authorization: Bearer` предпочтительнее общего токена;
опциональный `[registry] node_secret` в node.toml — ручное предвыданное
значение, `[registry] authkey` — персональный ключ в теле `/register`. Старые
агенты и старые реестры совместимы (тело без node_secret игнорируется).

**Перевыпуск.** `POST /register`, аутентифицированный **личным секретом этой
же ноды**, идемпотентен: секрет НЕ ротируется (иначе verify-шаг профиля и
`retry-install` лишились бы валидного токена, а живой агент был бы обесточен).
Ротация происходит только при перерегистрации по authkey/общему токену
(старый хеш инвалидируется). При отсутствии `node_secret_enc` (legacy-строка)
`retry-install` НЕ перевыдаёт секрет, если нода уже регистрировалась; иначе
выдаёт свежий (fallback-правило DEF-A).

### 7.3 Матрица machine-аутентификации («Разрешить общий секрет»)

Применяется на ВСЕХ machine-маршрутах (`/register`, `/heartbeat`, `/report`,
`/retire`, `GET /config`, `GET /api/node/status`); 401/403 идут раньше
400-валидации тела.

| Режим | Bearer = общий | Bearer = node_secret |
|---|---|---|
| ON (default) | принимается | принимается (различение по sha256-lookup среди нод) |
| OFF | **401** `shared_token_disabled`; `/register` — исключение: общий Bearer с валидным authkey в теле пропускается | принимается; node_id маршрута (тело или `X-ShareDD-Node-ID` для /config) обязан совпадать с владельцем секрета, иначе **403** `node_id_mismatch` |

`/register` при ON (FULL legacy-совместимость): валидный общий токен без
`authkey` регистрирует новую ноду под системным `legacy` (квота
`legacy_max_nodes`); нода реального пользователя всё равно требует `authkey`.
`/register` при OFF: авторизация по authkey (новая/своя нода) либо по своему
node_secret (перевыпуск, владелец совпадает). При `[users] enabled=false`
(control-БД нет) переключателя не существует — общий токен остаётся
единственным транспортом (прежнее поведение).

Неизвестный Bearer (не общий токен и не известный `node_secret`): при ON —
**401 `stale_node_secret`** (агент сбрасывает хранимый секрет и повторяет
регистрацию общим токеном), при OFF — **401 `unauthorized`**. HTTP-статус
всегда 401: произвольная строка валидной не признаётся.

Хранение: settings control.db, ключ `legacy_shared_secret`; бутстрап при
отсутствии строки — из `[users] allow_shared_secret` (default true) при
первом чтении/записи; рестарт сохраняет режим. Панель:

| Метод/путь | Тело | Ответ |
|---|---|---|
| `GET /panel/api/admin/legacy-secret` | — | `{enabled, affected_nodes, as_of}`; `affected_nodes` — живые кандидаты без секрета (`null` при ошибке расчёта) |
| `POST /panel/api/admin/legacy-secret` | `{enabled}` | `{enabled, persisted:true}`; неудача записи → 5xx без изменения режима (fail-closed) |

Применение OFF мгновенное (проверка на каждом запросе), OFF не даёт
постоянных исключений: единственные пути — node_secret/authkey.

### 7.4 GET /api/node/status (DEF-B)

Read-only подтверждение, что реестр ЗНАЕТ ноду и она реально зарегистрировалась
(используется verify-шагом штатного профиля; провал `/register` делает установку
`failed`, а не ложный `done`).

| Аспект | Значение |
|---|---|
| Query | `node_id` (NAME-HASH) |
| Bearer | **только** персональный `node_secret` этой ноды; общий `node_token` не принимается (не доказывает личность) и чужой секрет отклоняется |
| 200 | `{node_id, registered:true, owner}` — нода есть среди живых кандидатов; секретов в теле нет |
| 403 `node_id_mismatch` | Bearer — общий токен либо секрет другой ноды |
| 404 `not_registered` | секрет свой, но нода ещё не вызывала `/register` |
| 400 `validation_failed` | `node_id` отсутствует/невалиден |
| 401 | нет/неизвестный Bearer (middleware) |

Ограниченные повторы (30×2с) — в самом verify-шаге профиля, т.к. агент
стартует асинхронно после `systemctl`.

### 6.1 M2 delivered (schema/packages AND HTTP endpoints)

Реализовано в `registry/internal/{control,beget,provision,sshx,cryptoutil}`
(docs/specs/beget-provisioning.md, docs/specs/implementation-summary-beget.md).
HTTP-эндпоинты Beget/профилей **смонтированы** в кабинете (см. §2.1):
`/api/me/beget/link` (GET/PUT), `/api/me/beget/regions`, `/api/me/vps`,
`/api/me/operations`, `/api/me/operations/{id}/cancel`. Установка профиля — шаги
по SSH (`provision.ApplyProfile` через `sshx`; внутренний ops-путь, не через HTTP).
Схема контрольной БД расширена (миграция внутри `control.Open`):

```
beget_links(user_id UNIQUE, login_enc, password_enc, ssh_key_id, ssh_pub, ssh_priv_enc, ...)
profiles(name UNIQUE, ..., published)
profile_versions(profile_id, version, playbook, vars_schema, UNIQUE(profile_id,version))
-- одна строка на профиль: правки UPDATE in place; version остаётся 1
-- колонка playbook хранит ТЕКСТ ШАГОВ (shell-команды); имя историческое
operations(user_id, node_id, kind, state, idem_key UNIQUE, provider_vps_id, profile_id, ...)
cloud_resources(vps_id PK, user_id, node_id, region, config_id, state, ...)
```

Секреты (`login_enc/password_enc/ssh_priv_enc`, `operations.request_json`)
в API-представления НЕ сериализуются.

## 8. Addendum: selection, приглашения на N, профили, VPS-поток

Реализация docs/specs/addendum-profiles-vps.md. Ветка `dev`.

### 8.1 Секция `selection` (PUT /panel/api/config)

`editableConfig.Selection = {masters_per_domain}` (1..8; 0/9 → 400). Merge-семантика
как у остальных секций: `selection` отсутствует → не трогаем. Hot-apply: селекция
читает `cfg.Selection.MastersPerDomain` под `cfgMu` на каждом тике — новое значение
влияет на число слотов следующего тика без рестарта. `GET /panel/api/config`
отдаёт эффективное значение. Это НЕ `[srmd] max_nodes_per_domain`.

**Конверт ответа `PUT /panel/api/config`.** Успех — `200 {ok:true, changed:[...],
persisted:true, dns_push?}`. Если рантайм применён, но запись TOML не удалась
(например, `/etc/sharedd` не в `ReadWritePaths` при `ProtectSystem=strict`) —
**500** `{ok:false, persisted:false, changed:[...], error:"не сохранено: <причина>",
message:"Изменения применены, но не записаны в <path>: <причина>"}`. Раньше
отвечала `200 ok:true` с `persisted:false` — silent partial success; убрано, чтобы
панель показывала «не сохранено», а не «применено».

### 8.2 Приглашения на N регистраций

```
-- invites += max_uses INTEGER NOT NULL DEFAULT 1, used_count INTEGER NOT NULL DEFAULT 0
```

| Метод/путь | Тело | Ответ |
|---|---|---|
| `POST /panel/api/admin/invites` | `{ttl_hours?, max_uses?}` (max_uses 1..1000, дефолт 1) | 201 `{id, invite_url, expires_at, prefix, max_uses}` |
| `GET /panel/api/admin/invites` | — | `{invites:[{id, prefix, created_at, expires_at, status, used_by, used_by_username, max_uses, used_count}]}`; status `active\|exhausted\|expired\|revoked` |
| `DELETE /panel/api/admin/invites/{id}` | — | 204 идемпотентно (исчерпанное терминально) |

Регистрация атомарна: `UPDATE invites SET used_count=used_count+1, used_by=?,
used_at=? WHERE token_hash=? AND revoked_at IS NULL AND expires_at>?
AND used_count<max_uses` — гонка не превышает max_uses. Код исчерпания —
`invite_used` (совместимо с UI; отдельного `invite_exhausted` нет). Счётчик
приглашения с квотой нод не связан (регистрация создаёт `max_nodes=0`).

### 8.3 Профили установки

Профиль — упорядоченный список **шагов** (shell-команд), выполняемых по SSH на
VPS пользователя. Формат текста шагов (как в mtplane): строка `# Название`
открывает шаг, последующие непустые строки — его команда (многострочная
сохраняется). Строка `#без пробела` — shell-комментарий внутри команды.

Админ (Bearer `panel.token`):

| Метод/путь | Тело | Ответ |
|---|---|---|
| `GET /panel/api/admin/profiles` | — | `{profiles:[{id,name,description,published,created_at,updated_at,steps,vars_schema,format,blueprint}]}` — только системные/опубликованные (`created_by=0`); личные копии пользователей в админке не перечисляются (видны владельцу через `/api/me/profiles`) |
| `POST /panel/api/admin/profiles` | `{name, description?, steps, vars_schema?, published?}` | 201 профиль (создаёт версию 1) |
| `PUT /panel/api/admin/profiles/{id}` | `{name?, description?, steps?, vars_schema?, published?}` | 200; `steps`/`vars_schema` перезаписывают единственную строку контента in place (версий больше нет, published не сбрасывается). `name` — метаданные; имя тримится, пустое → 400, коллизия без учёта регистра → 409 `duplicate_name`, неизменное имя — no-op |
| `DELETE /panel/api/admin/profiles/{id}` | — | 204 |

Admin PUT: `description` omitted keeps its value; `description:""` clears it.
Either `steps` or `vars_schema` overwrites that one row, preserving the omitted
field from the current row. Metadata-only saves do not change content or
publication. DTO includes `format` and (for graphs) `blueprint`; there is no
`versions` field.
Profile DB failures are logged with operation and redacted error/SQLite code:
unknown errors → `500 internal`, other SQLite constraints →
`409 constraint_conflict`, duplicate name → `409 duplicate_name`;
validation remains `400` with its message. Latest-version read errors are not
silently returned as empty steps. See `docs/specs/fix-profile-control-errors.md`.

Пользователь (session-cookie + CSRF), только свои:

| Метод/путь | Назначение |
|---|---|
| `GET /api/me/profiles` | `{profiles:[свои], published:[опубликованные], enabled, max}` |
| `POST /api/me/profiles` | создать локальный профиль (published=false), лимит `[provisioning] max_user_profiles` (дефолт 5) |
| `POST /api/me/profiles/{id}/copy` | копия видимого профиля (опубликованного ИЛИ своего; чужой неопубликованный/несуществующий → 404 `not_found`) в свой черновик: `published=false`, `created_by=пользователь`, имя «`<оригинал> (моя копия)`», при коллизии «…(моя копия) -2», -3…; шаги и `vars_schema` — дословно из последней видимой версии (секретных ЗНАЧЕНИЙ в копии нет — `vars_schema` хранит только объявления, как их видит зритель профиля). Лимит → 403 `profile_limit_reached`; флаг выключен → 403 `user_profiles_disabled`; коллизия не разрешилась → 409 `duplicate_name`. Успех → 201 с JSON нового профиля |
| `PUT /api/me/profiles/{id}` | правка своего (чужой → 404). `steps`/`vars_schema` перезаписывают единственную строку контента (`expected_version` принимается и игнорируется, 409 из-за него не бывает); `name` (опция) — метаданные: непустое, тримится, уникально без учёта регистра среди видимых профилей, коллизия → 409 `duplicate_name` |
| `DELETE /api/me/profiles/{id}` | удалить свой (чужой → 404) |

YAML/песочницы больше нет. Потолок текста шагов — 64 KiB → 400 `steps_rejected`.
Пользовательские профили включены по умолчанию (`[provisioning] user_profiles_enabled`,
nil = true). Выключение флага (`=false`) отвергает и создание/правку своих
профилей, и выбор собственного неопубликованного профиля при создании VPS →
403 `user_profiles_disabled` (опубликованные admin-профили флагом не
затрагиваются).
Остаточный риск: шаги — обычные shell-команды, исполняемые как root на
**своём** VPS пользователя; владелец VPS и так распоряжается своей машиной.
Реестр шаги локально не запускает, а его собственные креды/секреты в SSH-сессию
не инъектятся и из неё недостижимы.

#### Переменные профиля ({{ var }}, §6)

`vars_schema` — таблица объявленных переменных:
`{"vars":[{"name":"testvar","default":"x","description":"...","secret":true}]}`.
Подстановка — серверный `text/template`, КАЖДОЕ значение shell-квотится РОВНО
один раз (`control.RenderStep`); своего шаблонизатора у пользователя нет, инъекция
через значение невозможна. Поддерживаются ОБА синтаксиса — документированный
`{{ var }}` и исторический `{{.var}}` (в т.ч. смешанные в одной команде). Имя
переменной, совпадающее со встроенной функцией шаблонизатора, тоже подставляется.
Защищённые переменные (инъектируются сервером, в схеме не объявляются):
`node_id`, `node_secret`, `registry_url`. Их объявление в `vars_schema` или
передача в request `vars` отклоняется с `400 validation_failed`, даже с пустым
значением. `registry_url` всегда равен `panel.public_url`, включая пустую строку;
старого исключения с сохранением входного URL больше нет. Общий реестровый
`node_token` в vars НЕ инъектируется.
Обычные прокси-переменные — `proxy_port`, `proxy_sni`, `proxy_secret`:
**явный install override > непустой объявленный profile default > registry default**.
Совместимые имена `port`, `sni`, `secret` теперь нормализуются к этим трём
ключам ПЕРЕД merge: приоритет сохраняется даже между разными именами одной
переменной. Конфликтующие явно переданные alias/canonical значения отклоняются,
а не молча перезаписываются. Общий `provisionBaseVars` выдаёт оба имени с одним
проверенным значением для cloud/manual/Beget; сохранённые профили не меняются.
`port`/`proxy_port` обязаны совпадать с текущим `[shared_proxy].port`; несовпадение
override/default — ошибка, не скрытая замена и не изменение порта регистратора.
Источник registry defaults — `[shared_proxy]`:
`proxy_port` = `port` (default 443), `proxy_sni` = `tls_domain`, `proxy_secret` =
client secret из `users` (ровно один entry — его секрет; несколько — секрет
пользователя с алфавитно-первым именем; ноль — пустая строка). `proxy_secret` —
СЕКРЕТ: маскируется (`***`) как `node_secret`. Отсутствующий request key наследует
непустой declared default; отсутствующий/пустой declared default наследует
registry default. Явный `proxy_secret:""` сохраняется и подавляет оба дефолта.
Но если шаги используют `secret`/`proxy_secret`, пустое/нестроковое значение
отклоняется ДО SSH. Маскируются оба имени, включая shell-quoted форму значения.
Упомянутый в шагах `registry_url` также обязан быть непустым. Для cloud-install
`node_id` — полный ожидаемый NAME-HASH, атомарно сохранённый сервером до SSH;
он не подменяется неполным label и не считается регистрацией. В других путях,
где полный ID не подготовлен, отсутствующий обязательный `node_id` по-прежнему
отклоняется до SSH; защиты от пустого значения не отключаются.
Явный пустой `proxy_port` или `proxy_sni` — ошибка, не fallback. Reset означает
удаление ключа из request `vars`, не отправку `""`/`null`/показанного placeholder.
`proxy_port` — десятичные цифры (строка или целое JSON-число), 1..65535;
`proxy_sni` — строка, валидный hostname по registry validator (ASCII domain с
точкой, ≤253 байта, labels 1..63, без underscore, wildcard, trailing dot,
схемы/path/port/пробелов или IP literal). Значения не тримятся сервером. Непустые schema
defaults для этих двух полей проверяются при save и install; protected legacy
declarations/invalid defaults дают диагностику, хранимые версии не переписываются.
`ad_tag`/валидные `ad_tag_*` — обычные переменные (request > schema default);
автоматической инъекции значения нет; никаких глобальных ad-tag настроек/aliases.
Пользователь переопределяет объявленные и добавляет свои сверх. Валидация имени
`^[a-zA-Z_][a-zA-Z0-9_]*$`, значение ≤ 4096, число переменных ≤ 64, суммарный блоб
≤ 32 KiB. Значения `secret:true` (а также `node_secret`/`authkey`/`password`/
`proxy_secret`/`secret`) маскируются (`***`) в `last_error`/выводе операции; маскирование
выполняется ДО обрезки вывода, так что длинный секрет не оставляет префикс.

#### Installation defaults metadata / UI handoff (dev)

`GET /api/me/install-defaults` использует `requireSession`: нет/просрочена сессия
→ `401 unauthorized`, disabled user → `403 user_disabled`, users disabled →
`503 users_disabled`, unavailable control store/session lookup →
`503 control_unavailable`. GET ничего не изменяет, CSRF не нужен; panel Bearer
не заменяет cookie. Ответ — ровно три поля, без envelope:
`proxy_port` (number), `proxy_sni` (string), `proxy_secret_configured` (boolean:
выбранный registry client secret непустой). **Никаких** `node_token`, `node_secret`,
`proxy_secret` plaintext, `registry_url`, merged vars или synthetic defaults.
Админ использует существующий `GET /panel/api/config` (panel Bearer), не новый
cabinet GET; cabinet не запрашивает привилегированный panel config.

Profile list metadata остаётся raw `vars_schema`, не effective install vars.
`ProfileVar.default` имеет `omitempty`: отсутствие/`""` означает inheritance,
а не explicit empty override. UI держит override-state отдельно от display value:
untouched/reset keys omitted; явный secret/ad_tag empty остаётся в request.
Открытие/metadata-only save не должно сохранять synthetic rows/defaults.

UI всегда показывает у `proxy_port`:
«Порт установки должен совпадать с [shared_proxy].port регистратора.
Изменение здесь не меняет настройки регистратора или допуск ноды к мастерству.»
Это не SSH port; admission, agent port enforcement и antiscan не изменяются.
Подробный merge/reset/retry контракт и файлы: `docs/specs/implementation-standard-vars.md`.

### 8.4 Каталог тарифов и VPS-поток

`GET /api/me/beget/regions` → `{regions:[...], configurations:[{id,name,cpu,ram,disk,
price,period?,region?,available}]}`. `price` — `null`, если провайдер не отдал
цену (НЕ 0).

`POST /api/me/vps` — поток операции: `pending/sent → confirmed →
(ожидание RUNNING ограниченно) → installing → done|failed`. Платный заказ (ровно
один `CreateVps` на `idem_key`) отправляется синхронно (pending-строка персистится
ДО вызова), поэтому ответ несёт `confirmed`/`has_vps=true`; ожидание `RUNNING` и
установка профиля выполняются в фоновом воркере с СОБСТВЕННЫМ бюджетом
(`vps_ready_timeout_min` + N×`install_timeout_min`), не под 30-секундным
запросным context. Состояние и вывод операции персистятся — UI опрашивает
`GET /api/me/operations`. Одна операция не может выполняться дважды (guard по
operation id); при остановке реестра воркеры отменяются и новых платных вызовов
не инициируют. Шаги профиля выполняются по SSH (`registry/internal/sshx`) на VPS
пользователя с его расшифрованными из Beget-связки кредами (приватный ключ, иначе
пароль аккаунта). Каждый шаг исполняется `bash -eo pipefail -s` (тело на stdin):
ошибка середины многострочного шага или пустой вход пайпа (`curl … | bash`) НЕ
даёт ложный `done`. Таймаут одного шага — `[provisioning] install_timeout_min`;
первый упавший шаг прерывает профиль, вывод обрезается. Отсутствие SSH-кредов →
операция `failed` БЕЗ платного заказа (код `ssh_creds_unavailable`).

**Предсозданная личность (§7.1):** до платного вызова выделяется
детерминированный `node_id`, генерируется свежий `node_secret`, пишется его
sha256 + привязка владельца, резервируется место в квоте. Плейн-секрет
инъектируется ТОЛЬКО в vars установки (`node_secret`); `POST /register`
принимает Bearer с этим секретом для своей ноды. Секрет не возвращается ни одним
read-эндпоинтом и не логируется.

**Ожидание готовности (§7.3):** фоновый воркер ограниченно ждёт `RUNNING`
(`[provisioning] vps_ready_timeout_min`, дефолт 10; интервал опроса
`vps_poll_interval_sec`, дефолт 10с). Таймаут → `waiting_vps` (место в квоте
занято, новый заказ невозможен).

**Свой профиль (§7.4):** владелец может выбрать свой `published=false` профиль
при создании VPS; чужой неопубликованный → 404 `not_found`. `profile_id=0` →
штатный профиль `sharedd-node`. Выбранный профиль сохраняется в
`operations.profile_id` (миграция `ensureColumn`, `0` у legacy-записей) и
отдаётся в operation JSON; `retry-install` повторяет именно его, а не дефолт.

В vars профиля инъектируются `node_id`, `registry_url` (panel.public_url),
персональный `node_secret` (привязан к node_id через node_owners) и встроенные
прокси-переменные `proxy_port`/`proxy_sni`/`proxy_secret` (из `[shared_proxy]`,
§8.3) — профиль может сам поднять прокси и вытянуть клиентский креденшл.
При
`retry-install` секрет НЕ перевыдаётся: возвращается сохранённая AES-GCM копия
(§7.2), так что команда несёт РЕАЛЬНЫЙ токен, а не `''` (DEF-A). Новый секрет
выдаётся только когда его ещё нет, либо при отсутствии копии и условии, что
нода ещё ни разу не регистрировалась. Штатный профиль явно передаёт
предсозданный id (`--name=<node_id>`) и завершается verify-шагом: агент поднялся
И подтвердил регистрацию через `GET /api/node/status` (§7.4) — провал
`/register` или no-op установка дают `failed`, а не ложный `done`.

**Восстановление после рестарта (DEF-C):** операции, застигнутые в `installing`,
при старте реестра до-подхватываются фоновым resume-путём (`InstallingOperations`
→ `InstallOnConfirmed`), без новых платных вызовов; подготовить операцию не
удалось — она терминализируется `failed`, чтобы `retry-install` был доступен.
`retry-install` из `installing` разрешён, когда воркер уже не держит операцию
(`runningOp == false`), иначе 409 `in_progress`.

### 8.5 Новые ключи `[provisioning]`

```toml
[provisioning]
max_creates_per_day = 5
cooldown_minutes = 30
max_user_profiles = 5           # 1..100
install_timeout_min = 20        # 1..1440 — таймаут ОДНОГО шага профиля
vps_ready_timeout_min = 10      # 1..1440 — ограниченное ожидание RUNNING (§7.3)
vps_poll_interval_sec = 10      # 1..600 — интервал опроса статуса VPS
user_profiles_enabled = true    # false = пользовательские профили выключены
```

### 8.6 GET /api/me/nodes/{id}/detail — per-user трафик 24ч

Поле `users.traffic_by_user` в ответе (тот же owner-check + session, что у
DELETE /api/me/nodes/{id}): `null` или массив за ПОСЛЕДНИЕ 24 ЧАСА:

```json
{"user": "string", "ingress_bytes": 123, "egress_bytes": 456}
```

Источник: агент кладёт в metrics_snapshot per-user cumulative ключи
`sharedd_user_ingress_bytes_total{user="X"}` /
`sharedd_user_egress_bytes_total{user="X"}` (только раздаваемые регистратором
пользователи); регистратор пишет дельты в `history.traffic_user` и на
node-detail суммирует их за 24ч по пользователю (сортировка по `user`).
`traffic_by_user` попадает в `absent` только если `users != nil` и поле null
(нет БД / нет наблюдений). `unique_ips_by_user` не меняется.
