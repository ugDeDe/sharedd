# Stage 02 — independent GP scheduler and repeatable health recovery

Date: 2026-10-17. **Status: partially_verified.** The independent scheduler and
same-IP recovery via the existing registry reverify path are implemented. This
is **not** the full proposed evidence-on-register contract in 01 §3.3. Review
findings H1/H2/M1 are fixed with regressions (see follow-up below). Lint and
independent re-review remain open; no provider automation is introduced.

## Actual files changed in this stage

- `node/internal/agent/main.go`: start GP before initial registry requests;
  finite `globalpingIteration`; GP-driven registration without a reverse kick;
  share the successfully registered IP with heartbeat; synchronize registration
  type and personal-secret reads/writes now that startup GP can overlap register.
- `node/internal/agent/terminate.go`: replace blocking IP-change/dead waits and
  lifetime `sync.Once` guards with repeatable, coalesced recovery state; one
  shared single-flight/backoff gate for runtime registration; deferred bounded
  retire notification; updated ban message. No health-triggered process exit.
- `node/internal/agent/healthcheck.go`: finite context-bounded GP polling,
  including interrupted hung GETs; only finished local measurements succeed;
  missing config/IP is a finite failure. GP report requests have an 85s timeout
  to allow registry verification (ordinary metrics reports retain 10s). Report
  404 (the actual registry contract) and legacy 410 schedule recovery rather
  than leaving a missing candidate unnoticed.
- `node/internal/agent/shared_config_sync.go`: a successful authenticated config
  fetch closes the bootstrap-only registration exception.
- `node/internal/agent/registry_api.go`: synchronized personal Bearer read and
  a finite error for nil configuration.
- `node/internal/agent/localhealth.go`, `netwatch.go`: correct obsolete comments
  about health-triggered exits and GP being muted by local health.
- `node/internal/agent/terminate_test.go`: replace the obsolete blocking-wait/
  restart assertions with same-IP repeated recovery, concurrent nonblocking
  termination, deferred/coalesced retire, cooldown, and credential tests; keep
  terminate JSON parsing checks.
- `node/internal/agent/recovery_test.go`: initial 403 plus muted/pruned GP
  iterations, failed/created-only GP, pending/hung polling, network backoff,
  register-before-report order, deduplicated measurement, no reverse kicks,
  and a terminate signal racing an in-flight registration.
- `node/internal/agent/review_recovery_test.go`: H1/H2/M1 regressions, including
  empty shared config, same-credential bootstrap retries, the bootstrap exit
  conditions, auth-refusal handling, and common monotonic 429 silence.
- `registry/internal/server/terminate.go`: current health-recovery wording,
  including removal of obsolete permanent-stop comments.
- `registry/internal/server/terminate_test.go`: the existing message regression
  now requires automatic GP recovery wording, not mandatory IP change/restart.
- `registry/internal/server/recovery_stage2_test.go`: ten same-IP reverify
  cycles through existing handlers, independent negative verdict despite a
  positive client boolean, enforced cooldown, and subsequent verified recovery.
  Also security-enabled pending-secret claim/config/quota/GP tests after cooldown
  expiry or an eligible IP change, and the real missing-candidate report 404.
- `docs/specs/hosting-blueprints/implementation-stage-02.md`: this report.

Stage-01 SSH/provision changes were left untouched. No UI, provider, dependency,
SQLite schema, persisted state format, installers, commits, or memory writes.

## Runtime and machine contract actually used

1. GP starts before initial IP resolution, registration, and shared-config fetch.
   Each iteration runs regardless of `ip_ban`, `dead`, metrics mute, or prune
   cooldown. Missing SNI/config/IP or a non-listening proxy returns to the next
   scheduled tick; it does not invent a successful measurement. The existing
   local TCP wait remains bounded at 75s, creation HTTP at 15s, and local polling
   at 45s including request cancellation. A whole iteration may additionally
   include resolver, retire, registration and report I/O; it is not a single
   45-second total operation.
