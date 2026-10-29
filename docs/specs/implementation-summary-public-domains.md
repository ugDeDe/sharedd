# implementation-summary — публичный `domains[]` в GET /statistics/api/list

Ветка `dev`, модуль `registry/` only. Аддитивное поле, существующие ключи
(`nodes`, `quarantine`) не тронуты.

## Что сделано

- `handleStatsList` теперь добавляет третий ключ ответа:
  `"domains": [ ... ]` (всегда присутствует, даже при `masters_per_domain==1`
  → `slots_total: 1`). Старые клиенты ключ игнорируют.
- Новый helper `(*Registry).buildPublicDomainsLocked(now)` — единственный
  источник нового массива. Читает `cfgMu` внутри (порядок локов `mu → cfgMu`
  соблюдён), вызывается под `r.mu.RLock` из `handleStatsList`.
- Типы: `publicDomain`, `publicDomainMaster` в `stats.go`.

## Форма (как в spec'е)

```json
"domains": [
  {
    "domain": "a.example.com",
    "port": 443,
    "slots_filled": "1/2",
    "slots_total": 2,
    "masters": [
      {"node_id": "a1", "ip_masked": "203.x.x.7", "healthy": true, "ttl_remaining_sec": 4210}
    ]
  }
]
```

Источники (все — существующие, ничего не додумано):

| Поле | Источник |
|---|---|
| набор доменов | `cfg.Cloudflare.Domains` (trim + dedup, как в `buildProxyLinksLocked`) |
| `port` | `cfg.SharedProxy.Port` (0 → 443, только direct-construct в тестах) |
| слоты | `state.Assignments[domain]` (порядок списка = порядок слотов) |
| `slots_total` | `config.ResolveMastersPerDomain(cfg.Selection.MastersPerDomain)` |
| `slots_filled` | `len(Assignments[domain])` / `slots_total` |
| `ip_masked` | `maskPublicIP(c.IP)` — тот же маскировщик, что `nodes[].ip` |
| `healthy` | `c.IsFullyHealthy(cfg.ReportFreshnessTTL)`; нода отсутствует → `false` |
| `ttl_remaining_sec` | per-slot: `ttl − now−AssignmentsSince[domain][i]`; TTL ≤ 0 или `since` нулевой → поле опущено |

Решения Leader, применённые в коде:

1. `port` отдаётся из `shared_proxy.port` (как в публичных ссылках).
2. `healthy` — производный инвертированный «не мёртв», а не сырой `dead`.
3. Отдаются **оба**: `slots_filled` (строка) и `slots_total` (int).
4. `ip_masked` — через `maskPublicIP` (консистентно с `nodes[].ip`).
5. `ttl_remaining_sec` — переиспользована per-slot TTL-логика;
   `omitempty` на `*int64` → `null`/отсутствие при выключенном TTL.

Приватность/деградация:

- Только замаскированные IP; нет provider id, нет `cname_target`.
- Свободный текст не используется → `sanitizePublicDetail` не нужен.
- Порядок: домены сортируются по имени (стабильно), слоты — порядок
  `Assignments` (стабильно). UI сам сортирует чипы мастеров.
- `masters` всегда массив (пустой `[]`, не `null`); нет мастера →
  `slots_filled: "0/M"`.

## Тесты (`registry/internal/server/stats_test.go`)

Детерминированные, без wall-clock-окон:

- `TestStatsListDomainsAggregate` — два домена по 2 слота, домены конфига в
  разном порядке + дубль: проверяет стабильный порядок (`a` → `b`),
  агрегат `1/2` и `2/2`, `slots_total`, `port`, `healthy`;
  `masters[].ip_masked == nodes[].ip` для той же ноды;
  TTL выключен → `ttl_remaining_sec` отсутствует.
- `TestStatsListDomainsTTLAndSingleSlot` — `masters_per_domain=1`
  → `slots_total=1`, `slots_filled="1/1"`; TTL включён → остаток ≈ 1080 c
  (30 мин − 12 мин, допуск ±10 c).
- `TestStatsListDomainsDeadSlot` — слот с нездоровой и отсутствующей нодой:
  `healthy=false`, у отсутствующей ноды `ip_masked` пуст (не утекает).

Существующие `TestStatsListSanitization`, `TestStatsListMasterTTLCountdown`,
`TestProxyLinks`, карантинные тесты — не менялись, проходят.

## Верификация

```
cd registry && gofmt -l .          # пусто
go vet ./...                        # чисто
go test -race ./...                 # ok (server 66s)
go build -mod=readonly ./...        # ok
golangci-lint run ./...             # 0 issues
```

Статус: **verified**. Не коммичено, не запушено.

## Файлы

- `registry/internal/server/stats.go` — изменён (+ helper, типы, ключ `domains`).
- `registry/internal/server/stats_test.go` — изменён (+3 теста, 2 helper'а).
- `docs/specs/implementation-summary-public-domains.md` — этот файл.
