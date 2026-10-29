#!/usr/bin/env bash
# Согласованный бэкап состояния sharedd-регистратора.
#
# SQLite копируется через `sqlite3 .backup` (учитывает WAL), а не обычным cp
# работающей БД. Секреты и конфиг сохраняются с правами 0600.
#
# Использование:
#   sudo bash scripts/backup_registry.sh [--state-dir DIR] [--etc-dir DIR] [--out DIR]
#
# По умолчанию: /var/lib/sharedd, /etc/sharedd, /var/backups.

set -euo pipefail
umask 077

STATE_DIR="${SHAREDD_STATE_DIR:-/var/lib/sharedd}"
ETC_DIR="${SHAREDD_ETC_DIR:-/etc/sharedd}"
OUT_DIR="${SHAREDD_BACKUP_DIR:-/var/backups}"

die() { echo "backup: $*" >&2; exit 1; }
say() { echo "backup: $*"; }

while [ $# -gt 0 ]; do
    case "$1" in
        --state-dir) shift; STATE_DIR="${1:?--state-dir requires a path}" ;;
        --etc-dir)   shift; ETC_DIR="${1:?--etc-dir requires a path}" ;;
        --out)       shift; OUT_DIR="${1:?--out requires a path}" ;;
        -h|--help)
            sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        *) die "unknown argument: $1" ;;
    esac
    shift
done

[ "$(id -u)" -eq 0 ] || die "run as root (master.key and databases are 0600)"
command -v sqlite3 >/dev/null 2>&1 || command -v python3 >/dev/null 2>&1 || die "sqlite3 or python3 is required for a consistent live backup"

mkdir -p "$OUT_DIR"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
DEST="$OUT_DIR/sharedd-$STAMP"
mkdir -m 700 "$DEST"

copy_file() {
    local src="$1"
    [ -f "$src" ] || return 0
    install -m 0600 "$src" "$DEST/$(basename "$src")"
    say "copied $(basename "$src")"
}

backup_db() {
    local src="$1"
    [ -f "$src" ] || return 0
    local base
    base="$(basename "$src")"
    if command -v sqlite3 >/dev/null 2>&1; then
        sqlite3 "$src" ".backup '$DEST/$base'"
    else
        python3 -c 'import sqlite3,sys; s=sqlite3.connect(sys.argv[1]); d=sqlite3.connect(sys.argv[2]); s.backup(d); d.close(); s.close()' "$src" "$DEST/$base"
    fi
    chmod 0600 "$DEST/$base"
    say "sqlite backup $base"
}

copy_file "$STATE_DIR/registry_state.json"
# [state] file может быть переопределён в registry.toml — берём оба штатных имени.
if [ -f "$ETC_DIR/registry.toml" ]; then
    copy_file "$ETC_DIR/registry.toml"
    state_from_toml="$(sed -n 's/^[[:space:]]*file[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' "$ETC_DIR/registry.toml" | head -n1)"
    if [ -n "$state_from_toml" ] && [ -f "$state_from_toml" ]; then
        copy_file "$state_from_toml"
    fi
fi
copy_file "$STATE_DIR/master.key"
backup_db "$STATE_DIR/control.db"
backup_db "$STATE_DIR/registry.db"

[ -n "$(find "$DEST" -maxdepth 1 -type f -print -quit)" ] || die "nothing to back up in $STATE_DIR"

(
    cd "$DEST"
    find . -maxdepth 1 -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum > SHA256SUMS
    chmod 0600 SHA256SUMS
)

say "backup ready: $DEST"
