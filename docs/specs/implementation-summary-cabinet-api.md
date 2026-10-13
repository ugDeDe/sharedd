# implementation-summary-cabinet-api.md — Кабинет пользователя + session API

Ветка `dev`. Контракт: `docs/api-contract.md` §2.1 (authoritative endpoint list),
`docs/specs/beget-provisioning.md`. Область: **registry module only**; UI,
реальные вызовы Beget, commit/push — вне области.

## Files changed

| File | Change |
|---|---|
| `registry/internal/control/control.go` | `node_owners.deleting` в схеме + идемпотентная мини-миграция `ensureColumn` (PRAGMA table_info → ALTER TABLE); сентинель `ErrUserDisabled` |
| `registry/internal/control/users.go` | **new** `Authenticate(username,password)` — bcrypt-вход, один `ErrBadCredentials` для unknown/неверного пароля, `ErrUserDisabled` для заблокированного |
| `registry/internal/control/sessions.go` | `DeleteSession(token)` (logout) |
| `registry/internal/control/authkeys.go` | `LiveAuthkey(userID)` → prefix/created_at/active без полного ключа |
| `registry/internal/control/nodes.go` | `OwnedNode`, `NodesByUser(userID)` (legacy=0), `MarkNodeDeleting(nodeID)` (квоту не освобождает) |
| `registry/internal/control/operations.go` | `OperationsByUser(userID)` (включая `state=unknown`) |
| `registry/internal/server/cabinet_api.go` | **new** — session middleware, CSRF double-submit, IP rate-limit, `POST /api/auth/{register,login,logout}`, `GET /api/me`, `/api/me/nodes`, `/api/me/authkey`, `/api/me/profiles`, `DELETE /api/me/nodes/{node_id}` |
| `registry/internal/server/cabinet_beget.go` | **new** — Beget link (PUT/GET), regions, `POST /api/me/vps` через `provision`, `GET /api/me/operations`, `POST /api/me/operations/{id}/cancel` |
| `registry/internal/server/users_control.go` | `openControl` грузит AES-GCM master key (`cryptoutil.LoadMasterKey`), деградация beget-секции до 503 при неудаче |
| `registry/internal/server/run_main_impl.go` | поля `masterKey`, `begetFactory`, `regLimiter` + `rateMu`; `mountCabinet` в `buildMux` |
| `registry/internal/server/cabinet_api_test.go` | **new** — обязательный список тестов (ниже) |

## Endpoints (docs/api-contract.md §2.1)

| Метод/путь | Поведение |
|---|---|
| `POST /api/auth/register` | `{username,password}` → role=user, max_nodes=0; rate-limit 5/10мин по IP → 429 `rate_limited` |
| `POST /api/auth/login` | bcrypt → cookie `sharedd_session` (HttpOnly,Secure,Lax) + `sharedd_csrf` (читаемая) |
| `POST /api/auth/logout` | session+CSRF → удалить сессию, снять куки, 204 |
| `GET /api/me` | `{username,role,max_nodes,used_nodes}` |
| `GET /api/me/nodes` | свои ноды: `{node_id,ip,status,quarantine?,master_domains?,availability,dc2_ok}` — без provider-id/ssh |
| `GET /api/me/authkey` | `{prefix,created_at,active}` — полный ключ не отдаётся |
| `GET /api/me/profiles` | только `published=1` (+ `vars_schema` последней версии) |
| `PUT /api/me/beget/link` | `{login,password,code?}`: Auth + AddSSHKey (мок в тестах), AES-GCM Seal, секреты не возвращаются; отказ провайдера → 400 `beget_auth_failed` с `reason` (`bad_credentials`/`code_required`/`rate_limited`/`provider_error`) |
| `GET /api/me/beget/link` | `{verified,last_error}` — без секретов |
| `GET /api/me/beget/regions` | каталог через проверенную связку |
| `POST /api/me/vps` | confirm:true + idem_key обязательны; квота; профиль published+vars; → операция |
| `GET /api/me/operations` | журнал (вкл. `unknown`), без provider_vps_id/idem_key |
| `POST /api/me/operations/{id}/cancel` | pending/failed → `failed("cancelled by user")`; sent/confirmed/unknown/deleting/deleted → 409 `already_sent` |
| `DELETE /api/me/nodes/{node_id}` | owner-check → 404 чужим; `MarkNodeDeleting`, квота держится |

