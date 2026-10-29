# implementation-summary-addendum.md — profiles, invites N, selection, VPS-поток

Ветка `dev`. Реализация `docs/specs/addendum-profiles-vps.md` (авторитетный
контракт) поверх `invitations-migration-contract.md` и `beget-provisioning.md`.
Незакоммиченная работа других задач не тронута; коммит/пуш не выполнялись.

## Статус по пунктам

| # | Пункт | Статус | Ключевые файлы |
|---|---|---|---|
| 1 | `selection.masters_per_domain` в редактируемом конфиге + hot-apply | ✅ | `internal/server/config_editor.go`, `internal/server/deps_config.go` |
| 2 | Приглашение на N регистраций (max_uses/used_count, атомарный consume, статус) | ✅ | `internal/control/control.go`, `invites.go`, `internal/server/invites_admin_api.go` |
| 3 | Профили: admin CRUD + версии, user CRUD (только свои), max_user_profiles | ✅ | `internal/control/profiles.go`, `internal/server/profiles_admin_api.go`, `profiles_user_api.go` |
| 4 | Песочница playbook (на сохранении И перед запуском) | ✅ | `internal/ansible/sandbox.go`, `internal/ansible/runner.go`, `internal/provision/provision.go` |
| 5 | VPS-поток: каталог+цена, раннер, `[provisioning]`, lifecycle, vars | ✅ | `internal/beget/client.go`, `internal/provision/provision.go`, `internal/server/{cabinet_beget.go,users_control.go,run_main_impl.go}` |
| 6 | Штатный профиль `sharedd-node` (published) | ✅ | `internal/control/profiles.go` (`EnsureDefaultProfile`) |

## 1. selection

- `editableSelection{MastersPerDomain}` в `editableConfig.Selection`; nil-секция =
  не трогать; валидация 1..8 (`maxMastersPerDomain`), 0/9 → 400.
- `GET /panel/api/config` отдаёт эффективное значение (`resolveMastersPerDomain`).
- `PUT` под `cfgMu` пишет `r.cfg.Selection.MastersPerDomain` — селекция читает его
  на каждом тике, рестарт не нужен.

## 2. Приглашения

- Схема: `max_uses INTEGER NOT NULL DEFAULT 1`, `used_count INTEGER NOT NULL DEFAULT 0`
  (+ `ensureColumn` для существующих БД).
- `CreateInvite(ttlHours, maxUses, now)`; admin-API: `max_uses` 1..1000, дефолт 1,
  ответ содержит `max_uses`.
- Consume: `UPDATE ... SET used_count=used_count+1 ... WHERE ... AND used_count<max_uses`;
  гонка не превышает лимит. `classifyInviteTx` → исчерпание = `ErrInviteUsed`
  (код `invite_used`, отдельного `invite_exhausted` нет).
- `Invite.Status` → `active|exhausted|expired|revoked`; список отдаёт
  `max_uses/used_count/status`.
- Отзыв: `used_count < max_uses` (исчерпанное терминально).

## 3. Профили

- `control`: `AllProfiles`, `ProfilesByOwner`, `OwnedProfile`, `DeleteProfile`
  (версии удаляются первыми — FK), `SetProfilePublished`, `UpdateProfileDescription`,
  `CountUserProfiles`.
- Admin API: `GET/POST /panel/api/admin/profiles`, `PUT/DELETE /{id}`. Правка
  playbook = новая запись в `profile_versions` (история не перезаписывается) и
  сброс `published`.
- User API: `GET/POST /api/me/profiles`, `PUT/DELETE /{id}` — только свои (чужой
  → 404), лимит `max_user_profiles` (дефолт 5), `published=false`, флаг
  `user_profiles_enabled` (дефолт true) → иначе 403 `user_profiles_disabled`.
  Выключенный флаг отвергает и создание/правку, и выбор своего неопубликованного
  профиля при создании VPS (`selectableProfileVersion`; опубликованные
  admin-профили не затрагиваются). Решение владельца — включено по умолчанию.
  Остаточный риск честно: шаги исполняются как root на **своём** VPS пользователя;
  владелец VPS и так может всё на своей машине; креды/секреты реестра локально не
  исполняются и в SSH-сессию не инъектятся, из неё недостижимы.

## 4. Песочница

- `ansible.ValidatePlaybook`: YAML-парсинг (yaml.v3), ровно один play, запрет
  `hosts: localhost|127.0.0.1|::1|0.0.0.0|0.0.0.0|host регистратора` (строкой,
  списком, через запятую), текстовый поиск `connection: local`, `delegate_to`,
  `local_action`, `delegate_facts`, `add_host`.
- Вызывается: (а) на сохранении (`profiles_*_api.go` → 400 `playbook_rejected`);
  (б) перед запуском (`provision.ApplyProfile` + дублирование в `Runner.Run`,
  который пишет текст в temp-файл 0700 внутри `playbooks_dir`).
- Потолок playbook ≤ 64 KiB.

## 5. VPS-поток

- Бегет: `GetConfigurations(ctx)` (`ConfigurationInfo` с `Price *float64`);
  `GET /api/me/beget/regions` → `{regions, configurations}`; цена `null` когда
  провайдер её не отдал (не 0).
- `[provisioning]`: `playbooks_dir` (дефолт рядом со state, создаётся 0700),
  `max_user_profiles` (5), `install_timeout_min` (20), `user_profiles_enabled`
  (true). `setupProvisioning` строит `ansible.New(dir, "")`, ставит таймаут,
  создаёт штатный профиль.
