# implementation-summary-manual-servers.md — свой токен нод и свои серверы

Ветка `dev`. Реализация `docs/specs/addendum-manual-servers.md` (§1 self-rotate
authkey, §2 manual hosts, §4 тесты). Переиспользует существующие кирпичи:
`control.RotateAuthkey`/`RevokeAuthkeys`, `control.AuthorizeNode`/`ReleaseNode`,
`provision.InstallProfile`, `cryptoutil`, `sshx`, `machineapi.ValidatePublicIPv4`.

## Статус по пунктам задания

| # | Пункт | Статус | Где |
| --- | --- | --- | --- |
| 1 | `POST /api/me/authkey/rotate` (полный ключ один раз, отзыв всех), `DELETE /api/me/authkey` (204), `GET` — prefix-only; один ключ на ПОЛЬЗОВАТЕЛЯ | ✅ | `cabinet_api.go` |
| 2 | `manual_hosts` + ensureColumn-миграция; GET/POST `/api/me/servers`, POST `.../{id}/install`, DELETE `.../{id}`, POST `.../{id}/check`; валидация по контракту; AES-GCM, секрет нигде не отдаётся/не логируется, маскируется в выводе | ✅ | `control/control.go`, `control/manual_hosts.go`, `server/cabinet_manual.go` |
| 3 | Тот же executor/шаги и деньги-инварианты, что у Beget: слот на сервер при accept, предсозданные `node_id`+`node_secret`, `done` только после подтверждённой регистрации, abort на первом шаге, ноль провайдерских вызовов; retry — та же нода; общая install-логика вынесена в `provision.InstallProfile` (не дублируется) | ✅ | `provision/provision.go`, `server/cabinet_manual.go` |
| 4 | Owner-проверки на каждом объекте; квота; DELETE освобождает слот и не трогает пул-ноду | ✅ | `cabinet_manual.go`, `control/manual_hosts.go` |
| 5 | Тесты = §4 items 1-6 (fake SSH, без сети) | ✅ | `server/manual_servers_test.go` |

## Эндпоинты

| Метод/путь | Ответ / коды |
| --- | --- |
| `POST /api/me/authkey/rotate` (session+CSRF) | 200 `{authkey}`; отзывает все прежние ключи |
| `DELETE /api/me/authkey` (session+CSRF) | 204 |
| `GET /api/me/servers` | 200 `{servers:[…]}` (без секретов; `operation?` — если установка запускалась) |
| `POST /api/me/servers` | 201; 400 `validation_failed` |
| `POST /api/me/servers/{id}/install` | 200 операция; 403 `quota_exceeded`; 400 `ssh_unavailable`; 409 `already_done`/`in_progress`; 404 `not_found` |
| `POST /api/me/servers/{id}/check` | 200 `{verified,last_error}` |
| `DELETE /api/me/servers/{id}` | 204; 404 `not_found` |

## Файлы

| Файл | Изменение |
| --- | --- |
| `internal/control/control.go` | Схема `manual_hosts` + ensureColumn `node_id`/`operation_id` |
| `internal/control/manual_hosts.go` | НОВЫЙ: тип `ManualHost` + CRUD/`SetManualHostCheck`/`SetManualHostNode` |
| `internal/control/operations.go` | `OpKindManualInstall` |
| `internal/provision/provision.go` | `InstallRequest`: `Port`/`Secrets`/`Confirm`; маскировка секретов и пост-проверка перед `done`; публичный `SSHExecutor()` |
| `internal/sshx/sshx.go` | экспорт `ValidatePrivateKey` |
| `internal/server/cabinet_api.go` | маршруты и хендлеры rotate/delete authkey |
| `internal/server/cabinet_manual.go` | НОВЫЙ: `/api/me/servers` (list/create/install/check/delete), валидация, `registryIP`, установка через `provision.InstallProfile` |
| `internal/server/run_main_impl.go` | `mountMeServers` |
| `internal/server/manual_servers_test.go` | НОВЫЙ: §4 items 1-6 + owner checks + check |
| `docs/api-contract.md` | §2.1 строки + §2.2 (валидация и коды) |

