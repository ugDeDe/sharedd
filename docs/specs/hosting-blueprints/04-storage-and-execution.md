# 04. SQLite, операции и durable исполнение

**Предлагаемая модель, не существующая миграция.** Общие требования —
[01](01-requirements.md), wire API — [03](03-cabinet-api.md), блоки —
[05](05-blueprint-profiles.md).

## 1. Существующие данные и ограничения

Control store — SQLite (`registry/internal/control`), не PostgreSQL.
Operational candidates/assignments/bans находятся в JSON state; history —
другая SQLite. Нет общей транзакции между JSON state и control SQLite.

- `beget_links.user_id UNIQUE`: одна связка на пользователя, credentials
  уже зашифрованы; существующий клиент/SSH key привязан к ней.
- `profiles` и `profile_versions`: immutable номер версии и текст `playbook`;
  операции содержат только `profile_id`. Текущий resume загружает latest,
  поэтому исторической точной версии нельзя достоверно восстановить по ID.
- `cloud_resources.vps_id PRIMARY KEY`: недостаточно для нескольких
  providers/connections; external IDs могут совпадать.
- `node_owners`, `quota_reservations`, `pending_node_secrets` — постоянные
  ownership и claim/quota evidence. Prune candidate их не удаляет.
- `operations` и `operation_steps` уже используются API и worker; оставить
  ID/историю и согласовать их с новыми checkpoints, не создавать второй
  независимый paid operations service.

## 2. Минимальное расширение схемы

Это логический каталог колонок/ограничений для миграции; не SQL для запуска.
Integer FK — внутренние ID, timestamps UTC. JSON-поля ограничены размером
и проверяются приложением. Foreign keys включены на всех соединениях.

| Сущность | Обязательные данные / ограничения |
|---|---|
| `hosting_connections` | id PK, user_id FK, provider enum, label, encrypted credential bundle, credential_revision, revision, verified_at, capabilities snapshot, created_at; индекс owner |
| `connection_balances` | connection_id PK/FK, user_id, credential_revision, amount TEXT nullable, currency nullable, scope, checked_at, last_attempt_at, safe_error, next_refresh_at; запись CAS revision |
| `cloud_resources` новая форма | id surrogate PK, connection_id FK, user_id, external_id nullable, node_id nullable, state, revision, configuration snapshot/ref, installation_profile_version_id nullable, vars snapshot, replaces_resource_id, replaced_by_resource_id, managed-IP provenance, retirement; UNIQUE(connection_id,external_id) для non-null |
| `management_policies` | id PK, owner, current_resource_id nullable, current_node_id, mode manual/profile, revision, profile_version_id nullable, encrypted/validated input refs snapshot, activation episode watermark, process_current_ban; UNIQUE(current resource), сохраняется после candidate prune |
| `profile_versions` расширение | format text/graph, blueprint_json nullable, schema_version, content_hash; прежние playbook/vars_schema сохраняются; UNIQUE(profile_id,version), неизменяемое содержимое |
| `operations` расширение | connection_id, resource_id, profile_version_id nullable, run_id nullable, request_hash, credential_revision, dispatch_generation, result certainty; прежние колонки остаются для adapters |
| `command_keys` | user, method, path, key, hash, operation_id, immutable ack; UNIQUE(user,method,path,key) |
| `profile_runs` | id, owner, pinned version, entry, policy_id/revision, inputs snapshot, current resource, state/revision, next_wake_at, cancel_requested, created/updated_at |
| `block_checkpoints` | run_id, node_id, invocation index, state, attempt, intent/hash/key, provider_operation_id, effect certainty, encrypted output refs, error, next_attempt_at, lease generation; UNIQUE(run,node,invocation) |
| `ban_deliveries` | policy_id, stable episode_id, delivery_id, run/checkpoint, state; UNIQUE(policy_id,episode_id), UNIQUE(delivery_id) |
| `replacement_reservations` | owner, policy_id, old_resource_id, new_resource_id, episode_id, state, generation; не больше одной unresolved replacement на policy и old resource |
| `retirement_fences` | owner, resource_id, node_id/identity generation, reason superseded/deleting, revision, created_at, confirmed_deleted_at; admission смотрит здесь независимо от candidates |

