# HANDOFF.md — состояние работ и что делать дальше (для новой сессии)

Ветка `dev` — **всё закоммичено** (2026-10-14, локально, БЕЗ push). Секреты здесь
НЕ хранятся: они в конфигах на серверах и в переписке владельца.

## 0. Деплой 2026-10-14 (что уже в проде)

- **Реестр: `V1.1.0` выкачен** на `registrar.ddproxy.xyz` (healthz=200, все ноды
  отчитываются). Бэкап: `/var/backups/sharedd-20261013-222428` (бинарь `prev24`
  + `registry_state.json` + `control.db` + `registry.toml` + sha256).
  Откат: `install .../sharedd-registry.prev24 /usr/local/bin/sharedd-registry` +
  вернуть `registry_state.json` из бэкапа.
- **Нода `test-lk-x3ha7` (169.128.68.34): агент `V1.1.1` выкачен** (бэкапы
  `sharedd-node-agent.prev*`); в `/etc/sharedd/node.toml` добавлен
  `[telemt] api_addr = "http://127.0.0.1:9091"` → включён DC2-режим.
- **Проверено end-to-end** (temp-сессия, удалена): `hosting.cpu/ram` считаются,
  `net_ingress/egress` непустые, `users.traffic_by_user` = `[{u0cb271,…}]`,
  `telemt.control_api` = ok/12 DC, `absent=['actions']`.
- Прочие ноды пока со старыми агентами (новый агент нужен и им для пп.3-5).
- **Реестр `V1.1.2` выкачен** (2026-10-14 22:41) — категоризованная карточка ноды
  с графиками. Бэкап: `/var/backups/sharedd-20261013-224108` (бинарь `prev25` +
  state/control/config). Проверено рендером под temp-сессией.
- **Реестр `V1.1.3` выкачен** (2026-10-14 22:48) — карточка ноды разбита на
  ВКЛАДКИ (как mtplane node.html): **Обзор · Метрики · Проверки · Журнал ·
  Действия** (владелец потребовал разделение, «всё в одну страницу» отклонено).
  Бэкап: `/var/backups/sharedd-20261013-224841` (`prev26` + state/control/config).
  Вкладка «Обзор» по умолчанию, активная — в хэше `#/nodes/<id>?tab=…`.
- **Реестр `V1.1.5` выкачен** (2026-10-13 23:14) — **переустановка manual-сервера
  с выбором профиля** (серверу в состоянии `done` доступна «Переустановить»,
  можно сменить профиль; квота не задваивается, старая привязка ноды снимается,
  нода ре-регистрируется). Бэкап `prev27` + `/var/backups/sharedd-20261013-231432`.
  Тесты: `manual_install_reinstall_test.go`, `manual_servers_test.go` (reinstall).
- **Разбор ad_tag на test-lk:** на ноде стоит `d3cf7b9e…` = дефолт ШТАТНОГО
  профиля #2; ваш `55f160fb…` — в профиле #4 «mtproxyl-bin khinkal» (ваш,
  unpublished). Установка шла legacy-путём под профилем #2, профиль #4 не
  использовался. Поставить `55f160fb`: переустановить сервер «test-lk» в
  кабинете, выбрав профиль #4 (теперь возможно).
- **Переработка статистики ноды (как mtplane node.html):** ВКЛАДКИ (Обзор/Метрики/Проверки/Журнал/Действия) + графики
  инлайн-SVG (`ui/lib/chart.ts`: A доступность 7д, C соединения 24ч по `clients`,
  E уникальные IP, F трафик по юзерам). График «me writers» УДАЛЁН. Спеки —
  `specs/node-stats.md`, `specs/implementation-summary-node-stats.md`,
  `DESIGN.md` §13. QA дизайнера: PASS.
  **Бэкенд-пробелы (не нарисованы, НЕ фейкованы):** D хостинг-CPU/RAM-история (G1/G5),
  E/F тренды за 24ч (G2/G3), B переключатель окна проверок (G4) — нужны
  сохраняемые time-series. LOW-ниты QA: нет оси X у графика C, `title` у столбцов A.
  ⚠️ Спеки легли в корневой `specs/`, а прежние — в `docs/specs/` (разнобой, свести?).

## 1. Прод

- Регистратор: `registrar.ddproxy.xyz` (Ubuntu 24.04, x86_64, Caddy, systemd
  `sharedd-registry`). Доступ root по паролю (владелец присылал в чат).
- Конфиг `/etc/sharedd/registry.toml` (user-режим включён, тумблер «общий секрет»
  ON, `[selection] masters_per_domain=2`, `[shared_proxy] port=443`,
  `tls_domain=m.beboo.ru`, 1 общий пользователь).
