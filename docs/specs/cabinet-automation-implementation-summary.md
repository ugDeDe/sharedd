# Реализация: ЛК, автопилот, параметры нод, публичные названия

**Дата:** 2026-10-19.
**Источник требований:** `docs/specs/cabinet-automation-implementation-plan.md`.
**Статус:** первый рабочий срез, `partially_verified` по полному плану.

## Что реализовано

### 1. Защита state и платных команд

- `loadState` возвращает ошибку для существующего повреждённого/нечитаемого файла.
- `Run()` завершается с понятной ошибкой вместо запуска с пустым state и последующей перезаписи.
- Отсутствующий файл остаётся допустимым первым запуском.
- В кабинете ключ и тело платной команды сохраняются в `sessionStorage`:
  - автопилот (`sharedd.pending.fleet`);
  - одиночное создание облачного ресурса (`sharedd.pending.cloud-create`);
  - пакетное создание (`sharedd.pending.cloud-batch`).
- Сетевая ошибка/таймаут/5xx переводят команду в `unknown`, а не в отказ.
- Повтор с теми же параметрами использует тот же `Idempotency-Key`.
- Изменение параметров при неизвестном предыдущем исходе блокируется.
- Конфликт ключа больше не «лечится» новым ключом.
- Добавлена явная кнопка «Начать новый заказ» — только после проверки журнала/ресурсов.
- `auto_replace` по умолчанию выключен; включение требует отдельного согласия на будущие платные замены.

Файлы: `registry/internal/server/run_main_impl.go`, `ui/pages/cabinet/pending.ts`, `ui/pages/cabinet/main.ts`, `ui/pages/cabinet/index.html`.

### 2. Логические ноды и постоянные номера

- Добавлены `node_slots` и `user_node_counters`.
- `node_owners.slot_id` — nullable-связь с логической нодой.
- Номер выделяется транзакционно, уникален внутри пользователя и не переиспользуется.
- Слот создаётся при первой привязке владельца, переиспользуется при повторной регистрации/переустановке.
- Legacy-ноды слотов не получают.
- Startup-миграция `BackfillNodeSlots` идемпотентна и не объединяет исторические технические ID по догадкам.

Файлы: `registry/internal/control/node_slots.go`, `control.go`, `nodes.go`, `manual_install.go`.

### 3. Публичные названия

- Публичные DTO получили `display_name`, `owner_username`, `owner_node_number`.
- Пользовательская нода: `login · Нода №N`.
- Legacy/нода без пользовательского владельца: прежнее имя регистрации (`node_id`).
- Подпись вычисляется сервером пакетно, без SQL под `Registry.mu`.
- Обновлены список нод, карточка ноды, мастера доменов и карантин.
- IP, секреты и measurement ID по-прежнему не публикуются.

Файлы: `registry/internal/server/stats.go`, `ui/pages/stats/main.ts`.

### 4. Быстрые параметры установки

- Автопилот получил поле имени нод (1–7 символов) и редактор параметров установки.
- Используется существующий `vars_schema` и общий `ui/lib/vars.ts`: ad_tag, порт, SNI, секрет и объявленные переменные.
- `installation_vars` передаются в `POST /api/me/fleets`, мержатся со схемой и сохраняются снимком в `fleets.installation_vars_json`.
- Воркер передаёт параметры в общую процедуру `startCloudInstall`.
- Повтор заказа сравнивает имя, конфигурацию и параметры установки.
- `ad_tag` валидируется как 32 hex-символа (пустая строка — явное отсутствие тега).
- Имя ноды остаётся отдельным полем; `label`, `node_id`, `node_secret`, `registry_url` по-прежнему нельзя передать через `vars`.
- Личные значения по умолчанию (`user_profile_defaults`): обычные параметры сохраняются на профиль и подставляются в manual/cloud/Beget/fleet/blueprint-установки. Приоритет: форма > личные defaults > профиль > регистратор. Секреты сохранять запрещено.
- В автопилоте есть отдельный opt-in «Запомнить эти параметры для профиля».

Файлы: `registry/internal/server/fleet_api.go`, `fleet_worker.go`, `profile_defaults.go`, `registry/internal/control/fleets.go`, `profiles.go`, `user_profile_defaults.go`, `ui/pages/cabinet/main.ts`, `index.html`, `api.ts`.

### 5. Переименование ноды

- `GET/PATCH /api/me/nodes/{node_id}/settings`.
- Меняется только `node_slots.name`; технический ID, секрет, владелец, квота и история не затрагиваются.
- Optimistic locking по `expected_revision`; конфликт — `409`.
- Чужая нода — `404`.
- В карточке ноды (вкладка «Управление») появилось поле названия и постоянный номер.
- Шапка карточки показывает `название · Нода №N` и технический `node_id` рядом.
- Runtime-изменение ad_tag честно возвращает `unsupported`: агент пока не умеет применять его на живой ноде.

Файлы: `registry/internal/server/cabinet_node_settings.go`, `ui/pages/cabinet/node-detail.ts`, `api.ts`.

### 6. Восстановление автопилота

