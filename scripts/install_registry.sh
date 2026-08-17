#!/usr/bin/env bash
# sharedd — установка регистратора с Caddy (автоматический TLS) и веб-панелью.
#
# Использование:
#   sudo bash install_registry.sh                     # интерактивно (бинарник ищется рядом)
#   sudo bash install_registry.sh --binary /path/sharedd-registry --domain ha.example.com
#   sudo bash install_registry.sh --bare              # без Caddy (HTTP напрямую)
#   sudo bash install_registry.sh --only-caddy        # доустановить/починить только Caddy
#
# Что делает:
#   1. Ставит Caddy (официальный apt/dnf/yum/apk репозиторий → fallback: бинарник с
#      GitHub Releases → --caddy-binary /путь). Если Caddy недоступен вовсе — установка
#      НЕ роняется: registry поднимается, панель доступна по http://IP:8080/panel.
#   2. Ставит sharedd-registry в /usr/local/bin + systemd-юнит (с StateDirectory).
#   3. Генерирует /etc/sharedd/registry.toml (интерактивно: Cloudflare, SNI, домены;
#      токен панели и MTProto-секрет генерируются автоматически).
#   4. Права: /etc/sharedd и /var/lib/sharedd принадлежат sharedd-registry (панель
#      правит конфиг на лету), с верификацией записи от имени сервисного пользователя.
#   5. Панель: https://<домен>/panel (токен выводится в конце и в credentials.txt).
#
# Идемпотентно: существующие конфиги не затираются без --reconfigure.

set -euo pipefail

# ---------------------------------------------------------------- стиль (как MTProxyL)
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; CYAN='\033[0;36m'
BOLD='\033[1m'; DIM='\033[2m'; NC='\033[0m'
SYM_OK='✓'; SYM_ERR='✗'; SYM_WARN='!'; SYM_ARROW='→'

INSTALL_LOG="/tmp/sharedd-install.log"
SVC_USER="sharedd-registry"
ETC_DIR="/etc/sharedd"
STATE_DIR="/var/lib/sharedd"
REG_PORT="8080"

say()  { echo -e "  ${CYAN}${SYM_ARROW}${NC} $*"; }
ok()   { echo -e "  ${GREEN}${SYM_OK}${NC} $*"; }
warn() { echo -e "  ${YELLOW}${SYM_WARN}${NC} $*"; }
err()  { echo -e "  ${RED}${SYM_ERR}${NC} $*" >&2; }
die()  {
    err "$*"
    echo -e "  ${DIM}── хвост ${INSTALL_LOG} ──${NC}" >&2
    tail -n 12 "$INSTALL_LOG" 2>/dev/null | sed 's/^/  /' >&2 || true
    echo -e "  ${DIM}━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}" >&2
    exit 1
}
hline() { echo -e "  ${DIM}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"; }
banner() {
    echo ""
    hline
    echo -e "  ${BOLD}sharedd${NC} — установка регистратора"
    echo -e "  ${DIM}registry + Caddy (TLS) + веб-панель /panel${NC}"
    hline
    echo ""
}

# ---------------------------------------------------------------- аргументы
BINARY=""
DOMAIN=""
EMAIL=""
PANEL_TOKEN=""
CF_TOKEN=""
CF_ZONE=""
CF_DOMAINS=""
TLS_DOMAIN=""
CADDY_BINARY=""
EXPOSE_HTTP=0
BARE=0
ONLY_CADDY=0
SKIP_CADDY=0
RECONFIGURE=0
ASSUME_YES=0