- `Provisioner.Provision`: раннер-гейт (нет раннера → durable операция `failed`
  БЕЗ платного заказа) → `Create` (ровно один `CreateVps` на idem_key) →
  `GetInfo` → IP → `ApplyProfile` → `installing` → `done|failed` (вывод обрезан
  в `last_error`).
- `handleMeVps`: `profile_id=0` → штатный `sharedd-node`; `AuthorizeNode`
  (место + владелец для node_secret); vars инъектируются `registry_url`
  (panel.public_url) и `node_secret` (привязан к node_id); vars валидируются
  после инъекции; приватный ключ расшифровывается в temp-файл 0600.

## 6. Штатный профиль

`EnsureDefaultProfile()` идемпотентно создаёт published-профиль `sharedd-node`
(закреплённая версия telemt + агент sharedd, конфиг с `authkey`/`api_addr`,
systemd, права). Playbook проходит песочницу (проверяется перед установкой
раннера).

## Обязательные тесты (contract §5)

| # | Требование | Тест | Статус |
|---|---|---|---|
| 1 | selection через PUT меняет слоты; 0/9 → 400 | server: `TestConfigEditorSelectionMastersPerDomain` | ✅ |
| 2 | max_uses=10: 10 успешных, 11-я `invite_used`; 12 параллельных → 10 | control: `TestInviteMultiUseExactLimit`, `TestInviteMultiUseConcurrentCap` | ✅ |
| 3 | admin CRUD+версии; user только свои; чужой → 404 | server: `TestAdminProfilesCRUDAndVersions`, `TestUserProfilesScopedToOwn` | ✅ |
| 4 | песочница отклоняет на сохранении И перед запуском; валидный принят | ansible: `TestValidatePlaybook*`; provision: `TestApplyProfileSandboxRejectsBeforeRun`; server: `TestAdminProfilesCRUDAndVersions` | ✅ |
| 5 | один `CreateVps` на idem_key; таймаут→unknown без повтора; `ApplyProfile` один раз; нет раннера → failed без заказа | provision: `TestProvisionAppliesProfileOnce`, `TestProvisionTimeoutUnknownNoInstall`, `TestProvisionNoRunnerFailsWithoutOrder` (+ существующие idempotency/timeout) | ✅ |
| 6 | регионы+тарифы; нет цены → `null` | server: `TestBegetCatalogPricingNull` | ✅ |
| 7 | `testvar` через extra-vars; `ansible_host`/`hosts`/`connection` отклоняются на сохранении И перед запуском; значение ≤4096, >64 → ошибка | ansible: `TestRunPassesExtraVarsJSON`; control: `TestProfileVarsMergeAndLimits`; provision: `TestApplyProfileVarsAndIdempotency`, `TestMaskSecretValues` | ✅ |
| 8 | предсозданная личность: агент с выданным `node_secret` регистрируется e2e; retry-install без платных вызовов; таймаут → `waiting_vps` без второго заказа | server: `TestCabinetPreProvisionedIdentityRegister`, `TestCabinetRetryInstallNoPaidCall`; provision: `TestRetryInstallZeroCreateVps`, `TestRetryInstallRefusals`, `TestProvisionReadyTimeoutWaitingVps` | ✅ |

Существующие тесты, затронутые изменением контракта, обновлены:
`control/invites_test.go` (сигнатура `CreateInvite`), `server/cabinet_api_test.go`
(статус операции `done` вместо `confirmed`; мок-раннер; `GetInfo`/
`GetConfigurations` в `fakeBeget`; сигнатура `CreateInvite`),
`server/invites_migration_test.go` (статус `exhausted` вместо `used`),
`provision/provision_test.go` (`GetInfo` в моке).

## Верификация

```
cd registry:
  gofmt -l .                         → пусто
  go vet ./...                       → ok
  go test -race -count=1 ./...       → all ok
  go build -mod=readonly ./...       → ok
  golangci-lint run ./...            → 0 issues
```

## Отклонения / что не сделано (честно)

1. **authkey в vars**: пользовательский authkey хранится только хешем и нечитаем.
   Автоматическую ротацию (инвалидировала бы ключи других нод) не делаем —
   `authkey` прокидывается из `vars` запроса, если передан; регистрация агента
   обеспечивается персональным `node_secret` (инъектируется всегда).
   Штатный профиль поэтому не требует `authkey` (schema: поля необязательны).
2. **Поток синхронный**: `POST /api/me/vps` ждёт `Create`+`ApplyProfile` и
   возвращает финальное `done|failed`; фонового «дожатия» установки нет.
   `install_timeout_min` ограничивает прогон.
   `// ponytail: sync flow; add async worker if the POST latency matters`.
3. **Ожидание `RUNNING`**: ✅ закрыто §7.3 — `Provisioner.waitRunning` опрашивает
   `GetInfo` ограниченно (`vps_ready_timeout_min`, дефолт 10; интервал
   `vps_poll_interval_sec`, дефолт 10с); таймаут → `waiting_vps` (место занято,
   нового заказа нет). Готовность = статус `running/active/started/online/ready`
   ИЛИ пустой статус (совместимость с провайдерами без `status`).
4. **`AuthorizeNode` до заказа**: слот места резервируется до `Create`; при
   провале провайдера место остаётся занятым (нода в `node_owners`) до
   `release-node` — сознательный в пользу контракта §4 («ошибка не создаёт новый
   заказ»); не освобождается автоматически.
5. **Точный endpoint каталога Beget** (`/v1/vps/configuration`) взят по аналогии;
   в тестах транспорт мокан, на реальном API формат ответа нужно подтвердить.
