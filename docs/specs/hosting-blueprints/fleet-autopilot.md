# Fleet Autopilot

Status: implemented (cabinet autopilot UI + admin API).

One request creates N nodehost servers, installs the chosen text profile,
registers the agents, optionally enables the system nodehost replacement
policy, and reports per-item progress. A background loop reconciles paid
outcomes that a crash left uncertain; there is no "reconcile" button for
fleets.

## API contract (frozen)

All routes are session-cookie + CSRF. The POST is a command and requires
`Idempotency-Key` (user-scoped, like other cabinet commands). Ownership
failures are `404 not_found`.

### `POST /api/me/fleets`

```json
{
  "connection_id": 12,
  "name": "base",
  "count": 2,
  "configuration": {
    "name": "pool-node",
    "cpu": "AMD Ryzen 9 7950X",
    "cores": 2,
    "ram": 4,
    "disk": 40,
    "diskType": "SSD NVMe",
    "os": "ubuntu-24.04",
    "location": "DE",
    "period": "1m",
    "networkSpeed": "..."
  },
  "installation_profile_id": 6,
  "installation_vars": {
    "ad_tag": "0123456789abcdef0123456789abcdef"
  },
  "auto_replace": true
}
```

Response: `202 {"fleet_id": 7}`.

Validation:

- `connection_id` is owned by the session user and is `nodehost`; `beget`
  → `501 not_implemented`.
- `count` is `1..20`.
- `configuration` follows the same rules as cloud-resource create
  (`validCloudConfig`), including a non-empty provider `name`.
- `installation_profile_id` is the installable profile, own or published.
  Versioning is gone (one content row per profile). The legacy
  `installation_profile_version_id` is still accepted as an alias and is
  resolved to its parent profile; its content is not pinned separately.
- Item labels are `name-01 .. name-NN` (zero-padded). `name` defaults to
  `fleet` and is capped at 7 characters so every label passes the node-id
  NAME prefix rule (`1..10` characters before the generated hash).
- Replaying the same key with the same body returns the same `fleet_id`.
  Reusing it with a different body → `409 idempotency_conflict`.

### `GET /api/me/fleets/{id}`

```json
{
  "id": 7,
  "state": "running|succeeded|partial|failed",
  "count": 2,
  "auto_replace": true,
  "created_at": 1710000000,
  "items": [
    {
      "idx": 1,
      "label": "base-01",
      "resource_id": 31,
      "node_id": "base-01-abcde",
      "state": "pending|creating|installing|verifying|admitted|error",
      "last_error": "",
      "ip": "203.0.113.10"
    }
  ]
}
```

`resource_id` is `null` until a paid create is accepted. `last_error` is a
safe machine code (`provider_error`, `provider_timeout`, `quota_exceeded`,
`node_not_registered`, `system_graph_unavailable`, …), never provider text.

### `GET /api/me/fleets`

```json
{
  "fleets": [
    {"id": 7, "state": "succeeded", "count": 2, "auto_replace": true,
     "created_at": 1710000000, "succeeded": 2, "errors": 0}
  ]
}
```

## Storage

Additive tables (no changes to existing rows):

- `fleets(id, user_id, connection_id, count, auto_replace,
  profile_version_id, configuration_json, fleet_key, state, created_at,
  updated_at)` with `UNIQUE(user_id, fleet_key)`.
- `fleet_items(id, fleet_id, idx, label, resource_id, node_id, state,
  last_error, updated_at)` with `UNIQUE(fleet_id, idx)`.

`configuration_json` is the immutable create snapshot used by the worker and
by the policy input `configuration_ref`. `fleet_key` is the Idempotency-Key.

## System graphs

`internal/blueprint/system.go` embeds the two graphs from
`internal/blueprint/testdata/` with `//go:embed`:

- `system-nodehost-replacement` (`nodehost_replacement.json`)
- `system-beget-ip-or-replacement` (`beget_ip_or_replacement.json`)

At startup `seedSystemGraphs` canonicalises each graph and calls
`control.EnsureSystemGraphProfile`: `created_by=0`, `published=1`,
`format=graph`, first version carries `content_hash`. Seeding is idempotent
and never overwrites existing content. `control.SystemGraphVersionID(name)`
is the lookup used by the fleet worker.

