# implementation-summary-invitations.md — приглашения, node_secret, миграция общего секрета

Ветка `dev`. Реализация docs/specs/invitations-migration-contract.md
(авторитетный контракт) + разделы 5 и 7 docs/specs/invitations-legacy-ui.md
(семантика ошибок для UI). Пункт §4 контракта (идемпотентная
конфиг-автомиграция установщика регистратора) — см. раздел
«Установщик: конфиг-автомиграция» ниже.

## Файлы

### registry (модуль sharedd/registry)

| Файл | Изменение |
|---|---|
| `internal/control/control.go` | Схема: таблицы `invites`, `settings`; ensureColumn `node_owners.node_secret_hash`/`node_secret_prefix`; новые sentinel-ошибки приглашений |
| `internal/control/invites.go` | НОВЫЙ: CreateInvite / Invites (JOIN users → used_by_username) / RevokeInvite (идемпотентен, used терминален) / CreateUserWithInvite (валидация полей → INSERT user → условный UPDATE invite в одной транзакции; классификация причин гонки) / Invite.Status |
| `internal/control/settings.go` | НОВЫЙ: LegacySecretEnabled (чтение + бутстрап-строка из default) / SetLegacySecret (один upsert) |
| `internal/control/nodes_secret.go` | НОВЫЙ: SetNodeSecret (sha256+префикс; пустой секрет = снять хеш) / NodeSecretOwner (hash-lookup) / CountNodesWithoutSecret (счётчик миграции по живым кандидатам) |
| `internal/control/users.go` | Хелпер hashPassword (общий с CreateUser и CreateUserWithInvite) |
| `internal/config/config.go` | `[users] allow_shared_secret` (*bool, nil = true) — только бутстрап |
| `internal/server/api_auth.go` | requireNodeToken → requireNodeAuth: классификация Bearer (общий vs node_secret по sha256-lookup) в контекст; gateMachineAuth — матрица contract §1; legacySharedSecretEnabled; nodeSecretOwner (fail-closed при ошибке БД) |
| `internal/server/run_main_impl.go` | Все 5 machine-маршрутов на requireNodeAuth + гейт; Registry.legacyShared (atomic.Bool); выдача node_secret в ответе `/register` (registerResponse); randomURLSecret; mountInvites |
| `internal/server/users_control.go` | openControl: бутстрап legacyShared из settings (config-дефолт при ошибке); authorizeUserRegister(w, body, machineAuthInfo) — свой node_secret заменяет authkey при перевыпуске |
| `internal/server/healthcheck.go` | Гейт матрицы в /report (после decode, до валидации — 401 раньше 400) |
| `internal/server/invites_admin_api.go` | НОВЫЙ: POST/GET/DELETE `/panel/api/admin/invites`, GET/POST `/panel/api/admin/legacy-secret`, inviteURL из panel.public_url |
| `internal/server/users_admin_api.go` | writeControlErr: маппинг invite-ошибок → 403 с кодами контракта |
| `internal/server/cabinet_api.go` | /api/auth/register: поле `invite`, CreateUserWithInvite |
| Тесты | `internal/control/invites_test.go` (НОВЫЙ: §5.1, §5.2, §5.5); `internal/server/invites_migration_test.go` (НОВЫЙ: §5.3, §5.4, §5.6 + admin-API приглашений/переключателя); `internal/server/cabinet_api_test.go` (существующие тесты саморегистрации переведены на приглашения — открытая регистрация удалена контрактом) |

### node (модуль sharedd/node)

| Файл | Изменение |
|---|---|
| `internal/config/config.go` | Опциональные `[registry] node_secret` (Bearer) и `[registry] authkey` (тело /register) |
| `internal/agent/main.go` | loadNodeSecret (файл → конфиг), persistNodeSecret (/var/lib/sharedd/node_secret, 0600), clearNodeSecret (401), парс `{node_secret}` из 200 /register (старый реестр — молча игнорируем), authkey в payload |
| `internal/agent/registry_api.go` | Bearer: node_secret предпочтительнее общего токена |
| Тесты | `internal/agent/register_test.go`: сохранение выданного секрета (0600, bearer-переключение, старый реестр), 401 → сброс → перевыпуск |

## Auth matrix — заметки по реализации

- Классификация Bearer — один раз на запрос в `requireNodeAuth`: сначала
  sha256-lookup среди node_owners (node_secret → kindNode+owner), иначе
  constant-time сравнение с `[security] node_token` (kindShared), иначе 401.