2. `selfTerminate` records one active recovery episode and returns without
   network I/O, IP-change waiting, systemd actions, or process exit. Concurrent
   signals coalesce. Successful registration clears only the same episode, not
   a newer signal received while the request was in flight. Later episodes can
   recover indefinitely. A local dead notice is sent once, best effort, by a
   subsequent GP iteration with a 4s timeout, rather than by the caller.
3. A successful **completed local** measurement with a nonempty ID can trigger
   one recovery registration for that latest measurement. There is no cached
   evidence retry loop: a later scheduled check produces a new measurement.
   Failed GP/API outage/creation-only results cannot trigger GP recovery register.
   Before any successful registration or authenticated config fetch, heartbeat
   may instead make one bootstrap registration attempt per tick, using the same
   credentials and common backoff gate even if metrics are muted. This exception
   allows a pending secret to claim via `/register` before `/config` is permitted;
   it does not treat missing config as GP evidence. The first register/config
   success ends the exception for this process; subsequent health recovery still
   requires completed GP. A health signal never resets the bootstrap latch.
4. `POST /register` remains unchanged: `node_id`, `ip`, optional `node_type`
   and `authkey`, existing server/personal/pending Bearer selection. A health
   failure does not rotate identity or secrets. GP recovery specifically does
   not clear the personal/pending secret on 401; bootstrap now preserves those
   credentials too. The post-bootstrap ordinary-register credential-reissue
   policy remains unchanged. GP recovery registration success
   does not enqueue a GP kick. Sharing the registered IP prevents heartbeat from
   immediately registering it again and introducing an indirect kick.
5. For a banned same IP, HTTP 200 from `/register` means entry into existing
   **reverify quarantine, not healthy admission**. Then the same iteration sends
   its measurement ID via existing `POST /report`. The registry independently
   GET-fetches the measurement and applies its existing binding/verdict logic.
   Client `GlobalpingOK=true` is not authority. Healthy selection still requires
   the existing verified GP, metrics, TCP, port, freshness and quarantine rules.
   Local metrics mute does not suppress that GP report; bad metrics remain bad
   and do not become healthy merely because registration succeeded.
6. The existing failed-reverify cooldown remains 15 minutes, enforced by the
   registry. No new bypass, claim, owner, quota, auth, or rate-limit route was
   added. Runtime registration callers share one gate: at most one in-flight
   registration, delta-seconds `Retry-After` takes precedence, other failures use
   exponential delay starting at 5s plus jitter, bounded below 5min. Calls during
   backoff do not send requests. Every authoritative registration 429 (startup,
   bootstrap, GP or heartbeat) updates the shared monotonic prune-silence gate;
   ordinary network backoff is not a prune ban. GP continues locally during that
   delay; heartbeat/metrics reports and GP register/report traffic respect the
   prune deadline. Config synchronization retains its existing separate schedule.
   Actual retries occur on later
   heartbeat/GP ticks, so may be later than the backoff deadline.

## Tests and actual verification

Commands ran separately in their modules; RTK compressed output only.

| Working directory | Command | Result |
|---|---|---|
| `node/` | focused new recovery tests before implementation | RED: missing finite helpers/state (build failure) |
| `node/` | `go vet ./...` | PASS |
| `node/` | `go test -race ./...` | PASS, after H1/H2/M1 fixes: 147 tests in 3 packages |
| `node/` | `go build -mod=readonly ./...` | PASS |
| `node/` | `gofmt -l .` | PASS, no paths |
| `node/` | focused recovery tests with `-race -count=10` | PASS, 120 test/subtest executions before the final concurrent-registration test was added; that test passed in the final full suite |
| `registry/` | `go vet ./...` | PASS |
| `registry/` | `go test -race ./...` | PASS, after H1/H2/M1 fixes: 471 tests in 13 packages |
| `registry/` | `go build -mod=readonly ./...` | PASS |
| `registry/` | `gofmt -l .` | PASS, no paths |
| `registry/` | `go test -race ./internal/server -run '^TestSameIPRepeatedRecoveryRequiresIndependentGP$' -count=10` | PASS, 10 executions (100 complete repeated-recovery cycles) |
| repository | `git diff --check` | PASS |
| environment | `command -v golangci-lint` | executable absent; lint NOT RUN |
| environment | `command -v staticcheck` | executable absent; standalone staticcheck NOT RUN |

