# implementation-summary-dc2.md — DC2 health (ME-пул DC2 как критерий MetricsOK)

Branch: `dev` (uncommitted, on top of M1 working-tree changes — none reverted, nothing committed).
Contract: `docs/specs/dc2-health.md` (authoritative), wire/contract updates in `docs/api-contract.md` §3.
Scope honored: agent + registry only. Multi-master DNS, Beget, UI, auto-recreation — untouched.

## Design in one line

`MetricsOK` is no longer `telemt_me_writers_active_current > 0` when `[telemt] api_addr`
is configured: the agent evaluates the telemt Control API snapshot
`GET <api_addr>/v1/stats/dcs` and the registry treats `dc2_ok` as the report's
metrics verdict. Empty `api_addr` = compatibility mode (`dc2_fallback_metrics`),
old metric criterion, zero behavioral change otherwise.

## Files changed

| File | Change |
|---|---|
| `node/internal/config/config.go` | `[telemt] api_addr` (trimmed, trailing `/` stripped, must be absolute http(s) URL when set; empty = fallback mode) |
| `node/internal/config/config_test.go` | `TestLoadNodeConfigTelemtAPIAddr`: empty stays empty, trailing slash trimmed, non-URL rejected |
| `node/internal/agent/healthcheck.go` | `HealthReport` gains `dc2_ok`/`dc2_reason`/`dc2_diag` wire fields; `dc2StatsResponse`/`dc2DCStats` parsers (only spec'd fields, verified live shape on telemt 3.4.25); pure `evaluateDC2` (spec criterion incl. ±300s/120s freshness); `fetchDC2Stats` (GET, 5s timeout, 1MB body cap); `RunMetricsCheck` rewritten: dc2 verdict decides `MetricsOK` when `api_addr` set, `/metrics` snapshot still collected for panel/traffic; fallback mode keeps old criterion and always tags `dc2_fallback_metrics` |
| `node/internal/agent/dc2_test.go` | **new** — criterion table (healthy / api not ok / me disabled / stale old / stale future-drift / stale zero / dc2 absent / no endpoints / no fresh), exact boundary freshness (now-300s, now+120s healthy), diag content, integration: dc2-healthy ignores missing writer metric, dc2 failure carries reason code, HTTP 500 → `dc2_api_error`, fallback mode old behavior, TELEMT COMPATIBILITY anti-spam |
| `registry/internal/machineapi/security.go` | `HealthReportPayload` gains `dc2_ok`/`dc2_reason`/`dc2_diag` (wire contract shared with the agent; non-empty `dc2_reason` marks a dc2-aware agent) |
| `registry/internal/server/healthcheck.go` | derivation rule: `metricsOK = payload.DC2OK` when `payload.DC2Reason != ""`, else legacy logic (old agents unchanged, no migration); failure/recover latch, metrics-latch, GP-quarantine, terminate, prune — untouched |
| `registry/internal/server/dc2_report_test.go` | **new** — wire decode (no `DisallowUnknownFields` 400), `dc2_ok` beats contradictory `metrics_ok` for dc2-aware agents, legacy agent unaffected, fail-threshold latch drop → master moves to B, **no quarantine / no terminate / no IP ban on dc2 failure**, recovery rejoins queue after `recover_threshold` |
| `docs/api-contract.md` | §3 notes the /report dc2 fields and that the registry treats them as the replacement for `metrics_ok` |

## Node config example

```toml
[telemt]
config_path = "/opt/telemt/telemt.toml"
api_addr    = "http://127.0.0.1:8080"   # NEW; empty = legacy me_writers criterion
```

## DC2 criterion (pure function `evaluateDC2`)

```
ok == true
AND middle_proxy_enabled == true
AND now-300s <= generated_at_epoch_secs  (drift > 120s into the future = stale too)
AND exists dcs[i].dc == 2
AND dcs[dc=2].available_endpoints > 0
AND dcs[dc=2].fresh_alive_writers > 0
```

`coverage_pct` and `alive >= required_writers` deliberately not required.

## Reason codes (`dc2_reason` / `report.Error` prefix "dc2: ")

| code | meaning |
|---|---|
| `dc2_api_error` | HTTP/transport error or `ok != true` |
| `dc2_me_disabled` | `middle_proxy_enabled == false` |
| `dc2_stale` | snapshot older than 300s, >120s in the future, or `generated_at <= 0` |
| `dc2_absent` | no row with `dc == 2` |
| `dc2_no_endpoints` | `available_endpoints == 0` |
| `dc2_no_fresh` | `fresh_alive_writers == 0` |
| `dc2_fallback_metrics` | `api_addr` unset → legacy metric criterion (compat mode; `dc2_ok` mirrors `metrics_ok`) |

`dc2_diag` (omitted on transport error): `dc2_generated_at`, `dc2_alive`, `dc2_fresh`, `dc2_rtt_ms` (zeros when no dc2 row).

## Registry behavior

- Selection: dc2 failure flows through the existing `MetricsHealthy` latch into
  `IsFullyHealthy` → node leaves the master queue; recovery returns it after
  `recover_threshold`. Thresholds, quarantines, terminate, prune untouched.
- `dc2_ok=false` **never** triggers quarantine, `ip_ban`, terminate, or any
  VPS re-creation (pinned by `TestDC2FailureBlocksSelectionRecoversByLatch`).
- Old agents (no dc2 fields): `dc2_reason` empty → previous logic, no migration.

## Test status — mandatory list from dc2-health.md

| Required behavior | Test(s) | Status |
|---|---|---|
| parses dcs snapshot: healthy / absent / fresh==0 / stale / me disabled | `TestEvaluateDC2` (+ boundary cases) | ✅ pass |
| fallback without api_addr — old criterion, TELEMT COMPATIBILITY not spammed | `TestRunMetricsCheckFallbackMode`, `TestTelemtCompatSuspicionDoesNotSpam` | ✅ pass |
| registry: dc2_ok=false → not selected; recovery after recover-threshold | `TestDC2DerivationRule`, `TestDC2FailureBlocksSelectionRecoversByLatch`, `TestDC2FieldsAcceptedOnWire` | ✅ pass |

## Verification

```
node:     gofmt -l .                → clean
          go vet ./...              → clean
          go test -race ./...       → ok (3 packages)
          go build -mod=readonly    → ok
          golangci-lint run ./...   → 0 issues
registry: gofmt -l .                → clean
          go vet ./...              → clean
          go test -race ./...       → ok (5 packages with tests)
          go build -mod=readonly    → ok
          golangci-lint run ./...   → 0 issues
```

DC2 tests re-run with `-count=1` (cache bypassed): 16 node-side, 3 registry-side — all pass.

## Deviations / notes

1. **dc2-awareness marker**: the registry uses non-empty `dc2_reason` (not a
   protocol-version field) to detect a dc2-capable agent. In compat mode the
   agent always sends `dc2_fallback_metrics`, so the marker is reliable.
2. **dc2 mode tolerates `/metrics` scrape failure**: Control API and /metrics
   belong to the same telemt process; a scrape hiccup alone must not kill a
   node whose dc2 verdict is healthy (and a non-empty `report.Error` on a
   healthy report would trip the local dead-watchdog recovery check).
   The snapshot is still collected for the panel and traffic accounting.
3. **`dc2_stale` also covers `generated_at <= 0`** and future drift >120s per
   spec ("абс. дрейф > 120s в будущее — тоже несвежий"); boundaries
   `now-300s`/`now+120s` exactly are healthy (strict inequalities in code).
4. **`dc2_api_error` sends no `dc2_diag`**: nothing is known about the snapshot
   when transport fails; diag fields would be misleading zeros.
5. **Body cap 1MB + 5s timeout** on the Control API call (not in spec; defensive
   — the agent polls this every metrics cycle).

Not committed — left as working-tree changes on `dev` as instructed.