- Матрица применяется гейтом в хендлере (после decode тела, до валидации —
  401 раньше 400): OFF + kindShared → 401 `shared_token_disabled`, кроме
  `/register` с непустым authkey в теле (валидность проверяет существующий
  authorizeUserRegister → 403 `bad_authkey` при мусоре); OFF + kindNode →
  node_id маршрута обязан совпадать с владельцем, иначе 403
  `  node_id_mismatch`. ON — прежнее поведение + FULL legacy-совместимость на
  `/register`: `authorizeUserRegister` при `kindShared && authkey=="" &&
  nodeAPISecurityEnabled() && legacySharedSecretEnabled()` вызывает
  `authorizeLegacySharedRegister` — новая нода привязывается к системному
  `legacy` транзакционным `AuthorizeNode` (квота `legacy_max_nodes`, перебор →
  403 `quota_exceeded`), нода реального пользователя без authkey → прежний
  403 `bad_authkey`, legacy-нода → прежний пропуск.
- Неизвестный Bearer: `requireNodeAuth` при ON отвечает 401
  `stale_node_secret` (иначе `unauthorized`) — агенту сигнал сбросить
  протухший `node_secret`; принимается по-прежнему только точный общий токен.
  Существующая обработка 401 в агенте (`register` → `clearNodeSecret`, след.
  попытка с общим токеном) уже покрывает восстановление — тест
  `node/internal/agent/register_test.go: TestRegister401ClearsStaleSecret`.
- `/config`: node_id маршрута — заголовок `X-ShareDD-Node-ID` (агент шлёт
  всегда); OFF + node_secret без заголовка → 403 mismatch (fail-closed).
- control == nil (`[users] enabled=false` или БД не открылась): переключателя
  не существует, общий токен работает как раньше; node_secret-lookup мимо.
- Durable-режим: settings-строка + atomic.Bool-зеркало в Registry; POST
  legacy-secret пишет БД первым (неудача → 5xx, зеркало не трогаем),
  применение мгновенное на всех запросах, рестарт перечитывает settings.
  Бутстрап при первом чтении: строка создаётся значением
  `[users] allow_shared_secret` (nil = true); ошибка чтения при старте —
  держим конфиг-дефолт (пул не блокируем), лог.
- Перевыпуск: каждый успешный `/register` генерирует новый секрет
  (SetNodeSecret затирает старый хеш). Ответ без control-БД — прежний (без
  тела; старые агенты не парсят 200-тело).

## Статус обязательных тестов (contract §5)

| # | Требование | Тест | Статус |
|---|---|---|---|
| 1 | Гонка register на одном invite → ровно один аккаунт | control: TestInviteRaceExactlyOneAccount (8 горутин, 1 успех + 7 invite_used) | ✅ |
| 2 | expired/used/revoked → различные коды; поля не расходуют invite | control: TestInviteDistinctRejectionCodes (+ TestInviteDuplicateUsernameRollsBack); server: TestInvitesAdminAPIAndRegistration | ✅ |
| 3 | OFF: общий токен отклонён на 5 маршрутах; node_secret принимается; чужой node_id → 403; перевыпуск по authkey | server: TestLegacyOffAuthMatrix | ✅ |
| 4 | ON: общий токен работает везде | server: TestLegacyOnSharedTokenWorksEverywhere | ✅ |
| 5 | Рестарт: OFF остаётся OFF; бутстрап ON при пустом settings | control: TestLegacySecretSurvivesRestart | ✅ |
| 6 | affected_nodes: с секретом не считается, без — считается | server: TestAffectedNodesCount (+ мгновенное применение OFF) | ✅ |
| 7 | Установщик (идемпотентность/бэкап) | `scripts/install_registry.sh`: migrate_registry_config + `--migrate-config` dry-run (см. раздел «Установщик» ниже) | ✅ |

### Хотфикс: FULL legacy при ON (prod 403 bad_authkey)

При ON валидный общий токен без authkey ранее упирался в 403 `bad_authkey`
для ЛЮБОЙ ноды без записи в node_owners — новый агент не мог
зарегистрироваться, все его отчёты шли 404 `unknown node_id`. Исправлено:
новая нода при ON+общий токен привязывается к `legacy` (квота), а протухший
persistent `node_secret` даёт различаемый 401 `stale_node_secret`.

