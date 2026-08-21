#!/bin/bash
#
# install-mtproto-antiscan.sh
#
# Универсальная установка защиты MTProto-прокси от сканеров.
# Рассчитан на СВЕЖИЕ/ГОЛЫЕ сервера: сам ставит и запускает все зависимости,
# не зависит от уже существующих цепочек iptables.
#
# Использование:
#   sudo bash install-mtproto-antiscan.sh [опции]
#
# Опции:
#   -p, --port ПОРТ         Порт MTProto-прокси (по умолчанию: 443)
#   -i, --ipset-name ИМЯ    Имя ipset (по умолчанию: ipsum_scanners)
#   -u, --ipsum-url URL     URL списка ipsum (по умолчанию: level 1 stamparm/ipsum)
#   -c, --cron-interval N   Интервал автообновления в минутах (по умолчанию: 30)
#   -h, --help              Показать эту справку
#

set -uo pipefail

PORT=443
IPSET_NAME="ipsum_scanners"
IPSUM_URL="https://raw.githubusercontent.com/stamparm/ipsum/master/levels/1.txt"
CRON_INTERVAL=30

usage() {
    sed -n '1,20p' "$0" | grep '^#' | sed 's/^# \{0,1\}//'
    exit "${1:-0}"
}

while [ $# -gt 0 ]; do
    case "$1" in
        -p|--port) PORT="$2"; shift 2 ;;
        --port=*) PORT="${1#*=}"; shift ;;
        -i|--ipset-name) IPSET_NAME="$2"; shift 2 ;;
        --ipset-name=*) IPSET_NAME="${1#*=}"; shift ;;
        -u|--ipsum-url) IPSUM_URL="$2"; shift 2 ;;
        --ipsum-url=*) IPSUM_URL="${1#*=}"; shift ;;
        -c|--cron-interval) CRON_INTERVAL="$2"; shift 2 ;;
        --cron-interval=*) CRON_INTERVAL="${1#*=}"; shift ;;
        -h|--help) usage 0 ;;
        *) echo "[!] Неизвестный параметр: $1" >&2; usage 1 ;;
    esac
done

log() { echo -e "[*] $*"; }
err() { echo -e "[!] $*" >&2; }
die() { err "$*"; exit 1; }

[ "$(id -u)" -eq 0 ] || die "Скрипт нужно запускать от root (sudo)."

if ! [[ "$PORT" =~ ^[0-9]+$ ]] || [ "$PORT" -lt 1 ] || [ "$PORT" -gt 65535 ]; then die "Некорректный порт: $PORT"; fi
if ! [[ "$CRON_INTERVAL" =~ ^[0-9]+$ ]] || [ "$CRON_INTERVAL" -lt 1 ] || [ "$CRON_INTERVAL" -gt 59 ]; then die "Некорректный интервал cron: $CRON_INTERVAL"; fi

ANTISCAN_CHAIN="ANTISCAN_MTPROTO"
UPDATE_SCRIPT="/usr/local/bin/update-ipsum.sh"
LOCK_FILE="/var/run/update-ipsum.lock"
LOG_FILE="/var/log/update-ipsum.log"
CRON_LINE="*/${CRON_INTERVAL} * * * * ${UPDATE_SCRIPT} >/dev/null 2>&1"

log "Параметры: port=$PORT ipset=$IPSET_NAME cron_interval=${CRON_INTERVAL}m"

# --- 0. Установка зависимостей (принудительно, для голых машин) ---

if command -v apt-get >/dev/null 2>&1; then
    log "Обновляю списки пакетов и ставлю зависимости (apt)..."
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    apt-get install -y -qq iptables ipset curl cron util-linux kmod \
        || die "apt-get install не удался — проверь доступ к репозиториям."
    CRON_SERVICE="cron"
elif command -v yum >/dev/null 2>&1 || command -v dnf >/dev/null 2>&1; then
    PKG=$(command -v dnf || command -v yum)
    log "Ставлю зависимости ($PKG)..."
    "$PKG" install -y iptables ipset curl cronie util-linux kmod \
        || die "$PKG install не удался — проверь доступ к репозиториям."
    CRON_SERVICE="crond"
else
    die "Не найден apt-get/yum/dnf — поставь iptables, ipset, curl, cron вручную и перезапусти скрипт."
fi

log "Убеждаюсь, что нужные модули ядра загружены..."
modprobe ip_set 2>/dev/null || true
modprobe ip_set_hash_ip 2>/dev/null || true
modprobe xt_set 2>/dev/null || true

log "Включаю и запускаю сервис $CRON_SERVICE..."
systemctl enable --now "$CRON_SERVICE" 2>/dev/null \
    || service "$CRON_SERVICE" start 2>/dev/null \
    || err "Не удалось автоматически включить $CRON_SERVICE, проверь вручную: systemctl status $CRON_SERVICE"

for bin in iptables ipset curl crontab awk grep flock; do
    command -v "$bin" >/dev/null 2>&1 || die "Команда '$bin' всё ещё не найдена после установки — остановка."