6. **UI** (`ui/pages/**`) для новых полей/эндпоинтов не трогал — только backend
   + `docs/api-contract.md` (§8).
7. **Установщик** (`scripts/install_registry.sh`) в этой задаче не менялся:
   ключи `[provisioning]` имеют Go-дефолты, так что работают без правки TOML;
   идемпотентная домиграция `playbooks_dir`/`max_user_profiles`/
   `install_timeout_min`/`user_profiles_enabled` в установщик — при желании
   отдельным шагом.

---

## Догон: §6 переменные и §7 обязательные фиксы (эта задача)

| # | Пункт | Статус | Ключевые файлы |
|---|---|---|---|
| §6 | Таблица переменных профиля (`vars_schema` = `{"vars":[{name,default,description,secret}]}`), extra-vars, дефолты, лимиты/запрет имён, маскирование секретов | ✅ | `internal/control/profiles.go`, `internal/ansible/runner.go` (без изменений), `internal/provision/provision.go`, `internal/server/cabinet_beget.go` |
| §7.1 | Предсозданная личность: node_id + node_secret + owner-binding + резерв слота ДО платного вызова; `/register` принимает секрет; не логируется/не отдаётся | ✅ (было; покрыто тестом) | `internal/server/cabinet_beget.go` (`provisionVars`), `internal/control/nodes_secret.go`, `internal/server/api_auth.go` |
| §7.2 | `POST /api/me/operations/{id}/retry-install`: переиспользует `provider_vps_id`, ноль `CreateVps`, отказ при нет VPS / активна / `done`/`deleted` | ✅ | `internal/provision/provision.go` (`RetryInstall`), `internal/server/cabinet_beget.go`, `internal/server/cabinet_api.go` |
| §7.2 | `operations.profile_id`: профиль сохраняется при создании и повторяется `retry-install` (не подменяется дефолтом); отдаётся в operation JSON | ✅ | `internal/control/control.go`, `internal/control/operations.go`, `internal/provision/provision.go`, `internal/server/cabinet_beget.go` |
| §7.3 | Ограниченное ожидание `RUNNING`; таймаут → `waiting_vps`, место занято | ✅ | `internal/provision/provision.go` (`waitRunning`), `internal/config/config.go`, `internal/control/operations.go` |
| §7.4 | Свой `published=false` профиль выбирается владельцем; чужой неопубликованный → 404 | ✅ | `internal/server/cabinet_beget.go` (`selectableProfileVersion`) |
| §7.5 | admin JSON профиля отдаёт `versions` | ✅ | `internal/server/profiles_admin_api.go`, `internal/control/profiles.go` (`CountProfileVersions`) |

### Детали §6

- `control.ProfileVar`, `ParseProfileVars`, `MergeProfileVars`, `ValidateExtraVars`,
  `ValidateProfileVarsSchema`, `SecretVarNames`, `IsForbiddenVarName`.
- Дефолты объявленных переменных применяются, пользователь переопределяет и
  добавляет свои сверх объявленных. Значения идут Ansible'у как `--extra-vars`
  JSON (существующий раннер; `Request` не менялся).
- Валидация: имя `^[a-zA-Z_][a-zA-Z0-9_]*$`, значение ≤ 4096 символов, ≤ 64
  переменных, суммарный JSON ≤ 32 KiB; запрещены `ansible_*`, `hosts`,
  `connection`, `delegate_to`, `local_action`, `remote_user`, `inventory` — И на
  сохранении (`AddProfileVersion`→`ValidateProfileVarsSchema`), И перед запуском
  (`ApplyProfile`→`ValidateExtraVars`).
- `secret:true` значения (а также `node_secret`/`authkey`/`password`) маскируются
  в выводе прогона (`MaskSecrets` → `last_error`).

### Детали §7

- Новые состояния: `waiting_vps`. Новые sentinel'ы: `provision.ErrAlreadyDone`.
- `provision.New` дефолты `VpsReadyTimeout=10m`, `VpsPollInterval=10s`; сервер
  передаёт значения из `[provisioning]`.
- `RetryInstall` разрешён из `confirmed`/`failed`/`waiting_vps`; `GetInfo`+`Install`
  только, никогда `CreateVps`.
- `RetryInstall` берёт профиль из `operations.profile_id` (сохранён при создании,
  `Provision` кладёт `req.ProfileID = profileID`); переданный `profile_id` —
  fallback только для legacy-операций (`op.ProfileID == 0`). Операция JSON несёт
  `profile_id` (create/retry ответы и `GET /api/me/operations`); деньги-инварианты
  не изменены.
- `/api/me/vps` и `retry-install` используют общий `provisionVars` (инъекция
  свежего `node_secret` + `registry_url`), `selectableProfileVersion`,
  `MergeProfileVars` перед инъекцией и `ValidateExtraVars` после.

### Обновлённые тесты

`control/beget_test.go` (`TestValidateVars` → `TestProfileVarsMergeAndLimits`),
`provision/provision_test.go` (schema в `TestApplyProfileVarsAndIdempotency`,
`mockBeget.infoStatus/infoIP`), `server/cabinet_api_test.go` (`fakeRunner` теперь
запоминает vars). Новые: `server/addendum_fixes_test.go`,
`ansible/runner_test.go::TestRunPassesExtraVarsJSON`,
`provision` retry/readiness/mask тесты. Новые (профиль retry):
`control/control_test.go::TestOpenMigratesOperationsProfileID`,
`provision/provision_test.go::{TestRetryInstallUsesStoredProfile,TestRetryInstallLegacyFallsBack}`,
`server/addendum_fixes_test.go::TestCabinetRetryInstallPreservesProfile`.

