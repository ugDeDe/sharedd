# 03. API кабинета: текущее и предлагаемое

**Новые маршруты ниже — проектный контракт, не работающий API.** Внешние
вызовы определены в [02](02-provider-api.md), состояния — в
[04](04-storage-and-execution.md), schema графов — в [05](05-blueprint-profiles.md).

## 1. Существующие маршруты и совместимость

Источник истины: `mountCabinet`, `mountMeProfiles`, manual/admin mounting
в `registry/internal/server/`. Старые payload/status не заменять новыми DTO.

| Уже существует | Назначение / ограничения совместимости |
|---|---|
| `GET /api/me`, `GET /api/me/nodes`, `GET /api/me/nodes/{node_id}/detail` | Session и собственные ноды |
| `GET /api/me/beget/link`, `PUT /api/me/beget/link` | Единственная legacy Beget-связка |
| `GET /api/me/beget/regions` | Legacy каталог |
| `POST /api/me/vps` | Legacy заказ; существующий body `idem_key` сохраняется |
| `GET /api/me/operations` | Существующая история/step logs, формат сохраняется |
| `POST /api/me/operations/{id}/cancel` | Только отмена ещё pending покупки; **не** cancel графа и не универсальная отмена in-flight |
| `POST /api/me/operations/{id}/retry-install` | Явный legacy retry установки, не повтор покупки |
| `DELETE /api/me/nodes/{node_id}` | Существующий контракт удаления ноды, не подмена новым cloud-resource DELETE |
| `GET/POST /api/me/servers`, `DELETE /api/me/servers/{id}` | Собственные manual SSH hosts |
| `POST /api/me/servers/{id}/check`, `POST /api/me/servers/{id}/install` | Проверка SSH и manual installation с существующей identity/idempotency |
| `GET/POST /api/me/profiles`, `PUT/DELETE /api/me/profiles/{id}`, `POST /api/me/profiles/{id}/copy` | CRUD текстовых профилей; новые поля добавляются совместимо |
| `GET/POST /panel/api/admin/profiles`, `PUT/DELETE /panel/api/admin/profiles/{id}` | Панельный admin CRUD/publish; сохраняет текущий panel auth |

Legacy handlers после миграции — адаптеры над теми же ownership/quota/
operation services. Никакого второго writer ресурсов/резервов. Старый body
`idem_key` транслируется внутренне; новым маршрутам body `idem_key` запрещён.

## 2. Общие правила новых маршрутов

- `/api/me/*`: session cookie, активный пользователь, object ownership на
  каждом ID и всех ссылках inputs. Недоступный/чужой ID — 404 `not_found`.
  Admin role не даёт обход владения через `/api/me`; административный доступ
  остаётся в `/panel/api/admin/*` с существующим `panelAuthorized`.
- Каждая mutation, включая verify/estimate/validate/refresh/cancel/resume,
  проходит существующий CSRF middleware. Отсутствующая session — 401,
  disabled — 403 `user_disabled`, CSRF — 403 `csrf_invalid`.
  Заголовок CSRF — `X-CSRF-Token` с текущим double-submit cookie.
- JSON только `application/json`, строгий decode, unknown fields и второй
  JSON value запрещены. Обычный body ≤64KiB; graph validate/CRUD ≤256KiB;
  SSH source ≤64KiB отдельно. Слишком большой body — 413 `body_too_large`.
- Все ответы `Cache-Control:no-store`; secretref не разрешается в plaintext
  response. Логи/redaction до сериализации, включая ошибки и stdout.
- Базовые лимиты: reads 120/min/session, metadata writes 30/min/session,
  commands 10/min/user; burst равен лимиту. Дополнительно один активный
  mutation на resource и один in-flight balance на connection revision.
  HTTP429 `rate_limited`, `Retry-After` целые секунды. Эти серверные пределы
  не отменяют меньшие provider limits. Handler I/O deadline 15s; provider
  mutation выполняется worker, не в HTTP handler.