An initial registry full-suite invocation exceeded the tool's 120s limit. The
longer rerun exposed the old mandatory-IP-change message assertion; it was
updated meaningfully, then the complete vet/race/build/format sequence passed.
No live Globalping/telemt/systemd/provider calls were required by these tests.

## Review follow-up — H1, H2, M1

Read `review-stages-01-02.md` directly. All three findings are addressed locally;
the original review artifact is left unchanged for independent re-review.

### Fixes and regression coverage

- **H1:** `SendReport` now recognizes `/report`'s actual 404 as well as legacy
  410. `TestMissingCandidateReportRecoversWhileMetricsMuted` starts with a
  successful registration, removes the mock candidate, mutes metrics, and runs
  only finite GP iterations (no dead timer/metrics loop can rescue the test).
  It checks the report refusal arms recovery, the next fresh measurement causes
  exactly same-ID `/register` then `/report`, personal Bearer is retained, and
  no reverse GP kick is queued. Both statuses are covered. Ordinary report
  401/403 remain errors, not missing-candidate recovery or secret-discard signals.
  The authenticated registry test additionally pins the real handler's 404 after
  deleting its candidate without deleting ownership/credentials.
- **H2:** `recoveryState.bootstrapped` is a process-lifetime latch, set by the
  first successful registration or authenticated config fetch, not by a health
  refusal. Before it is set, the finite heartbeat helper permits authenticated
  bootstrap retries even in active recovery or metrics mute, while respecting
  single-flight, network backoff and prune silence. Bootstrap 401 preserves the
  same pending credential rather than falling back to another credential.
  `TestPendingBootstrapRetriesWithoutSharedConfig` proves a real empty shared
  cache produces no GP evidence, initial 403 and subsequent 401 remain finite,
  repeated pre-deadline retries send no requests, a later 200 completes bootstrap
  without process restart, and a new health episode cannot reuse the exception.
  `TestSuccessfulConfigEndsBootstrapException` covers failed versus successful
  config fetches and the ordinary recovery gate once config has succeeded.
- **H2 authenticated integration:** `TestPendingSecretBootstrapAfterBan` uses
  a real control SQLite pending secret, users/quota enabled, configured machine
  authentication, and the production mux. It tests separately cooldown expiry
  on the same IP and a changed eligible IP. Before claim, `/config` remains
  forbidden and ownership is not created; after claim/re-register the same
  secret is bound to the same ID, no rotated secret is returned, the pending
  record is consumed and exactly one owner/quota slot is used. `/config` becomes
  authorized only for that identity. Same-IP recovery remains quarantined until
  the report's independent GET verification succeeds. No registry production
  auth/claim/admission code changed.
- **M1:** authoritative 429 handling now calls `gate.noteBan` in the common
  HTTP registration path. The runtime retry gate also honors a longer existing
  prune deadline. Heartbeat no longer converts every registration failure's
  exponential backoff into prune silence. Tests cover startup and GP 429 with
  `Retry-After: 900`, both deadlines, continued local GP iterations without
  further registry requests, the heartbeat bootstrap helper during silence,
  shorter server deadlines not reducing existing silence, and network errors
  leaving the prune gate off. Metrics/heartbeat retain their existing
  `gate.silent()` checks; tests exercise finite helpers, not infinite daemon loops.

### Files touched by this review-fix pass only

1. `node/internal/agent/healthcheck.go`
2. `node/internal/agent/main.go`
3. `node/internal/agent/terminate.go`
4. `node/internal/agent/shared_config_sync.go`
5. `node/internal/agent/terminate_test.go` (isolate prune gate per recovery test)
6. `node/internal/agent/recovery_test.go`
7. `node/internal/agent/review_recovery_test.go` (new)
8. `registry/internal/server/recovery_stage2_test.go`
9. `docs/specs/hosting-blueprints/implementation-stage-02.md`

### Follow-up commands actually run

