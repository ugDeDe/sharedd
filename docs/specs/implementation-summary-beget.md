# implementation-summary-beget.md — M2: личный Beget, профили, Ansible

Branch: `dev`. Spec: `docs/specs/beget-provisioning.md` (authoritative).
Порт-референс: `../mtplane/internal/providers/beget/client.go`,
`../mtplane/internal/cryptoutil/cryptoutil.go`. UI — вне scope (следующая фаза).
Существующие незакоммиченные M1/DC2/multi-master изменения не тронуты.

## Файлы

| Файл | Изменение |
|---|---|
| `registry/internal/cryptoutil/cryptoutil.go` | **new** — AES-GCM: `LoadMasterKey(path)` (32B, файл вне БД, 0600, битый не перезаписывается), `Seal`/`Open`, `GenerateSSHKeyPair` (ed25519, OpenSSH через `golang.org/x/crypto/ssh` — без самодельной маршалки) |
| `registry/internal/control/control.go` | расширение встроенной схемы: `beget_links`, `profiles`, `profile_versions`, `operations` (+ индексы), `cloud_resources` |
| `registry/internal/control/operations.go` | **new** — журнал платных операций: `CreateOperation` (UNIQUE(idem_key), персист ДО API), `OperationByIdemKey`/`Operation`, `UpdateOperationState`, `CreatesInLast24h`, `RecentCreateOps`, `UnknownOperations`; `UpsertCloudResource`/`SetCloudResourceState`/`CloudResource`; состояния `pending/sent/confirmed/unknown/failed/deleting/deleted`; `RequestJSON` — `json:"-"` |
| `registry/internal/control/beget.go` | **new** — `beget_links` CRUD; `BegetLinkView` без секретов; `MarkBegetLinkVerified`/`SetBegetLinkError`/`SetBegetLinkSSHKey` |
| `registry/internal/control/profiles.go` | **new** — профили и версии: `CreateProfile`, `AddProfileVersion` (сброс published), `PublishProfile`, `LatestPublishedVersion`, `ProfileVersion`; `ValidateVars` (минимальная JSON-схема: required + типы, неизвестные ключи отклоняются) |
| `registry/internal/beget/client.go` | **new** — порт с исправлениями: `Auth/AddSSHKey/ListSSHKeys/GetRegions/GetList/GetInfo/CreateVps/Remove`; все вызовы принимают `context`; **авто-ре-auth на 401 только для GET** (неидемпотентный create не повторяется); **нет `createBegetVpsWithFallback`**; секретов в текстах ошибок нет; `APIError.IsValidation()` |
| `registry/internal/provision/provision.go` | **new** — машина состояний: `Create` (идемпотентный шлагбаум, лимиты, валидация региона до платного вызова, РОВНО ОДИН CreateVps, классификация failed/unknown), `Reconcile`/`ReconcileUnknown` (сверка GetList по hostname), `Remove` (ошибка сохраняет `provider_vps_id`, state=deleting + backoff), `ApplyProfile` |
| `registry/internal/ansible/runner.go` | **new** — закреплённый playbook, SSH только на VPS; reject localhost/loopback/хоста регистратора; защита пути playbook; парсинг `changed/failed/unreachable`; таймаут |
| `registry/internal/config/config.go` | `[database] master_key_path` (дефолт `master.key` рядом со state-файлом); секция `[provisioning]` (`max_creates_per_day` деф. 5, `cooldown_minutes` деф. 30) + валидация |
| `registry/internal/cryptoutil/cryptoutil_test.go`, `registry/internal/control/beget_test.go`, `registry/internal/beget/client_test.go`, `registry/internal/provision/provision_test.go`, `registry/internal/ansible/runner_test.go` | **new** — обязательные тесты §7 (см. ниже) |

## Деньги-критичные инварианты — как соблюдены