- `FleetsInProgress` и startup-resume запускают воркеры для `running`-пакетов после рестарта.
- Повтор использует сохранённые idempotency-ключи; новый платный Create не появляется.
- `resumeCloudDeletes` после рестарта проверяет `deleting`-ресурсы у провайдера: отсутствующий VPS закрывается локально, существующий удаляется по тому же external_id. Новый платный вызов не создаётся.
- Установленная, но ещё не подтверждённая проверками нода остаётся в `verifying`; флот не считается успешным до полного health proof. По истечении 10 минут — `health_timeout`, без новой покупки.
- При замене VPS через `replaces_resource_id` новая нода наследует постоянный слот заменяемой.

Файлы: `registry/internal/server/fleet_worker.go`, `run_main_impl.go`, `cabinet_cloud.go`, `registry/internal/control/fleets.go`, `cloud_resources.go`.

### 7. Независимая загрузка разделов и readiness

- Общий refresh кабинета использует `Promise.allSettled`: сбой Beget не скрывает ноды и журнал.
- Индикатор показывает `частично`, если обновились не все источники.
- Добавлен `/readyz`: проверяет state, control-БД и master key при включённом user-режиме. `/healthz` остаётся liveness.

### 8. DNS-синхронизация и чистота ЛК
- В панели «Мастера и DNS» показана таблица `desired/applied`, ошибка, число попыток и время следующего повтора.
- Старый завершённый флот больше не показывается и не здоровается тостом при входе в ЛК: хранимый fleet id тихо проверяется, терминальный очищается.
- Блок «История заказов» из автопилота удалён по требованию владельца.
- В кабинете refresh разделов независимый: сбой Beget больше не скрывает ноды и журнал.

Файлы: `ui/pages/cabinet/main.ts`, `index.html`, `ui/pages/panel/views/dns.ts`, `ui/pages/panel/index.html`.

### 8.1. Backup/restore

- `scripts/backup_registry.sh`: state, control/history SQLite через `.backup`, конфиг и master key, manifest SHA256.
- `scripts/restore_registry.sh`: проверка manifest, остановка службы, восстановление прав и запуск, только с `--yes`.
- Описание и ограничения: `docs/specs/backup-restore.md`.

### 9. Runtime ad_tag

- Желаемая и применённая ревизии хранятся отдельно (`node_runtime_settings`).
- `PATCH /api/me/nodes/{node_id}/settings` сохраняет `ad_tag` как `pending`.
- Индивидуальные настройки отдаются только агенту с персональным `node_secret`; общий токен их не получает.
- Агент подтверждает результат через `POST /api/node/settings/status`; успех показывается только после ack.
- MTProxyL применяет тег через CLI, перезапускает прокси и проверяет `/metrics`; при ошибке пытается вернуть прежнее значение.
- Classic/MEKO и явное удаление тега (`clear`) честно получают `unsupported`, без записи неизвестных TOML-ключей.
- Повтор одной ревизии не вызывает лишний рестарт; старая ревизия не перетирает новую.
- В карточке ноды есть поле ad_tag и статус применения.

Файлы: `registry/internal/control/node_runtime.go`, `registry/internal/server/cabinet_node_settings.go`, `node_settings_status.go`, `run_main_impl.go`, `registry/internal/machineapi/security.go`, `node/internal/agent/runtime_settings.go`, `shared_config_sync.go`, `ui/pages/cabinet/node-detail.ts`.

### 10. Финансовый лимит и предпросмотр политики

- `[provisioning] max_paid_per_day` — опциональный дневной лимит платных попыток cloud/fleet (0 = выключен).
- Резервирование попыток транзакционное; два конкурентных заказа не обходят лимит.
- `429 rate_limited` при исчерпании.
- В политике управления перед включением показывается человекочитаемый список действий из graph: создать VPS, установить профиль, дождаться проверок, переключить ресурс, удалить старый VPS. Платные действия выделены предупреждением.

Файлы: `registry/internal/control/paid_limits.go`, `registry/internal/config/config.go`, `cabinet_cloud.go`, `fleet_api.go`, `ui/pages/cabinet/blueprint-management.ts`.

## Проверки

Выполнено:

```bash
cd registry && go vet ./... && go build -mod=readonly ./... && go test ./... -count=1
cd node && go vet ./... && go build -mod=readonly ./... && go test ./... -count=1
bash scripts/build_ui.sh
bash scripts/build_ui.sh -check
```

Все перечисленные команды прошли на 2026-10-19.

`go test -race` для обоих модулей запускался отдельно; результат фиксируется в сессии выполнения.

## Что ещё не реализовано

Полный план шире этого среза. Не реализовано:

1. **Расширение runtime-настроек за пределы ad_tag/MTProxyL**: другие параметры, classic/MEKO и явный `clear` пока возвращают `unsupported`.
2. **Immutable-снимки для manual/cloud/Beget-установок** (для флотов снимок параметров уже есть).
3. **Browser QA** новых сценариев (потеря ответа, повтор, мобильные, клавиатура).

Эти пункты нельзя считать выполненными по данному документу.