### Осталось / риски

1. **Синхронный поток** сохранён: `POST /api/me/vps` ждёт до `vps_ready_timeout_min`
   (дефолт 10м) + прогон установки. `// ponytail: sync flow; add async worker if latency matters`.
2. **Статусы Beget**: `vpsReady` эвристически матчит подстроки; точный набор
   статусов провайдера в реальном API не подтверждён (тесты мокают).
3. **`retry-install` перевыдаёт `node_secret`** (как и первичный create): если
   прошлая установка дошла до записи конфига, но упала позже, агент с прежним
   секретом инвалидируется. Для упавшей установки безопасно; для «переустановки
   поверх живого агента» потребуется явное решение (не в этом контракте).
4. **UI** под новые поля (`versions`, `waiting_vps`, retry-install, vars-таблица,
   `vps_ready_timeout_min`) не обновлён — backend + `docs/api-contract.md`.

---

## Догон: фиксы `docs/specs/review-addendum.md` (эта задача)

Ветка `dev`. Исправлены CRITICAL-1, HIGH-2 и MED-3/4/5/6 из адверсарного
ревью. Коммит/пуш не выполнялись, ничего не откатывалось.

| # | Sev | Фикс | Ключевые файлы |
|---|---|---|---|
| CRITICAL-1 | CRITICAL | Песочница playbook — структурный рекурсивный обход YAML вместо regex | `internal/ansible/sandbox.go` |
| CRITICAL-1 | — | Ремень и подтяжки: `--limit <host>` + `--connection=ssh` в команде | `internal/ansible/runner.go` |
| HIGH-2 | HIGH | Привязка `node_secret` к своей ноде — ВСЕГДА (не зависит от legacy-тумблера) | `internal/server/api_auth.go` |
| MED-3 | MED | `--limit`/`--connection=ssh` (см. выше; закрыт вместе с CRITICAL-1) | `internal/ansible/runner.go` |
| MED-4 | MED | Уменьшение `masters_per_domain` сжимает лишние слоты (+ DNS-набор) | `internal/server/selection_core.go` |
| MED-5 | MED | Освобождение слота при отказе/отмене ДО платного вызова | `internal/provision/provision.go`, `internal/server/cabinet_beget.go` |
| MED-6 | MED | `last_error` без сырого тела/request-id провайдера | `internal/beget/client.go`, `internal/provision/provision.go` |

### CRITICAL-1 — структурная песочница

- `ValidatePlaybook` парсит YAML в `yaml.Node` и рекурсивно обходит ДЕРЕВО
  (`checkNode`/`checkKey`): кавычки вокруг ключа/значения, play-level `vars`,
  вложенные `block`, merge-ключи (`<<`), алиасы — всё видно.
- Отклоняется: любой ключ `connection` со значением != `ssh` (local/smart/…);
  `delegate_to`/`local_action`/`add_host`/`delegate_facts`/`remote_user`/
  `inventory` на ЛЮБОЙ глубине; любой ключ `ansible_*` (особенно в `vars:`);
  `hosts` не равный ровно литералу `all`.
- Сохранены текстовые слои (`connection: local`, список токенов) и
  `guardPlayHosts`/`guardTarget` как defence-in-depth; вызов на сохранении и
  перед запуском не изменился (`ApplyProfile` + `Runner.Run`).
- `Runner.Run` всегда добавляет `--connection=ssh` и `--limit <req.Host>`.

### HIGH-2 — владелец node_secret

- `gateMachineAuth`: для `kind==machineAuthNode` условие `ownerNodeID != nodeID`
  теперь безусловно (было только при выключенном legacy-секрете). Общий
  `node_token` (kind==shared) не затронут.

### MED-5 — освобождение слота

- `ReleaseNode(nodeID)` вызывается: в `handleOperationCancel` после успешного
  CAS `pending→failed`; в `Provisioner.Create` при отказе валидации каталога
  (до `MarkCreateSent`); в `Provisioner.Provision` при отсутствии раннера.
- После отправки платного вызова (`sent`/`unknown`/`waiting_vps`) слот НЕ
  освобождается (fails-safe против двойного заряда).

### MED-6 — маскирование ошибок провайдера

- `beget.APIError.Summary()` — код/сообщение/статус без `Body`/`RequestID`.
  `provision.providerErrMessage` используется при записи `last_error`
  (`settleCreate` validation/unknown, `Remove`).

### Тесты (детерминированные)

- `ansible/sandbox_test.go::TestValidatePlaybookStructuralCounterexamples` —
  кавычки (ключ/значение), `vars: ansible_connection`, вложенный `delegate_to`,
  `add_host`, `connection: smart`, шаблонный/списочный `hosts` → REJECT;
  легитимные плейбуки (в т.ч. `connection: ssh`) → ACCEPT.
- `ansible/runner_test.go::TestRunPassesLimitAndSSHConnection` — `--limit`/
  `--connection=ssh` в аргументах.
- `server/review_addendum_test.go::TestNodeSecretOwnershipEnforcedWithLegacySharedOn`
  — секрет A с `node_id=B` на `/heartbeat` и `/report` → 403 при
  `legacyShared=true`; свой `node_id` авторизуется.
- `server/review_addendum_test.go::TestCancelReleasesQuotaSlot` — отмена
  pending-заказа возвращает `UsedSlots` к прежнему значению.