- Ошибка сохраняет **текущий `controlAPIError`**, без новой envelope:

```json
{"error":"revision_conflict","message":"Настройки изменились; обновите данные"}
```

`error` — строка машинного кода, `message` — безопасный русский текст.
Не возвращать `success:false`, nested error вместо строки или provider body.
Диагностика validate — в успешном DTO `valid:false`, не дополнительная
несовместимая error envelope.

### 2.1 Idempotency и confirm

Для новых command POST create/replace/change-ip, DELETE resource и
POST profile-runs/cancel/resume обязателен `Idempotency-Key` (UUID).
Остальные metadata endpoints не требуют его: PATCH/PUT защищены revision,
balance/verify coalesced. Estimate/validate не имеют side effects.

Namespace `(user_id, method, canonical_path, key)`; сохраняются canonical
request hash, credential/policy revision, response/operation ID до network.
Повтор того же тела возвращает тот же 202 и тот же ID, даже для завершённой
операции; новое тело с тем же key — 409 `idempotency_conflict`. Новая команда
для занятого ресурса с другим key — 409 `resource_busy`. Retention записи
не меньше lifetime ресурса/незавершённых effects; не истекать unknown.
Повтор key не становится разрешением provider retry.

Все команды с покупкой/удалением: body `confirm:true`. Для DELETE cloud
resource **нет тела**, используются `X-Confirm-Action: delete` и
`Idempotency-Key`; это единственный новый destructive header contract.
Нет confirm — 400 `confirmation_required`. Cancel/resume сами по себе не
требуют confirm покупки; resume не вправе разрешить новую непредусмотренную
покупку. Предварительный estimate не резервирует цену/деньги.

Успешная асинхронная resource-команда всегда HTTP202:

```json
{"operation_id":301,"state":"pending","resource_id":81}
```

Create выделяет внутренний resource ID до provider I/O; external ID может
ещё отсутствовать. `state:pending` в повторяемом ack — состояние принятия,
актуальное состояние читать по operation detail, не по replay ack.
Run ack также имеет `run_id`; при ещё неизвестном ресурсе `resource_id:null`.

## 3. Hosting connections (всё предлагаемое)

ID — внутреннее положительное integer, provider `nodehost|beget`, owner
вычисляется из session и никогда не принимается из тела.

| Метод / путь | Вход | Успех |
|---|---|---|
| `GET /api/me/hosting-connections` | — | 200 `{connections:[Connection]}` |
| `POST /api/me/hosting-connections` | create ниже | 201 `Connection`, без внешней покупки |
| `PATCH /api/me/hosting-connections/{id}` | `{expected_revision,label?,credentials?}` | 200 `Connection`; полная замена переданного credential bundle |
| `DELETE /api/me/hosting-connections/{id}` | без тела | 204; 409 `connection_in_use` при resource/run/unknown effect/policy reference |
| `POST /api/me/hosting-connections/{id}/verify` | `{code?:"2FA"}` | 200 `{verified:true,checked_at,capabilities}` |
| `POST /api/me/hosting-connections/{id}/balance/refresh` | `{}` | 200 `Balance` даже при provider error (stale/unavailable) |
| `GET /api/me/hosting-connections/{id}/catalog` | optional `location` | 200 `{provider,credential_revision,checked_at,capabilities,configuration_schema,catalog}` |

Verify при business отказе: 422 `provider_auth_failed` или
`provider_2fa_required`; при timeout 504 `provider_timeout`; rate 429.
Credential input examples:

```json
{"provider":"nodehost","label":"Основной API","credentials":{"api_key":"<write-only>"}}
```

```json
{"provider":"beget","label":"Облако","credentials":{"cloud_login":"<write-only>","cloud_password":"<write-only>","hosting_api_login":"<write-only optional>","hosting_api_password":"<write-only optional>"}}
```