Не дублировать секреты в нескольких plain JSON: credentials и временные
root/SSH/token outputs шифруются действующим механизмом control store;
graph/checkpoint хранит secretref. Ключ шифрования — вне БД, backup требует
его отдельного защищённого сохранения. Redacted вывод/ошибки ограничить
64KiB на step; streaming также с redaction, не только финальная карточка.
Удаление secret возможно лишь когда нет unresolved checkpoint, которому
он нужен для reconcile. Старые владельцы не получают секреты новой связи.

## 3. Policy, identity и квота

Policy — самостоятельная сущность, переживает очистку candidate и переводится
на replacement при switch. Node management lookup идёт через owner/policy,
а не только map живых candidates. Старая identity остаётся связанной с fence.
Новая VPS получает новый node_id/pending secret; на той же VPS смена IP
не меняет node_id, owner или personal secret. Нельзя переносить файл
`/var/lib/sharedd/node_id` со старой VPS на replacement.

Обычный create транзакционно резервирует место с учётом persistent owners,
pending claims, active resources и unresolved purchase intents; одно
логическое место не считать несколько раз. Полная квота может временно
допустить **одну связанную replacement**, если старая VPS принадлежит policy:
это reservation пары old/new, а не увеличение max_nodes. Pending secret
новой ноды получает только этот reservation; другой create его не использует.

Условия освобождения: подтверждённо failed до создания либо подтверждённое
удаление соответствующего ресурса/claim. `cancel_requested`, timeout и unknown
не освобождают слот. После допуска new switch переносит обычный слот на new;
old удерживает cleanup obligation и fence, блокируя следующую replacement
до завершения очистки или явного admin решения. Так невозможно нарастить
цепочку оплачиваемых VPS при повторяющихся банах.

## 4. Durable ban delivery без ложного atomic outbox

Источник события — registry verdict `ip_ban`, не сообщение агента и не
неуспешные метрики. В JSON ban record и persisted history event записать
стабильный `episode_id`: один на переход в ban, повтор report/tick его
не меняет. Новый ban после подтверждённого recovery — новый episode ID.
Legacy ban без ID получает ID один раз с сохранением до доставки.

Поскольку JSON и control SQLite разные stores, **нельзя утверждать атомарный
outbox**. Протокол:

1. Persist ban + episode в owning state/history пути, затем wake reconciler.
2. Reconciler периодически и на startup сканирует persisted ban episodes,
   upsert `ban_deliveries` с UNIQUE(policy,episode). Crash между шагами
   восстанавливается повторным сканированием.
3. Crash после delivery insert до scheduler wake: next_wake_at/queued
   deliveries подбираются на startup, доставка at-least-once.
4. Worker CAS claim delivery; повторная доставка видит checkpoint и не
   начинает вторую покупку. Перед paid dispatch заново проверить mode,
   policy revision, current resource, активный episode и owner enabled.
5. Если GP recovery снял ban до dispatch — `skipped_recovered`, без покупки.
   Если уже отправлено — reconcile, не удалять оплаченный ресурс самовольно.

Observation хранит watermark. При activation с process_current_ban=false
текущий episode считается увиденным; после restart он не превращается в
«новый». При true разрешён ровно этот текущий episode. `dead`/prune/metrics
не генерируют provider mutation и не маскируются под ip_ban.

## 5. Исполнитель: порядок транзакций и сетевых действий

### 5.1 Состояния

Run: `pending → running → waiting_event|waiting_retry|reconciling|paused|needs_attention
→ running → succeeded|failed|cancelled`.
`cancel_requested` — durable флаг промежуточного состояния; detail может
показывать его как состояние до завершения in-flight reconciliation.
Долгая подписка в waiting_event не держит goroutine/SSH/TCP соединение.

Resource: `planned → provisioning → installing → admission_pending → admitted
→ superseded|deleting → deleted`; альтернативы `unknown|error` сохраняют ID
и certainty, а не обнуляют оплаченный объект. Failed installation оставляет
реальную VPS управляемой и позволяет отдельное явное recovery.

Checkpoint: `ready → claimed → dispatched → confirmed|rejected|unknown`;
read-only waits также могут быть waiting. Unknown SSH ведёт needs_attention,
unknown provider mutation — reconciling. Исходные legacy operation states
сохраняются, новая detail view маппит расширенные states явно, без переписи
старой истории.

### 5.2 Одна mutation

1. Короткая SQLite transaction: validate references, quota, policy revision;
   создать operation/resource/checkpoint, command key и response ack.
2. Worker берёт lease через CAS `state,revision,lease_generation`; один активный
   effect на ресурс. Записать точный intent, provider key и `dispatched` до I/O.
