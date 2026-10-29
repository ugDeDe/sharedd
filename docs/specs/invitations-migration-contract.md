# invitations-migration-contract.md — приглашения, общий секрет, миграция

Ветка `dev`. Авторитетный контракт для реализации (UI-поведение:
`docs/specs/invitations-legacy-ui.md`). Решения здесь — финальные.

## 1. Модель доступа к machine API

Два уровня секрета, путаница с секретами Telegram-прокси запрещена.

- **Общий секрет** — существующий `[security] node_token` (Bearer).
- **Персональный секрет ноды** — НОВЫЙ `node_secret` (32B random, urlsafe),
  выдаётся ноде при регистрации, хранится только хеш (sha256) в
  `node_owners.node_secret_hash` + префикс. Передаётся агентом как
  `Authorization: Bearer <node_secret>`; различение с общим — по наличию хеша
  среди нод (lookup по sha256), неоднозначности исключены длиной/случайностью.

Матрица (на КАЖДОМ machine-маршруте: `/register`, `/heartbeat`, `/report`,
`/retire`, `/config`):

| Режим | Bearer = общий | Bearer = node_secret |
|---|---|---|
| «Разрешить общий секрет» = ON | принимается | принимается (hash→node_id, владелец не нужен для транспорта) |
| = OFF | **отклоняется 401** `shared_token_disabled` | принимается; node_id маршрута обязан совпадать с владельцем секрета (иначе 403) |

`/register` дополнительно: ON — общий токен достаточен для legacy-перерегистрации;
OFF — требуется authkey (новая/своя нода) либо валидный node_secret в теле
(перевыпуск). Выдача `node_secret`: только в ответе `POST /register`
(`{"node_secret":"..."}`, один раз), при перерегистрации своей ноды —
перевыпуск нового секрета (старый инвалидируется). Потерял секрет →
перерегистрация (владелец совпадает) → новый секрет.

Миграция старых агентов: пока ON — старые агенты работают на общем токене без
изменений. После OFF нода БЕЗ персонального `node_secret`/`authkey` **теряет
доступ**: на всех machine-маршрутах (включая `/register`) возвращается 401
`shared_token_disabled`. Автоматического самолечения по heartbeat НЕТ —
старый агент не шлёт `authkey` и не парсит `node_secret` из тела ответа, а его
`/register` под общим Bearer тоже отклоняется. Доступ восстанавливается только
действием оператора: выдать ноде `authkey`/`--node-secret`, либо нода должна
была хотя бы раз перерегистрироваться новым агентом при ON и сохранить выданный
`node_secret`. Счётчик `affected_nodes` (§3) — предпроверочный гейт перед
выключением, а не доказательство самовосстановления: OFF включать только при
`affected_nodes == 0`.

Анти-регресс: OFF не даёт постоянных исключений; единственный путь —
персональный секрет/authkey.

## 2. Приглашения

Таблица `invites(id INTEGER PK, token_hash TEXT UNIQUE, prefix TEXT,
created_by INTEGER, created_at INT, expires_at INT, used_by INT, used_at INT,
revoked_at INT)`.

- Токен: 32B random → URL `/signin?invite=<token>`; хранится sha256 + префикс.
- `POST /panel/api/admin/invites` `{ttl_hours}` (default 72, допустимо 1..720)
  → `201 {id, invite_url, expires_at, prefix}`. `invite_url` = `panel.public_url`
  + `/signin?invite=<token>`; полный токен показывается ОДИН раз.
- `GET /panel/api/admin/invites` → список (id, prefix, created_at, expires_at,
  status: active|used|revoked|expired, used_by_username NULL).