while [ $# -gt 0 ]; do
    case "$1" in
        --binary)        BINARY="${2:?--binary требует путь}"; shift 2 ;;
        --binary=*)      BINARY="${1#*=}"; shift ;;
        --domain)        DOMAIN="${2:?--domain требует FQDN}"; shift 2 ;;
        --domain=*)      DOMAIN="${1#*=}"; shift ;;
        --email)         EMAIL="${2:?--email требует адрес}"; shift 2 ;;
        --email=*)       EMAIL="${1#*=}"; shift ;;
        --panel-token)   PANEL_TOKEN="${2:?}"; shift 2 ;;
        --panel-token=*) PANEL_TOKEN="${1#*=}"; shift ;;
        --cf-token)      CF_TOKEN="${2:?}"; shift 2 ;;
        --cf-token=*)    CF_TOKEN="${1#*=}"; shift ;;
        --cf-zone)       CF_ZONE="${2:?}"; shift 2 ;;
        --cf-zone=*)     CF_ZONE="${1#*=}"; shift ;;
        --cf-domains)    CF_DOMAINS="${2:?}"; shift 2 ;;
        --cf-domains=*)  CF_DOMAINS="${1#*=}"; shift ;;
        --tls-domain)    TLS_DOMAIN="${2:?}"; shift 2 ;;
        --tls-domain=*)  TLS_DOMAIN="${1#*=}"; shift ;;
        --caddy-binary)  CADDY_BINARY="${2:?--caddy-binary требует путь}"; shift 2 ;;
        --caddy-binary=*) CADDY_BINARY="${1#*=}"; shift ;;
        --expose-http)   EXPOSE_HTTP=1; shift ;;
        --bare)          BARE=1; shift ;;
        --only-caddy)    ONLY_CADDY=1; shift ;;
        --skip-caddy)    SKIP_CADDY=1; shift ;;
        --reconfigure)   RECONFIGURE=1; shift ;;
        -y|--yes)        ASSUME_YES=1; shift ;;
        -h|--help)       grep '^#' "$0" | head -24 | sed 's/^# \{0,1\}//'; exit 0 ;;
        *) die "неизвестный аргумент: $1 (см. --help)" ;;
    esac
done

if [ "$(id -u)" -ne 0 ]; then
    echo -e "${RED}Запустите от root:${NC} sudo bash $0 ..." >&2
    exit 1
fi
: > "$INSTALL_LOG"
banner

# systemd обязателен (юниты, StateDirectory); busybox/openrc — не поддерживаем
if ! command -v systemctl &>/dev/null || [ ! -d /run/systemd/system ]; then
    die "нужен systemd (systemctl + /run/systemd/system). Без systemd установка не поддерживается."
fi

TTY=0
[ -t 0 ] && TTY=1

ask() { # ask <varname> <prompt> [default] [secret]
    local var="$1" prompt="$2" def="${3:-}" secret="${4:-}" val=""
    if [ "$TTY" -eq 0 ]; then
        eval "val=\"\${$var:-$def}\""
        [ -n "$val" ] || die "не задано: $prompt (передайте флагом или запустите из терминала)"
    else
        local show_def=""
        [ -n "$def" ] && show_def=" [${def}]"
        if [ -n "$secret" ]; then
            read -r -s -p "  ${SYM_ARROW} ${prompt}${show_def}: " val </dev/tty; echo ""
        else
            read -r -p "  ${SYM_ARROW} ${prompt}${show_def}: " val </dev/tty
        fi
        [ -n "$val" ] || val="$def"
    fi
    printf -v "$var" '%s' "$val"
}

rand_hex() {
    if command -v openssl &>/dev/null; then openssl rand -hex "$1"
    else head -c "$1" /dev/urandom | od -An -tx1 | tr -d ' \n'; fi
}

# ---------------------------------------------------------------- пакетный менеджер
PKG=""
for pm in apt-get dnf yum apk; do
    command -v "$pm" &>/dev/null && { PKG="$pm"; break; }
done
[ -n "$PKG" ] || die "не найден пакетный менеджер (apt/dnf/yum/apk)"
say "пакетный менеджер: ${BOLD}${PKG}${NC}"

apt_wait() {
    [ "$PKG" = "apt-get" ] || return 0
    local waited=0
    while fuser /var/lib/dpkg/lock-frontend >/dev/null 2>&1; do
        [ "$waited" -eq 0 ] && say "apt занят, ждём..."
        sleep 3; waited=$((waited + 3))
        [ "$waited" -ge 90 ] && die "apt заблокирован более 90 секунд"
    done
}

pkg_install() {
    case "$PKG" in
        apt-get) apt_wait; apt-get install -y -qq "$@" >>"$INSTALL_LOG" 2>&1 ;;
        dnf)     dnf install -y -q "$@" >>"$INSTALL_LOG" 2>&1 ;;
        yum)     yum install -y -q "$@" >>"$INSTALL_LOG" 2>&1 ;;
        apk)     apk add --no-cache "$@" >>"$INSTALL_LOG" 2>&1 ;;
    esac
}