| # | Требование | Тест |
|---|---|---|
| H1 | новая нода + общий + ON → 200, owner=legacy, секрет выдан; повтор — 200 | `server: TestLegacyOnSharedTokenNewNodeBindsLegacy` |
| H2 | квота `legacy_max_nodes`: перебор → 403 `quota_exceeded`, без вставки | `server: TestLegacyOnSharedTokenQuotaEnforced` |
| H3 | OFF: общий без authkey → 401 `shared_token_disabled` | `server: TestLegacyOffSharedTokenRejectsNewNode` |
| H4 | существующая legacy-нода: перерегистрация неизменна, квоту не тратит | `server: TestLegacyOnExistingLegacyReRegisterUnchanged` |
| H5 | нода реального пользователя без authkey → по-прежнему 403 `bad_authkey` | `server: TestLegacyOnUserOwnedNodeStillNeedsAuthkey` |
| H6 | неизвестный Bearer: ON → 401 `stale_node_secret`, OFF → 401 `unauthorized` | `server: TestUnknownBearerStaleNodeSecretCode` + `agent: TestRegister401ClearsStaleSecret` |

## Установщик: конфиг-автомиграция (contract §4)

### Файлы

| Файл | Изменение |
|---|---|
| `scripts/install_registry.sh` | НОВОЕ `migrate_registry_config <file> [dry]` (awk-only, без yq/jq) + флаг `--migrate-config PATH` (dry-run: diff на stdout, exit 0, до root/systemd-проверок) + вызов в upgrade-ветке `install_registry()` (существующий конфиг, ДО `systemctl restart`) |
| `scripts/install_node.sh` / `scripts/install_node_web.sh` | НОВЫЕ флаги `--authkey` / `--node-secret` (+ `--authkey=` / `--node-secret=` формы): дописывают `[registry] authkey` / `[registry] node_secret` в node.toml, если ключа ещё нет. НОВЫЙ `add_registry_key()` (байт-в-байт одинаков в обоих файлах; в общий конвейер паритета не входит и его не трогает) |
| `configs/node.example.toml` | Комментарии `[registry]`: документация `--authkey`/`--node-secret` и семантики node_secret |

### Алгоритм миграции (install_registry.sh)

`migrate_registry_config` (contract §4, шаги 1–4):

1. **Бэкап** `registry.toml → registry.toml.bak-<unix_ts>` (`cp -a`), хранятся
   последние 3 (`find | sort -r | tail -n +4 | rm`). Если изменений нет —
   ни бэкапа, ни записи (идемпотентный прогон чистый).
2. **Дозапись только отсутствующих ключей** — один проход awk:
   - `[users] enabled = true, legacy_max_nodes = 100000, allow_shared_secret = true`
   - `[database] control_db = "<dir(state.file)>/control.db"`, `master_key_path = "<dir(state.file)>/master.key"` — каталог берётся из `[state] file` самого конфига; нет секции — дефолт `/var/lib/sharedd`
   - `[selection] masters_per_domain = 1`
   - `[provisioning] max_creates_per_day = 5, cooldown_minutes = 30` (дефолты `registry/internal/config/config.go`: `defaultMaxCreatesPerDay`/`defaultCooldownMinutes`)
   - секция существует → ключи дописываются в её конец; секции нет → секция целиком в конец файла
3. **Существующие значения не трогаются**: явное `enabled = false`,
   `legacy_max_nodes = 500`, `masters_per_domain = 4` остаются байт-в-байт
   (ключ считается заданным только по некомментированной строке `key =`).
4. **Идемпотентно**: второй прогон — «изменений нет».

Заголовки секций детектируются TOML-совместимо: перед сравнением снимается
inline-комментарий и пробелы внутри скобок (`[ users ]`, `[users] # c`
распознаются как `users`). Без нормализации валидный, но нетипичный заголовок
считался «секцией отсутствует», `emit_new()` дописывал вторую таблицу того же
имени → невалидный TOML; теперь дубль невозможен.

Формат: комментарии и порядок строк сохраняются (строки копируются как есть);
CRLF-файлы детектируются, новые строки наследуют CRLF. Дефолты совпадают с
резолвером `registry/internal/config` — двойное применение (миграция + Go-дефолты)
не меняет эффективных значений.

### Dry-run транскрипт (`--migrate-config`)

Старый конфиг = `configs/registry.example.toml` как есть (новых секций в нём нет):

