# Repository Guide

## Modules and Entrypoints

- There is no root Go module or `go.work`. Run Go commands separately in `registry/`, `node/`, and `tools/uigen/`; root-level `go test ./...` is invalid.
- `registry/main.go` is the control plane and HTTP server. It owns node admission and health, master selection, Cloudflare DNS, persisted state, and the panel/public APIs.
- `node/main.go` is the VPS agent. It registers, reports heartbeat/metrics/Globalping results, and applies registry config to the local telemt or MTProxyL config.
- Machine API authentication is deliberately asymmetric: the registry requires `[security] node_token`; agents require the matching `[registry] token`. Do not introduce `security.node_token` into node config.

## Verification

- Match CI for each application module: `cd registry && go vet ./... && go test -race ./... && go build -mod=readonly ./...`, then repeat in `node/`.
- Run one test with `cd registry && go test -race -run '^TestName$' .` (or the same command in `node/`). Tests use local `httptest` servers and do not require live Cloudflare, Globalping, telemt, or systemd.
- Validate installers and build scripts with `bash -n scripts/*.sh`.
- If `ui/` changed, run `bash scripts/build_ui.sh` and commit the regenerated `registry/*.html`; then run `bash scripts/build_ui.sh -check`. CI currently does not perform this generated-file check.

## UI Generation

- Edit `ui/pages/**`, `ui/components/**`, and `ui/lib/**`, not embedded `registry/panel.html`, `registry/stats.html`, `registry/dashboard.html`, or `registry/links.html`; regeneration overwrites those files.
- `tools/uigen` bundles each page's `index.html`, `page.css`, and `main.ts` with Go-embedded esbuild. Node.js/npm is not part of this build.
- Page directories beginning with `_` generate previews under untracked `dev/preview/`, not registry assets.

## Build and Release

- `bash scripts/build_all.sh` builds static Linux/amd64 registry and node packages into `dist/`. Override `GOOS`, `GOARCH`, or `VERSION` through the environment when needed.
- Each component build emits both a versioned tarball and a bare binary plus SHA256 files. Web installers download the bare `sharedd-registry` and `sharedd-node-agent` assets from `releases/latest/download/...`.
- `.github/workflows/ci.yml` only verifies source; it does not publish releases. Release assets are uploaded separately, so never infer asset identity from its filename. Both binaries support `--version`; verify exact output (`sharedd-registry` or `sharedd-node-agent`) before publishing or installing.

## Runtime Invariants

- Node IDs are persisted in `/var/lib/sharedd/node_id` and have the form `NAME-HASH`: a 1-10 character name plus five lowercase alphanumeric characters. Preserve legacy-ID migration when changing validation.
- The registry persists operational assignments in its JSON state file and permanent block history in SQLite. Changes to state structs must account for restart/load behavior and existing persisted data.
- The agent's one-shot apply path is a safety transaction: fetch config, stop proxy, atomically patch its config while preserving ownership/mode, restart, wait for metrics, and roll back on failure. Do not bypass it in installers.
- `healthcheck.globalping_validity_min` must be strictly greater than `node_defaults.globalping_ms` expressed in minutes. Stale/missing verified Globalping state quarantines a node and requests an immediate agent check; it is not itself an IP ban.
- For MTProxyL superexpert mode, patch `/opt/mtproxyl/superexpert.toml`, not its generated `mtproxy/config.toml`; MTProxyL overwrites the latter on restart.
- `shared_proxy.port` (default 443) is the registry source of truth. Installers must fail before replacing the agent when the selected telemt config uses another port; mismatched running nodes are ineligible for mastery and must not keep the sharedd antiscan INPUT hook.
- Antiscan is owned by the node agent: it atomically refreshes `sharedd_scanners` from stamparm/ipsum every 30 minutes and hooks `ANTISCAN_MTPROTO` on the shared port. Do not reintroduce TTL-based filtering.