3. Закрыть transaction/глобальные locks. Выполнить ограниченный context I/O.
4. Короткая transaction: записать outcome и redacted output только если lease
   generation совпадает; stale worker не переписывает новый checkpoint.
5. Persist next wake / branch. Следующий worker продолжает по checkpoints.

Dispatch CAS — граница in-flight для mode flip/cancel. Она сериализуется с
изменением policy revision; захваченный запрос может физически уйти уже после
возврата PUT manual, что отражается в UI как ранее начатый in-flight. Для
не начатых dispatch после PUT manual полномочий нет. При падении между
`dispatched` и реальной отправкой безопасно считать результат unknown.

Lease expiry не даёт права второму worker повторить mutation, пока старый
вызов теоретически жив. Новый worker делает reconciliation; старое поколение
лишено записи. Один процесс/SQLite writer не решает внешнее exactly-once.
Nodehost stable key помогает create, но Beget и arbitrary SSH требуют
консервативного unknown. Mutex/SQLite transaction никогда не держится через
provider HTTP, GP verify, SSH или ожидание события.

### 5.3 Retry, cancel, resume

Safe reads: bounded retry в пределах deadline. Provider pending: persisted
poll, не бесконечный loop. Mutations: retry только с документированной
идемпотентностью и тем же intent/key; иначе reconcile. Cleanup имеет отдельный
checkpoint и retry budget, никогда ребро обратно к create.

Cancel/run disable/mode flip отменяет contexts и будущие effects. Если
запрос мог уйти, reconciler дочитывает результат read-only, сохраняет
реальные resources/slots и показывает needs_attention; «cancel» не refund.
Run resume не меняет pinned version/входы/разрешения. Ручное подтверждение
не превращает неизвестную shell-команду в safe retry. User disabled или
выключенные private profiles прекращают future dispatch и подписки;
разрешено только безопасное reconciliation начатого эффекта.

### 5.4 Fence и переключение

После доказанного admission new записать durable retirement fence old,
затем CAS current resource/policy generation в SQLite. Admission old обязан
читать fence до claim, включая startup recovery; память кэшируется только
с безопасной инвалидизацией. Crash между fence и switch возобновляет switch,
не снимает fence автоматически. JSON candidate cleanup после этого
идемпотентен; dual-store reconcile восстанавливает расхождения.
Перед provider delete ещё раз проверить old/new IDs, admission evidence и
ownership. Confirm absence через provider GET/list; только затем old deleted
и cleanup reservation завершена. При провале delete оставить current=new,
old fenced/deleting. Повтор cleanup не идёт через initial/create.

## 6. Миграция и совместимость

1. Backup control/history/JSON и encryption key до schema upgrade; записать
   schema version и preflight counts. Проверить restore на копии.
2. Additive tables/columns с nullable pinned version для legacy. Backfill
   каждой `beget_links` в одну Beget connection с owner и теми же encrypted
   credentials; durable mapping legacy_link_id → connection_id UNIQUE.
3. Для смены PK `cloud_resources` создать новую SQLite таблицу и mapping,
   копировать в transaction; старый vps_id сохранить как external_id именно
   связанной owner Beget connection. Конфликты/missing links — карантин
   миграции и report, не присвоение случайной connection и не потеря строки.
4. Backfill manual policy всем owned nodes/resources, включая pruned owners.
   Не включать PROFILE из наличия старого профиля установки.
5. Старым profile_versions установить format=text, не менять playbook.
   Новые runs обязаны pin. Legacy operations оставить с nullable pin и
   меткой `legacy_unpinned`; не приписывать им последнюю версию как исторический
   факт. При явном новом retry пользователь выбирает/подтверждает snapshot,
   который сохраняется для новой попытки, исходная история неизменна.
6. Переключить legacy adapters на единые services, обеспечить один quota
   ledger и owner mapping; не запускать два workers на одну операцию.
7. Миграция повторяемая, resumable после crash; UNIQUE mapping не допускает
   дублей. Проверить old/new API на одной БД. Backup restore — путь rollback,
   не попытка запустить старый бинарник на несовместимо изменённой схеме.

Тесты: пустая БД, реальный legacy fixture без секретов, две connections с
одинаковым external ID, pruned owners, pending secrets, legacy unknown и
installing operations, interrupted backfill, повтор upgrade, восстановление
backup. Не стирать пользовательские IP, профили и собственные SSH hosts при
переходе на новую схему.
