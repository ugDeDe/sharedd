#!/usr/bin/env bash
# Сборка sharedd-node-agent: статический Go-бинарник + tar.gz с install_full.sh
# + голый бинарник dist/sharedd-node-agent — asset для web-установщика (install_node_web.sh)
# Env: GOOS (default linux), GOARCH (default amd64), VERSION (default: короткий git-хэш или dev)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PKG_DIR="$REPO_ROOT/node"
DIST_DIR="$REPO_ROOT/dist"

GOOS="${GOOS:-linux}"
GOARCH="${GOARCH:-amd64}"
VERSION="${VERSION:-$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo dev)}"

PKG_NAME="sharedd-node-agent"
ARCHIVE_NAME="${PKG_NAME}-${VERSION}-${GOOS}-${GOARCH}.tar.gz"

echo "==> building $PKG_NAME $VERSION for $GOOS/$GOARCH"

cd "$PKG_DIR"
[[ -f go.mod ]] || { echo "==> go.mod not found" >&2; exit 1; }
go mod download

STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
    go build -mod=readonly -trimpath -ldflags="-s -w" -o "$STAGE/$PKG_NAME" .

if [ "$GOOS" = "$(go env GOOS)" ] && [ "$GOARCH" = "$(go env GOARCH)" ]; then
    [ "$("$STAGE/$PKG_NAME" --version)" = "$PKG_NAME" ] || { echo "==> wrong binary identity" >&2; exit 1; }
fi

# --- содержимое архива ---
mkdir -p "$STAGE/pkg"
mv "$STAGE/$PKG_NAME" "$STAGE/pkg/$PKG_NAME"
cp "$REPO_ROOT/configs/node.example.toml" "$STAGE/pkg/"
cp "$REPO_ROOT/systemd/sharedd-node-agent.service" "$STAGE/pkg/"

# полный установщик с базовым регистратором registrar.ddproxy.xyz
install -m 0755 "$REPO_ROOT/scripts/install_node.sh" "$STAGE/pkg/install_full.sh"


mkdir -p "$DIST_DIR"
tar -C "$STAGE" -czf "$DIST_DIR/$ARCHIVE_NAME" pkg
(cd "$DIST_DIR" && sha256sum "$ARCHIVE_NAME" > "${ARCHIVE_NAME}.sha256")

# голый бинарник для web-установщика: залейте его же в GitHub Release — ссылка
# releases/latest/download/sharedd-node-agent тогда работает «из коробки»
install -m 0755 "$STAGE/pkg/$PKG_NAME" "$DIST_DIR/$PKG_NAME"
(cd "$DIST_DIR" && sha256sum "$PKG_NAME" > "${PKG_NAME}.sha256")

echo "==> done: $DIST_DIR/$ARCHIVE_NAME и $DIST_DIR/$PKG_NAME (+ .sha256)"
