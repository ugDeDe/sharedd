# Bounded Globalping measurement freshness safety fix

## Provider evidence (GET only)

Inspected the official OpenAPI response document with an HTTP GET:
<https://api.globalping.io/v1/spec.yaml>.

- `paths./v1/measurements/{id}.get.responses.200` resolves to
  `components.responses.measurement200`, which includes
  `components.schemas.MeasurementResponse`.
- `MeasurementResponse.required` includes `createdAt`, `updatedAt`, and `status`.
  Top-level `createdAt` is a string with `format: date-time`, described as
  **“The date and time when the measurement was created.”**
- `components.examples.getHttpMeasurementResponse.value` contains
  `status: finished`, `createdAt: '2023-07-14T18:34:05.161Z'`, and
  `updatedAt: '2023-07-14T18:34:06.310Z'`. This is RFC 3339 with fractional
  seconds and UTC `Z`; Go's `time.Time` JSON decoder handles it directly.
- `MeasurementStatus` enumerates `in-progress` and `finished`.
  The GET description notes measurements may remain retrievable for six months:
  successful retrieval is not proof of recent execution.

Only the provider's **top-level creation timestamp** is used. Neither
`updatedAt`, nested TLS certificate `createdAt`, probe HTTP `headers.date`, HTTP
response `Date`, nor the node's `checked_at` dates independent health proof.
Creation time is conservative: waiting for completion cannot rejuvenate an old
measurement. No measurement was created and no live node/provider mutation was
performed during this task.

## Storage and trust boundary

- `globalping.Measurement.CreatedAt` decodes `createdAt` as `time.Time`.
  Missing/null is zero; malformed timestamps fail JSON decoding.
- `Candidate.GlobalpingMeasurementAt` persists as
  `globalping_measurement_at`. It is assigned only after the existing independent
  GET/binding checks succeed, top-level status equals `finished`, and creation
  time is not after the registry verification receipt. **Future-clock tolerance
  is explicitly zero.** A future result clears this proof rather than becoming
  eligible when the clock catches up. Missing time remains zero.
- `LastGlobalpingAt` / `last_globalping_at` remains the verification **receipt**
  time for compatibility. It, `GPLast.At`, history timestamps and `checked_at`
  are never fallbacks for blueprint measurement freshness.
- `EndpointRegisteredAt` / `endpoint_registered_at` advances on IP changes and
  changes of an already known port, and trusted GP proof is cleared alongside
  existing GP/TCP invalidation. The initial port report uses `RegisteredAt`;
  re-registering the same endpoint does not reset its epoch. The effective lower
  bound is the later of `RegisteredAt` and `EndpointRegisteredAt`.
- JSON additions are backward compatible: legacy candidates load with zero
  trusted measurement time and fail closed for destructive automation until
  new qualifying proof arrives. No receipt-to-measurement migration is made.

## Gates and intentionally preserved semantics

`blueprintResourceReady` requires nonzero registration and trusted measurement
timestamps, `GlobalpingValidityTTL > 0`, measurement age no greater than that
TTL, no future timestamp, and proof at/after both registration/endpoint epochs.
Existing identity, resource state, port, quarantine, termination, metrics and
TCP requirements remain; local evidence is also bounded by the endpoint epoch.

The existing call sites apply the same helper to `pool.admit`, fence, switch,
and automated delete-old (including its final check before `cloudDeleteDo`).
Missing, malformed, incomplete, future, expired or pre-epoch GP proof cannot
authorize replacement/deletion: the gate returns `readiness_unverified`.
Destructive boundaries were not refactored or bypassed.

Report/queue/quarantine semantics and the receipt-based legacy freshness sweep
are intentionally not redesigned. Independently bound old/missing-time results
may still update their legacy report verdict, but cannot authorize automatic
replacement. An inconclusive fetch/decode retains any previous proof **without
renewing its measurement timestamp**; only that original proof's validity may
still qualify. No heartbeat workaround, mandatory IP change or restart is added.
No route, graph schema, cabinet DTO, UI or generated asset changes are included.

## Focused regression and verification

`TestSafetyGlobalpingMeasurementFreshness` uses a local GET-only mock, real health
report ingestion, persisted-state reload and `blueprintResourceReady`. Its table
covers old, fresh, missing, malformed, future, pre-registration, pre-endpoint and
not-finished proof plus IP/port invalidation. Fresh report/receipt/detail times
cannot rescue historical evidence; future proof is not retained for later use.
Existing safety fixtures only gain the trusted field and retarget their two
GP-age mutations to measurement rather than receipt time.

Requested bounded commands (working directory `registry/`):

```sh
go test -vet=off -run '^TestSafetyGlobalpingMeasurementFreshness$' ./internal/server
go build -mod=readonly ./internal/globalping ./internal/state ./internal/server
```

**Verification: verified within this bounded scope.** Both commands above passed
once, invoked through `rtk`; the test reported 11 passes (the parent and its 10
table cases). Changed Go files were formatted with `gofmt`; the scoped
`git diff --check` passed. Other server tests
were compiled as package fixtures, not executed. No all-package tests, race
runs, vet sweeps, UI generation, commits, memory operations or delegation were
performed. Live infrastructure health and unrelated behavior are not verified.

Files changed for this fix:

- `registry/internal/globalping/client.go`
- `registry/internal/state/state.go`
- `registry/internal/server/healthcheck.go`
- `registry/internal/server/run_main_impl.go` (two-line IP proof/epoch invalidation)
- `registry/internal/server/blueprint_engine.go` (`blueprintResourceReady` only)
- `registry/internal/server/blueprint_safety_test.go`
- `docs/specs/hosting-blueprints/safety-gp-freshness.md`
