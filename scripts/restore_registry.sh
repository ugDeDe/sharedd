#!/usr/bin/env bash
# Восстановление sharedd-регистратора из бэкапа backup_registry.sh.
#
# Останавливает службу, проверяет SHA256SUMS, возвращает state/БД/конфиг и
# запускает службу. Требует явного --yes: операция перезаписывает рабочие данные.
#
# Использование:
#   sudo bash scripts/restore_registry.sh --yes BACKUP_DIR
#
# Перед восстановлением сделайте свежий бэкап текущего состояния.

set -euo pipefail
umask 077

STATE_DIR="${SHAREDD_STATE_DIR:-/var/lib/sharedd}"
ETC_DIR="${SHAREDD_ETC_DIR:-/etc/sharedd}"
UNIT="${SHAREDD_UNIT:-sharedd-registry}"
CONFIRM=0

die() { echo "restore: $*" >&2; exit 1; }
say() { echo "restore: $*"; }

while [ $# -gt 0 ]; do
    case "$1" in
        --yes) CONFIRM=1 ;;
        --state-dir) shift; STATE_DIR="${1:?--state-dir requires a path}" ;;
        --etc-dir)   shift; ETC_DIR="${1:?--etc-dir requires a path}" ;;
        -h|--help)
            sed -n '2,11p' "$0" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        -*) die "unknown option: $1" ;;
        *) BACKUP_DIR="$1" ;;
    esac
    shift
done

[ "$CONFIRM" -eq 1 ] || die "refusing to overwrite live data; pass --yes"
[ -n "${BACKUP_DIR:-}" ] || die "backup directory is required"
[ -d "$BACKUP_DIR" ] || die "not a directory: $BACKUP_DIR"
[ -f "$BACKUP_DIR/SHA256SUMS" ] || die "SHA256SUMS is missing"
[ "$(id -u)" -eq 0 ] || die "run as root"

(
    cd "$BACKUP_DIR"
    sha256sum -c SHA256SUMS
)

mkdir -p "$STATE_DIR" "$ETC_DIR"
install_file() {
    local name="$1" dest="$2"
    [ -f "$BACKUP_DIR/$name" ] || return 0
    install -m 0600 "$BACKUP_DIR/$name" "$dest"
    say "restored $name"
}

if [ "${SHAREDD_SKIP_SYSTEMCTL:-0}" != "1" ] && command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet "$UNIT" 2>/dev/null; then
    say "stopping $UNIT"
    systemctl stop "$UNIT"
fi

install_file registry_state.json "$STATE_DIR/registry_state.json"
install_file master.key "$STATE_DIR/master.key"
install_file control.db "$STATE_DIR/control.db"
install_file registry.db "$STATE_DIR/registry.db"
install_file registry.toml "$ETC_DIR/registry.toml"

# Владелец файлов — служебный пользователь, если он существует.
if id -u sharedd-registry >/dev/null 2>&1; then
    chown -R sharedd-registry:sharedd-registry "$STATE_DIR" "$ETC_DIR/registry.toml" 2>/dev/null || true
fi
chmod 0600 "$STATE_DIR"/*.json "$STATE_DIR"/*.db "$STATE_DIR"/master.key "$ETC_DIR/registry.toml" 2>/dev/null || true

if [ "${SHAREDD_SKIP_SYSTEMCTL:-0}" != "1" ] && command -v systemctl >/dev/null 2>&1 && systemctl list-unit-files "$UNIT.service" >/dev/null 2>&1; then
    say "starting $UNIT"
    systemctl start "$UNIT"
fi

say "restore complete"
