# 06. Этапы реализации и критерии проверки

**Это план будущей работы.** В этой поставке созданы только семь Markdown
файлов; программные изменения и тесты приложения не заявляются выполненными.
Навигация: [обзор](README.md), [требования](01-requirements.md),
[провайдеры](02-provider-api.md), [API](03-cabinet-api.md),
[storage/executor](04-storage-and-execution.md), [графы](05-blueprint-profiles.md).

## 1. Порядок работ и выходные критерии

| Этап | Работа | Критерий завершения |
|---|---|---|
| 0. Зафиксировать baseline | Прочитать AGENTS, status/diff, текущие auth/quota/SSH/machine contracts; сохранить ownership пользовательских правок | План diff без перезаписи `sshx.go`/tests; согласованные DTO extensions |
| 1. SSH liveness | Согласовать с текущими правками пользователя context-close; добавить keepalive, корректную финализацию шагов/lock | Тихая healthy команда проходит; blackhole около60s; все blocking stages cancel-safe |
| 2. Независимый GP recovery | Запуск циклов до потенциально блокирующих вызовов; неблокирующий terminate state; completed LOCAL GP → register evidence; registry verify | Same-IP recovery без restart, repeated cycles, no kick storms, auth сохранён |
| 3. SQLite migration | Connections, balances, surrogate IDs, pins, policy, checkpoints, episodes/delivery/fences; backup/restore | Repeatable migration fixtures, legacy API adapters без второго writer |
| 4. Provider read/auth/balance | Nodehost catalogs/account/operation reads; Beget auth/catalog/network/account adapters | Mock contract/error tests, secret redaction, nullable currency/scope, no paid I/O |
| 5. Явные MANUAL commands | Estimate/create/delete/replace/change-ip, idempotency, quota, persisted intents/reconcile | Manual ban zero mutations; command повтор не дублирует оплату; change-ip без скрытого create |
| 6. Durable graph engine | Strict schema/ports/dominance, allowlist blocks, runs/checkpoints/event delivery | Crash matrix, no arbitrary cycles/HTTP, version pins, cancellation certainty |
| 7. Готовые profiles | Nodehost replacement; Beget staged IP lifecycle/fallback; retirement fence | New admitted before old deleted, cleanup isolated; Beget gate остаётся закрыт без evidence |
| 8. UI | Design/specs → implementation vanilla TS → designer QA, framework-free graph prototype decision | Keyboard/a11y/loading/error/stale/unknown; build_ui и generated assets sync |
| 9. Общая приёмка | Security review + независимый code reviewer, module checks, разрешённые integration gates | Закрыты critical/high; статус проверки явно verified/partially_verified |

Этапы 1–2 можно выпустить до provider automation, но они не должны включать
автопокупки. Этап 5 первым реализует ручные команды; PROFILE не включается
по умолчанию. Graph schema/block registry и backend contracts фиксируются
до UI реализации. Новые tables/routes этого плана не считать существующими
до фактического merge и проверки.

## 2. Тестовая стратегия

Для реальной реализации сначала минимальные regression tests на меняемые
инварианты; mock provider HTTP через `httptest`, SSH test server/blackhole,
fake clock для scheduler/backoff/lease. Временные окна календаря использовать
с фиксированными anchor timestamps, не `now-1h` относительно полуночи.
Не требуются живые Cloudflare/GP/telemt/systemd для unit/integration suite.

### 2.1 SSH

- Timeout handshake 20s, общий step budget default20min; context раньше
  каждого из них. Неположительный library fallback не подменяет config default.
- Cancel dial, handshake, NewSession, exec request, blocked stdin write,
  Wait и output readers. Нет утечки goroutine/lock; следующий install стартует.
- Тихий stdout дольше60s с живым keepalive не завершает команду.
- TCP blackhole после successful handshake → transport loss около60s;
  явный EOF быстрее. SSH request reply false считается живым ответом.
- Step/operation final state, sanitized partial logs, uncertainty checkpoint;
  произвольный shell после restart автоматически не повторяется.

### 2.2 GP и machine identity

- Initial register получает terminate/401/429/timeout, fetch config недоступен:
  scheduler всё равно стартует. Config/IP отсутствует → конечная попытка,
  следующий tick; GP API pending/outage не зацикливает один run навсегда.
- ip_ban/dead/prune/metrics-muted не отключают GP. Отказ report не блокирует
  scheduler. Одновременные terminate от трёх loops — один переход состояния.
- Successful LOCAL completed measurement на том же IP → регистрация прежней
  identity, новый evidence independently verified registry до admission.
  Forged/wrong target/port/type/stale/uncompleted evidence отклоняется.