- `DELETE /panel/api/admin/invites/{id}` → revoke (idempotent).
- Регистрация: `POST /api/auth/register {username,password,invite}`.
  Валидация атомарная: `UPDATE invites SET used_by=?,used_at=? WHERE id=?
  AND used_by IS NULL AND revoked_at IS NULL AND expires_at>?` — гонка даёт
  ровно один успех. Коды ошибок: `invite_required`, `invite_invalid`,
  `invite_expired`, `invite_used`, `invite_revoked` (403). Неуспешная валидация
  полей (логин/пароль) НЕ расходует приглашение (invite проверяется и
  резервируется после валидации полей, в одной транзакции с созданием юзера).
- Приглашение обязателено ВСЕГДА в user-режиме (открытой саморегистрации больше
  нет). Прежнее поведение «регистрация без invite» удаляется.

## 3. Переключатель «Разрешить общий секрет»

Хранение durable: таблица `settings(key TEXT PK, value TEXT)` в control.db,
ключ `legacy_shared_secret` = `"on"|"off"`. Бутстрап: при отсутствии строки —
значение из `[users] allow_shared_secret` (default **true**), и строка
создаётся при первом чтении/записи.

- `GET /panel/api/admin/legacy-secret` → `{enabled, affected_nodes, as_of}`.
  `affected_nodes` — счётчик из §1 (только живые кандидаты без секретa);
  `as_of` — время расчёта; при ошибке расчёта `affected_nodes: null`.
- `POST /panel/api/admin/legacy-secret` `{enabled}` → пишет settings,
  ответ `{enabled, persisted:true}`; любая неудача записи → 5xx без изменения
  состояния в памяти (fail-closed), частичный успех невозможен (один UPDATE).
- Рантайм-применение мгновенное (проверка на каждом запросе), рестарт сохраняет
  режим. `config_editor` читает эффективное значение из settings.

## 4. Конфиг и установщик

Новые ключи `[users]`: `allow_shared_secret` (bool, default true) — только
бутстрап; фактическое состояние — settings. Валидация: при
`users.enabled=false` ключи игнорируются.

Автомиграция конфига в установщике регистратора (`scripts/install_registry*.sh`,
если файла нет — создать в рамках задачи): при обновлении ДО старта сервиса:
1) бэкап `registry.toml` → `registry.toml.bak-<ts>`; 2) дозаписать ТОЛЬКО
отсутствующие ключи: `[users] enabled=true, legacy_max_nodes=100000,
allow_shared_secret=true`, `[database] control_db, master_key_path` (дефолтные
пути), `[selection] masters_per_domain=1`, `[provisioning]` дефолты;
3) существующие значения НЕ трогать (явный OFF не понижать); 4) идемпотентно.
Паритет функций `install_node.sh`/`install_node_web.sh` не нарушать
(это установщики АГЕНТА; там — только опциональный флаг `--authkey`/`--node-secret`
в node.toml, без изменения общих функций).

## 5. Обязательные тесты

1. Два параллельных register с одним invite → ровно один аккаунт.
2. expired/used/revoked → различные коды; невалидные поля не расходуют invite.
3. OFF: общий токен отклонён на всех 5 маршрутах; node_secret принимается,
   чужой node_id → 403; перерегистрация с authkey выдаёт новый секрет.
4. ON: общий токен работает везде (legacy-поведение байт-в-байт).
5. Переживание рестарта: OFF остаётся OFF; bootsrap-дефолт ON при пустом settings.
6. `affected_nodes`: нода с секретом не считается; без секрета считается.
7. Установщик: идемпотентный прогон; явные значения сохраняются; бэкап создаётся.

## 6. Гейтинг продакшена (блокеры деплоя)

- Пункт 5 полностью зелёный; обе модели go vet/test/build/lint; build_ui -check;
  shellcheck/parity для изменённых скриптов.
- Beget-создание VPS остаётся недоступным (раннер не подключён): в кабинете
  визорд не должен обещать успех — API уже отвечает ошибкой без линка; в отчёте
  о деплое пометить фичу как выключенную по факту.
- Бэкапы на проде до замены: registry.toml, registry_state.json, registry.db,
  master.key (+ права). Откат: старый бинарь + бэкап state (формат state новой
  версии нечитаем старой — бэкап обязателен).