- `server/review_addendum_test.go::TestMastersPerDomainDecreaseTrimsSlots` —
  `Masters=3`→`1` оставляет один слот (и `AssignmentsSince`).
- `provision/provision_test.go::{TestCreatePreSendFailureReleasesSlot,
  TestCreatePostSendFailureKeepsSlot, TestProviderErrorNotLeakedIntoLastError}`.

### Верификация (эта задача)

```
cd registry:
  gofmt -l .                         → пусто
  go vet ./...                       → ok
  go test -race -count=1 ./...       → all ok
  go build -mod=readonly ./...       → ok
  golangci-lint run ./...            → 0 issues
```

---

## Догон: CRITICAL-1 round 2 — alias-ключи в песочнице (эта задача)

Ветка `dev`. Закрыт переживший обход из `review-addendum.md` «Re-check round 2»:
структурный обход классифицировал ключ по `k.Value`, а у `yaml.AliasNode` это
**имя якоря**, не реальный ключ, поэтому `? &k connection / : ssh / ? *k / : local`
принимался и исполнялся на контроллере. Коммит/пуш не выполнялись.

| # | Sev | Фикс | Файлы |
|---|---|---|---|
| CRITICAL-1 | CRITICAL | Разрешение alias в позиции ключа И значения до классификации | `internal/ansible/sandbox.go` |
| — | (укрепление) | Защита от самоссылающихся алиасов (`tasks: &x [*x]`) | `internal/ansible/sandbox.go` |

### Что изменено

- `resolveAlias(*yaml.Node)` — разворачивает цепочку alias-узлов до якорного.
- В mapping-обходе `k, v := resolveAlias(...), resolveAlias(...)` **до** проверки
  merge-ключа и `checkKey`; `checkKey` также разворачивает `v` сам. Теперь
  `connection`/`delegate_to`/`local_action`/`add_host`/`delegate_facts`/
  `remote_user`/`inventory`/`ansible_*`/`hosts` нельзя спрятать за alias-ключом
  или alias-значением. Merge-key/alias-following поведение сохранено.
- `checkNode` получил `seen map[*yaml.Node]bool`: yaml.v3 принимает
  самоссылающиеся алиасы в узел (`- hosts: all` + `tasks: &x [*x]`), иначе
  прежний обход зацикливался/переполнял стек. Якорные узлы, достижимые
  несколькими путями, проверяются один раз (покрытие не теряется — якорь
  определяется раньше использования).

### Тесты (не вакуумные — проверены на старой логике)

- `TestValidatePlaybookStructuralCounterexamples` (+): точный counterexample
  (`? &k connection / *k` → `local`) → REJECT; alias-ключ `ansible_connection`
  в `vars` (значение якоря `"ansible_conn\x65ction"`, текстовый слой substring
  не ловит) → REJECT; alias-ключ `delegate_to` (`"delegate_\x74o"`) → REJECT;
  дубликат `connection` (`ssh` затем `"local"`) → REJECT (last-wins не
  понижает до local). Легитимный play с alias-значением `connection: *c`
  (якорь `ssh`) → ACCEPT.
- `TestValidatePlaybookCyclicAliasTerminates` — самоссылающиеся алиасы
  возвращаются без зависания/переполнения.
- Все прежние тесты песочницы зелёные.

### Проверено и уже безопасно

- **Document-узел**: `ValidatePlaybook` разворачивает ровно один документ и
  требует единственный play; вложенных документов нет.
- **Элементы sequence (в т.ч. mapping-элементы `tasks`/`block`)**: рекурсия
  `checkNodeSeen` их обходит; alias-элемент разворачивается.
- **Составные ключи** (ключ — mapping/sequence): `k.Value == ""` → политика
  `default` (не ключевое слово Ansible), значения всё равно обходятся; спрятать
  строковый keyword невозможно.
- **`hosts`/`connection` со не-scalar значением**: теперь разрешаются; иначе
  отклоняются (fail-closed).
- **Много-документный YAML**: отклоняется `ansible-playbook` 2.21 (см.
  review-addendum); песочница и так требует ровно один play.

### Верификация (round 2)

```
cd registry:
  gofmt -l .                         → пусто
  go vet ./...                       → ok
  go test -race -count=1 ./...       → all ok
  go build -mod=readonly ./...       → ok
  golangci-lint run ./...            → 0 issues
```

---

## Догон: CRITICAL round 3 — шаблонные ключи + раннер-бэкстоп (эта задача)

Ветка `dev`. Закрыт переживший обход из `review-addendum.md` «Re-check round 3»:
Ansible **шаблонизирует ключи аргументов `set_fact` во время исполнения**, поэтому
ключ, собирающийся в `ansible_connection` (`{{ 'ansible_' ~ 'connection' }}` или
`{{ k }}` с extra-var `k=ansible_connection`), невидим для проверки литеральных
ключей, но исполняется Ansible → смена транспорта на `local` → выполнение на
контроллере (регистраторе) от root. `--limit`/`--connection=ssh` это НЕ сдерживают.
Коммит/пуш не выполнялись.

| # | Sev | Фикс | Файлы |
|---|---|---|---|
| CRITICAL | CRITICAL | Раннер всегда добавляет `-e ansible_connection=ssh` ПОСЛЕДНИМ `-e` (после пользовательских vars) — высший приоритет, бьёт `set_fact` и явный `connection: local` | `internal/ansible/runner.go` |
| CRITICAL | CRITICAL | Песочница отклоняет любой mapping-ключ с `{{`/`{%` на любой глубине (ключ обязан быть литеральным скаляром) | `internal/ansible/sandbox.go` |
| MEDIUM | MEDIUM | Песочница отклоняет загрузчики чужих plays/roles/файлов: `import_playbook`, `roles`, `include_tasks`, `import_tasks`, `include_role`, `include_vars` | `internal/ansible/sandbox.go` |