say "Проверяю зависимости (curl, tar)..."
need=()
for bin in curl tar; do
    command -v "$bin" &>/dev/null || need+=("$bin")
done
if [ "${#need[@]}" -gt 0 ]; then
    [ "$PKG" = "apt-get" ] && { apt_wait; apt-get update -qq >>"$INSTALL_LOG" 2>&1 || true; }
    pkg_install "${need[@]}" || pkg_install curl tar
fi
ok "зависимости на месте"

# ---------------------------------------------------------------- пользователь сервиса
ensure_user() { # ensure_user <name> — useradd → debian adduser → busybox adduser
    local name="$1"
    id "$name" &>/dev/null && return 0
    if command -v useradd &>/dev/null; then
        useradd --system --no-create-home --shell /usr/sbin/nologin "$name"
    elif adduser --help 2>&1 | grep -q -- '--system'; then
        adduser --system --no-create-home --shell /usr/sbin/nologin --group "$name"
    elif command -v adduser &>/dev/null; then
        adduser -S -H -s /sbin/nologin "$name" 2>/dev/null || adduser -SDH -s /sbin/nologin "$name"
    else
        return 1
    fi
    id "$name" &>/dev/null
}

# ---------------------------------------------------------------- Caddy
install_caddy_from_binary() {
    [ -n "$CADDY_BINARY" ] && [ -f "$CADDY_BINARY" ] || return 1
    say "ставлю Caddy из указанного файла: $CADDY_BINARY"
    install -m 0755 "$CADDY_BINARY" /usr/local/bin/caddy
}

install_caddy_from_github() {
    say "ставлю Caddy из релизов GitHub (fallback)..."
    local machine arch ver
    machine="$(uname -m)"
    case "$machine" in
        x86_64)  arch="amd64" ;;
        aarch64) arch="arm64" ;;
        armv7l)  arch="armv7" ;;
        *) warn "неподдерживаемая архитектура для GitHub-fallback: $machine"; return 1 ;;
    esac
    ver="$(curl -fsSL --retry 3 --retry-delay 2 --max-time 30 \
        https://api.github.com/repos/caddyserver/caddy/releases/latest 2>>"$INSTALL_LOG" \
        | grep '"tag_name"' | head -1 | cut -d'"' -f4)" || true
    if [ -z "$ver" ]; then
        warn "api.github.com недоступен (блокировка/нет сети) — версию Caddy не узнать"
        return 1
    fi
    local tmp
    tmp="$(mktemp -d)"
    if ! curl -fSL --retry 3 --retry-delay 2 --max-time 180 \
        "https://github.com/caddyserver/caddy/releases/download/${ver}/caddy_${ver#v}_linux_${arch}.tar.gz" \
        -o "$tmp/caddy.tgz" 2>>"$INSTALL_LOG"; then
        rm -rf "$tmp"
        warn "github.com недоступен — скачивание Caddy ${ver} не удалось"
        return 1
    fi
    tar -C "$tmp" -xzf "$tmp/caddy.tgz" caddy || { rm -rf "$tmp"; return 1; }
    install -m 0755 "$tmp/caddy" /usr/local/bin/caddy
    rm -rf "$tmp"
}

caddy_create_runtime() { # пользователь + юнит для непакетного caddy (по официальному caddy.service)
    command -v caddy &>/dev/null || return 1
    # пакетный caddy идёт со своим юнитом — не трогаем
    [ -f /lib/systemd/system/caddy.service ] || [ -f /usr/lib/systemd/system/caddy.service ] && return 0
    ensure_user caddy || warn "не смог создать пользователя caddy"
    mkdir -p /etc/caddy /var/lib/caddy
    chown caddy:caddy /var/lib/caddy 2>/dev/null || true
    if [ ! -f /etc/systemd/system/caddy.service ]; then
        cat > /etc/systemd/system/caddy.service <<'CADDY_UNIT'
[Unit]
Description=Caddy web server
Documentation=https://caddyserver.com/docs/
After=network.target network-online.target
Requires=network-online.target

[Service]
Type=notify
User=caddy
Group=caddy
ExecStart=/usr/local/bin/caddy run --environ --config /etc/caddy/Caddyfile
ExecReload=/usr/local/bin/caddy reload --config /etc/caddy/Caddyfile --force
TimeoutStopSec=5s
LimitNOFILE=1048576
PrivateTmp=true
ProtectSystem=full
AmbientCapabilities=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
CADDY_UNIT
    systemctl daemon-reload
    fi
}