- Cooldown/Retry-After соблюдены, GP при этом идёт; 10 повторных ban/recovery
  циклов не упираются в `sync.Once` или process exit.
- Recovery register не запускает обратный GP kick. Coalesced initial kick
  допускает один immediate GP, без register→GP→register бесконечного цикла.
- Shared server Bearer, pending claim и personal secret не теряются.
  Same VPS IP change сохраняет identity; replacement — новый node_id.
- Fence superseded/deleting блокирует old reclaim после restart/prune,
  даже с хорошим GP; health ban recovery fence не снимает.

### 2.3 Provider API и деньги

- Nodehost 202→pending→running→succeeded с resourceId; failed после202;
  pagination всех страниц без бесконечного повторного cursor.
- Стабильный provider create key после crash; 409 in-flight не создаёт новый.
  402 пауза, 403 scope/test/limit отдельно, 429 Retry-After; 5xx unknown.
- Nodehost GET server password никогда не попадает в DTO/log/graph.
  Change-ip lost response → read reconcile, не второй POST.
- Beget auth code omission без2FA, error string при200, refresh revoked token,
  private credentials revision. Purchase POST не повторяется после401.
- Beget order modern additional_ip/error_v2 и deprecated ip_address/error,
  conflicting fields, wrong VPS/region, HTTP200 бизнес-отказ.
- Attach уже привязанного адреса read no-op; detach не считается освобождением
  billing. Remove с ip_action DELETE; RELEASE не проходит тест контракта.
- Unknown Beget IP order сначала inventory reconcile, без fallback create.
  Несколько возможных совпадений → needs_attention. Недостаток средств
  не ведёт к покупке VPS. Подтверждённый безопасный отказ допускает fallback
  только из явного PROFILE.
- Manual change-ip failure, unknown, unsupported — **ноль create**.
- Новая VPS получила banned IP → old не удалён. New admit требует fresh
  verified GP + metrics/TCP; не ожидать мастерства в selection queue.
- Очистка old VPS/IP неуспешна → retry cleanup checkpoint, create counter
  не увеличивается. Unrelated IP/ресурс не удалён.

### 2.4 Balance

- Decimal number/string конвертируется без float drift в amount string.
  Валюта неизвестна → null, не RUB. Nodehost API scope отдельный от site.
- Beget outer status success + inner answer.status error — ошибка, не0;
  missing user_balance/NaN/wrong type → unavailable/stale.
- VPS-only Beget unsupported не ломает cloud connection; UI не называет
  hosting result подтверждённым VPS balance.
- Последний good snapshot сохраняется при timeout, freshness stale сразу;
  без good snapshot amount=null/unavailable. checked_at не обновляется ошибкой.
- Concurrent refresh coalesced, TTL60s/throttle/deadline; tenant A не видит
  tenant B. Late revision1 result не перезаписывает revision2 credentials/cache.
- Пароль только form POST body, не query/log/traces; отдельные hosting/cloud
  credentials. Никаких topup endpoints и сумм разных валют.

### 2.5 Management, graph и crash matrix

- MANUAL бан → zero provider mutations во всех каналах и после startup.
  Мигрированные nodes manual. Profile activation default игнорирует current ban.
- Profile→manual vs dispatch CAS: before claim — zero sends, after claim —
  in-flight reconciliation без дальнейших effects/автоудалений. Manual→profile
  race не обрабатывает прошлый эпизод без true process_current_ban.
- Две доставки одного episode, два workers, startup reconcile — одна
  логическая paid action. Crash после JSON persist до delivery insert и после
  insert до wake не теряет событие. Новый recovered→ban имеет новый episode.
- Полная квота допускает один linked replacement; второй replacement и
  unrelated create запрещены. Cancel in-flight/unknown не освобождает слот.
- Crash до dispatch, после intent persist, после отправки до ответа, после
  ответа до checkpoint, после fence до switch, после switch до delete,
  после provider delete до absence confirm. Везде reconcile без повторной покупки.
- Новый worker по истёкшему lease не отправляет дубликат старого in-flight;
  поздний старый worker не перезаписывает checkpoint нового поколения.
- Граф rejects unknown blocks/params, 129 nodes, >256 edges, >256KiB,
  >64KiB SSH, wrong types, cycles/fan-out, unreachable source и неисполненную
  branch dependency. Размер данных учитывает serialized bytes, не символы.
- Владение secretref/config/server/version проверяется на каждом уровне,
  а не только при открытии editor. Secrets только refs/encrypted outputs.
- Text profile edit не меняет pinned run; legacy operation не получает
  выдуманную историческую версию. Copy не переносит credentials другой стороны.
