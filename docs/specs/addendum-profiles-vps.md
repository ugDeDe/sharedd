# addendum-profiles-vps.md — добавка к контракту: секция selection, приглашения
# на N регистраций, профили (админ/пользователь), завершение создания VPS

Ветка `dev`. Дополняет `docs/specs/invitations-migration-contract.md` и
`docs/specs/beget-provisioning.md`. Решения обязательные.

## 1. `masters_per_domain` из админки

- Секция `selection` добавляется в редактируемый конфиг панели
  (`editableConfig.Selection`), поле `masters_per_domain` 1..8
  (валидация как в config.go: `MaxMastersPerDomain`).
- Обычный `PUT /panel/api/config` (merge, nil-секция = не трогать), как у
  остальных секций. Рантайм-применение без рестарта (cfg под cfgMu),
  новое значение сразу влияет на число слотов следующего тика селекции.
- UI: числовое поле с подписью «Мастеров на домен», подсказка про слоты и
  что это НЕ `[srmd] max_nodes_per_domain`; предупреждение при изменении —
  смена набора A-записей домена (DNS перестроится сам, ссылки не меняются).

## 2. Приглашение на N регистраций

- `invites` получает `max_uses INTEGER NOT NULL DEFAULT 1`,
  `used_count INTEGER NOT NULL DEFAULT 0` (миграция ensureColumn).
- `POST /panel/api/admin/invites {ttl_hours, max_uses}` (max_uses 1..1000,
  дефолт 1) → `{id, invite_url, expires_at, prefix, max_uses}`.
- Регистрация: атомарный `UPDATE invites SET used_count=used_count+1,
  used_by=?, used_at=? WHERE token_hash=? AND revoked_at IS NULL
  AND expires_at>? AND used_count < max_uses` — гонка не превышает max_uses.
- Список: `max_uses`, `used_count`, `status`
  (active = used_count<max_uses и не истёк/не отозван; exhausted =
  used_count>=max_uses; expired; revoked). Код исчерпания — `invite_used`
  (совместимо с UI), отдельного «invite_exhausted» не вводим.
- UI: поле «Число регистраций» (дефолт 1), в таблице «N/M использовано».
- Регистрация по invite по-прежнему создаёт юзера с `max_nodes=0`; счётчик
  приглашения с квотой нод не связан.

## 3. Профили установки

Модель уже есть (`profiles`, `profile_versions`). Добавляем:

- **Админ**: `GET/POST /panel/api/admin/profiles`,
  `PUT/DELETE /panel/api/admin/profiles/{id}`; поля name, description,
  published, playbook (YAML), vars_schema (JSON-схема допустимых переменных).
  Версии: правка создаёт новую запись в `profile_versions` (не перезаписывать
  историю), `published` управляет видимостью пользователям. Валидация:
  name уникален, playbook ≤ 64 KiB, YAML парсится.
- **Пользователь** (по запросу владельца): `GET/POST /api/me/profiles`,
  `PUT/DELETE /api/me/profiles/{id}` — только свои; `published=false`
  (локальные, не видны другим), лимит `[provisioning] max_user_profiles`
  (дефолт 5).
- **Песочница пользовательских плейбуков (обязательна).** При сохранении и
  перед запуском отклоняем плейбук, если он:
  - не парсится как YAML, или не содержит ровно один play;
  - имеет `hosts: localhost|127.0.0.1|::1` или IP/имя самого регистратора;
  - содержит `connection: local`, `delegate_to`, `local_action`,
    `delegate_facts`, `add_host` (регулярный поиск по тексту + структуре).
  Запуск: inventory строится на сервере ровно из одного хоста — VPS этого
  пользователя (`--limit`), `--connection=ssh`, приватный ключ из хранилища,
  таймаут из `[provisioning] install_timeout_min` (дефолт 20).
  Резидуальный риск (документируется): плейбук исполняется с root на **своём**
  VPS пользователя и теоретически может атаковать третьи хосты **с VPS**, но не
  регистратор. Флаг `[provisioning] user_profiles_enabled` (дефолт true,
  отключаемо админом).

## 4. Завершение создания VPS (Beget) — что доделать

Уже есть: beget-клиент, provision (атомарная резервация, один `CreateVps` на
`idem_key`, `unknown`→сверка), профили, ansible-раннер с `guardTarget`.
Доделать:

1. **Каталог тарифов и цены**: добавить в beget-клиент
   `GetConfigurations(ctx)` (тарифы: id, название, cpu/ram/disk, цена) и
   отдавать в `GET /api/me/beget/regions` и регионы, и тарифы с ценой за
   период; если провайдер не отдаёт цену — `price: null` и UI пишет
   «цена не указана», без выдумывания.
