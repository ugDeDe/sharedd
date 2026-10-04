# Repository Guide

## Modules and Entrypoints

- There is no root Go module or `go.work`. Run Go commands separately in `registry/`, `node/`, and `tools/uigen/`; root-level `go test ./...` is invalid.
- Registry layout: thin root `registry/main.go` (--version + `server.Run()`); all logic lives in `registry/internal/server`, with extracted packages: `internal/state` (persisted JSON state format, dependency-free), `internal/config` (TOML load/validation/hot-apply types), `internal/history` (SQLite bans/events/traffic), `internal/machineapi` (wire contracts shared with the agent + request validation; Bearer middleware stays in server), `internal/globalping` (GET-only measurement client, binding validation), `internal/webassets` (generated pages + fonts, see UI Generation).
- Master selection is decomposed in `internal/server/selection_core.go`: pure functions (`sortQueue`, `pickLeastLoaded`, `rotateByTTL`, `reassignDead`, `fillEmpty`, `reconcileStints`) over a `selectionView` with side effects collected in `selectionSink`; `evaluateAssignments` is only the orchestrator. Keep new selection logic pure and table-tested there.
- `server` keeps short type aliases (`deps_state.go`, `deps_config.go`, `deps_history.go`, `deps_globalping.go`, `html.go`) for historical names. New packages import the owning package directly; do not add cross-package back-references.
- `node/main.go` is a thin root (--version + `agent.Run()`); all logic lives in `node/internal/agent`, with `node/internal/config` (TOML load/validation, node ID persistence) as the dependency-free leaf. The agent registers, reports heartbeat/metrics/Globalping results, and applies registry config to the local telemt or MTProxyL config. Patched telemt TOML must pass `validateTOMLText` before any write.
- Termination policy: there is no node-side tombstone. The registry is the single source of truth for blocks; `dead` records are lifted by the first re-register after local recovery (unlimited return cycles), `ip_ban` keeps the reverify/cooldown flow.
- Machine API authentication is deliberately asymmetric: the registry requires `[security] node_token`; agents require the matching `[registry] token`. Do not introduce `security.node_token` into node config.

## Verification

- Match CI for each application module: `cd registry && go vet ./... && go test -race ./... && go build -mod=readonly ./... && golangci-lint run ./...`, then repeat in `node/`. Code must be `gofmt`-clean.
- Run one test with `cd registry && go test -race -run '^TestName$' .` (or the same command in `node/`). Tests use local `httptest` servers and do not require live Cloudflare, Globalping, telemt, or systemd.
- Validate installers and build scripts with `bash -n scripts/*.sh` plus `shellcheck -S warning scripts/*.sh`; keep both clean — CI enforces them and `bash scripts/check_installer_parity.sh` (byte-identical pipeline functions across `install_node.sh` / `install_node_web.sh`).
- If `ui/` changed, run `bash scripts/build_ui.sh` and commit regenerated `registry/internal/webassets/*.html`; CI runs `build_ui.sh -check` and fails on stale generated files.
- Do not seed tests from wall-clock-relative timestamps inside calendar windows (e.g., "now-1h" vs the dashboard's midnight-based day range) — that class of time-of-day flakes is banned; use deterministic anchors.

## UI Generation

- Edit `ui/pages/**`, `ui/components/**`, and `ui/lib/**`, not embedded `registry/internal/webassets/{panel,stats,dashboard,links}.html`; regeneration overwrites those files.
- `tools/uigen` bundles each page's `index.html`, `page.css`, and `main.ts` with Go-embedded esbuild and writes into `registry/internal/webassets/` (`//go:embed` is package-directory-relative). Node.js/npm is not part of this build.
- Page directories beginning with `_` generate previews under untracked `dev/preview/`, not registry assets.

## Build and Release

- `bash scripts/build_all.sh` builds static Linux/amd64 registry and node packages into `dist/`. Override `GOOS`, `GOARCH`, or `VERSION` through the environment when needed.
- Each component build emits both a versioned tarball and a bare binary plus SHA256 files. Web installers download the bare `sharedd-registry` and `sharedd-node-agent` assets from `releases/latest/download/...` and verify `<asset>.sha256` when reachable (missing checksum file degrades to a warning; mismatch aborts).
- `.github/workflows/ci.yml` only verifies source (vet/tests/build/gofmt/golangci-lint/shellcheck/ui-check/installer-parity); it does not publish releases. Release assets are uploaded separately, so never infer asset identity from its filename. Both binaries support `--version`; verify exact output (`sharedd-registry` or `sharedd-node-agent`) before publishing or installing.

## Runtime Invariants

- Node IDs are persisted in `/var/lib/sharedd/node_id` and have the form `NAME-HASH`: a 1-10 character name plus five lowercase alphanumeric characters. Preserve legacy-ID migration when changing validation.
- The registry persists operational assignments in its JSON state file (owned by `internal/state`, including legacy metrics-latch migration) and permanent block history in SQLite (`internal/history`). Changes to state structs must account for restart/load behavior and existing persisted data; `internal/state/state_test.go` pins the on-disk format.
- The agent's one-shot apply path is a safety transaction: fetch config, stop proxy, atomically patch its config while preserving ownership/mode, restart, wait for metrics, and roll back on failure. Do not bypass it in installers.
- Expected telemt metric names are hard requirements for node health (`telemt_me_writers_active_current` et al.). When they are missing, the agent logs a `TELEMT COMPATIBILITY` diagnostic at most once per hour; do not turn it into per-tick spam.
- `healthcheck.globalping_validity_min` must be strictly greater than `node_defaults.globalping_ms` expressed in minutes. Stale/missing verified Globalping state quarantines a node and requests an immediate agent check; it is not itself an IP ban.
- For MTProxyL, config path is detected from `/opt/mtproxyl/mtproxy/config.toml` (or `telemt.toml`), configured via MTProxyL CLI (`expert`/`secret`).
- `shared_proxy.port` (default 443) is the registry source of truth. Installers must fail before replacing the agent when the selected telemt config uses another port; mismatched running nodes are ineligible for mastery and must not keep the sharedd antiscan INPUT hook.
- Antiscan is owned by the node agent: it atomically refreshes `sharedd_scanners` from stamparm/ipsum every 30 minutes and hooks `ANTISCAN_MTPROTO` on the shared port. Do not reintroduce TTL-based filtering. The standalone `install-mtproto-antiscan.sh` helper was removed — do not resurrect a second owner of the same job.