Для Beget cloud pair обязательна, hosting pair либо обе заданы, либо обе
отсутствуют. 2FA code передаётся только в verify; не сохраняется. Нельзя
принимать пустую строку как «оставить старый пароль»: omission credentials
сохраняет bundle, переданный объект заменяет его полностью. Provider immutable.
`label` 1..100 символов. PATCH revision mismatch 409 `revision_conflict`.
Смена credentials в активной неизвестной mutation — 409 `connection_busy`,
чтобы не потерять возможность reconcile; read refresh не блокирует PATCH.

```json
{"id":7,"provider":"nodehost","label":"Основной API","revision":2,"credential_revision":2,"credentials_configured":true,"verified_at":null,"capabilities":{"create":false,"change_ip":false,"delete":false},"balance":{"amount":null,"currency":null,"scope":"nodehost_api","freshness":"unavailable","checked_at":null,"last_attempt_at":null,"error":null}}
```

Capabilities вычисляет сервер после verify и учёта integration gates; клиент
не может выставить их PATCH. Catalog содержит только allowlisted provider
данные, не список серверов чужого аккаунта; credentials/JWT/password удалены.

## 4. Cloud resources

`resource_id` всегда surrogate control ID, не UUID Beget/строка Nodehost.
Доступные commands только к managed resource этой connection/owner;
произвольный provider external ID не принимается как разрешение на удаление.

| Метод / путь | Вход | Успех |
|---|---|---|
| `GET /api/me/cloud-resources` | query `cursor?`, `limit=1..100` default50 | 200 `{resources:[Resource],next_cursor:null\|string}` |
| `GET /api/me/cloud-resources/{id}` | — | 200 `Resource` |
| `POST /api/me/cloud-resources/estimate` | `{connection_id,configuration}` | 200 `{amount:string\|null,currency:string\|null,period,checked_at,source,configuration_hash}` |
| `POST /api/me/cloud-resources` | create ниже + Idempotency-Key | 202 resource ack |
| `POST /api/me/cloud-resources/{id}/replace` | `{confirm:true,expected_revision,configuration?,installation_profile_version_id?,vars?}` + key | 202 ack с ID нового ресурса |
| `DELETE /api/me/cloud-resources/{id}` | X-Confirm-Action delete + key, без тела | 202 ack удаляемого ресурса |
| `GET /api/me/cloud-resources/{id}/ip-options` | — | 200 `{resource_id,options:[{id,label,estimated_charge,currency}],supported,reason}` |
| `POST /api/me/cloud-resources/{id}/change-ip` | `{confirm:true,expected_revision,subnet_id?}` + key | 202 ack той же VPS, без fallback create |

```json
{"connection_id":7,"configuration":{"name":"pool-node","cpu":"AMD Ryzen 9 7950X","cores":2,"ram":4,"disk":40,"diskType":"SSD NVMe","os":"ubuntu-24.04","location":"DE","period":"1m","networkSpeed":"1gbps"},"installation_profile_version_id":12,"vars":{"NODE_NAME":"pool"},"management":{"mode":"manual"},"confirm":true}
```

Configuration — строгий provider-discriminated объект, provider берётся из
connection. Nodehost поля/единицы из [02](02-provider-api.md), password не
принимается; Beget: `display_name,hostname,configuration_id|configuration_params,
image_id,ssh_keys,region`, один вариант тарифа. Validate catalog availability,
типы, bounds, SSH key scope, vars schema до резерва/покупки. Иные provider
поля отвергаются. Installation version только `format:text`, доступная
опубликованная admin или своя; 404 для невидимой, 422 для graph вместо text.

`management` обязательно с `mode:manual|profile`; manual не принимает profile
поля. Profile требует `profile_version_id` формата graph и `inputs` по
версии; bind выполняется после initial installation/admission. Повторная
initial покупка из графа в этом случае запрещена: start run с entry `observe`.
Отсутствующий management в новом create — 400, UI всегда явно шлёт manual.

