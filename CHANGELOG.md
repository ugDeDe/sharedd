# Changelog

Все заметные изменения проекта. Формат — Keep a Changelog; версии
соответствуют тегам релизов.

## [1.0.3] - 2026-10-05

### Added

- **Возврат dead-нод**: класс `dead` больше не вечен. Агент после
  локального вердикта шлёт `/retire` (бан остаётся в вечной истории),
  самостоятельно ждёт оздоровления метрик (перепроверка раз в 30 с), затем
  перезапускается и регистрируется заново; регистратор снимает `dead`-
  запись первым же `/register` и проверяет здоровье с нуля. Число циклов
  возврата не ограничено.
- **CI-релизы**: workflow закоммичен (раньше `.github/` был в
  `.gitignore` и CI не запускался вовсе). Тег `vX.Y.Z`/`VX.Y.Z` собирает
  статические бинарники на GitHub-hosted раннере и публикует их в релиз;
  `releases/latest/download/...` для web-установщиков обновляется сразу.
- `golangci-lint` в CI собирается тем же тулчейном, что и проект, —
  готовый бинарник action падал из-за несовпадения версии Go с `go.mod`.

### Changed

- **Маска публичного IP**: первый и последний октет (`159.x.x.70`) вместо
  первых двух (`159.194.x.x`); ветка IPv6 удалена — пул IPv4-only.
- **Структура node**: по образцу registry — тонкий корневой `main.go`,
  вся логика в `internal/agent`, `internal/config` — зависимо-свободный
  лист (TOML, валидация, персистентность node ID).
- **MTProxyL**: режим superexpert заменён на динамическое управление через
  CLI (`secret add`, `expert set ...`, `expert apply`); конфиг и секреты
  приходят от регистратора, в инсталлерах не хардкодятся.

### Removed

- **Tombstone ноды** (`/var/lib/sharedd/terminated.json`): файл больше не
  существует, единственный источник правды о блокировках — регистратор.
  Перезапуск службы с устаревшим tombstone больше не «хоронил» ноду без
  перепроверки здоровья.

### Fixed

- Тест CLI-флоу MTProxyL (`TestApplySharedConfigViaMtproxylCLI`) падал и на
  main: гейт доступности CLI смотрит реальный PATH — теперь тест
  подкладывает фейковый бинарник.

## [Unreleased]

### Changed (refactor)

- **Структура registry**: монолитный `package main` (~8.6k строк) разделён на
  пакеты: тонкий корневой `main.go`, `internal/server` (вся логика),
  `internal/state` (владелец JSON-state-формата, без зависимостей),
  `internal/config` (TOML, валидация, hot-apply типы), `internal/history`
  (SQLite bans/events/traffic), `internal/machineapi` (wire-контракты с
  агентом + валидация запросов), `internal/globalping` (GET-клиент
  верификации), `internal/webassets` (сгенерированные страницы + шрифты).
  HTTP API, state-файл и SQLite-схема не изменились.
- **Селекция мастеров**: 315-строчный `evaluateAssignments` стал оркестратором
  над чистыми функциями (`sortQueue`, `pickLeastLoaded`, `rotateByTTL`,
  `reassignDead`, `fillEmpty`, `reconcileStints`) с табличными тестами;
  побочные эффекты собираются в `selectionSink`.
- **CI**: добавлены блокирующие gofmt, golangci-lint (registry/node/uigen),
  shellcheck (`-S warning`) всех скриптов, `build_ui.sh -check` и проверка
  парности установщиков (`check_installer_parity.sh`).
- **Web-установщики** сверяют sha256 скачанного бинарника (отсутствие файла
  суммы — предупреждение, расхождение — прерывание); node-web дополнительно
  проверяет ELF-магию.
- **systemd hardening**: registry — строгий профиль (ProtectSystem=strict,
  SystemCallFilter, CapabilityBoundingSet и др.), агент — консервативный
  (нужны iptables/ipset/systemctl). Юниты обновлены и в репозитории, и в
  heredoc'ах установщиков.

### Added

- Агент: pre-валидация пропатченного telemt.toml до записи (`validateTOMLText`)
  — битый патч отклоняется мгновенно вместо отката через нерестарт прокси.
- Агент: диагностика `TELEMT COMPATIBILITY` (не чаще раза в час), когда
  `/metrics` отвечает, но ключевые метрики отсутствуют — внятное указание на
  несовместимую сборку telemt вместо «metric not found».

### Fixed

- Парсер Prometheus-метрик агента: значения label'ов с `}`, `,` и escape-
  последовательностями больше не ломают разбор per-user серий.
- Установщик регистратора: флаг `--yes` задокументирован как no-op для
  совместимости (раньше — молчаливая мёртвая переменная).
- Тест `TestTrafficHistoryAndCounterReset`: детерминированные метки времени
  внутри календарного окна «Сегодня» (падал при запуске после полуночи).

### Removed

- Корневой скрипт `install-mtproto-antiscan.sh`: единственный владелец
  antiscan — агент ноды (`ANTISCAN_MTPROTO`, ipset `sharedd_scanners`);
  второй владелец той же задачи создавал дублирующие цепочки iptables.