## Инварианты

- **Session**: cookie `sharedd_session`, HttpOnly+Secure+SameSite=Lax, TTL 7д из `sessions`
  (M1). CSRF — double-submit: cookie `sharedd_csrf` (не HttpOnly) vs заголовок
  `X-CSRF-Token`, `subtle.ConstantTimeCompare`; применяется ко всем мутирующим
  `/api/me/*` и `logout`.
- **Ownership**: каждая объектная операция проверяет владельца; чужие объекты →
  404 (существование не подтверждаем). Чужой `idem_key` в `POST /api/me/vps` → 404.
- **VPS-заказ через `provision`**: handler не вызывает `CreateVps` сам. Передаёт
  управление `provision.Create`, который (a) персистит `operations(pending)` ДО
  платного вызова, (b) `UNIQUE(idem_key)` + ON CONFLICT → ровно один `CreateVps`
  на ключ в т.ч. под гонкой. `node_id` детерминирован из `idem_key`
  (`vps<6hex>-<5hex>` — 44 бита, валиден для `machineapi.ValidateNodeID`).
- **Квота линейно-атомарна**: `provision.Create` в ОДНОЙ транзакции резервирует
  место (distinct node_id по `node_owners ∪ cloud_resources ∪ заказам`) и
  проверяет `max_nodes`, `max_creates_per_day`, cooldown и отсутствие другого
  незавершённого заказа (H3). Подтверждённый VPS расходует место (H1).
- **Секреты**: Beget login/password/SSH-priv и `provider_vps_id` не сериализуются
  в API-представлениях (`BegetLinkView`, `operationJSON`, `nodeJSON`).
- **Fail-closed**: `[users] enabled=false` → 503 `users_disabled`; control-БД/мастер-ключ
  недоступны → 503 `control_unavailable`/`secrets_unavailable`.

## Test status — обязательный список

| Требование | Тест | Статус |
|---|---|---|
| register max_nodes=0 блокирует VPS create (без вызова провайдера) | `TestCabinetRegisterDefaultQuotaBlocksVpsCreate` | ✅ |
| cross-user: чужая нода/операция | `TestCabinetCrossUserAccessDenied` | ✅ |
| CSRF обязателен на POST | `TestCabinetCSRFRequiredOnPost` | ✅ |
| истечение сессии | `TestCabinetSessionExpiry` | ✅ |
| idem_key replay → та же операция | `TestCabinetVpsIdempotentReplay` | ✅ |
| конкурентный replay → один CreateVps | `TestCabinetVpsConcurrentSameIdem` | ✅ |
| секреты отсутствуют во всех ответах | `TestCabinetSecretsNeverReturned` | ✅ |

Плюс: `TestCabinetAuthkeyPrefixOnly`, `TestCabinetLogout`,
`TestCabinetRegisterRateLimited`, `TestCabinetCancelRefusedAfterSent`.

## Verification

```
cd registry
gofmt -l .                      → clean
go vet ./...                    → clean
go test -race ./...             → ok (control, provision, server, …)
go build -mod=readonly ./...    → ok
golangci-lint run ./...         → 0 issues
```

## Deviations / notes

1. **CSRF только на сессионных маршрутах**: `register`/`login` без CSRF (сессии
   ещё нет — double-submit бессмыслен); мутирующие `/api/me/*` и `logout` — с CSRF.
