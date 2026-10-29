# Stage 01 — SSH transport liveness

Date: 2026-10-16. Scope: SSH liveness only. **Verification: partially_verified**
(registry vet/race tests/build pass; golangci-lint is not installed).

## Files

- `registry/internal/sshx/sshx.go`: keepalive and transport-loss cause, built on
  the user's existing context-triggered socket closure and handshake deadline.
- `registry/internal/sshx/sshx_test.go`: short-interval local SSH and blocked-write
  regression tests; the user's existing tests remain in place.
- `registry/internal/provision/provision_test.go`: regression for step/operation
  finalization on transport loss, cancellation, and deadline.
- `docs/specs/hosting-blueprints/implementation-stage-01.md`: this report.

No dependencies, APIs, schema, UI, provider integrations, or node code changed.
No commits or memory writes were made. Pre-existing untracked blueprint documents
and the user's SSH changes were not replaced.

## Behavior

- After handshake, send global `keepalive@openssh.com` requests with
  `wantReply=true` every 20 seconds. Both positive and negative SSH replies
  acknowledge a live transport.
- An independent 60-second watchdog starts after handshake and resets after
  each successful request/reply exchange. On expiry it cancels with
  `sshx.ErrTransportLost` (`ssh_transport_lost`). The existing context hook
  closes the raw socket, interrupting a blocked request write or reply wait.
  Only one keepalive request is outstanding; a blackhole does not accumulate
  goroutines. The keepalive goroutine is joined before `Run` returns.
- Stdout/stderr silence is not a timeout condition. The absolute step budget
  still applies; output and keepalives do not extend it.
- TCP connect and handshake retain their separate 20-second limits and earlier
  context cancellation. Provisioning still passes its configured step timeout,
  default 20 minutes. The library's nonpositive-timeout fallback remains 15 minutes.
- Session setup failures retain the cancellation/transport-loss cause through
  error wrapping. Running-command failure returns partial output after SSH output
  readers finish, preserving the user's buffer/race-safety changes.

## Provisioning inspection

No production provision/worker changes were necessary:

- `Provisioner.runSteps` marks the current step `error`, records `finished_at`
  and a truncated, secret-masked output tail, then returns without another step.
- `InstallProfile` persists operation `failed` with the wrapped reason/output
  and retains the existing VPS identity/resource reservation. It does not buy,
  delete, or automatically replay shell commands after SSH failure.
- `provisionWorker.run` deletes the operation's running-map guard in its deferred
  cleanup when the installation returns. It does not hold its mutex over SSH I/O.
  Existing `TestProvisionWorkerGuardsOperation` verifies guard release and that
  another run can start; it passed in the full race suite.
- The new `TestInstallProfileSSHFailureFinalizes` verifies persisted operation
  failure, retained VPS ID, stopped subsequent steps, and a finished error step
  for transport loss, cancellation, and deadline errors.

## Tests and actual results

All commands ran in `registry/`. Full-module command output was compressed with
`rtk`; the invoked Go commands and flags were unchanged.

| Command | Result |
|---|---|
| `go test -race ./internal/sshx -count=5` | PASS, `ok sharedd/registry/internal/sshx 9.043s` |
| `go test -race ./internal/sshx ./internal/provision -count=5` | PASS, SSH `9.626s`, provision `103.748s` |
| `go test -race ./internal/sshx -run '^(TestRunKeepalive\|TestKeepaliveBlockedWrite)$' -count=20` | PASS, RTK reported 200 passed in 1 package |
| `go vet ./...` | PASS, no diagnostics |
| `go test -race ./...` | PASS, RTK reported 467 passed in 13 packages; no race reports |
| `go build -mod=readonly ./...` | PASS, no diagnostics |
| `golangci-lint run ./...` | NOT RUN: executable absent; `Failed to spawn process: No such file or directory (os error 2)` |
| `gofmt -l internal/sshx/sshx.go internal/sshx/sshx_test.go internal/provision/provision_test.go` | PASS, no paths printed |
| `git diff --check` | PASS, no diagnostics |

`TestRunKeepalive` uses a local SSH server, a 10ms interval and 150ms liveness
budget (3s outer safety deadline). It covers unanswered session-open/exec,
unread stdin exceeding the SSH channel window, command/output wait, cancellation
while a probe awaits its reply, and silent success after 25 negative probe
replies (longer than one liveness budget). Failure retains partial output.
`TestKeepaliveBlockedWrite` uses `net.Pipe` with no reader to deterministically
block the request write, then verifies watchdog and explicit cancellation both
close the socket and allow the request goroutine to exit. Local SSH server and
request-handler completion is awaited; no goroutine-count sleeps/heuristics or
new test dependencies were introduced. Existing handshake/session deadline and
success/nonzero-exit/disconnect tests also pass.

## Security review and outstanding limits

- Scope review before implementation: no new command input, credentials, target
  selection, authentication bypass, retries, or external endpoints. Keepalive
  payload is empty. Existing secret masking and shell rendering remain unchanged.
- Existing `InsecureIgnoreHostKey` policy remains unchanged: host-key pinning and
  SSH-target trust remain existing security limitations, not solved by liveness.
- No independent reviewer/delegation tool was available in this session; review
  was local. This does not claim the broader blueprint's independent review gate.
- Tests use real monotonic timers with short injected durations and synchronized
  server events, not a virtual clock. No live 60-second network blackhole or
  real VPS install was run. Detection is approximately 60 seconds since the last
  keepalive acknowledgment, subject to process scheduling; a server that answers
  probes while its command hangs still waits for the configured step budget.
- Legacy persistence remains `step=error`, `operation=failed`, with the reason
  in the operation error text. Durable `needs_attention`/uncertainty checkpoints,
  execution leases, new reason/status columns, and restart reconciliation belong
  to later storage/executor stages and are **not** implemented here. Remote shell
  side effects are not rolled back by socket closure; explicit retry can replay
  commands and still requires operator judgment. Existing best-effort persistence
  error handling was not redesigned.
- Remaining local verification: install a repository-compatible golangci-lint,
  then run `golangci-lint run ./...` from `registry/`. No release/integration gate
  for provider lifecycle, Globalping, billing, or UI is claimed by this stage.