Replace может опустить configuration/install version/vars: берётся pinned
snapshot текущего ресурса, не latest профиля. Новый ресурс в той же connection;
для смены connection нужен отдельный create. Management переносится при
switch CAS. Revision проверяется по current Resource; параллельные delete/
change-ip/replace взаимоисключены. Beget `subnet_id` запрещён; Nodehost
subnet_id integer из options транслируется в `subnetId`.

Resource DTO:

```json
{"id":81,"connection_id":7,"provider":"nodehost","external_id":"srv_example","node_id":"pool-abc12","state":"admitted","revision":4,"current_ip":"203.0.113.10","managed_ipv4":["203.0.113.10"],"replaces_resource_id":null,"replaced_by_resource_id":null,"retirement":null,"active_operation_id":null}
```

Список не вызывает paid calls; sync inventory только read. Наличие external
resource в аккаунте не даёт права «импортировать» его как managed без
отдельного контракта (в v1 отсутствует). IP-options для Beget возвращает
`supported:false,reason:"primary_ip_transition_unverified"` до gate.

## 5. Management policy ноды

`GET /api/me/nodes/{node_id}/management` → 200:

```json
{"mode":"manual","profile_version_id":null,"inputs":{},"process_current_ban":false,"revision":1,"current_resource_id":81,"active_run_id":null}
```

`PUT /api/me/nodes/{node_id}/management`:

```json
{"mode":"profile","profile_version_id":24,"inputs":{"connection_id":7,"configuration_ref":"cfg_81","installation_profile_version_id":12},"process_current_ban":false,"expected_revision":1}
```

200 тот же DTO с новой revision; manual тело
`{"mode":"manual","expected_revision":2}`. `process_current_ban` по
умолчанию false, не permission на неизвестные исторические эпизоды. True
привязывает только текущий durable episode, в UI требует явного согласия.
Inputs не могут переносить чужую connection/config/server/secretref.
Политика доступна владельцу даже после candidate prune, через persistent
ownership lookup. 409 `revision_conflict`; неподходящий provider/версия/inputs
422 `profile_incompatible`. Semantics in-flight описаны в [01](01-requirements.md).

## 6. Profiles, validation, runs и operations

Существующий text CRUD сохраняет `steps` и `vars_schema`; предложение:
добавить `format:"text"|"graph"` (default text). Graph payload вместо steps
содержит `blueprint` по [schema](05-blueprint-profiles.md). Одновременно
steps/blueprint запрещены. PUT создаёт новую immutable версию, с
`expected_version` (CAS, 409) для graph; метаданные профиля отдельно не меняют
содержимое старых версий. Старые text callers сохраняют совместимый режим.
POST201 / PUT200 / DELETE204 / copy201 существующего CRUD остаются.
Admin publish — существующее поле published в panel CRUD; graph нельзя
publish без validator и необходимых capability gates. Частный пользователь
не может публиковать глобально. Copy доступного профиля создаёт частную
копию содержимого, но не копирует чужие credentials/inputs/ownership.
DELETE профиля, на который есть новые pinned run/policy, даёт 409
`profile_in_use`; исторические версии не стираются.

