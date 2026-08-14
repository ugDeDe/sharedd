#!/usr/bin/env bash
# Сборка sharedd-registry: статический Go-бинарник + tar.gz с install_full.sh
# + голый бинарник dist/sharedd-registry — asset для web-установщика (install_registry_web.sh)
# Env: GOOS (default linux), GOARCH (default amd64), VERSION (default: короткий git-хэш или dev)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PKG_DIR="$REPO_ROOT/registry"
DIST_DIR="$REPO_ROOT/dist"

GOOS="${GOOS:-linux}"
GOARCH="${GOARCH:-amd64}"
VERSION="${VERSION:-$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo dev)}"

PKG_NAME="sharedd-registry"
ARCHIVE_NAME="${PKG_NAME}-${VERSION}-${GOOS}-${GOARCH}.tar.gz"

echo "==> building $PKG_NAME $VERSION for $GOOS/$GOARCH"

cd "$PKG_DIR"
if [[ ! -f go.mod ]]; then
    echo "==> go.mod not found, initializing"
    go mod init "sharedd/registry"
fi
go get github.com/pelletier/go-toml/v2 github.com/cloudflare/cloudflare-go
go mod tidy

STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
    go build -trimpath -ldflags="-s -w" -o "$STAGE/$PKG_NAME" .

# --- содержимое архива ---
mkdir -p "$STAGE/pkg"
mv "$STAGE/$PKG_NAME" "$STAGE/pkg/$PKG_NAME"
cp "$REPO_ROOT/configs/registry.example.toml" "$STAGE/pkg/"
cp "$REPO_ROOT/systemd/sharedd-registry.service" "$STAGE/pkg/"

# полный установщик: registry + Caddy (авто-TLS) + веб-панель (V4)
install -m 0755 "$REPO_ROOT/scripts/install_registry.sh" "$STAGE/pkg/install_full.sh"


mkdir -p "$DIST_DIR"
tar -C "$STAGE" -czf "$DIST_DIR/$ARCHIVE_NAME" pkg
(cd "$DIST_DIR" && sha256sum "$ARCHIVE_NAME" > "${ARCHIVE_NAME}.sha256")

# голый бинарник для web-установщика: залейте его же в GitHub Release — ссылка
# releases/latest/download/sharedd-registry тогда работает «из коробки»
install -m 0755 "$STAGE/pkg/$PKG_NAME" "$DIST_DIR/$PKG_NAME"

echo "==> done: $DIST_DIR/$ARCHIVE_NAME (+ .sha256) и голый бинарник $DIST_DIR/$PKG_NAME"