install_caddy() { # 0 = caddy есть и запускаем; 1 = не вышло
    if command -v caddy &>/dev/null; then
        ok "Caddy уже установлен ($(caddy version 2>/dev/null | head -1 | cut -d' ' -f1))"
        return 0
    fi
    if [ -n "$CADDY_BINARY" ]; then
        install_caddy_from_binary || die "бинарник из --caddy-binary не сработал"
        caddy_create_runtime; return 0
    fi
    local repo_ok=1
    case "$PKG" in
        apt-get)
            say "ставлю Caddy из официального репозитория (cloudsmith)..."
            pkg_install debian-keyring debian-archive-keyring apt-transport-https gpg || true
            if curl -1sLf --retry 3 --max-time 30 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
                    | gpg --batch --yes --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg 2>>"$INSTALL_LOG" \
               && curl -1sLf --retry 3 --max-time 30 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
                    > /etc/apt/sources.list.d/caddy-stable.list 2>>"$INSTALL_LOG"; then
                apt_wait
                apt-get update -qq >>"$INSTALL_LOG" 2>&1 || true
                pkg_install caddy || repo_ok=0
            else
                warn "dl.cloudsmith.io недоступен"; repo_ok=0
            fi
            ;;
        dnf|yum)
            say "ставлю Caddy из copr @caddy/caddy..."
            { pkg_install 'dnf-command(copr)' || pkg_install yum-plugin-copr || true; }
            if { dnf copr enable -y @caddy/caddy >>"$INSTALL_LOG" 2>&1 || yum copr enable -y @caddy/caddy >>"$INSTALL_LOG" 2>&1; }; then
                pkg_install caddy || repo_ok=0
            else
                repo_ok=0
            fi
            ;;
        apk)
            say "ставлю Caddy из apk (включаю community)..."
            grep -q 'community' /etc/apk/repositories 2>/dev/null || {
                local main_repo
                main_repo="$(grep -m1 '/main' /etc/apk/repositories 2>/dev/null || true)"
                [ -n "$main_repo" ] && echo "${main_repo/main/community}" >> /etc/apk/repositories
                apk update >>"$INSTALL_LOG" 2>&1 || true
            }
            pkg_install caddy || repo_ok=0
            ;;
    esac
    if [ "$repo_ok" -ne 0 ] || ! command -v caddy &>/dev/null; then
        install_caddy_from_github || return 1
    fi
    caddy_create_runtime
    command -v caddy &>/dev/null
}

# ---------------------------------------------------------------- параметры
if [ "$ONLY_CADDY" -eq 0 ] && [ "$BARE" -eq 0 ] && [ "$SKIP_CADDY" -eq 0 ] && [ "$TTY" -eq 1 ]; then
    echo -e "  ${BOLD}Домен панели/API${NC}: A-запись этого FQDN должна указывать на ЭТОТ сервер."
    echo -e "  ${DIM}По нему Caddy получит сертификат; ноды и панель будут ходить по https.${NC}"
fi
if [ "$BARE" -eq 0 ] && [ "$SKIP_CADDY" -eq 0 ]; then
    ask DOMAIN "Домен для панели и API регистратора (FQDN)" "$DOMAIN"
    resolved_ip="$(getent ahostsv4 "$DOMAIN" 2>/dev/null | awk 'NR==1{print $1}' || true)"
    if [ -z "$resolved_ip" ]; then
        warn "DNS: ${DOMAIN} пока не резолвится — сертификат не получится, пока A-запись не укажет на сервер."
    else
        say "DNS: ${DOMAIN} → ${resolved_ip} (убедитесь, что это IP этого сервера)"
    fi
    ask EMAIL "Email для Let's Encrypt (опционально, Enter — пропустить)" "$EMAIL"