- Данные: `/var/lib/sharedd/{control.db, registry_state.json, registry.db, master.key}`.
- Бэкапы: `/var/backups/sharedd-*` (в т.ч. `sharedd-owner-hotfix-*`,
  `sharedd-profile-fix-*`) со sha256.
- Откат: старые бинари лежат как `/usr/local/bin/sharedd-registry.prevNN`
  (последний — `prev24`, это до V1.1.0). **Формат state сменился на списки** —
  откат ТОЛЬКО вместе с бэкапом `registry_state.json`.
- Юнит: `ProtectSystem=strict`, `ReadWritePaths=-/etc/sharedd -/var/lib/sharedd`
  (без второго пути панель не может сохранять конфиг — уже исправлено).

## 2. Процедура выката (проверенная)

```bash
cd /home/usr/Desktop/shareDDproxy/sharedd
VERSION=Vx.y.z-desc GOOS=linux GOARCH=amd64 bash scripts/build_registry.sh
# scp dist/sharedd-registry → /usr/local/bin/sharedd-registry.new
# на сервере: cp -a бинарь prevNN; stop; install .new → бинарь; start; healthz=200
```
Тестовые наборы владелец просил НЕ гонять: только `gofmt/vet` по изменённым
пакетам + `go build -mod=readonly ./...` + `bash scripts/build_ui.sh -check`.

## 3. НЕЗАВЕРШЁННОЕ (по порядку)

1. **Залить фикс IP в кабинете** (уже собран в дереве, не выкачен):
   `ui/pages/cabinet/node-detail.ts` показывает реальный IP (маскировка осталась
   только на публичных `/statistics`). Собрать `build_ui.sh` + выкатить.
2. **«Мастер онлайн, 0 клиентов» — РЕШЕНО (нода test-lk-x3ha7, 2026-10-13).**
   Причина: на ноде был ДУБЛЬ пользователя с одним секретом —
   `hello|dddd…` (создан инсталл-профилем) и `u0cb271|dddd…` (создан агентом из
   `shared_proxy.users`). Секрет `eee`+`dddd…`+SNI `m.beboo.ru` раздаётся один,
   но telemt атрибутирует трафик ПЕРВОМУ (`hello`), а `counted()` считает только
   `u0cb271` → 0. Fix: `mtproxyl secret remove hello true` (клиентов не рвёт —
   секрет тот же, ключ остался за `u0cb271`). Итог: 0 → ~1000+ клиентов.
   Реестр НЕ трогали. Рецидив: если ноду переустановить тем же профилем, `hello`
   вернётся (профиль хардкодит `hello`, регистратор генерит `uXXXXXX`). Устойчивый
   фикс — на стороне агента: при apply обнулять/переименовывать локального юзера
   с секретом, совпадающим с секретом реестрового пользователя (сейчас
   `applyMtproxylCLI`/`ensureUser` только добавляют и «чужих юзеров не трогают»).
3. **CPU/RAM хоста — СДЕЛАНО, ВЫКАЧЕНО (test-lk, проверено).** Агент пишет в
   снапшот `sharedd_host_cpu_percent`/`sharedd_host_ram_percent` (`/proc/stat`
   дельта, `/proc/meminfo`), регистратор отдаёт `hosting.cpu_percent/ram_percent`
   + условный `absent[]`, кабинет рисует строки CPU/ОЗУ. Файлы:
   `node/internal/agent/hoststats.go(+_test)`, `healthcheck.go`, `main.go`,
   `registry/internal/server/{panel,node_detail_api}.go`, `ui/pages/cabinet/node-detail.ts`.
4. **Per-user трафик 24ч — СДЕЛАНО, ВЫКАЧЕНО (test-lk, проверено).** Агент кладёт
   в снапшот `sharedd_user_{ingress,egress}_bytes_total{user="X"}` (только
   shared-юзеры), реестр пишет дельты в SQLite-таблицу `traffic_user` (retention
   35д) и отдаёт `users.traffic_by_user:[{user,ingress_bytes,egress_bytes}]` за
   24ч; кабинет рисует таблицу. Файлы: `node/internal/agent/{main,healthcheck}.go`,
   `registry/internal/history/history.go`, `registry/internal/server/{panel,healthcheck,node_detail_api}.go`,
   `ui/pages/cabinet/{api,node-detail}.ts`.
   ⚠️ **Тут же найден и исправлен КРИТИЧНЫЙ продовый баг:** константы агента
   `telemt_user_octets_{from,to}_client` писались БЕЗ суффикса `_total`, а telemt
   3.x отдаёт `…_total` → октеты не матчились НИКОГДА: **агрегатный трафик нод был
   0 всегда (дашборд!), и per-user пустой**. Фикс (коммит `fix(node): match telemt
   octets counters…`): матч обоих имён. После выката агрегат и per-user пошли.