2. **Cancel через `failed`**: состояния `cancelled` в beget-provisioning §1 нет,
   поэтому отмена pending/failed пишет `state=failed, last_error="cancelled by
   user"` (не добавляет новое состояние и не путает cooldown-эвристику). `sent`+
   защищены 409.
3. **`dc2_ok` в списке нод** = `Candidate.MetricsOK`: для dc2-осведомлённых агентов
   реестр уже пишет туда `dc2_ok` (healthcheck.go), отдельное поле в state не
   заводили.
4. **`node_id` из `idem_key`**: контракт §2.1 не даёт node_id в теле `POST /api/me/vps`;
   он детерминированно выводится из `idem_key` (`vps<6hex>-<5hex>`, 44 бита, формат
   NAME-HASH) — стабилен для сверки/provision.
5. **Rate-limit по `RemoteAddr`**: за реверс-прокси это IP прокси. Кабинет:
   5 регистраций/10мин, логин 10/15мин на IP + 5 неудач/15мин на username
   (in-memory, карта ограничена). X-Forwarded-For — при необходимости.
6. **Master key грузится в `openControl`** (когда `[users] enabled=true` и путь
   задан); при неудаче пул работает, beget-секция → 503.
7. **`DELETE /api/me/nodes/{node_id}`** ставит durable-маркер `node_owners.deleting`
   (новая колонка + PRAGMA-миграция); физическое удаление/освобождение квоты
   остаются за release-node/ops-фазой.

Не закоммичено — рабочие изменения на `dev`, M1/DC2/multi-master/beget не
затронуты и не реверчены.

---

## Fix round — security-review + code-review (2026-10-12)

Источники: `docs/specs/security-review-report.md`, `docs/specs/code-review-report.md`.
Порядок — по приоритету отчёта. Каждой находке — детерминированный тест.