### Что изменено

- `runner.go`: после пользовательского `-e <json>` добавлен
  `args = append(args, "-e", "ansible_connection=ssh")`; playbook appended
  последним. Extra-vars — источник наивысшего приоритета: `ansible_connection`
  из `-e` перебивает и runtime `set_fact`, и play-ключ `connection`. Так как
  пользовательские vars идут первым `-e`, later-wins не даёт их переопределить.
  `--connection=ssh` и `--limit req.Host` сохранены как ремень и подтяжки.
- `sandbox.go` (`checkNodeSeen`, mapping-цикл): перед `checkKey` добавлены
  (1) `k.Kind != yaml.ScalarNode` → REJECT (не-скалярный ключ прячет структуру);
  (2) `strings.Contains(k.Value, "{{") || strings.Contains(k.Value, "{%")` →
  REJECT. Это закрывает и точный counterexample, и variant через extra-var-ключ.
- `checkKey`: в список запрещённых добавлены `import_playbook`, `roles`,
  `include_tasks`, `import_tasks`, `include_role`, `include_vars`.
- `nodeKind(*yaml.Node)` — helper для сообщений об ошибке.
- Существующие проверки (aliases/merge/`seen`-cycle-guard/forbidden names) не тронуты.

### Тесты (детерминированные)

- `TestValidatePlaybookStructuralCounterexamples` (+) — REJECT: точный round-3
  counterexample (`set_fact: "{{ 'ansible_' ~ 'connection' }}": local` с
  последующим `command: touch /tmp/pwned`); variant `set_fact: "{{ k }}": local`
  (ключ из extra-var); шаблонный ключ play-level и во вложенном block;
  `import_playbook`/`roles`/`include_tasks`/`import_tasks`/`include_role`/
  `include_vars`. ACCEPT: легитимный play с **шаблонным значением**
  (`command: "echo {{ testvar }}"`, `msg: "{{ msg }}"`) — ограничены только ключи.
- `runner_test.go`: `TestRunPinsAnsibleConnectionViaExtraVars` — команда содержит
  ровно два `-e`, последний `ansible_connection=ssh` и стоит ПОСЛЕ
  пользовательского JSON-blob; `--limit <host>` и `--connection=ssh` на месте.
  `TestRunPinsAnsibleConnectionWithoutVars` — бэкстоп есть и без vars.
  `TestRunPassesExtraVarsJSON` обновлён: читает ПЕРВЫЙ `-e` (пользовательские vars),
  т.к. последний теперь — бэкстоп.

### Верификация (round 3)

```
cd registry:
  gofmt -l .                         → пусто
  go vet ./...                       → ok
  go test -race -count=1 ./...       → all ok
  go build -mod=readonly ./...       → ok
  golangci-lint run ./...            → 0 issues
```



---

## Догон: профили — шаги по SSH вместо Ansible (эта задача)

Ветка `dev`. Решение владельца: **Ansible убран полностью**. Профиль — это
упорядоченный список shell-команд, выполняемых по SSH на VPS пользователя
(формат mtplane). Незакоммиченная работа не откатывалась; коммит/пуш не выполнялись.

| # | Изменение | Ключевые файлы |
|---|---|---|
| 1 | Порт `sshx` (context-aware, таймауты, без глобального состояния; Target/Auth/Run/WaitSSH) | `internal/sshx/sshx.go` (+`sshx_test.go`) |
| 2 | `ParseProfileSteps` + `RenderStep` + `ShellQuote`: text/template, КАЖДОЕ значение shell-квотится один раз | `internal/control/steps.go` (+`steps_test.go`) |
| 3 | Переменные: таблица `vars_schema` (name/default/description/secret), дефолты + overrides/extra; лимиты (имя, ≤4096, ≤64); секреты маскируются | `internal/control/profiles.go`, `internal/provision/provision.go` |
| 4 | Выполнение шагов по SSH: креды владельца из Beget-связки (ключ/пароль, расшифровка master.key), таймаут шага, обрезанный вывод, abort на первом падении | `internal/provision/provision.go`, `internal/server/cabinet_beget.go` |
| 5 | Удалён `internal/ansible` (+тесты), `SetRunner/PlaybookRunner`, ключ `[provisioning] playbooks_dir`, песочница YAML | `internal/{ansible,provision,server,config}` |
| 6 | Деньги-инварианты сохранены: один `CreateVps` на idem_key; retry без покупок; `waiting_vps` держит слот; предсозданная личность (`node_id`+`node_secret`) в шагах | `internal/provision/provision.go` |
| 7 | Штатный профиль `sharedd-node` — steps-профиль (telemt + агент + фиксация node_id, регистрация по node_secret) | `internal/control/profiles.go` |
| 8 | go.mod: `gopkg.in/yaml.v3` больше не требуется | `registry/go.mod` |

### API-переименование

Поле `playbook` → `steps` в JSON профилей (admin/user) и в моделях. Колонка БД
оставлена исторической (`profile_versions.playbook`) — см. комментарий в
`control/profiles.go`; переименование потребовало бы миграции существующих баз.

### Встроенные переменные (инъектирует сервер)

