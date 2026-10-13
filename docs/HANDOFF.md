# HANDOFF.md — состояние работ и что делать дальше (для новой сессии)

Ветка `dev`, всё **не закоммичено** (прод собран из рабочего дерева). Секреты здесь
НЕ хранятся: они в конфигах на серверах и в переписке владельца.

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
  (последний — `prev23`). **Формат state сменился на списки** — откат ТОЛЬКО
  вместе с бэкапом `registry_state.json`.
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
3. **CPU/RAM хоста — КОД ГОТОВ, НЕ ВЫКАЧЕН.** Агент теперь пишет в снапшот
   `sharedd_host_cpu_percent`/`sharedd_host_ram_percent` (`/proc/stat` дельта,
   `/proc/meminfo`), регистратор отдаёт `hosting.cpu_percent/ram_percent` +
   условный `absent[]`, кабинет рисует строки CPU/ОЗУ. Файлы:
   `node/internal/agent/hoststats.go(+_test)`, `healthcheck.go`, `main.go`,
   `registry/internal/server/{panel,node_detail_api}.go`, `ui/pages/cabinet/node-detail.ts`.
   Проверки зелёные (node/registry gofmt+vet+build, `build_ui.sh -check`).
   Чтобы заработало: выкатить новый бинарь агента на ноды + собрать/перезапустить реестр.
4. **Per-user трафик 24ч — КОД ГОТОВ, НЕ ВЫКАЧЕН.** Агент кладёт в снапшот
   `sharedd_user_{ingress,egress}_bytes_total{user="X"}` (только shared-юзеры),
   реестр пишет дельты в новую SQLite-таблицу `traffic_user` (retention 35д) и
   отдаёт `users.traffic_by_user:[{user,ingress_bytes,egress_bytes}]` за 24ч;
   кабинет рисует таблицу. Файлы: `node/internal/agent/{main,healthcheck}.go`,
   `registry/internal/history/history.go`, `registry/internal/server/{panel,healthcheck,node_detail_api}.go`,
   `ui/pages/cabinet/{api,node-detail}.ts`. Проверки зелёные (node/registry
   gofmt+vet+build+targeted tests, `build_ui.sh -check`).
   ⚠️ Полный `go test ./internal/server/...` в дереве падает на ДОСУЩЕСТВУЮЩЕМ
   WIP-тесте (`cabinet_api_test.go`/`addendum_fixes_test.go`, untracked, 503
   control-store) — к трафику отношения нет. Выкат: ребилд агента + реестра.
5. **telemt Control API: DC/endpoints, качество/здоровье** (отдельный срез).
6. **Действия ноды** для пользователя (проверка/конфиг/переустановка).
7. **Коммит `dev`** — логическими коммитами (нужно согласие владельца). Прод уже
   отставал на 23 сборки из незакоммиченного дерева.
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