fi

NEED_CONFIG=0
[ "$ONLY_CADDY" -eq 0 ] && [ ! -f "$ETC_DIR/registry.toml" ] && NEED_CONFIG=1
[ "$RECONFIGURE" -eq 1 ] && NEED_CONFIG=1

if [ "$ONLY_CADDY" -eq 0 ] && [ "$NEED_CONFIG" -eq 1 ]; then
    hline
    echo -e "  ${BOLD}Параметры регистратора${NC}"
    echo -e "  ${DIM}Cloudflare API token: https://dash.cloudflare.com/profile/api-tokens${NC}"
    echo -e "  ${DIM}(права: Zone → DNS → Edit; Zone ID — на странице домена, справа внизу)${NC}"
    ask CF_TOKEN "Cloudflare API token" "$CF_TOKEN" secret
    ask CF_ZONE "Cloudflare Zone ID" "$CF_ZONE"
    ask CF_DOMAINS "Managed-домены A-записей прокси (через запятую)" "$CF_DOMAINS"
    ask TLS_DOMAIN "SNI-домен маскировки (tls_domain)" "${TLS_DOMAIN:-www.microsoft.com}"
elif [ "$ONLY_CADDY" -eq 0 ]; then
    say "конфиг $ETC_DIR/registry.toml существует — оставляю как есть"
fi

[ -n "$PANEL_TOKEN" ] || PANEL_TOKEN="$(rand_hex 24)"