`node_id`, `node_secret`, `node_token` ([security] node_token), `registry_url`
(panel.public_url). Объявленные профилем: `port` (default 443), `authkey`,
`api_addr`. Все значения shell-квотируются при подстановке; секретные
(`node_secret`/`node_token`/`authkey`/`password` + `secret:true`) маскируются в
`last_error`.

### Тесты (детерминированные, без сети)

- `control/steps_test.go`: парсинг (заголовки, многострочные команды, shell-
  комментарий `#безпробела`), shell-квотирование враждебных значений (пробелы,
  кавычки, `$(...)`, бэктики, `\n`, `;`), built-in vs custom переменные,
  отсутствующая переменная → `''` (не `<no value>`).
- `provision/provision_test.go`: abort на первом упавшем шаге, маскирование
  секретов, retry-install → НОЛЬ `CreateVps`, нет SSH-кредов → `failed` без
  платного заказа, таймаут готовности → `waiting_vps`.
- `server/addendum_fixes_test.go`: `node_secret` инъектируется в шаги (извлекается
  из прогнанной команды) и принимается на `/register`; retry сохраняет профиль.
- `sshx/sshx_test.go`: `Auth.Empty`/`Target.Empty`, отказ на пустых/битых кредах.

### Верификация (эта задача)

```
cd registry:
  gofmt -l .                         → пусто
  go vet ./...                       → ok
  go test -race -count=1 ./...       → all ok
  go build -mod=readonly ./...       → ok
  golangci-lint run ./...            → 0 issues
bash scripts/build_ui.sh -check      → без изменений
rtk grep -rn ansible registry/ ui/ scripts/ → пусто
```

### UI (сделано минимально)

Поле профиля переименовано в «Шаги (shell-команды)» (textarea вместо YAML),
подсказки про Ansible/песочницу/запрещённые имена убраны, текст про
`{{ var }}` описывает серверную shell-квотированную подстановку. Отдельная
доработка UI не требуется для сборки; поведение/контракт описаны в §8 API.

### Осталось / риски

1. **Извлечение `node_secret` в тесте** — по regex из прогнанной команды
   (`--node-secret='…'`): тест завязан на форму шага штатного профиля. Это
   проверка реальной подстановки, но при смене штатных шагов regex нужно
   обновить.
2. **Хост-ключ VPS не пинится** (`ssh.InsecureIgnoreHostKey`, как в mtplane):
   VPS создан только что, known_hosts для него нет. Пининг — при появлении
   долгоживущего инвентаря.
3. **Точные URL штатных шагов** (telemt-инсталлятор, `install_node.sh`) не
   проверяются офлайн; при смене upstream — правка профиля.

---

## Догон: фиксы `docs/specs/review-ssh-steps.md` (H1–H4, M5/M6/M8, L9/L11)

Ветка `dev`. Локальные правки SSH-пути шагов; коммит/пуш не выполнялись,
ничего не откатывалось. Устаревшие утверждения выше (синхронный поток,
`node_token` в vars, retry перевыдаёт секрет, только `{{.var}}`) — заменены
ниже.

| # | Тема | Фикс | Ключевые файлы |
|---|---|---|---|
| H1 | весь Provision под 30с ctx → таймауты 10м/20м мертвы | платный заказ синхронно (pending ДО вызова), ожидание `RUNNING` + install-шаги — фоновым воркером со своим бюджетом (`vps_ready_timeout + N×install_timeout`); guard по operation id; shutdown отменяет воркеры | `internal/server/provision_worker.go`, `internal/server/cabinet_beget.go`, `internal/provision/provision.go` (`InstallOnConfirmed`, `ProvisionBudget`) |
| H2 | документированный `{{ var }}` не работал | `RenderStep` поддерживает ОБА синтаксиса: переменная регистрируется функцией (`{{ var }}`) И картой (`{{.var}}`), значение квотится ровно один раз | `internal/control/steps.go` |
| H3 | многострочный/пайповый шаг → ложный `done` | каждый шаг исполняется `bash -eo pipefail -s` (тело на stdin); штатный профиль завершается verify-шагом (`systemctl is-active` + reachability `/healthz`) | `internal/provision/provision.go` (`runSteps`), `internal/control/profiles.go` |
| H4 | утечка слота квоты после `AuthorizeNode` | вся валидация (vars, ssh-креды) — ДО `AuthorizeNode`; после резерва ошибка → `ReleaseNode` через defer | `internal/server/cabinet_beget.go` |
| M5 | общий `node_token` в пользовательских шагах | `node_token` в vars НЕ инъектится; штатный профиль использует персональный `node_secret` как Bearer | `internal/server/cabinet_beget.go` (`provisionVars`) |
| M6 | retry перевыдаёт `node_secret` | retry выдаёт секрет ТОЛЬКО если его ещё нет (`rotateSecret=false`); существующий хеш сохраняется | `internal/server/cabinet_beget.go`, `internal/control/nodes_secret.go` (`NodeHasSecret`) |
| M8 | штатный профиль не передавал `--name` | добавлен `--name={{ node_id }}` (плейн-text `{{ var }}`); `install_node.sh`/`install_node_web.sh` принимают полный предсозданный id | `internal/control/profiles.go`, `scripts/install_node.sh`, `scripts/install_node_web.sh` |
| L9 | truncate раньше mask | `truncateOutput(MaskSecrets(...))` | `internal/provision/provision.go` |
| L11 | мусор/устаревшее | комментарий «для Ansible» убран; `go mod tidy` чист (`yaml.v3` — легитимная транзитивная запись via cloudflare-go/testify) | `internal/control/beget.go` |