| Finding | Fix | File(s) | Test |
|---|---|---|---|
| **C1** (security, CRITICAL) — неатомарные квоты, N разных `idem_key` → N `CreateVps` | Новая атомарная резервация `Store.ReserveCreateOperation`: одна транзакция — INSERT pending ON CONFLICT, затем (место, дневной лимит, cooldown, H3) поверх вставленной строки; нарушение → ROLLBACK + sentinel. `provision.Create` использует её вместо pre-check + `CreateOperation`. | `internal/control/operations.go`, `internal/provision/provision.go` | `TestCreateDistinctKeyBlockedWhilePending`, `TestCabinetVpsConcurrentDistinctIdemOnePaidCall` (N конкурентных разных ключей при quota=1 → ровно 1 `CreateVps`, ровно 1 подтверждённая операция) |
| **H1** (security) — `max_nodes` считал `node_owners`, а не провиженные VPS | Место = `COUNT(DISTINCT node_id)` по `node_owners(legacy=0) ∪ cloud_resources(state≠deleted) ∪ operations(state NOT IN failed/deleted)`; включает только что вставленную pending-строку. Advisory `UsedSlots` в handler для fail-fast до Beget-связки. | `internal/control/operations.go`, `internal/server/cabinet_beget.go` | `TestCreateQuotaCountsProvisionedVps`, `TestCabinetVpsQuotaCountsProvisionedVps` |
| **H3** (security) — нет блока нового заказа при незавершённом | Резервация отклоняет новый `idem_key`, если у пользователя есть create-операция в `pending/sent/unknown` (кроме своего ключа) → `ErrInProgress` → HTTP 409 `create_in_progress`. | `internal/control/operations.go`, `internal/provision/provision.go`, `internal/server/cabinet_beget.go` | `TestCreateDistinctKeyBlockedWhilePending`, `TestCabinetVpsBlockedWhileOperationPending` |
| **H4** (security) — cross-user `idem_key` race отдаёт чужую операцию | `provision.Create` сверяет `op.UserID` и в pre-read, и в ветке `created=false` → `ErrIdemConflict` → 404 `not_found`. | `internal/provision/provision.go`, `internal/server/cabinet_beget.go` | `TestCreateCrossUserIdemConflict`, `TestCabinetVpsCrossUserIdemNotFound` |
| **R1** (security RE-CHECK, HIGH) — отмена во время платного вызова освобождала слот | `state=sent` теперь реально пишется: новый `Store.MarkCreateSent` (CAS `pending→sent`) вызывается непосредственно ПЕРЕД `CreateVps`; проигравший CAS провайдера не зовёт. Отмена — `Store.CancelPendingOperation` (CAS `pending→failed`), разрешена только из `pending`; `sent/confirmed/unknown/deleting/deleted/failed` → 409 `already_sent`. Оба CAS сериализуются единственным соединением пула, ровно один выигрывает. `max_nodes=1` + create + cancel-in-flight больше не даёт двух `CreateVps`. Ошибка валидации провайдера по-прежнему `failed` (слот освобождён); потерянный/неоднозначный ответ по-прежнему `unknown`. | `internal/control/operations.go`, `internal/provision/provision.go`, `internal/server/cabinet_beget.go` | `TestCreateCancelDuringInFlightDoesNotFreeSlot`, `TestCreateCancelBeforeSendSucceeds` (provision), `TestCabinetCancelDuringInFlightCreateDoesNotFreeSlot` (server) |
| **H2** (security) — логин без лимитов/lockout | Per-IP лимит 10/15мин + per-account lockout 5 неудач/15мин; униформа 401 `bad_credentials` для unknown/disabled (disabled логируется). | `internal/server/cabinet_api.go`, `internal/server/run_main_impl.go` | `TestCabinetLoginRateLimitedPerIP`, `TestCabinetLoginLockoutPerAccount`, `TestCabinetLoginUniformErrorForDisabled` |
| **H1** (code-review) — cross-tenant eviction по client-supplied IP | Same-IP замена owner-scoped: старая нода другого владельца не вытесняется (`authorizeUserRegister` возвращает userID → `sameIPReplacementAllowedLocked`, fail-closed при ошибке чтения владельца). | `internal/server/run_main_impl.go`, `internal/server/users_control.go` | `TestRegisterSameIPDoesNotEvictOtherTenant` |
| **M1** (code-review) — `userJSON.omitempty` выкидывал нулевые `used_nodes`/`disabled` | `omitempty` убран. | `internal/server/users_admin_api.go` | `TestAdminUserCRUD` (raw-body check) |
| **M3** (code-review) — idle-move схлопывал multi-slot домен в 1 | Перенос ОДНОГО слота домена на idle-ноду с сохранением distinct-IP инварианта. | `internal/server/selection_core.go` | `TestFillEmptyIdleMovesOneSlotOnly` |
| **M1** (security) — CSRF не привязан к сессии, без `__Host-` | Токен = `sha256("sharedd-csrf:"+sessionToken)` (проверка server-side по HttpOnly-сессии, cookie не участвует); cookie `__Host-sharedd_csrf` (Secure, Path=/, без Domain). | `internal/server/cabinet_api.go` | `TestCabinetCSRFIgnoresTossedCookie`, `TestCabinetCSRFRequiredOnPost` |
| **M2** (security) — username enumeration/disabled-oracle на логине | Униформа 401 (см. H2); статус disabled не раскрывается. | `internal/server/cabinet_api.go` | `TestCabinetLoginUniformErrorForDisabled` |
| **M3** (security) — `nodeIDForIdem` = 20 бит | `vps<6hex>-<5hex>` из полного sha256(idem_key) — 44 бита, валиден `machineapi.ValidateNodeID`. | `internal/server/cabinet_beget.go` | `TestNodeIDForIdemValidAndDistinct` |
| **M4** (security) — права `master.key` не проверялись | На загрузке существующего ключа отвергаются group/other-биты (≠0600). | `internal/cryptoutil/cryptoutil.go` | `TestLoadMasterKeyPermissions` |
| **M6** (security) / **L7** (code-review) — rate-limiter map без границы | Свежие-ключи pruning + жёсткий потолок карты (`rateLimiterMaxKeys`). | `internal/server/cabinet_api.go` | `TestIPRateLimiterMapBounded` |
| **L5** (code-review) — `UnmarshalJSON` fast-path `&&` вместо `||` | Исправлено на `||`. | `internal/state/state.go` | `TestDNSOperationUnmarshalPrefersNewFormat` |
| **L1/L2** (code-review) — мёртвый прод-код | `applyDNSTargets` убран из прода (перенесён тест-хелпером в `dns_ops_test.go`); `beget.Client.SetTokenCallback` + поле `onToken` удалены. | `internal/server/dns.go`, `internal/server/dns_ops_test.go`, `internal/beget/client.go` | покрыты существующими тестами |
| **L3** (code-review) — stale-комментарии `upsertARecord` | Заменены на `reconcileARecord`. | `internal/server/dns.go`, `internal/server/srmd.go` | — |
| **L4** (code-review) — §6.1 контракта устарел | Отмечено, что Beget-эндпоинты смонтированы (не смонтирован только Ansible-раннер). | `docs/api-contract.md` | — |