```
$ bash install_registry.sh --migrate-config old1.toml
  dry-run --migrate-config: предполагаемые изменения (файл не тронут)
--- old1.toml
+++ old1.toml.mig.A7fGZ1
@@ -108,3 +108,19 @@

 [shared_proxy.users]
 hello = "0123456789abcdef0123456789abcdef"
+
+[users]
+enabled = true
+legacy_max_nodes = 100000
+allow_shared_secret = true
+
+[database]
+control_db = "/var/lib/sharedd/control.db"
+master_key_path = "/var/lib/sharedd/master.key"
+
+[selection]
+masters_per_domain = 1
+
+[provisioning]
+max_creates_per_day = 5
+cooldown_minutes = 30
```

Явные значения сохраняются (`[users] enabled=false`, `legacy_max_nodes=500`,
`[selection] masters_per_domain=4` не тронуты; дописано только
`allow_shared_secret = true` в конец секции `[users]`):

```
@@ -10,5 +10,14 @@
 enabled = false
 legacy_max_nodes = 500

+allow_shared_secret = true
 [selection]
 masters_per_domain = 4
```

### Прогон верификации

| Проверка | Результат |
|---|---|
| `bash -n` на 3 изменённых скриптах | OK |
| `shellcheck -S warning` (v0.10.0) на 3 скриптах | 0 замечаний |
| `bash scripts/check_installer_parity.sh` | `parity ok` × 4 (toml_escape, update_registry_config, detect_proxy_unit, rescue_proxy); `add_registry_key` дополнительно сверен `diff`-ом — байт-в-байт |
| Миграция: вставка секций (конфиг без новых ключей) | все 4 секции в конец, пути от `[state] file` |
| Миграция: существующие секции + явные значения | ключи дописаны внутрь секции; `enabled=false`, `500`, `4` сохранены |
| Миграция: закомментированный `# enabled = false` | ключ добавлен (комментарий ≠ значение) |
| Миграция: CRLF-файл | старые строки `^M$` сохранены, новые строки тоже CRLF; результат валиден (tomllib) |
| Миграция: идемпотентный второй прогон | «изменений нет», новых бэкапов не появляется |
| Миграция: бэкап + хранение 3 | `.bak-<ts>` создан `cp -a`; из 4 старых остались 3 новейших |
| Миграция: `[users] # …` (inline-комментарий) | заголовок распознан; ключи дописаны внутрь `[users]`, дубля таблицы нет |
| Миграция: `[ database ]` (пробелы внутри скобок) | заголовок распознан; `master_key_path` дописан внутрь, дубля нет |
| Миграция: `[selection]  # keep` | заголовок распознан; `masters_per_domain` дописан внутрь, дубля нет |
| TOML-валидность результатов | `python3 tomllib` парсит все прогнанные варианты (включая формы выше), ровно по одной таблице каждой секции, значения совпадают с Go-дефолтами |

Команды прогона (в `/tmp/opencode/migtest`, функция извлекалась из
установщика для реального (не dry) прогона без systemd):

```
cp configs/registry.example.toml old1.toml
bash scripts/install_registry.sh --migrate-config old1.toml        # dry-run, diff
bash harness.sh scripts/install_registry.sh old1.toml              # реальный прогон: бэкап+запись
bash harness.sh scripts/install_registry.sh old1.toml              # идемпотентный второй
python3 -c "import tomllib; tomllib.load(open('old1.toml','rb'))"  # валидность
```

## Верификация

```
cd registry: gofmt-clean; go vet ./...; go test -race ./... (все пакеты ok);
             go build -mod=readonly ./...; golangci-lint run ./... → 0 issues
cd node:     gofmt-clean; go vet ./...; go test -race ./... (все пакеты ok);
             go build -mod=readonly ./...; golangci-lint run ./... → 0 issues
```

Незакоммиченная работа других задач не тронута; коммит/пуш не выполнялись.

## Отклонения от контракта (сознательные, минимальные)

1. **Формат токена**: 32B random как hex-строка (64 симв.), не base64-url —
   hex тривиально urlsafe и переиспользует существующий `randomSecret`.
   Хранение и префикс — как у authkeys (sha256 + первые 8 символов).
2. **Отзыв использованного приглашения**: no-op (204), статус остаётся `used`
   — «used» правдивее «revoked» для потреблённого токена; идемпотентность
   соблюдена.
3. **`/register` при OFF с общим Bearer + authkey в теле**: пропускается
   гейтом (требование §1 «требуется authkey»); невалидный authkey даёт
   существующий 403 `bad_authkey`.
4. **Ответ `/register` всегда JSON** (`{"node_secret":"..."}` или `{}`) при
   user-режиме — тело 200 больше не пустое; агенты (включая старые) тело
   200 не парсят, поэтому legacy-совместимость сохранена.