- Owner disabled / private profiles disabled / policy invalidated: future
  dispatch остановлен; только read reconciliation начатых эффектов.
- Wait подписка переживает restart без goroutine lifetime/SSH. Arm нового
  ресурса создаёт successor observe того же pinned profile, не initial purchase.

### 2.6 API, миграции и UI

Все новые маршруты проверить на no-session, invalid CSRF для mutation,
cross-owner ID/ref, disabled user, bounded rate/body/context. Error всегда
`{"error":"code","message":"safe text"}`. Admin panel auth не заменяется
случайно cabinet session. Повтор command key — тот же ack, changed payload409.
DELETE resource использует confirm header без JSON body; old operation cancel
по-прежнему только pending purchase.

Migration fixtures покрывают схему/backup/restore из [04](04-storage-and-execution.md).
Legacy link/VPS/manual install/profile CRUD продолжает работать через те же
services. Нет двойного quota учёта/ownership. Один external ID разных
connections не конфликтует. Prune сохраняет management policy/owner/fence.

UI проверять в браузере на desktop/mobile: keyboard graph editing,
type errors, save CAS conflict, explicit manual/profile choice, stale balance,
pending/unknown/cancel_requested/needs_attention и redacted logs. В графовом
JSON из UI нет plaintext credentials. Disabled capabilities не изображаются
рабочими кнопками.

## 3. Обязательные команды проверки реализации

Запускать отдельно в каждом application module, root Go module отсутствует:

```bash
# workdir: registry/
go vet ./...
go test -race ./...
go build -mod=readonly ./...
golangci-lint run ./...
gofmt -l .
```

```bash
# workdir: node/
go vet ./...
go test -race ./...
go build -mod=readonly ./...
golangci-lint run ./...
gofmt -l .
```

`gofmt -l` не должен выводить файлов. Не форматировать попутно чужие правки.
Если меняется UI, из корня:

```bash
bash scripts/build_ui.sh
bash scripts/build_ui.sh -check
```

Редактировать `ui/pages/**`, `ui/components/**`, `ui/lib/**`, а не embedded
HTML вручную; generated assets включаются в будущий implementation diff.
Если меняется `tools/uigen`, отдельно в его module выполнить `go vet ./...`,
`go test -race ./...`, `go build -mod=readonly ./...`, `gofmt -l .` и
применимые CI lint checks. Если меняются shell installers/build scripts:

```bash
bash -n scripts/*.sh
shellcheck -S warning scripts/*.sh
bash scripts/check_installer_parity.sh
```

Не запускать эти команды ради одной документации как доказательство
реализации; в документальном diff достаточно проверки ссылок, примеров,
полноты и сохранности пользовательского кода.

## 4. Review gates

Перед выпуском программной реализации обязателен security review:
auth/owner/CSRF, PII и provider credentials, SSRF/SSH target trust,
shell vars injection, root secrets, purchase replay, quota bypass,
event/lease fencing, deletion provenance и redaction. Независимый reviewer
проверяет correctness и trace каждого lifecycle с отказами. Critical/high
закрыть; остальные findings фиксировать с конкретным решением. В текущем
documentation-only задании делегирование не выполняется.

## 5. Не закрывать интеграционные ворота mock-тестами

| Gate | Что известно | Что нужно до объявления verified |
|---|---|---|
| Beget old primary removal | Order/attach/detach/remove IP документированы, promote-primary нет | Отдельно разрешённый стенд: new secondary → OS/proxy/agent switch → same identity admission → old primary delete → reboot/reconnect → inventory только new managed IPv4; rollback и billing evidence |
| Beget balance | POST getAccountInfo и user_balance документированы для hosting | Подтверждение VPS-only доступа и релевантности cloud spend; до этого scope unverified/currency null, UI без «VPS balance» |
| Nodehost live lifecycle | API docs + read-only test keys | Отдельное разрешение на оплату, create202/operation/IP/reboot/delete и стоимость; read-only test key этого не доказывает |
| Advertised IP/network | Echo может возвращать old source; нужна явная public_ip | Проверка интерфейса/route/proxy binding/SSH watchdog и rollback на поддерживаемом образе |

Live paid actions запрещены без отдельной авторизации. При незакрытом gate
его capability false, соответствующий шаблон draft/не публикуется; остальная
поставка получает честный `partially_verified` с точным перечнем ворот.

## 6. Формат итоговой передачи реализации

Указать изменённые modules/files, применённые migration versions и backup,
фактически выполненные команды и результаты, acceptance failures/open gates,
статус verified/partially_verified/not_verified. Отдельно отметить, что
MANUAL остаётся default и новые PROFILE версии требуют выбора пользователя.
Наличие этих семи документов само по себе не является реализацией этапов.
