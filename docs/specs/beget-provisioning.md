# specs/beget-provisioning.md — личный Beget, профили, Ansible

Ветка `dev`. Registry + node-модуль. Счёт списывается с **личного аккаунта
пользователя**; главный инвариант: **ошибка здоровья/установки никогда не
порождает новый платный заказ**, и ни один заказ не отправляется дважды.

## 1. Хранилище (control store, расширение M1)

```sql
beget_links(id PK, user_id UNIQUE, login_enc BLOB NOT NULL, password_enc BLOB NOT NULL,
            ssh_key_id INTEGER, ssh_pub TEXT, ssh_priv_enc BLOB,
            created_at INTEGER, verified_at INTEGER, last_error TEXT)
profiles(id PK, name UNIQUE, description, published INTEGER DEFAULT 0,
         created_by INTEGER, created_at INTEGER, updated_at INTEGER)
profile_versions(id PK, profile_id, version INTEGER, playbook TEXT NOT NULL,
                 vars_schema TEXT, created_at INTEGER, UNIQUE(profile_id,version))
operations(id PK, user_id, node_id TEXT, kind TEXT, state TEXT NOT NULL,
           idem_key TEXT UNIQUE NOT NULL, provider_vps_id TEXT, region TEXT,
           config_id TEXT, request_json TEXT, last_error TEXT,
           created_at INTEGER, updated_at INTEGER)
cloud_resources(vps_id TEXT PK, user_id, node_id TEXT, region, config_id,
                state TEXT, created_at INTEGER, deleted_at INTEGER)
```

`state ∈ {pending, sent, confirmed, unknown, failed, deleting, deleted}`.
`unknown` — ответ потерян/неоднозначен; **автоповтор платного POST запрещён**.

## 2. Шифрование

`master.key` (32B) — отдельный файл (`[database] master_key_path`, права 600),
НЕ в БД. AES-GCM (как `mtplane/internal/cryptoutil`). Шифруются: Beget
login/password, приватный SSH-ключ. В API/логах секреты не отражаются.

## 3. Beget-клиент (перенос из mtplane с исправлениями)

Портировать `internal/providers/beget/client.go` методами:
`Auth, AddSSHKey, ListSSHKeys, GetRegions, GetList, GetInfo, CreateVps,
Remove`. Обязательные исправления дефектов mtplane:

- **Убрать `createBegetVpsWithFallback`** — повторные платные POST с
  мутацией тела запрещены. Один заказ = один вызов; неверная валидация
  ловится ДО вызова (каталог/регион/parameter-id).
- HTTP-клиент с контекстом/таймаутом; при таймауте после отправки —
  операция `unknown`, разрешение только через `GetList`-сверку.
- Ошибка удаления (`Remove`) **не очищает** `provider_vps_id`: ресурс
  остаётся, пока провайдер не подтвердит удаление.

## 4. Поток создания (пользовательский Beget)

1. Валидация: связка verified, лимиты пользователя, регион/тариф из каталога,
   подтверждение стоимости. Проверка идемпотентности по `idem_key`.
2. **ДО вызова API**: INSERT operations(state=pending) + cloud_resources.
3. Вызвать `CreateVps` **один раз**. Ответ с id → `confirmed` + vps_id.
4. Ошибка валидации → `failed` (без повторов). Таймаут/неоднозначность →
   `unknown`; фоновый reconcile: `GetList`, сопоставить по hostname/metadata
   (`sharedd node_id`), либо подтвердить, либо оставить `unknown` для ручного
   разбора. Никаких новых заказов.
5. Ожидание RUNNING → получение IP → установка профилем (§5) → регистрация
   агента в пуле.
6. Лимит: `max_creates_per_day` (конфиг) + cooldown после серии ошибок,
   персистится в БД, переживает рестарт.

## 5. Профили и Ansible

Модель: профиль = имя/описание/published + версии; версия = Ansible playbook
(YAML) + `vars_schema` (JSON-схема допустимых переменных). Пользователь
выбирает только published-профили и задаёт валидируемые параметры.
Произвольные playbook от пользователя не принимаются (первая версия).

Штатный профиль `sharedd-node`:
- фиксированная версия telemt, установка из закреплённого релиза;
- установка агента sharedd, конфиг с `authkey` пользователя и `[telemt] api_addr`;
- systemd-юниты, права файлов;
- idempotent: повторный прогон приводит VPS к состоянию без побочных эффектов;
- НЕ управляет покупкой/пересозданием; port из `[shared_proxy] port`;
  antiscan остаётся у агента.

Запуск: `ansible-playbook` (локально на регистраторе) через SSH к VPS,
ключ из `beget_links`. Только на VPS, никогда на хосте регистратора.
Таймаут и вывод — в `operations.last_error`/лог.

## 6. Запреты

- Нет фонового правила «держать N живых» — ошибка установки/здоровья
  оставляет VPS для повторной установки, не создаёт новый.
- Дополнительные платные IP и авто-пересоздание по health-check не входят.
- Удаление VPS — только явным действием пользователя/админа, с проверкой
  подтверждения у провайдера.

## 7. Тесты (минимум)

1. повторный запрос с тем же idem_key → та же операция, второй заказ не уходит.
2. параллельные запросы одного idem_key → один вызов CreateVps (мок-счётчик).
3. таймаут CreateVps → state=unknown, `GetList` подтверждает/отклоняет;
   автоповтор не отправлен.
4. ошибка Remove → vps_id сохраняется, state=deleting с backoff.
5. секреты Beget/SSH не появляются в ответах API и логах (проверка сериализации).
6. лимит max_creates_per_day и cooldown персистятся.
7. профиль: невалидные vars отклонены; повторный прогон Ansible — без изменений
   (проверка идемпотентности на моках runner).