5. **node_secret не выдаётся при `[users] enabled=false`** (control-БД нет —
   хранить хеш негде); переключатель и матрица OFF в этом режиме также
   неприменимы по определению.

## Риски / что дальше

- UI (`ui/pages/**`) ещё не использует новые эндпоинты — это задача
  invitations-legacy-ui фазы 2 (Phase 2/3 pipeline); бэкенд-контракт готов.
- ~~Установщик: авто-миграция registry.toml~~ — готово (раздел
  «Установщик: конфиг-автомиграция»); `allow_shared_secret` в существующих
  конфигах дописывается при обновлении, до миграции отсутствие ключа = дефолт true.
- Флаги `--authkey`/`--node-secret` установщиков ноды не встраиваются в
  готовую команду подключения из панели (`set_node_install_command`) —
  оператор добавляет их вручную при user-режиме реестра.
- После OFF агенты без `node_secret`/`authkey` теряют machine API: автоматического
  самолечения по heartbeat НЕТ (их `/register` под общим Bearer тоже отклоняется
  401 `shared_token_disabled`). Доступ возвращает оператор, выдав authkey/
  `--node-secret`. Счётчик `affected_nodes` — предпроверочный гейт перед OFF, а
  не доказательство самовосстановления.
- Известный gap: `invites.created_by` = 0 by design — панель аутентифицируется
  единым `[panel] token`, per-admin identity нет, атрибутировать автора нечем;
  колонка заведена под будущий per-admin auth.
- `/register` 200 с `node_secret` отдаётся с `Cache-Control: no-store`; ошибки
  аутентификации machine-API (`requireNodeAuth`) и admin-guard отдают единый
  JSON-envelope `{"error":...}` вместо прежнего plain text.

## Installer ownership fix (migrate_registry_config)

**Проблема (подтверждена на проде):** `scripts/install_registry.sh`
`migrate_registry_config()` писал мигрированный конфиг через `mv -f "$tmp" "$file"`
(`$tmp` из `mktemp`). Установщик бежит от root → файл становился `root:root 0600`.
Сервис `sharedd-registry` читает `/etc/sharedd/registry.toml` от своего
пользователя → после апгрейда crash-loop `config error: failed to read config ...
permission denied`; требовал ручного `chown`.

**Фикс:** перед заменой снимаем владельца/группу/права целевого файла
(`stat -c '%u:%g:%a'`), после `mv` восстанавливаем их на новом файле
(`chown` + `chmod`). Числовые uid/gid — имя сервисного пользователя не
хардкодится, работает при любом layout. Бэкап (`.bak-<ts>`) остаётся как был
(`cp -a` сохраняет owner:group:mode). `mktemp` создаётся рядом с целью (tmp в её
каталоге). **Fail-closed:** если `stat`/разбор владельца не удались — миграция
прерывается, файл не трогается, временный файл удаляется.

**Доказательство (fakeroot, установщик под faked-root, файл `65534:65534 0660`):**

```
# fixed — владелец/права сохранены, ключи дописаны
-- before --  -rw-rw---- 1 65534 65534   81 ... registry.toml   stat: 65534:65534:660
-- after  --  -rw-rw---- 1 65534 65534  359 ... registry.toml   stat: 65534:65534:660
PASS: owner:group:mode preserved (65534:65534:660)
PASS: all migrated keys present  (enabled/legacy_max_nodes/allow_shared_secret/
      control_db/master_key_path/masters_per_domain/max_creates_per_day/cooldown_minutes)
# idempotent second run — без перезаписи (sha и inode не изменились)
PASS: second run did not rewrite config (sha)
PASS: second run did not replace inode
# fail-closed — stat не сработал
PASS: migration aborted (fail-closed) when owner cannot be resolved
PASS: target untouched: owner/group/mode unchanged
PASS: target untouched: content unchanged
PASS: temp file cleaned up on abort

# old (pre-fix contrast) — баг воспроизведён
-- before --  -rw-rw---- 1 65534 65534   81 ...   stat: 65534:65534:660
-- after  --  -rw------- 1     0     0  359 ...   stat: 0:0:600
PASS: bug reproduced: mv produced root:root 0600
```

Статика: `bash -n scripts/install_registry.sh` — OK;
`bash scripts/check_installer_parity.sh` — все 4 функции parity ok;
`shellcheck -S warning scripts/install_registry.sh` (v0.10.0) — чисто.
