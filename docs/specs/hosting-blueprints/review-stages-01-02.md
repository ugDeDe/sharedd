# Application review — stages 01–02

**Latest follow-up verdict (2026-10-17): PASS / APPROVE for the implemented
limited stages 01–02. H1, H2 and M1 are closed; no new blocking security
regression was identified. Verification remains partially_verified because
staticcheck/golangci-lint are unavailable.** The original review below is
preserved as history; the follow-up at the end supersedes its BLOCK verdict.

## Original review — before H1/H2/M1 fixes

Date: 2026-10-17. Scope: current application diff, including untracked tests.
**Verdict: BLOCK. Verification: partially_verified.** No application files were
edited, no commits made, no delegation performed, and no memory saved.

## Review Summary

| Severity | Count | Status |
|----------|-------|--------|
| CRITICAL | 0 | pass |
| HIGH | 2 | block |
| MEDIUM | 1 | warning |

Stage 01 has no blocking finding. Stage 02 has two recovery liveness defects
and a prune-silence inconsistency. These are application-control-flow findings,
not evidence that the unchanged registry authentication or GP verifier was
bypassed. Passing existing tests does not cover the scenarios below.

## Findings

### H1 — Missing-candidate reports do not actually schedule recovery

**Locations:** `node/internal/agent/healthcheck.go:872–874`,
`node/internal/agent/main.go:392–405`; actual server response:
`registry/internal/server/healthcheck.go:94–96`.

`SendReport` recognizes HTTP **410**, but this registry's `/report` returns
HTTP **404** for an unknown/pruned candidate. Only `/heartbeat` uses 410.
Consequently an already registered agent keeps `recovery.registered=true` after
the candidate disappears. A completed successful GP check continues taking the
report-only path; its 404 is logged as an ordinary error and does not arm recovery.

This matters specifically to the new independent path: while metrics are muted,
heartbeat is suppressed, so it cannot discover the missing candidate and repair
the registration. With `watchdog.dead_kill_ms=-1`, this can last indefinitely;
otherwise a later local dead transition may eventually mask the defect. The GP
scheduler runs, but the advertised metrics-independent prune recovery does not.
The existing HTTP status predates this diff; the new recovery dispatch handles
the wrong status and the new tests do not exercise the real report contract.

**Minimal fix:** recognize the registry's actual unknown-candidate 404 as a
recovery signal (retaining 410 compatibility if desired), then use the existing
completed-GP registration/backoff path. Do not interpret 401/ordinary 403 as
permission to discard credentials or bypass authorization.

**Test acceptance:** begin with a successful registration and metrics-muted
state, remove the registry candidate, and return the real `/report` 404. The
agent must arm recovery; a subsequent fresh successful GP iteration must issue
one gated same-ID `/register` followed by `/report`, retaining the secret and
without a reverse GP kick. Run with local dead notification disabled so it
cannot accidentally rescue the test. Cover both 404 and supported 410 behavior.

### H2 — Initial ban refusal can permanently strand a pending-secret install

**Locations:** `node/internal/agent/main.go:226–229,296–308,392–405`,
`node/internal/agent/terminate.go:102–104`. Supporting unchanged contracts:
`registry/internal/server/run_main_impl.go:288–310`,
`registry/internal/server/api_auth.go:152–161`,
`node/internal/agent/healthcheck.go:523–547`.

A valid pending install secret may receive `403 terminate:ip_ban` before its
claim is processed: `/register` checks existing IP-ban cooldown before
`authorizeUserRegister`. The new agent sets `recovery.active=true`, after which
ordinary startup/heartbeat registration retries are forbidden until a successful
local GP measurement exists.

However, an unclaimed pending secret is deliberately authorized only for
`/register`, not `/config`. On a fresh process the shared-config cache is empty,
and GP obtains its required fake SNI exclusively from that cache. Thus:

`initial terminate → active recovery → /config forbidden → GP missing SNI →
no recovery register → pending secret never claimed`.

Cooldown expiry cannot break this dependency cycle. Even changing to an unbanned
IP cannot trigger ordinary registration while `active` remains true. This is
not merely the acknowledged finite “skip GP when config is missing” limitation:
the new recovery gate prevents the only authenticated operation that can make
configuration obtainable. The previous IP-change/restart path could retry that
claim after an address change; the replacement state machine cannot.

**Minimal fix:** provide a bounded, backoff-gated authenticated bootstrap retry
when no registration/configuration has ever completed. It must use the same
identity and pending secret and preserve the registry's cooldown, claim, owner,
quota and admission checks. Do not grant pending secrets access to `/config` or
allow missing configuration to count as successful GP evidence. Distinguish
bootstrap from recovery of a previously registered node.