Only the nodehost graph is pinned by fleets; beget fleets are rejected at the
API boundary.

## Worker

`server/fleet_worker.go` runs one worker per `fleet_id` in a dedicated
`fleetWorkers` map (never shares the operation-id namespace). Items are
processed sequentially:

1. **Create** through the same acceptance helper as
   `POST /api/me/cloud-resources` (`cloudCreateAccept`): planned resource +
   pending operation rows, then the existing poll worker. The per-item key is
   `fleet:<fleet_id>:<idx>`, so a replay can never issue a second paid Create.
2. **Wait** for the resource to become `ready` (`cloudCreateBudget`).
3. **Install** through the same helper as `POST .../install`
   (`startCloudInstall`) with the profile's current content row (no default
   substitution), expected node id and claim-on-first-register secret reuse.
4. **Wait** for the install operation (`done`/`failed`, bounded by the
   provision budget). Transient install failures get at most one retry.
5. **Policy**:
   - `auto_replace=true`: enable node management `mode=profile` with the
     `system-nodehost-replacement` version and inputs
     `{connection_id, configuration_ref, installation_profile_id}`,
     `process_current_ban=false`.
   - `auto_replace=false`: if a policy row exists and is not `manual`, switch
     it to `manual`. No policy row is created (default is manual).
6. **Admit**.

A failure records a safe code on the item and the loop continues. Fleet
state becomes `succeeded` (all admitted), `failed` (all failed) or `partial`
(mixed). Items that already succeeded are not redone.

### Admitted predicate

The worker calls the existing queue-health accessor (`blueprintResourceReady`)
after installation. A registered node whose independent health proof is not
fresh yet stays in `verifying`; the fleet remains `running` and is re-checked
periodically. After `FleetVerifyBudget` (10 minutes) the item becomes `error`
with `health_timeout`. This never triggers a new purchase. Destructive policy
guards still re-check health before acting.

## Auto-reconcile (no button)

`fleetReconcileLoop` runs in `Run()` every 60 seconds until shutdown
(context-cancelled).

It reads `CloudResourcesUncertain`:

- states `planned|creating|error` with empty `external_id`, or
- state `error` with `last_error=provider_reconcile_required`.

Per connection it reads `cloudAllServers` (read-only inventory). A resource
is marked `deleted` and its pending install reservation released only when:

- it has no external id and the provider has **no unaccounted servers**, or
- it has an external id that is **absent** from inventory.

Resources with a live worker are skipped. Provider calls are throttled to once
per resource per 60 seconds via an in-memory map. Nothing is re-created. The
manual `POST /api/me/cloud-resources/{id}/reconcile` endpoint is unchanged.

## Quota

No changes. Only live registrations/reservations consume quota, exactly as the
manual/beget/cloud install paths already do.

## Files

- `registry/internal/control/fleets.go` — fleet storage.
- `registry/internal/control/control.go` — additive schema.
- `registry/internal/control/cloud_resources.go` —
  `CloudResourcesUncertain`.
- `registry/internal/control/profiles.go` — `EnsureSystemGraphProfile`,
  `SystemGraphVersionID`.
- `registry/internal/blueprint/system.go` — embedded system graphs.
- `registry/internal/server/system_graphs.go` — startup seed + lookup.
- `registry/internal/server/fleet_api.go` — three routes.
- `registry/internal/server/fleet_worker.go` — worker + auto-reconcile.
- `registry/internal/server/cloud_install_accept.go` — shared install
  acceptance (HTTP route + fleet).
- `registry/internal/server/cabinet_cloud.go` — shared create acceptance
  (`cloudCreateAccept`) and the unchanged manual reconcile route.
- `registry/internal/server/run_main_impl.go` — seed + loop startup + worker
  join on shutdown.

## Tests

`registry/internal/server/fleet_test.go`:

- `TestFleetHappyPathAutoReplaceAndIdempotency` — two items, both create +
  install + policy-enabled; fleet POST replay is idempotent and a changed body
  conflicts.
- `TestFleetAutoReplaceOffLeavesManual` — no policy row is created.
- `TestFleetPerItemFailureContinues` — one provider failure, remaining items
  still succeed, fleet is `partial`.
