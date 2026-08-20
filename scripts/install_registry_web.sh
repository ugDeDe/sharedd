#!/usr/bin/env bash


# Всегда берём текущий релиз. Это делает ту же команду пригодной и для
# обновления: GitHub перенаправит её на asset последнего опубликованного тега.
BINARY_URL="https://github.com/ugDeDe/sharedd/releases/latest/download/sharedd-registry"
INSTALLER_URL="https://raw.githubusercontent.com/ugDeDe/sharedd/main/scripts/install_registry.sh"

set -euo pipefail

RED='\033[0;31m'; CYAN='\033[0;36m'
BOLD='\033[1m'; NC='\033[0m'

say() { echo -e "  ${CYAN}→${NC} $*"; }
die() { echo -e "  ${RED}✗ $*${NC}" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "запустите от root: sudo bash $0"
command -v systemctl &>/dev/null && [ -d /run/systemd/system ] || die "нужен systemd"

case "$BINARY_URL" in http://*|https://*) ;; *) die "пропишите ссылку в BINARY_URL в начале скрипта" ;; esac
case "$INSTALLER_URL" in http://*|https://*) ;; *) die "пропишите ссылку в INSTALLER_URL в начале скрипта" ;; esac

TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT

dl() { # dl <url> <file>
    if command -v curl &>/dev/null; then
        curl -fsSL --connect-timeout 15 -o "$2" "$1"
    elif command -v wget &>/dev/null; then
        wget -q -O "$2" "$1"
    else
        die "нужен curl или wget"
    fi
}

echo ""
say "sharedd — установка регистратора (web): ${BOLD}$BINARY_URL${NC}"
dl "$BINARY_URL" "$TMP/sharedd-registry"   || die "скачивание бинарника не удалось"
dl "$INSTALLER_URL" "$TMP/install.sh"      || die "скачивание установщика не удалось"
head -c 4 "$TMP/sharedd-registry" | grep -q $'\x7fELF' || die "по BINARY_URL не ELF-бинарник (проверьте ссылку)"
chmod +x "$TMP/sharedd-registry" "$TMP/install.sh"

# Повтор той же команды обновляет только бинарник и перезапускает сервис:
# существующие registry.toml и Caddyfile не спрашиваются и не переписываются.
# Для осознанной перенастройки передайте флаги установщику явно.
if [ "$#" -eq 0 ] && [ -f /etc/sharedd/registry.toml ]; then
    say "найдена существующая установка — обновляю бинарник, конфиг и Caddy не трогаю"
    set -- --skip-caddy
fi

# интерактивные вопросы установщика: stdin может быть пайпом curl — его ask
# читает с /dev/tty, но гарантировать подадим явно
if [ -e /dev/tty ]; then
    bash "$TMP/install.sh" --binary "$TMP/sharedd-registry" "$@" </dev/tty
else
    bash "$TMP/install.sh" --binary "$TMP/sharedd-registry" "$@"
fi