### Тесты (все на моках, сети нет)

- H1: `server/provision_worker_test.go` (`TestProvisionWorkerGuardsOperation`,
  `TestProvisionWorkerDetachedCtxAndStop`); `provision`
  `TestProvisionBudgetUsesConfiguredTimeouts`.
- H2: `control/steps_test.go` (`TestRenderStepBothSyntaxes`,
  `TestRenderStepBuiltinNamedVar`).
- H3: `provision.TestRunStepsWrapsBashPipefail`,
  `TestApplyProfileFailFastPipeAndMultiline` (реальный bash: `false\ntrue`,
  `false | bash`, verify-fail); `control.TestDefaultProfileStepsContract`.
- H4: `server/review_ssh_steps_test.go::TestMeVpsLateValidationNoSlotLeak`
  (assert `UsedSlots` неизменен на каждом позднем отказе).
- M5: `server::TestProvisionVarsExcludeSharedNodeToken`.
- M6: `server::TestRetryInstallKeepsExistingSecret`.
- M8: `server::TestDefaultProfilePassesExplicitName`.
- L9: `provision.TestMaskSecretsBeforeTruncation`.

### Осталось / риски

1. **`retry-install` + существующий секрет**: плейн-text секрета невосстановим,
   поэтому при уже выданном секрете он НЕ передаётся в vars — установщик на
   повторном прогоне не перезапишет существующий `node_secret` (это и защищает
   живого агента), но для упавшей ДО записи конфига установки пересинхронизировать
   секрет нельзя. Решение — вне этого контракта (например, `--force` для
   `node_secret` при согласии восстановить/пересоздать ноду).
   → **ЗАКРЫТО в DEF-A (см. «Re-check round 2» ниже):** секрет восстановим из
   `node_secret_enc`.
2. **Платный заказ остаётся в HTTP-запросе**: `CreateVps` — один вызов провайдера
   под 30-секундным ctx; долгие части (10м/20м) вынесены в воркер. Если и
   `CreateVps` окажется медленнее 30с — состояние `unknown`, повторов нет.
3. **Хост-ключ VPS не пинится** (как раньше).

## Re-check round 2 — DEF-A..DEF-D (review-ssh-steps.md)

| # | Дефект | Фикс | Файлы |
|---|---|---|---|
| DEF-A | `retry-install` рендерил `--registry-token=''` (секрет хранился только хешем) | восстановимая AES-GCM копия секрета в `node_owners.node_secret_enc` (ensureColumn); `SetNodeSecretWithEnc`/`NodeSecretEnc`; retry возвращает реальный секрет из копии; при отсутствии копии новый выдаётся ТОЛЬКО если нода не регистрировалась | `internal/control/control.go`, `internal/control/nodes_secret.go`, `internal/server/cabinet_beget.go`, `internal/server/run_main_impl.go` |
| DEF-B | verify доказывал «юнит жив», а не факт регистрации | `GET /api/node/status` (личный секрет + `registered`); verify-шаг профиля опрашивает его с ограниченными повторами → провал `/register` = `failed` | `internal/server/api_auth.go`, `internal/server/run_main_impl.go`, `internal/control/profiles.go` |
| DEF-C | рестарт в середине установки → op навсегда `installing`, retry 409 | при старте `resumeInstallingOps` до-подхватывает `installing` без новых платных вызовов; retry из `installing` разрешён, пока `runningOp==false` | `internal/server/cabinet_beget.go`, `internal/server/provision_worker.go`, `internal/control/operations.go`, `internal/server/run_main_impl.go` |
| DEF-D | `provisionConfig` читал cfg без `cfgMu` | чтение `[provisioning]` под `cfgMu.RLock` | `internal/server/cabinet_beget.go` |

**Смежное изменение:** `POST /register` с личным секретом СВОЕЙ ноды больше не
ротирует секрет (идемпотентно) — иначе verify-шаг и retry лишались валидного
токена, а живой агент обесточивался. Ротация остаётся для authkey/общего токена
(контракт §1, docs/api-contract.md §7.2).

### Тесты

- DEF-A: `server::TestRetryInstallRendersRealSecret` — retry рендерит реальный
  `--registry-token='<secret>'`/`--node-secret='<secret>'`, не `''`.
- DEF-B: `server::TestNodeStatusOwnershipAndRegistration` (свой секрет 200, чужой
  403, свой секрет под чужим node_id 403, общий токен 403, без токена 401,
  до регистрации 404); `server::TestInstallResultReflectsRegistration`
  (нет `/register` → `failed`; есть → `done`, `CreateVps==1`).
- DEF-C: `server::TestRetryInstallAllowedFromOrphanedInstalling`,
  `server::TestResumeInstallingOperation` (ноль новых `CreateVps`).
- DEF-D: `server::TestProvisionConfigRaceFree` (под `-race`).
- Контракт профиля: `control::TestDefaultProfileStepsContract` (verify →
  `/api/node/status`).

Прогон: `cd registry && gofmt -l . && go vet ./... && go test -race -count=1 ./...`.

### Остаток / риски (round 2)

1. Legacy-строка без `node_secret_enc`, нода УЖЕ регистрировалась: retry не может
   вернуть секрет и НЕ перевыдаёт (fail-loud 503) — до включения провижининга
   таких строк нет; путь восстановления — пересоздание ноды.
2. `node_secret_enc` требует `master.key`; без ключа копия не пишется, работает
   fallback-правило п.1.
3. Verify-шаг полагается на доступность `registry_url` с VPS (как и прежний
   `/healthz`).