# ---------------------------------------------------------------- установка регистратора
install_registry() {
    local listen_addr="127.0.0.1:${REG_PORT}"
    [ "$BARE" -eq 1 ] && listen_addr="0.0.0.0:${REG_PORT}"
    [ "$EXPOSE_HTTP" -eq 1 ] && listen_addr="0.0.0.0:${REG_PORT}"
    [ "$SKIP_CADDY" -eq 1 ] && listen_addr="0.0.0.0:${REG_PORT}"

    say "Устанавливаю sharedd-registry..."
    install -m 0755 "$BINARY" /usr/local/bin/sharedd-registry

    ensure_user "$SVC_USER" || die "не удалось создать системного пользователя $SVC_USER"

    # каталоги ВЛАДЕЛЬЦА сервиса: state (StateDirectory тоже подстрахует) и /etc —
    # панель правит registry.toml на лету (temp+rename требует write на каталог)
    install -d -m 0750 -o "$SVC_USER" -g "$SVC_USER" "$ETC_DIR"
    install -d -m 0750 -o "$SVC_USER" -g "$SVC_USER" "$STATE_DIR"
    # если каталоги были созданы раньше под root — исправляем владельца рекурсивно
    chown "$SVC_USER:$SVC_USER" "$ETC_DIR" "$STATE_DIR"
    chown -R "$SVC_USER:$SVC_USER" "$STATE_DIR"
    [ -f "$ETC_DIR/registry.toml" ] && chown "$SVC_USER:$SVC_USER" "$ETC_DIR/registry.toml"

    # верификация: сервисный пользователь РЕАЛЬНО может писать (bug report:
    # "write state error: permission denied" при root-владельце каталога)
    local wtest
    if command -v runuser &>/dev/null; then
        wtest="$(runuser -u "$SVC_USER" -- sh -c "touch $STATE_DIR/.wtest && echo ok" 2>&1 || true)"
    else
        wtest="$(su -s /bin/sh -c "touch $STATE_DIR/.wtest && echo ok" "$SVC_USER" 2>&1 || true)"
    fi
    [ "$wtest" = "ok" ] || die "$SVC_USER не может писать в $STATE_DIR: $wtest"
    rm -f "$STATE_DIR/.wtest"

    if [ "$NEED_CONFIG" -eq 1 ]; then
        USER_SECRET="$(rand_hex 16)"
        CF_DOMAINS_TOML="$(echo "$CF_DOMAINS" | tr ',' '\n' | sed 's/^ *//;s/ *$//' | grep -v '^$' | sed 's/.*/"&"/' | paste -sd, -)"
        [ -n "$CF_DOMAINS_TOML" ] || die "managed-домены не заданы"

        cat > "$ETC_DIR/registry.toml" <<REG_TOML
# sharedd registry (сгенерировано install_registry.sh $(date -Iseconds))
# Секции shared_proxy/cloudflare/node_defaults/globalping/panel правятся из панели.
[http]
addr = "${listen_addr}"

[state]
file = "${STATE_DIR}/registry_state.json"

[panel]
enabled = true
token = "${PANEL_TOKEN}"
events_max = 500

[healthcheck]
probe_interval_ms = 5000
probe_timeout_ms = 3000
selection_interval_ms = 3000
heartbeat_ttl_sec = 60
# Гистерезис здоровья ПО ПОДРЯД идущим проверкам (защёлки, не мгновенные флаги):
# fail_threshold подряд неудач гасит ноду (TCP-пробы и metrics-отчёты
# ноды); recover_threshold подряд удач возвращает в строй. report_freshness_min —
# dead-man's switch: ни одного отчёта столько минут = нода вне строя независимо
# от серий (агент молчит совсем).
fail_threshold = 3
recover_threshold = 2
report_freshness_min = 15
# Нода, непрерывно нездоровая дольше стольких минут, удаляется из пула
# (явный 0 = выключить рипер; ключ не задан = 60). После prune нода ещё
# и в карантине — регистрация отклоняется 429, серия карантинов растёт
# 15м → 30м → 1ч → 2ч → 3ч, обнуляется при реальном выздоровлении ноды.
prune_unhealthy_min = 60

# Принудительная ротация мастерства: максимум непрерывных минут ноды
# мастером одного домена — дальше домен уходит следующей здоровой ноде очереди
# (round-robin; отсчёт переживает рестарт регистратора). 0 = без лимита;
# ключ не задан = дефолт 30. Меняется из панели: Настройки → Ротация мастерства.
[rotation]
master_ttl_minutes = 30

# СРМД — Система Распределения и Масштабирования Доменов: держит не больше
# max_nodes_per_domain нод на домен. enabled=true — при росте пула создаёт
# сиротские домены с инкрементом от base_domain (shared.example.com →
# shared1.example.com, shared2.…) и сама дописывает их в cloudflare.domains;
# при сжатии пула сворачивает лишние в CNAME на оставшиеся, балансируя по
# активным клиентам (только по общему секрету). По стандарту ВЫКЛЮЧЕНО:
# панель просто даёт алерт «нод слишком много в очереди». Вкладка панели: «СРМД».
[srmd]
enabled = false
base_domain = ""
max_nodes_per_domain = 5

[node_defaults]
heartbeat_ms = 15000
globalping_ms = 300000
metrics_ms = 60000
sync_ms = 60000

[cloudflare]
api_token = "${CF_TOKEN}"
zone_id = "${CF_ZONE}"
domains = [${CF_DOMAINS_TOML}]
dns_ttl = 60
proxied = false

[globalping]
api_base = "https://api.globalping.io/v1"

[shared_proxy]
tls_domain = "${TLS_DOMAIN}"

[shared_proxy.users]
# mtproto secret: 32 hex; форматы с ee-префиксом — см. README
user1 = "${USER_SECRET}"
REG_TOML
        chown "$SVC_USER:$SVC_USER" "$ETC_DIR/registry.toml"
        chmod 0660 "$ETC_DIR/registry.toml"
        ok "конфиг записан: $ETC_DIR/registry.toml (0660 $SVC_USER)"
    else
        # существующий конфиг — убедимся, что сервис может его ЧИТАТЬ и (для панели) ПИСАТЬ
        chown "$SVC_USER:$SVC_USER" "$ETC_DIR/registry.toml" 2>/dev/null || true
        chmod 0660 "$ETC_DIR/registry.toml" 2>/dev/null || true
    fi

    local cred_file=$ETC_DIR/credentials.txt
    if [ ! -f "$cred_file" ]; then
        {
            echo "# sharedd registry — доступы ($(date -Iseconds))"
            echo "panel_token = ${PANEL_TOKEN}"
            [ "${USER_SECRET:-}" != "" ] && echo "mtproto_user1_secret = ${USER_SECRET}"
        } > "$cred_file"
        chmod 0600 "$cred_file"
        ok "доступы: ${cred_file} (0600)"
    else
        warn "credentials.txt уже есть — не трогаю (токен панели смотрите там)"
    fi

    cat > /etc/systemd/system/sharedd-registry.service <<'REG_UNIT'
[Unit]
Description=sharedd — HA Registry для MTProto-прокси
After=network.target

[Service]
ExecStart=/usr/local/bin/sharedd-registry -config /etc/sharedd/registry.toml
Restart=always
RestartSec=5
User=sharedd-registry
StateDirectory=sharedd
AmbientCapabilities=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
REG_UNIT
    systemctl daemon-reload
    systemctl enable sharedd-registry.service >>"$INSTALL_LOG" 2>&1
    systemctl restart sharedd-registry.service
    sleep 1
    systemctl is-active --quiet sharedd-registry.service && ok "sharedd-registry запущен" \
        || die "sharedd-registry не стартовал: journalctl -u sharedd-registry -n 50"
}