## Тесты

- `TestAuthkeyRotateDeleteAndIsolation` — §4.1: новый ключ жив, старый немедленно
  мёртв, `DELETE` отзывает, чужой ключ не затрагивается.
- `TestManualServerValidationMatrix` — §4.2: private/loopback/link-local/registry
  IP, порт 0/70000, оба секрета, ни одного, битый ключ, ключ > 16 KiB, bad
  `ssh_user`, пустой label → 400 с причиной; валидные password/key → 201.
- `TestManualInstallOneSlotAndRetryNoProviderCalls` — §4.3: один слот,
  повторный install переиспользует ту же операцию и ноду, `fakeBeget.creates()==0`.
- `TestManualServerSecretsNeverLeaked` — §4.4: секрет отсутствует в ответах и в
  `operations.last_error` (замаскирован), в БД — шифротекст.
- `TestManualInstallInjectsSecretAndConfirmsRegistration` — §4.5: шаги несут
  `--node-secret`, секрет привязан к ноде, `done` только при регистрации.
- `TestManualServerDeleteReleasesSlotKeepsPoolNode` — §4.6: удаление освобождает
  слот, нода остаётся в пуле.
- `TestManualServerOwnerChecks`, `TestManualServerCheck` — owner-проверки и check.

## Отклонения и оставшиеся пробелы

- «Секрет не в логах» проверяется косвенно (код секрет не логирует; ответы и
  `operations.output` покрыты тестом). Отдельного перехвата `log` нет.
- `check` выполняет `WaitSSH` + `cat /etc/os-release`; «версия ОС» только
  проверяется на доступность, не парсится (контракт не требует разбора).
- Реестр-reject IP работает, когда `panel.public_url` задан IP-адресом; при
  hostname-е собственный IP не резолвится (config не несёт отдельного поля).
- Manual-install не участвует в Beget-логике cooldown/`max_creates_per_day`
  (платных вызовов нет); место в квоте общее через `node_owners`.
- UI раздела «Свои серверы»/«Ключ доступа» (§3) не входит в этот бэкенд-объём.

## Раунд 7 — fix B1/B2 (QA design-qa-report.md, Round 7)

- **B2 — один сервер = одно место.** `manual_hosts` получила
  `UNIQUE(user_id, host, port)` (создаётся как `CREATE UNIQUE INDEX IF NOT EXISTS
  ux_manual_hosts_user_host_port`; табличная UNIQUE-константа для существующих БД
  в SQLite недоступна). Миграция безопасна для БД с уже накопленными дублями
  (`migrateManualHostUniqueness`): дубли схлопываются (сохраняется запись с
  операцией, иначе самая ранняя), у удаляемых строк снимаются `node_owners` и
  `quota_reservations`, затем создаётся индекс. `CreateManualHost` на нарушение
  UNIQUE возвращает `control.ErrDuplicate`; `POST /api/me/servers` маппит его в
  **409 `duplicate_host`** с русским сообщением. Второй слот квоты и вторая
  личность ноды не создаются. UI дополнительно блокирует повтор до отправки.
- **B1 — русские сообщения валидации.** `normalizeManualHost` отдаёт русские
  `message` (проверка IPv4/публичности/порта/ssh_user/секрета/ключа),
  `writeProfileSelectErr` — русские тексты (`user_profiles_disabled`,
  `profile_unavailable`, `not_found`); машиночитаемые `error`-коды не изменены.
  Общий `machineapi.ValidatePublicIPv4` остаётся английским для агента — перевод
  делается на слое кабинета.
- Тесты: `TestManualHostUniqueConstraintAndDedupeMigration` (control),
  `TestManualServerDuplicateHostRejected`, `TestProfileSelectErrorsAreRussian`,
  расширенный `TestManualServerValidationMatrix` (точные русские подстроки + наличие кириллицы).

Изменённые файлы: `internal/control/control.go`, `internal/control/manual_hosts.go`,
`internal/server/cabinet_manual.go`, `internal/server/cabinet_beget.go`,
`internal/control/control_test.go`, `internal/server/manual_servers_test.go`,
`docs/api-contract.md §2.2`.