- `TestFleetReconcileCleansUncertainResources` — empty inventory deletes an
  uncertain planned row and a `provider_reconcile_required` row with a stale
  external id.
- `TestSystemGraphSeedIdempotent` — double seed leaves one version with the
  same content hash.

Verification command:

```bash
cd registry
go vet ./internal/control ./internal/server ./internal/blueprint
go test -race -count=1 -run 'Fleet|SystemGraph|Reconcile' ./internal/control ./internal/server ./internal/blueprint
go build -mod=readonly ./...
gofmt -l internal
```

## UI (кабинет)

`ui/pages/cabinet/{index.html,main.ts,api.ts,page.css}`. Основной сценарий —
три раздела: «Пул» (`#/nodes`), «Хостинги» (`#/beget`), «Журнал»
(`#/operations`). Шапка и нижняя мобильная панель показывают только их;
«Серверы» (`#/servers`) и «Профили» (`#/profiles`) убраны в меню
«Дополнительно» (кнопка с `aria-expanded`, закрывается по Escape и клику
вне меню; ссылки — обычные `<a href>`). Все прежние маршруты, включая
редактор профилей, продолжают работать. Заголовки видов: `#/nodes` → «Пул»,
`#/operations` → «Журнал», `#/beget` → «Хостинги».

Карточка «Автопилот» стоит первой в `#/nodes` (выше ключа доступа) и
является основным путём: один платный заказ создаёт N нод.

Состав формы (без других кнопок, кроме отправки): проверенное подключение
Nodehost, количество 1–20, локация, CPU (фильтруется локацией), ОС, тип диска,
ядра/ОЗУ/диск с лимитами каталога, период, необязательная скорость сети и
текстовый профиль установки (свой или опубликованный). В запрос уходит
`profile_id`: у профиля одно содержимое, номер версии не выбирается. Граф в
автопилот не принимается. Чекбокс «Заменять ноды при бане автоматически» включён по
умолчанию и отправляется как `auto_replace`; его подпись честно объясняет:
ON — при бане платная замена создаётся ДО удаления старой, OFF — ноды живут,
замена вручную и бан сам по себе ничего не покупает.

Оценка цены — `POST /api/me/cloud-resources/estimate` с тем же периодом и
скоростью, умножается на количество на клиенте. Отправка идёт с
`Idempotency-Key` из `newIdemKey()`; ответ `202 {fleet_id}`. После этого
карточка опрашивает `GET /api/me/fleets/{id}` каждые 5 секунд (single-flight),
пока `state=running`, и останавливается на терминальном состоянии или при
уходе со страницы. Строки показывают label, бейдж состояния, IP и короткую
русскую причину из безопасного `last_error` (`insufficient_funds`,
`provider_timeout`, `ssh_unavailable`, …). В `localStorage` хранится только
id последнего флота (`sharedd.autopilot.fleet`), поэтому перезагрузка
возобновляет просмотр; секретов там нет.

По завершении обновляются списки нод и ресурсов. `succeeded` показывает
«N нод в пуле», `partial` — «X из N, ошибки по строкам», `failed` — ошибки
по строкам. Ручные формы «Создать ресурс» и «Ресурсы у провайдера» остаются
в «Хостингах»/«Серверах» как дополнительный путь; на странице «Пул»
кнопок «Установить», «Проверить SSH» или «Сверить» нет.

## Метрики трафика: telemt 3.5.9 `_total`

telemt 3.5.9 отдаёт `telemt_user_octets_from_client_total` /
`telemt_user_octets_to_client_total`. Совместимость закрывается на стороне
реєстратора: `normalizeTelemtMetrics` в `registry/internal/server` принимает
оба варианта имён (с `_total` и без), синтезирует `sharedd_user_*` и
`sharedd_traffic_*` ключи, никогда не перезаписывая реальные `sharedd_*`.
Старые агенты начинают отдавать трафик без раскатки; per-user counter reset
по-прежнему обрабатывает `trafficCounterDelta`. Агентский бинарь тоже
понимает `_total` (коммит `0ebf40d`), но для prod-нод это не требуется.