**Test acceptance:** use security-enabled handlers with a real pending secret
and an empty agent shared-config cache. Make initial registration encounter an
existing banned-IP cooldown; assert `/config` remains forbidden. After expiry
(and separately after changing to an eligible IP), the live agent must retry the
claim without restart, preserve the pending secret, consume exactly one owner
quota slot, fetch authorized configuration, and proceed to independent GP
verification. Ordinary active recovery must still require completed local GP
once bootstrap has succeeded. The current new registry recovery test uses the
security-disabled fixture and does not cover this interaction.

### M1 — GP/startup 429 updates registration backoff but not prune traffic silence

**Locations:** `node/internal/agent/terminate.go:108–129`,
`node/internal/agent/main.go:174–176,286–292,405–410`.

Only heartbeat's wrapper calls `gate.noteBan`. A 429 returned to the new GP
registration path sets `recovery.next`, but its caller discards `retryAfter` and
does not set `gate.banUntil`. Startup also ignores that return value. While the
registration single-flight/backoff gate prevents repeated `/register` requests,
healthy metrics reports can still hit the registry during the prune deadline.
This contradicts the stage report's claim that runtime paths respect the
existing prune traffic-silence deadline. The startup omission existed before;
the GP registration path introduces an additional caller with the same gap.

**Minimal fix:** apply an authoritative registration 429's Retry-After to the
shared prune traffic gate in the common registration path, rather than relying
on heartbeat alone. Do not turn ordinary network-error exponential backoff into
a prune ban; keep local GP checks running.

**Test acceptance:** have a successful local GP check reach `/register`, return
429 with `Retry-After: 900`, and assert both the shared retry deadline and prune
traffic gate advance. Further local GP checks must still run; registration and
normal report/heartbeat traffic must stay suppressed until expiry. Cover startup
too, and ensure shorter subsequent waits cannot shorten an active prune ban.

## Security and concurrency assessment

- Reviewed every changed agent file, both untracked recovery test files,
  registry terminate wording/tests, `sshx` implementation/tests and the added
  provision finalization test. Read `AGENTS.md`, `01-requirements.md`, and both
  implementation reports directly. Existing user SSH work was inspected as part
  of the combined diff, not rewritten or attributed wholesale to stage 01.
- No new critical injection, secret disclosure, credential substitution,
  authorization bypass or provider mutation was identified in these changes.
  The wire contract remains existing `/register` then `/report`, **not** the
  proposed evidence-on-register extension. Registration success alone is not
  IP-ban admission: same-IP reverify remains quarantined until the independent
  report verifier accepts it; other health/port/selection gates remain in place.
- Personal-secret reads used by concurrent requests now take `secretMu`;
  startup loads occur before GP starts. Registration type access is serialized.
  Runtime registration uses `TryLock` single-flight and a shared bounded retry
  deadline; no registration/recovery/secret mutex lock-order cycle was found.
  Recovery registration does not kick GP and publishes its registered IP so the
  next heartbeat need not immediately register it again. Epoch comparison
  protects a first terminate transition arriving during an older registration.
- Pending claims, owner/quota authorization and authenticated personal-secret
  re-registration remain registry-owned and unchanged. GP recovery preserves
  the stored secret on 401; ordinary-register credential reissue is pre-existing.
  H2 is an authorization/lifecycle integration failure, not a request to weaken
  these gates. The stage-02 test's positive-client/negative-verifier case and
  repeated cooldown cycles pass, but are not a new authenticated claim test.
- Local GP polling now has a context covering hung GETs and requires `finished`.
  Missing data and failures return to scheduling; creation alone cannot trigger
  recovery. A normal completed negative result without a local error still
  reaches `/report` when no recovery is required. Report silence is finite but
  post-registration report loss remains non-atomic, as explicitly documented.
- SSH has separate 20s dial/handshake bounds and an absolute operation budget.
  One outstanding keepalive plus an independent 60s watchdog avoids accumulating
  blocked probe goroutines. Both positive and negative replies reset liveness;
  stdout silence does not. Context cancellation closes the socket, breaking
  session setup, blocked stdin/request writes, output reads and wait; keepalive
  is joined before return. Wrapped cancellation/transport causes and partial
  output are retained. No new blocking teardown/timer leak was identified.

## Existing limitations, not new findings

- Registry measurement API timestamps are not authenticated by the current DTO;
  per-candidate report timestamps/measurement rings do not prove external
  measurement age or prevent all cross-episode/identity reuse. The unchanged
  verifier accepts non-`in-progress` terminal statuses, unlike the stricter new
  local poller. This diff does **not** close those gaps or alter those checks.
  No new verifier bypass was found; fresh-local-agent behavior is not a security
  defense against a malicious authenticated agent replaying older evidence.