# ---------------------------------------------------------------- Caddyfile + запуск
setup_caddy() {
    [ -n "$DOMAIN" ] || die "--only-caddy требует --domain"
    install_caddy || return 1

    if [ -f /etc/caddy/Caddyfile ] && ! grep -q "sharedd" /etc/caddy/Caddyfile; then
        cp -a /etc/caddy/Caddyfile "/etc/caddy/Caddyfile.bak.$(date +%s)"
        warn "существующий Caddyfile сохранён в Caddyfile.bak.* (перезаписываю)"
    fi
    local email_block=""
    [ -n "$EMAIL" ] && email_block="{
	email ${EMAIL}
}
"
    cat > /etc/caddy/Caddyfile <<CADDYFILE
# sharedd registry — reverse proxy (TLS: автоматический Let's Encrypt)
${email_block}${DOMAIN} {
	encode zstd gzip
	reverse_proxy 127.0.0.1:${REG_PORT}
	header {
		-Server
		X-Content-Type-Options nosniff
	}
}
CADDYFILE
    chown caddy:caddy /etc/caddy/Caddyfile 2>/dev/null || true
    chmod 0644 /etc/caddy/Caddyfile
    caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile >>"$INSTALL_LOG" 2>&1 \
        || die "Caddyfile невалиден: caddy validate --config /etc/caddy/Caddyfile"
    systemctl enable caddy >>"$INSTALL_LOG" 2>&1 || true
    systemctl restart caddy
    sleep 1
    systemctl is-active --quiet caddy || return 1
    ok "caddy запущен (${DOMAIN} → 127.0.0.1:${REG_PORT})"
    return 0
}

# ================================ main ================================

if [ "$ONLY_CADDY" -eq 0 ]; then
    HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    if [ -z "$BINARY" ]; then
        for cand in "$HERE/sharedd-registry" "$HERE/../dist/sharedd-registry" "/usr/local/bin/sharedd-registry"; do
            [ -f "$cand" ] && BINARY="$cand" && break
        done
    fi
    [ -n "$BINARY" ] && [ -f "$BINARY" ] || die "бинарник sharedd-registry не найден.
    Соберите: scripts/build_registry.sh  (или передайте --binary /путь/к/бинарнику)"
    say "бинарник: ${BOLD}${BINARY}${NC}"
    install_registry
fi

CADDY_STATUS="skipped"
if [ "$BARE" -eq 0 ] && [ "$SKIP_CADDY" -eq 0 ]; then
    if setup_caddy; then
        CADDY_STATUS="ok"
    else
        CADDY_STATUS="failed"
        echo ""
        warn "Caddy не установлен/не запустился — НО регистратор уже работает."
        if [ "$ONLY_CADDY" -eq 0 ]; then
            # переключаем registry на внешний HTTP, чтобы панель была доступна без Caddy
            if grep -q '127.0.0.1' "$ETC_DIR/registry.toml" 2>/dev/null; then
                sed -i 's/^addr = "127\.0\.0\.1:/addr = "0.0.0.0:/' "$ETC_DIR/registry.toml"
                systemctl restart sharedd-registry.service
                warn "registry переключён на 0.0.0.0:${REG_PORT} (панель: http://<ip>:${REG_PORT}/panel)"
            fi
        fi
        warn "Диагностика: хвост лога ниже; частые причины — заблокирован github/cloudsmith, нет DNS на домен."
        echo -e "  ${DIM}── хвост ${INSTALL_LOG} ──${NC}"
        tail -n 8 "$INSTALL_LOG" 2>/dev/null | sed 's/^/  /' || true
        echo -e "  ${DIM}━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
        warn "Позже доустановить Caddy:  sudo bash $0 --only-caddy --domain ${DOMAIN:-<fqdn>}"
        warn "Или принесите бинарник:     sudo bash $0 --only-caddy --domain <fqdn> --caddy-binary /путь/caddy"
    fi