5. **telemt Control API: DC/endpoints, качество/здоровье — СДЕЛАНО, ВЫКАЧЕНО
   (test-lk).** Агент отдаёт полный `/v1/stats/dcs` (`dc_stats`), реестр держит
   его in-memory и отдаёт `telemt.control_api{ok,generated_at,middle_proxy_enabled,
   dcs[]}`; вердикт DC2-здоровья не менялся. Для данных нужен `[telemt] api_addr`
   в node.toml (на test-lk включён).
6. **Действия ноды** для пользователя (проверка/конфиг/переустановка).
7. **Коммит `dev` — СДЕЛАНО (2026-10-14).** Всё дерево (было ~180 позиций, 23
   сборки) зафиксировано 5 логическими коммитами по слоям:
   `build:` инфра → `feat(registry):` cabinet/admin/provision/node-detail →
   `feat(node):` DC2+hoststats → `feat(ui):`+webassets → `docs:`.
   Локально, БЕЗ push. origin=`git@github.com:ugDeDe/sharedd.git`, у `dev` нет
   upstream — пушить `git push -u origin dev` (по согласию).
   ✅ Полный `go test ./...` в обоих модулях ЗЕЛЁНЫЙ (в т.ч. `-race`). Красные
   WIP-тесты разобраны (коммит `fix(registry): repair cabinet test suite...`):
   причина 503 — тест-харнесс не применял `SharedProxy.Port=443` (provision-vars
   валидация 1..65535), + реальный баг квоты `UsedNodes` не считал pending-брони;
   «дедлок» был следствием 503.
8. **Beget** — API отвечает `INCORRECT_CREDENTIALS` на его логин/пароль; ждём от
   владельца скриншот `cp.beget.com/settings/security/api` (включён ли API, 2FA).

## 4. Жёсткие правила, выученные на своих ошибках

- **Имя ноды НЕ генерировать.** Инсталлер (старый, с GitHub main) принимает
  `--name` 1..10 символов и **сам добавляет `-<5hex>`**. Имя берётся из поля
  сервера (`manual_hosts.label`). Секрет ноды привязывается **при первой
  регистрации** (`pending_node_secrets` → claim в `node_owners`), квота — там же.
- **Профиль = шаги**: строки `# Заголовок` задают шаг, все прочие строки — команда
  ЭТОГО шага. Нельзя вставлять служебные строки без `#` (я так сломал профиль:
  `=== STEP` попал в команды → `bash: ===: command not found`).
- **Старый инсталлер знает только** `--registry`, `--registry-token`, `--name`
  (+ у MTProxyL свои флаги). Никаких `--node-secret`/`--authkey` в профилях.
- **Деньги**: один `CreateVps` на `idem_key`, retry не покупает, `waiting_vps`
  держит слот, отказ здоровья/установки НЕ создаёт VPS.
- **Вывод ошибки установки**: причина пишется ПЕРВОЙ строкой, затем хвост вывода
  (`truncateOutput`, лимит 16000). Если причина не видна — это баг, не «норм».
- **Панель конфига** требует права на запись `/etc/sharedd` (юнит) и персистится в
  файл; проверять `persisted:true`.
- **Локальный браузер MCP** мокает `/statistics/api/list` — не верить скриншотам
  публичной статистики. Для проверки под юзером: временная сессия в `sessions`
  (sha256 токена), CSRF = `sha256("sharedd-csrf:"+token)`, потом удалить сессию.
- Ветка `dev` — **единственная копия** всей работы; коммит критичен.

## 5. Ключевые эндпоинты/модели, которые уже есть

- Пользователь: `/api/auth/{register,login,logout}`, `/api/me`, `/api/me/nodes`,
  `/api/me/nodes/{id}/detail`, `/api/me/authkey`(+rotate/revoke),
  `/api/me/servers`(+`{id}/install|check`, DELETE), `/api/me/profiles`(+copy,
  rename), `/api/me/beget/*`, `/api/me/operations` (с `steps[]`), `/api/node/status`.
- Админ (panel token): `/panel/api/admin/{users,profiles,invites,legacy-secret}`,
  `/panel/api/config`; удаление юзера `DELETE /panel/api/admin/users/{id}`.
- UI: `ui/pages/{cabinet,panel,signin,stats,dashboard,links}` → сборка
  `bash scripts/build_ui.sh` в `registry/internal/webassets/*.html` (встраиваются
  в бинарь; коммитить регенерированные ассеты).