- SSH host-key verification remains `InsecureIgnoreHostKey`. Uncertain remote
  shell side effects, durable `needs_attention` checkpoints, retirement fences,
  execution leases and restart reconciliation remain deferred. The provision
  test verifies the existing `step=error`/`operation=failed` behavior, not the
  full future blueprint. No automatic remote-command replay was added here.
- Recovery/backoff state is process-local; deferred retire is best-effort and
  coalesced. The separate pre-existing network watchdog can still restart under
  systemd. This review does not approve automated replacement/deletion,
  PROFILE execution, or full requirements-document compliance.

## Actual verification in this review

Go: `go1.27.1-X:nodwarf5 linux/amd64`. Commands ran in the separate modules,
not at repository root. `rtk` wrapped vet output only. No live external services
were used. No new reproduction test/application files were written; findings
above follow directly from the cited handlers and state transitions.

| Directory | Command | Actual result |
|-----------|---------|---------------|
| `node/` | `go vet ./...` | PASS, no diagnostics |
| `node/` | `go build -mod=readonly ./...` | PASS |
| `node/` | `go test -race ./internal/agent -count=1` | PASS, 15.164s |
| `node/` | `go test -race ./...` | PASS; agent 16.295s, config cached, root has no tests |
| `registry/` | `go vet ./...` | PASS, no diagnostics |
| `registry/` | `go build -mod=readonly ./...` | PASS |
| `registry/` | `go test -race ./internal/sshx ./internal/provision ./internal/server -run 'TestRun\|TestKeepalive\|TestInstallProfileSSHFailureFinalizes\|TestSameIPRepeatedRecoveryRequiresIndependentGP\|TestGPQuarantineFullLifecycle\|TestProvisionWorkerGuardsOperation' -count=3` | PASS; sshx 6.130s, provision 6.404s, server 1.683s |
| `registry/` | `go test -race ./...` | PASS; server 130.421s, control 42.712s, provision 21.951s, sshx 2.722s; remaining packages passed/cached or have no tests |
| both modules | `staticcheck ./...` | Attempted separately; unavailable: `staticcheck: command not found` |
| repository | `command -v golangci-lint` | Executable unavailable; golangci-lint not run |
| repository | `git diff --check` | PASS before artifact creation |

Full race tests passed, but staticcheck/lint remain unverified. The required new
regressions for H1/H2/M1 are not present in the current suite. **Release gate:
resolve H1 and H2, add their acceptance checks, rerun vet/race/build and obtain
staticcheck/lint results; then re-review.** M1 should also be corrected to match
the documented prune-silence contract.

## Follow-up re-review — H1/H2/M1 fixes

Date: 2026-10-17. **Verdict: PASS / APPROVE for the implemented limited stage.**
All three original findings are closed. No new CRITICAL/HIGH finding or blocking
security regression was identified in the fix pass. This supersedes the original
release-block recommendation above, not its historical observations.

| Severity | Remaining findings from this review | Status |
|----------|-------------------------------------|--------|
| CRITICAL | 0 | pass |
| HIGH | 0 | pass |
| MEDIUM | 0 | pass |

### Evidence reviewed and closure decisions

Re-read the updated `implementation-stage-02.md` directly, including its actual
bootstrap exception, follow-up commands, and limitations. Inspected current
`main.go`, `terminate.go`, `healthcheck.go`, the `shared_config_sync.go` change,
updated recovery-test isolation/network-backoff tests, all of
`review_recovery_test.go`, and the real secured registry test
`TestPendingSecretBootstrapAfterBan` in `recovery_stage2_test.go`.

- **H1 CLOSED:** `healthcheck.go:872–874` maps both actual 404 and compatible
  410 report refusals to missing-candidate recovery. The new agent regression
  starts registered, removes its candidate, mutes metrics, and exercises only
  finite GP iterations. It checks `/register /report /register /report`, same ID
  and personal Bearer, cleared recovery, and no reverse kick for both statuses.
  The secured registry test additionally pins the production handler's 404 after
  candidate deletion while ownership/authentication survive. Ordinary 401/403
  report refusals do not become missing-candidate signals or discard credentials.
- **H2 CLOSED:** `terminate.go:35,52–67,126–143` implements a mutex-protected
  process-lifetime bootstrap latch. Heartbeat invokes the finite retry helper
  before metrics/config availability can strand the pending claim. Retries
  retain credentials and use the existing registration single-flight/backoff
  and authoritative prune gate. First registration success or successful parsed
  authenticated config fetch (`shared_config_sync.go:443–445`) ends the
  exception; health transitions do not reset it. Established active recovery
  still requires completed successful local GP. A normal bootstrap registration
  can enqueue its initial coalesced kick; GP-driven recovery cannot reverse-kick.