elif [ "$BARE" -eq 1 ]; then
    CADDY_STATUS="bare"
fi

# ---------------------------------------------------------------- проверка
echo ""
hline
say "Проверка..."
if [ "$CADDY_STATUS" = "ok" ]; then
    cert_ok=0
    for i in 1 2 3 4 5 6; do
        if curl -fsS -m 10 "https://${DOMAIN}/healthz" >>"$INSTALL_LOG" 2>&1; then cert_ok=1; break; fi
        sleep 5
    done
    if [ "$cert_ok" -eq 1 ]; then
        ok "https://${DOMAIN} отвечает, сертификат получен"
    else
        warn "https://${DOMAIN} пока не отвечает — DNS не указывает на сервер или закрыты порты 80/443."
        warn "Смотрите: journalctl -u caddy -f"
    fi
elif [ "$CADDY_STATUS" != "skipped" ]; then
    curl -fsS -m 5 "http://127.0.0.1:${REG_PORT}/healthz" >>"$INSTALL_LOG" 2>&1 \
        && ok "registry отвечает на порту ${REG_PORT}" \
        || warn "registry не отвечает на ${REG_PORT} — journalctl -u sharedd-registry -n 50"
fi

if command -v ufw &>/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
    warn "ufw активен: нужны правила:  ufw allow 80,443/tcp"
fi
if command -v firewall-cmd &>/dev/null && systemctl is-active --quiet firewalld; then
    warn "firewalld активен: firewall-cmd --permanent --add-service={http,https} && firewall-cmd --reload"
fi

# ---------------------------------------------------------------- итог
echo ""
hline
echo -e "  ${GREEN}${BOLD}Установка завершена${NC}"
hline
PANEL_URL=""
if [ "$CADDY_STATUS" = "ok" ]; then
    PANEL_URL="https://${DOMAIN}/panel"
elif [ "$CADDY_STATUS" = "failed" ]; then
    PANEL_URL="http://<ip>:${REG_PORT}/panel (Caddy не встал — см. выше)"
elif [ "$CADDY_STATUS" = "bare" ]; then
    PANEL_URL="http://<ip>:${REG_PORT}/panel"
fi
[ -n "$PANEL_URL" ] && echo -e "  Панель:          ${BOLD}${PANEL_URL}${NC}"
echo -e "  Токен панели:    ${BOLD}${PANEL_TOKEN}${NC}"
[ "${USER_SECRET:-}" != "" ] && echo -e "  MTProto user1:   ${BOLD}${USER_SECRET}${NC}"
echo -e "  Доступы также в: ${ETC_DIR}/credentials.txt"
echo ""
echo -e "  ${DIM}Управление:${NC}"
echo "    journalctl -u sharedd-registry -f       # логи регистратора"
[ "$CADDY_STATUS" = "ok" ] && echo "    journalctl -u caddy -f                  # логи caddy / выдача сертификата"
echo "    systemctl restart sharedd-registry"
echo ""
if [ "$CADDY_STATUS" = "ok" ]; then
    echo -e "  ${DIM}Ноды: [registry] url = \"https://${DOMAIN}\"  (всё API проксируется Caddy)${NC}"
else
    echo -e "  ${DIM}Ноды: [registry] url = \"http://<ip>:${REG_PORT}\"${NC}"
fi
echo -e "  ${DIM}Настройки (SNI, Cloudflare, пользователи, интервалы) правятся из панели.${NC}"
hline
