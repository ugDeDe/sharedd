# specs/dc2-health.md — здоровье ноды по ME-пулу DC2

Заменяет критерий `telemt_me_writers_active_current > 0`.

## Источник данных на ноде (агент)

telemt Control API: `GET http://<telemt api listen>/v1/stats/dcs`
(подтверждено на сборке 3.4.25). Адрес конфигурируется: в node.toml секция

```toml
[telemt]
config_path = "/opt/telemt/telemt.toml"
api_addr    = "http://127.0.0.1:8080"   # NEW; если пусто — фолбэк на старую метрику
```

Ответ (см. пользовательский пример): `ok`, `data.middle_proxy_enabled`,
`data.generated_at_epoch_secs`, `data.dcs[]` с полями `dc`,
`available_endpoints`, `alive_writers`, `fresh_alive_writers`, `rtt_ms`.

## Критерий MetricsOK (новый)

```
ok == true
AND middle_proxy_enabled == true
AND generated_at_epoch_secs свежее now - 300s (абс. дрейф > 120s в будущее — тоже несвежий)
AND существует dcs[i].dc == 2
AND dcs[dc=2].available_endpoints > 0
AND dcs[dc=2].fresh_alive_writers > 0
```

Не требуем `coverage_pct == 100` и `alive >= required_writers`.

## Коды причин (report.Error / dc2_reason)

| code | значение |
|---|---|
| `dc2_api_error` | HTTP/транспорт ошибка или not ok |
| `dc2_me_disabled` | middle_proxy_enabled == false |
| `dc2_stale` | снимок устарел |
| `dc2_absent` | нет строки dc == 2 |
| `dc2_no_endpoints` | available_endpoints == 0 |
| `dc2_no_fresh` | fresh_alive_writers == 0 |
| `dc2_fallback_metrics` | api_addr не задан → старый критерий по me_writers (режим совместимости) |

Диагностика: `dc2_diag` —Alive/fresh для dc=-2, rtt dc2, generated_at.

## Правила на регистраторе

- HealthReport получает поля `dc2_ok bool`, `dc2_reason string`,
  `dc2_diag map[string]any`; `MetricsOK` = dc2_ok.
- Существующие fail/recover пороги, metrics-latch, GP-карантин, terminate —
  без изменений. Нода с dc2_ok=false не может быть выбрана мастером.
- Бан/пересоздание VPS по этой причине НЕ запускается (отказ ≠ ip_ban).
- Совместимость: старые агенты (нет dc2-полей) — регистратор использует
  прежнюю логику; авто-миграция не нужна.

## Тесты

- parses dcs snapshot: dc2 healthy / dc2 absent / fresh==0 / stale / me disabled.
- fallback без api_addr — прежний критерий, события TELEMT COMPATIBILITY не спамятся.
- регистратор: dc2_ok=false → нода не входит в селекцию; восстановление →
  возвращается после recover-порога.