### Verification (fix round)

```
cd registry
gofmt -l .                        → clean
go vet ./...                      → clean
go test -race ./...               → ok (control, provision, server, state, cryptoutil, beget, …)
go build -mod=readonly ./...      → ok
golangci-lint run ./...           → 0 issues
```

Инвариант R1, закреплённый тестом: при `max_nodes=1` + create + cancel-in-flight
`CreateVps` вызывается ровно один раз, второй заказ отклоняется (quota/`create_in_progress`).
Отмена ещё-pending (pre-send) операции по-прежнему проходит и слот освобождает.

### Отложено осознанно

- **M7 (security-report, Ansible hardening)** — раннер не смонтирован в HTTP,
  Command-injection нет (argv/exec); ужесточение отложено до проводки в HTTP.
- **M8 (security-report, Beget URL id escaping)** — id приходит от провайдера,
  `BaseURL` — константа (SSRF нет); косметика.
- **L2/L4/L5/L8–L12 (code-review LOW)** — гигиена/перф, не входят в мандат
  fix-round; L5 (state) и L1/L2/L3/L4 исправлены выше.
- **M4(b,c) ротация master.key** — операционный процесс (`key_id`,
  decrypt-old/encrypt-new), отдельная фаза.
- **L12 атомарная запись master.key** и **L4 code-review (`Expires` cookie)** —
  косметика, вне мандата.

## Диагностика подключения Beget (прод-симптом «Beget credentials rejected»)

Провайдер отдаёт `{"error":"INCORRECT_CREDENTIALS"}` **строкой** при HTTP 200.
Клиент трактовал строку как message и не заполнял `Code`, а handler глотал
ошибку и всегда отдавал общее «Beget credentials rejected».

- `internal/beget/client.go`: `Auth(ctx, code string)` — необязательный 2FA-код
  (`Code,omitempty`); строковая форма `error` сохраняется как `APIError.Code`.
- `internal/server/cabinet_beget.go`: `begetAuthFailure` → `reason` + конкретное
  русское сообщение; серверный лог `beget auth failed: login=u*** status=… code=…
  message=…` (пароль не логируется, логин маскируется); ответ 400
  `{"error":"beget_auth_failed","message":…,"reason":…}`.
- UI `ui/pages/cabinet`: поле «Код (2FA)», сообщение сервера показывается
  verbatim, при `reason=code_required` поле подсвечивается и фокусируется.

Тесты: `internal/beget/client_test.go` (`TestAuthCodePassthroughOnlyWhenNonEmpty`,
`TestAuthStringErrorCodeSurvives`); `internal/server/cabinet_beget_auth_test.go`
(таблица `begetAuthFailure`, reason в ответе, отсутствие пароля в логе).