1. **Ровно один CreateVps на idem_key.** `operations.idem_key UNIQUE` + `CreateOperation` = атомарный шлагбаум под одной SQLite-транзакцией; проигравший гонку получает `created=false` и **не** вызывает API. Тесты `TestCreateIdempotentReplay`, `TestCreateConcurrentOnePaidCall`.
2. **Операция персистится ДО API.** INSERT(state=pending) в `Create`, и только затем `CreateVps`.
3. **Таймаут/неоднозначность → unknown, разрешение только GetList.** `settleCreate` классифицирует: `APIError` 4xx → failed (без повторов); сеть/deadline/5xx/пустой ответ → unknown. `Reconcile` сопоставляет по `hostname` (=`sharedd-<node_id>`), подтверждает либо оставляет unknown. Тесты `TestCreateTimeoutThenReconcile`, `TestReconcileUnknownStaysUnknown`.
4. **Ошибка/здоровье/установка не порождают заказ/пересоздание.** `ApplyProfile` не имеет пути к `CreateVps`; ни один код не вызывает create из install/health. Тест `TestApplyProfileVarsAndIdempotency` проверяет `createCalls==0`.
5. **Ошибка Remove сохраняет `provider_vps_id`.** `Remove` при ошибке оставляет state=deleting и прежний vps_id; повтор в пределах backoff → `ErrBackoff` без вызова. Тест `TestRemoveFailureKeepsVPSID`.
6. **Секреты не в API/логах.** `BegetLink` и `Operation.RequestJSON` помечены `json:"-"`; `BegetLinkView` их не содержит; ошибки провайдера без login/password. Тесты `TestBegetLinkViewHasNoSecrets`, `TestInternalBegetLinkJSONHasNoSecrets`, `TestOperationJSONOmitsRequest`, `TestAuthErrorDoesNotLeakPassword`.

## Обязательные тесты §7

| # | Поведение | Тест(ы) | Статус |
|---|---|---|---|
| 1 | повтор idem_key → та же операция, второй заказ не уходит | `TestCreateIdempotentReplay` | ✅ |
| 2 | параллельные запросы одного idem_key → один CreateVps | `TestCreateConcurrentOnePaidCall` (8 горутин, мок-счётчик) | ✅ |
| 3 | таймаут CreateVps → unknown; GetList подтверждает/отклоняет; автоповтор не отправлен | `TestCreateTimeoutThenReconcile`, `TestReconcileUnknownStaysUnknown` | ✅ |
| 4 | ошибка Remove → vps_id сохраняется, deleting + backoff | `TestRemoveFailureKeepsVPSID`, `TestRemoveSuccess` | ✅ |
| 5 | секреты Beget/SSH не в ответах/логах (сериализация) | `TestBegetLinkViewHasNoSecrets`, `TestInternalBegetLinkJSONHasNoSecrets`, `TestOperationJSONOmitsRequest`, `TestAuthErrorDoesNotLeakPassword` | ✅ |
| 6 | лимит max_creates_per_day и cooldown персистятся | `TestLimitsPersistAcrossReopen`, `TestCooldownPersistsAcrossReopen` | ✅ |
| 7 | профиль: невалидные vars отклонены; повторный прогон Ansible без изменений | `TestApplyProfileVarsAndIdempotency`, `TestValidateVars` | ✅ |

Плюс: `cryptoutil` (master.key персист/права/corrupt, seal/open, ssh-keypair), `beget` (single-call, no-retry-on-401, GET-reauth, remove-error), `ansible` (reject localhost/reg-host, path-escape, parse changed, idempotent 2nd run, timeout).

## Проверка

```
cd registry
gofmt -l internal/                 → clean
go vet ./...                       → clean
go test -race ./...                → все пакеты ok (новые: ansible, beget, cryptoutil, provision; control расширен)
go build -mod=readonly ./...       → ok
~/go/bin/golangci-lint run --timeout 5m ./...  → 0 issues
```

Ansible (`/usr/bin/ansible-playbook`) присутствует; тесты раннера используют мок-команду (реальные playbook/SSH не запускаются). Реальные Beget API не вызываются — только `httptest`/мок-транспорт.

## Отклонения / риски

1. **Wiring в сервер не делался** — по scope UI/endpoints следующая фаза. Пакеты `beget/provision/ansible` не подключены к `buildMux`; схема расширена в `control.Open`, поэтому таблицы создаются сразу.
2. **`master_key_path`/`[provisioning]`** добавлены в конфиг и валидируются, но пока не потребляются сервером (следующая фаза — provisioning-API).
3. **Сопоставление при reconcile по `hostname = "sharedd-"+lower(node_id)`.** Если провайдер нормализует/обрезает hostname, сверка не найдёт VPS и операция останется `unknown` (безопасно: ручной разбор, новый заказ не уйдёт). Risk-note для фазы каталога/UI.
4. **GET-ре-auth при 401** оставлен (идемпотентно). Для неидемпотентных POST автоповтор убран полностью — это исправление дефекта mtplane.
5. **Cooldown/лимит derived из `operations`** (без отдельной таблицы): считается по состояниям sent/confirmed/unknown за 24ч и последним create-операциям. Меньше кода и нет дрейфа между двумя источниками.
6. **`PublishProfile` сбрасывает published при новой версии** — оператор публикует явно; защищает от тихой подмены установщика.

Not committed — рабочее дерево на `dev`, как указано.