- **H2 authenticated proof:** the inspected test uses configured machine Bearer
  security, a real SQLite pending-secret record, users enabled, a one-node quota,
  and the production mux—not the security-disabled lifecycle fixture. Both
  cooldown-expiry/same-IP and eligible-IP-change subtests prove: no early owner
  or consumed claim; `/config` forbidden while pending; same secret/id retained
  after claim and idempotent re-register; no secret rotation; exactly one owner
  and quota slot; config authorized for the claimed identity but not another ID.
  Same-IP registration remains in reverify quarantine until an independently
  fetched GP result succeeds. Agent scheduling and registry authorization are
  tested in their separate internal modules, not claimed as a live daemon E2E.
- **M1 CLOSED:** `main.go:231–240` applies authoritative registration 429 silence
  in the common HTTP path. `tryRegistration` inherits a longer existing prune
  deadline, and heartbeat no longer promotes ordinary network backoff to a ban.
  Tests verify startup/GP callers, `Retry-After: 900`, both gates, shorter waits
  not reducing silence, continued local GP checks without further register/report
  requests, and network failures not activating prune silence. Config sync keeps
  its existing independent schedule; this is not a blanket shutdown of all HTTP.

### Security/concurrency gate

Bootstrap is an exception to the **agent's local scheduling gate**, not an
exception to registry authentication, owner/pending claim, quota, cooldown or GP
admission. No registry production authorization/verifier route was changed by
these fixes. Missing configuration is not successful GP evidence. Bootstrap and
GP 401 responses preserve credentials rather than falling back to another
identity; invalid credentials still require a valid authenticated repair path.
The existing ordinary reissue behavior applies outside that bootstrap/GP mode.

The bootstrap latch and recovery state use the existing mutex; prune state is
snapshotted and its lock released before recovery locking. No new lock-order
cycle, concurrent registration fan-out, or reverse-kick loop was identified.
Focused repeated race tests covering the fixes and existing registration/GP
coordination passed. The original SSH/provision review remains applicable;
these fixes do not alter that implementation.

### Actual follow-up verification

| Directory | Command | Result in this re-review |
|-----------|---------|--------------------------|
| `node/` | `go vet ./...` | PASS, no diagnostics (`rtk` wrapper) |
| `node/` | `go test -race ./internal/agent -run 'Test(MissingCandidateReport\|PendingBootstrap\|Registration429\|SuccessfulConfigEnds\|ReportAuthRefusal\|RegistrationNetworkBackoff\|ConcurrentTerminateDuringRegistration\|CompletedGPRegistersBeforeReportWithoutKick\|GPPollFinite\|SameIPRecoveryRepeatedCycles\|TerminateNonblockingConcurrent\|RegistrationRetryGate\|Recovery401PreservesSecret)' -count=5` | PASS, 1.344s |
| `registry/` | `go vet ./...` | PASS, no diagnostics (`rtk` wrapper) |
| `registry/` | `go test -race ./internal/server -run '^(TestPendingSecretBootstrapAfterBan\|TestSameIPRepeatedRecoveryRequiresIndependentGP)$' -count=5` | PASS, 9.982s |
| both modules | `staticcheck ./...` | Attempted separately; `staticcheck: command not found` |
| environment | `command -v golangci-lint` | Unavailable; lint not run |
| repository | `git diff --check` | PASS before follow-up artifact edit |

Full suites/builds were not repeated in this follow-up, as requested. The updated
implementation report records full vet/race/readonly-build success after fixes;
the independent results claimed here are the focused commands above. No
application/test edits, commits, delegation or memory writes were made by this
re-review; only this review artifact was updated.

### Acceptance boundary and remaining open work

**Accept the limited implementation:** SSH transport liveness and bounded,
nonblocking, repeatable health recovery using existing `/register` followed by
independently verified `/report`, including authenticated pending bootstrap.
There is no remaining H1/H2/M1 application gate blocker.

**Do not treat this PASS as full blueprint or unrestricted production-security
certification.** Staticcheck/golangci-lint remain outstanding. The pre-existing
external-measurement age/replay and terminal-status validation gaps, SSH host-key
trust policy, non-atomic report loss, process-local recovery state, best-effort
retire, missing durable retirement/uncertainty fences and absence of a live
agent/VPS E2E remain open exactly as scoped above and in the updated implementation
report. No evidence-on-register extension, automated provider lifecycle,
replacement/deletion or PROFILE-execution approval is implied.