| Предлагаемый маршрут | Вход | Успех |
|---|---|---|
| `GET /api/me/blueprint-blocks` | — | 200 `{schema_version:1,blocks:[{type,ports,params_schema,effects,retry_class,capabilities}]}` |
| `GET /api/me/profiles/{id}/versions/{version}` | version положительное число, не `latest` | 200 `{profile_id,version,profile_version_id,format,steps?,blueprint?,vars_schema}` |
| `POST /api/me/profiles/validate` | `{format:"graph",blueprint,inputs?}` | 200 `{valid:boolean,diagnostics:[{node_id?,port?,code,message}]}`; ничего не сохраняет/исполняет |
| `POST /api/me/profile-runs` | `{profile_version_id,entry:"initial"\|"observe",resource_id?:integer,inputs,vars?,confirm:true}` + key | 202 `{operation_id,run_id,state:"pending",resource_id:integer\|null}` |
| `GET /api/me/profile-runs/{id}` | — | 200 `Run` ниже |
| `POST /api/me/profile-runs/{id}/cancel` | `{expected_revision}` + key | 202 run ack, может уже быть `cancel_requested` в detail |
| `POST /api/me/profile-runs/{id}/resume` | `{expected_revision,checkpoint_id,decision:"reconcile"\|"retry_safe"\|"acknowledge"}` + key | 202 run ack либо 409 `unsafe_resume` |
| `GET /api/me/operations/{id}` | новый detail рядом с существующим list | 200 `{id,kind,state,resource_id,run_id,last_error,steps,revision}` |

Run DTO:

```json
{"id":91,"operation_id":301,"profile_version_id":24,"entry":"observe","state":"waiting_event","revision":3,"resource_id":81,"policy_revision":2,"waiting":{"type":"ip_ban","after_episode_id":"ep_example"},"checkpoints":[],"last_error":null}
```

`vars` initial запуска проверяются по vars_schema pinned text installation
version, omission использует только её defaults. Observe не принимает vars:
используется сохранённый installation snapshot текущего ресурса. При
включении policy на существующей ноде этот snapshot также является источником
vars; никакой загрузки defaults новой latest версии при replacement.

`entry:observe` требует resource_id и действующую PROFILE policy с точно
этой версией/inputs; initial не принимает существующий resource_id и создаёт
policy после initial admit. Только один active run на policy. `confirm:true`
согласует именно выбранный blueprint и его bounded actions. У disabled
пользователя и при выключенных user profiles новые private runs запрещены;
действующие private runs при выключении приостанавливаются до dispatch.
Admin published profiles не зависят от разрешения private authoring, но
всегда зависят от active owner и policy revision.

Resume `reconcile` разрешает только чтение; `retry_safe` — только блок из
allowlist с доказанным safe retry. `acknowledge` подтверждает просмотр
checkpoint и оставляет needs_attention, если postcondition не доказано.
Ни одно decision не переигрывает arbitrary SSH или неизвестную покупку.
Cancel запрещает будущие effects, но не означает refund/освобождение слота.

## 7. Общая таблица отказов

| HTTP / code | Семантика |
|---|---|
| 400 `validation_failed`, `confirmation_required`, `idempotency_key_required` | Ошибка формы/типов/подтверждения |
| 401 `unauthorized` | Нет действующей session |
| 403 `csrf_invalid`, `user_disabled`, `quota_exceeded`, `user_profiles_disabled` | Проверка прав/политик до effect |
| 404 `not_found` | Нет объекта или ownership/visibility |
| 409 `revision_conflict`, `resource_busy`, `connection_in_use`, `connection_busy`, `idempotency_conflict`, `unsafe_resume`, `unmanaged_ip_attached` | Конфликт, не повторять с новым key вслепую |
| 422 `profile_invalid`, `profile_incompatible`, `provider_auth_failed`, `provider_2fa_required`, `capability_unavailable` | Семантически невыполнимый запрос до принятия |
| 429 `rate_limited` | Бounded retry по Retry-After |
| 502 `provider_error` / 504 `provider_timeout` | Только синхронное безопасное чтение/verify/estimate |
| 503 `control_unavailable` | Fail-closed, не отправлять provider mutation |
| 500 `internal` | Безопасное сообщение, детали только в redacted server log |

После 202 ошибки provider (включая 402/403/409/429/200 business error)
записываются в detail как safe `{code,message}`, а не меняют уже отданный
HTTP ответ. `insufficient_funds` — pause, `unknown` — reconcile; не выдавать
пользователю ложный «повторите покупку».