done

# --- 1. ipset ---

log "Создаю ipset '$IPSET_NAME' (если ещё не существует)..."
ipset create "$IPSET_NAME" hash:ip hashsize 4096 maxelem 1048576 -exist \
    || die "Не удалось создать ipset — проверь 'dmesg | tail' на предмет ошибок ядра."

log "Пишу скрипт автообновления в $UPDATE_SCRIPT..."
cat > "$UPDATE_SCRIPT" << EOF
#!/bin/bash
LOCK="$LOCK_FILE"
exec 200>"\$LOCK"
flock -n 200 || { echo "already running, exit"; exit 1; }

LOG="$LOG_FILE"
URL="$IPSUM_URL"
TMP=\$(mktemp)
RESTORE=\$(mktemp)

echo "\$(date): starting update" >> "\$LOG"

if ! curl -sL --retry 3 --retry-delay 2 --max-time 30 -o "\$TMP" "\$URL"; then
    echo "\$(date): curl failed" >> "\$LOG"
    rm -f "\$TMP" "\$RESTORE"
    exit 1
fi

LINES=\$(grep -cE '^[0-9]+\.' "\$TMP")
if [ "\$LINES" -eq 0 ]; then
    echo "\$(date): downloaded file has no valid IPs" >> "\$LOG"
    rm -f "\$TMP" "\$RESTORE"
    exit 1
fi

ipset destroy ipsum_tmp 2>/dev/null
echo "create ipsum_tmp hash:ip hashsize 4096 maxelem 1048576" > "\$RESTORE"
awk '{print "add ipsum_tmp " \$1}' "\$TMP" | grep -E '^add ipsum_tmp [0-9]+\.' >> "\$RESTORE"

ipset restore -exist < "\$RESTORE"
ipset swap ipsum_tmp $IPSET_NAME
ipset destroy ipsum_tmp
rm -f "\$TMP" "\$RESTORE"
echo "\$(date): updated with \$LINES IPs" >> "\$LOG"
EOF
chmod +x "$UPDATE_SCRIPT"

log "Первичное наполнение ipset..."
if ! "$UPDATE_SCRIPT"; then
    err "Первый запуск обновления не удался. Содержимое лога:"
    cat "$LOG_FILE" >&2 2>/dev/null
    err "Проверь доступ до raw.githubusercontent.com вручную:"
    err "  curl -v --max-time 20 $IPSUM_URL"
    err "Продолжаю установку правил — ipset будет пуст, пока не выполнишь обновление руками."
fi

log "Добавляю cron-задачу на автообновление каждые ${CRON_INTERVAL} минут..."
( crontab -l 2>/dev/null | grep -vF "$UPDATE_SCRIPT" ; echo "$CRON_LINE" ) | crontab - \
    || err "Не удалось прописать crontab — добавь вручную строку: $CRON_LINE"

# --- 2. Цепочка iptables ---

log "Создаю цепочку '$ANTISCAN_CHAIN'..."
iptables -N "$ANTISCAN_CHAIN" 2>/dev/null || log "  -> уже существует."

log "Подключаю цепочку к INPUT для порта $PORT..."
if ! iptables -C INPUT -p tcp --dport "$PORT" -j "$ANTISCAN_CHAIN" 2>/dev/null; then
    iptables -I INPUT 1 -p tcp --dport "$PORT" -j "$ANTISCAN_CHAIN"
    log "  -> добавлено."
else
    log "  -> уже есть."
fi

log "Правило DROP по ipset '$IPSET_NAME'..."
if ! iptables -C "$ANTISCAN_CHAIN" -m set --match-set "$IPSET_NAME" src -j DROP 2>/dev/null; then
    iptables -A "$ANTISCAN_CHAIN" -m set --match-set "$IPSET_NAME" src -j DROP
    log "  -> добавлено."
else
    log "  -> уже есть."
fi

# --- 3. Персистентность ---

if command -v netfilter-persistent >/dev/null 2>&1; then
    log "Сохраняю правила через netfilter-persistent..."
    netfilter-persistent save
elif command -v apt-get >/dev/null 2>&1; then
    log "Ставлю iptables-persistent для сохранения правил после ребута..."
    echo iptables-persistent iptables-persistent/autosave_v4 boolean true | debconf-set-selections
    echo iptables-persistent iptables-persistent/autosave_v6 boolean true | debconf-set-selections
    apt-get install -y -qq iptables-persistent && netfilter-persistent save
else
    err "Не удалось автоматически поставить persistence — сохрани правила вручную под свой дистрибутив."
fi

# --- Итог ---

echo
log "Готово. Текущее состояние:"
ipset list "$IPSET_NAME" 2>/dev/null | grep -E "Name:|Number of entries"
echo
iptables -L INPUT -n -v --line-numbers | head -3
echo
iptables -L "$ANTISCAN_CHAIN" -n -v --line-numbers
echo
log "Лог обновлений: $LOG_FILE"
log "Ручной запуск обновления: $UPDATE_SCRIPT"