2. **Инвентарь/раннер**: в `Run()` регистратора создать
   `ansible.New(profilesDir, "")` и `provisioner.SetRunner(...)`. Каталог
   плейбуков `[provisioning] playbooks_dir` (дефолт рядом со state, `0700`).
3. **Поток**: `POST /api/me/vps` → операция `pending/sent` → ожидание
   `RUNNING` → IP → `ApplyProfile(profile, ip, sshUser, keyPath, vars)` →
   в vars передаётся `authkey` пользователя и `node_secret`, чтобы агент
   зарегистрировался → статус операции `installing` → `done` при
   `Changed/Failed=0` или `failed` с выводом (обрезанным).
4. **Профиль по умолчанию** `sharedd-node` (published): установка
   закреплённой версии telemt + агента sharedd, конфиг с authkey и
   `[telemt] api_addr`, systemd, права. Идемпотентный.
5. Совместимость: без `beget_links.verified` создание отклоняется (уже есть);
   цена показывается на шаге подтверждения; повторная отправка с тем же
   `idem_key` возвращает ту же операцию.

## 6. Пользовательские переменные профиля ({{ var }})

- В профиле (админском и пользовательском) есть таблица переменных:
  `name` (обязательно), `default` (значение по умолчанию), `description`
  (необязательно). Хранится в `vars_schema` (JSON).
- Подстановка — **Ansible** `--extra-vars '<json>'`, без собственного
  шаблонизатора: `{{ testvar }}` в плейбуке заменяется Ansible'ом.
- При создании VPS пользователь видит объявленные переменные профиля с
  дефолтами и может их переопределить (и добавить свои сверх объявленных).
- Валидация имени: `^[a-zA-Z_][a-zA-Z0-9_]*$`, длина значения ≤ 4096,
  общее число переменных ≤ 64, суммарный размер extra-vars ≤ 32 KiB.
- **Запрещённые имена** (отклоняем при сохранении профиля И перед запуском):
  любые `ansible_*`, а также `hosts`, `connection`, `delegate_to`,
  `local_action`, `remote_user`, `inventory`. Иначе переменная уводит запуск
  на другой хост/под другим ключом.
- Значения экранируются как JSON (не как shell); в логи/вывод не попадают
  значения, помеченные секретными (`secret: true` в объявлении) — они
  маскируются в `operations.output`.

## 7. Обязательные фиксы (деньги и связность VPS)

1. **Личность ноды заранее**: при принятии заказа VPS регистратор выделяет
   `node_id` + свежий `node_secret`, пишет хеш секрета и привязку владельца,
   резервирует место в квоте **до** платного вызова; плейн-секрет передаётся
   только шагу установки (`--name`, `--node-secret`). `POST /register`
   принимает валидный `node_secret` для своей предсозданной ноды. Секрет не
   возвращается ни одним read-эндпоинтом и не логируется.
2. **Только «повторить установку»**: `POST /api/me/operations/{id}/retry-install`
   переиспользует существующий `provider_vps_id`, новых `CreateVps` не делает;
   отказ, если VPS ещё не создан / операция активна / уже `done`. Тест: две
   повторные установки → ноль дополнительных `CreateVps`.
3. **Ожидание готовности**: `POST /api/me/vps` ждёт `RUNNING` ограниченно
   (`[provisioning] vps_ready_timeout_min`, дефолт 10, интервал 10с); таймаут →
   `waiting_vps` (место в квоте остаётся занято, новый заказ невозможен).
4. **Свои профили**: владелец может выбрать свой `published=false` профиль при
   создании VPS; чужой неопубликованный → 404.
5. `profiles` admin JSON отдаёт число версий.

## 5. Тесты (обязательные)
2. invite max_uses=10: 10 успешных регистраций, 11-я → `invite_used`;
   параллельные 12 при max_uses=10 → ровно 10.
3. профили: админ CRUD + версии; пользователь правит только свои; чужой id → 404.
4. песочница: плейбук с `connection: local`/`hosts: localhost`/`delegate_to`
   отклоняется при сохранении И перед запуском; валидный — принимается.
5. VPS-поток (моки): один `CreateVps` на `idem_key`; таймаут → `unknown` без
   повторного заказа; `ApplyProfile` вызывается один раз; отсутствие
   `playbooks_dir`/раннера → операция `failed` с внятной ошибкой, без заказа.
6. Каталог: регионы и тарифы в ответе; отсутствие цены → `null` (не 0).
7. Переменные: `testvar` из профиля подставляется Ansible'ом (extra-vars);
   имя `ansible_host`/`hosts`/`connection` отклоняется при сохранении профиля
   и перед запуском; значение ≤4096, >64 переменных → ошибка.
8. Предсозданная личность: агент с выданным `node_secret` регистрируется
   end-to-end (мок); `retry-install` не делает платных вызовов; таймаут
   готовности → `waiting_vps` без второго заказа.