- New node tests were added first; the initial focused run failed to compile
  because the finite bootstrap helper did not yet exist. After implementation,
  `go test -race ./internal/agent -run
  'Test(MissingCandidateReport|PendingBootstrap|Registration429|SuccessfulConfigEnds|ReportAuthRefusal)'
  -count=10` passed (120 test/subtest executions). The registry integration
  fixture initially omitted required `node_type`; adding `classic` made the
  request conform to the security-enabled contract, without weakening validation.
- `registry/`: `go test -race ./internal/server -run
  '^TestPendingSecretBootstrapAfterBan$' -count=5` passed (15 test/subtest
  executions, including both actual pending-secret claim scenarios per run).
- Both modules: `go vet ./...`, `go test -race ./...`,
  `go build -mod=readonly ./...`, and `gofmt -l .` passed after the fixes.
  Final full-suite counts are in the verification table above. Repository
  `git diff --check` passed. Lint/staticcheck executables remain unavailable.
- Existing SSH/provision edits were untouched. No commits, memory writes,
  provider changes, evidence DTO extension, or new auth privileges were added.

## Exact gaps and limitations (do not claim full stage-02/spec compliance)

- **No `recovery_evidence` DTO on `/register`.** The safer, existing two-request
  reverify contract was chosen deliberately. There is no verification I/O or
  new authorization/CAS logic in registration, and no evidence-based fast admit.
  Loss of `/report` after register leaves the node quarantined until another
  report/check or existing expiry handling; this is not an atomic admission
  transaction and may require another GP interval. Dead retains the existing
  register-lifts-record rule, followed by ordinary health admission.
- **GP API measurement age is not independently authenticated by this stage.**
  The existing GET verifier checks target/IP, report port, type/protocol/SNI,
  results and ratio; it polls `in-progress` measurements. Its measurement DTO
  has no API creation/completion timestamp. Report timestamp/replay checks and
  the GP-validity latch are not proof of API measurement age. Cross-episode or
  cross-identity reuse of an old accessible measurement is therefore an existing
  gap, not solved by this fallback. The verifier also treats top-level statuses
  other than `in-progress` as terminal; this stage strictly requires `finished`
  only on the **local agent** side, not a new registry validation rule.
- **No durable retirement fences.** `superseded`/`deleting` resource fencing and
  schema migrations belong to later stages. This change must not be represented
  as safe enablement of automated replacement/deletion or PROFILE execution.
- Agent recovery/backoff/dedup state is in memory. Restart resets it; registry
  persistent bans, cooldown, ownership/pending claims, quota and admission remain
  authoritative and unchanged. Only the latest measurement ID is remembered;
  there is no durable or unbounded measurement replay ledger.
- Tests exercise finite scheduler iterations and polling directly, not a live
  daemon/supervisor deployment. Startup ordering is also verified by source
  inspection. Missing node TOML at process startup still fails config loading;
  missing telemt/shared configuration during an iteration is recoverable.
  The two Go modules' internal packages are tested separately: agent scheduling
  uses local HTTP mocks, while registry integration uses the real secured mux
  and control store. This is not one end-to-end live-agent/VPS integration test.
- Retire notification may be delayed until the next GP iteration and is
  best-effort, with no retry journal. Concurrent different health reasons are
  coalesced, not a durable ordered event stream. Registry remains the authority.
- The pre-existing network-change watchdog can still restart under systemd for
  network failure/IP change; health rejection itself no longer calls exit.
  Legacy persisted ban records can retain their old message text; new records
  use the updated constants without rewriting persisted state/history.
- GET verification still uses existing registry polling/timeouts; the agent's
  85s GP report timeout is finite, not a guarantee that every pathological
  verification outage finishes before the response timeout. Failure must leave
  health admission gated, with a future scheduled check available.
- Independent re-review is reserved for the leader as requested. No delegation
  was performed. In particular review the fallback's existing
  measurement-age/replay limitations before release; passing mocks do not close
  that gate. MANUAL/provider behavior remains unchanged (no purchases/deletes).

Remaining local command after installing a repository-compatible linter:

```bash
# Run separately from node/ and registry/:
golangci-lint run ./...
staticcheck ./...
```
