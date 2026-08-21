#!/usr/bin/env bash
# sharedd — установка агента ноды (node-agent) одной командой.
#
# Использование:
#   sudo bash install_node.sh                     # регистратор по умолчанию: https://registrar.ddproxy.xyz
#   sudo bash install_node.sh --registry https://ha.example.com --registry-token TOKEN --name helsinki
#   sudo bash install_node.sh --no-apply          # не трогать telemt.toml (только мониторинг)
#
# Что делает:
#   1. Ставит sharedd-node-agent в /usr/local/bin + systemd-юнит.
#   2. Пишет минимальный /etc/sharedd/node.toml (registry URL/token + путь к конфигу telemt).
#   3. one-shot конвейер применения конфига ДО старта демона:
#      стоп прокси → патч telemt.toml → старт → ожидание /metrics → откат
#      при провале; сбой любого шага → гарантированное восстановление прокси.
#   4. Запускает демона; финал — метрики прокси должны отвечать.
#
# Путь к конфигу telemt определяется АВТОМАТИЧЕСКИ:
#   есть /etc/telemt/telemt.toml           → классика (ванильный telemt)
#   есть /opt/mtproxyl/superexpert.toml    → MTProxyL superexpert (режим включается
#                                            автоматически, если выключен)
#   есть обА                               → интерактивный выбор 1/2 (в не-TTY —
#                                            fail-fast с подсказкой про --preset)
#   ничего нет, но MTProxyL установлен     → включаем superexpert (файл родится
#                                            копией текущего рабочего конфига)
# Явные переопределения: --preset classic|mtproxyl, --telemt-config PATH
# (env TELEMT_CONFIG). Патчить генерируемый /opt/mtproxyl/mtproxy/config.toml
# БЕССМЫСЛЕННО — MTProxyL пересобирает его из настроек; в superexpert-режиме
# источником правды становится superexpert.toml (копируется в config.toml
# при каждом (ре)старте прокси — поэтому на MTProxyL перезапуск идёт через
# `mtproxyl restart`; его делает сам агент — в one-shot прогоне и в sync).
#
# Идемпотентно: существующий node.toml не затирается без --reconfigure.

set -euo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; CYAN='\033[0;36m'
BOLD='\033[1m'; DIM='\033[2m'; NC='\033[0m'
SYM_OK='✓'; SYM_ERR='✗'; SYM_WARN='!'; SYM_ARROW='→'

INSTALL_LOG="/tmp/sharedd-node-install.log"
ETC_DIR="/etc/sharedd"
STATE_DIR="/var/lib/sharedd"

# ★ базовый регистратор проекта (можно переопределить --registry или env REGISTRY_URL)
DEFAULT_REGISTRY="https://registrar.ddproxy.xyz"

say()  { echo -e "  ${CYAN}${SYM_ARROW}${NC} $*"; }
ok()   { echo -e "  ${GREEN}${SYM_OK}${NC} $*"; }
warn() { echo -e "  ${YELLOW}${SYM_WARN}${NC} $*"; }
err()  { echo -e "  ${RED}${SYM_ERR}${NC} $*" >&2; }
die()  {
    err "$*"
    echo -e "  ${DIM}── хвост ${INSTALL_LOG} ──${NC}" >&2
    tail -n 12 "$INSTALL_LOG" 2>/dev/null | sed 's/^/  /' >&2 || true
    exit 1
}
hline() { echo -e "  ${DIM}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"; }

BINARY=""
REGISTRY_URL="${REGISTRY_URL:-}"
REGISTRY_TOKEN="${REGISTRY_TOKEN:-}"
NODE_NAME="${NODE_NAME:-}"
TELEMT_CONFIG="${TELEMT_CONFIG:-}"   # пустая = автоопределение ниже
PRESET=""
APPLY=1
RECONFIGURE=0
MTPROXYL_MODE=0  # 1 = конфиг MTProxyL superexpert (включаем режим, restart через CLI)
NONINTERACTIVE=0

# автоопределяемые пути к конфигу telemt
TELEMT_CLASSIC="/etc/telemt/telemt.toml"
TELEMT_MTPROXYL="/opt/mtproxyl/superexpert.toml"
MTPROXYL_DIR="/opt/mtproxyl"
MTPROXYL_SETTINGS="${MTPROXYL_DIR}/settings.conf"

while [ $# -gt 0 ]; do
    case "$1" in
        --binary)         BINARY="${2:?}"; shift 2 ;;
        --binary=*)       BINARY="${1#*=}"; shift ;;
        --registry)       REGISTRY_URL="${2:?--registry требует URL}"; NONINTERACTIVE=1; shift 2 ;;
        --registry=*)     REGISTRY_URL="${1#*=}"; NONINTERACTIVE=1; shift ;;
        --registry-token) REGISTRY_TOKEN="${2:?--registry-token требует токен}"; shift 2 ;;
		--registry-token=*) REGISTRY_TOKEN="${1#*=}"; shift ;;
		--name)           NODE_NAME="${2:?--name требует имя}"; shift 2 ;;
		--name=*)         NODE_NAME="${1#*=}"; shift ;;
        --preset)         PRESET="${2:?--preset classic|mtproxyl}"; shift 2 ;;
        --preset=*)       PRESET="${1#*=}"; shift ;;
        --telemt-config)  TELEMT_CONFIG="${2:?}"; shift 2 ;;
        --telemt-config=*) TELEMT_CONFIG="${1#*=}"; shift ;;
        --no-apply)       APPLY=0; shift ;;
        --reconfigure)    RECONFIGURE=1; shift ;;
        -h|--help)        grep '^#' "$0" | head -28 | sed 's/^# \{0,1\}//'; exit 0 ;;
        *) die "неизвестный аргумент: $1 (см. --help)" ;;
    esac
done

if [ -n "$NODE_NAME" ]; then
    printf '%s' "$NODE_NAME" | grep -qE '^[A-Za-z0-9]([A-Za-z0-9._-]{0,8}[A-Za-z0-9])?$' \
        || die "недопустимое имя '$NODE_NAME' — 1..10 символов: латиница/цифры/._- (без ._- по краям)"
fi

# --preset не перекрывает явный --telemt-config / env TELEMT_CONFIG
case "$PRESET" in
    "") ;;
    classic)  [ -z "$TELEMT_CONFIG" ] && TELEMT_CONFIG="$TELEMT_CLASSIC" ;;
    mtproxyl) [ -z "$TELEMT_CONFIG" ] && TELEMT_CONFIG="$TELEMT_MTPROXYL" ;;
    *) die "--preset: ожидается classic|mtproxyl, получено: $PRESET" ;;
esac

if [ "$(id -u)" -ne 0 ]; then
    echo -e "${RED}Запустите от root:${NC} sudo bash $0 ..." >&2
    exit 1
fi
: > "$INSTALL_LOG"

echo ""
hline
echo -e "  ${BOLD}sharedd${NC} — установка агента ноды"
echo -e "  ${DIM}регистратор: ${DEFAULT_REGISTRY} (дефолт)${NC}"
hline
echo ""

if ! command -v systemctl &>/dev/null || [ ! -d /run/systemd/system ]; then
    die "нужен systemd (systemctl + /run/systemd/system)."
fi

TTY=0
[ -t 0 ] && TTY=1

if [ -z "$REGISTRY_URL" ]; then
    if [ "$TTY" -eq 1 ] && [ "$NONINTERACTIVE" -eq 0 ]; then
        read -r -p "  ${SYM_ARROW} URL регистратора [${DEFAULT_REGISTRY}]: " input </dev/tty || true
        REGISTRY_URL="${input:-$DEFAULT_REGISTRY}"
    else
        REGISTRY_URL="$DEFAULT_REGISTRY"
    fi
fi
REGISTRY_URL="${REGISTRY_URL%/}"
case "$REGISTRY_URL" in
    http://*|https://*) ;;
    *) die "registry URL должен начинаться с http(s)://: $REGISTRY_URL" ;;
esac
if [ -z "$REGISTRY_TOKEN" ] && [ "$TTY" -eq 1 ] && [ "$NONINTERACTIVE" -eq 0 ]; then
    read -r -s -p "  ${SYM_ARROW} Node API token регистратора: " REGISTRY_TOKEN </dev/tty || true
    echo ""
fi
[ -n "$REGISTRY_TOKEN" ] || die "нужен обязательный Node API token. Скопируйте готовую команду: Панель → Доступ → Подключить ноду"
if [ -z "$NODE_NAME" ] && [ "$TTY" -eq 1 ] && [ "$NONINTERACTIVE" -eq 0 ]; then
    read -r -p "  ${SYM_ARROW} Имя ноды, 1..10 символов (пусто = node): " NODE_NAME </dev/tty || true
    if [ -n "$NODE_NAME" ]; then
        printf '%s' "$NODE_NAME" | grep -qE '^[A-Za-z0-9]([A-Za-z0-9._-]{0,8}[A-Za-z0-9])?$' \
            || die "недопустимое имя '$NODE_NAME' — 1..10 символов: латиница/цифры/._- (без ._- по краям)"
    fi
fi

toml_escape() {
    local value="$1"
    value="${value//\\/\\\\}"
    value="${value//\"/\\\"}"
    printf '%s' "$value"
}

update_registry_config() { # update_registry_config <node.toml>
    local file="$1" tmp line section=0 saw_registry=0 have_url=0 have_token=0
    local escaped_url escaped_token
    escaped_url="$(toml_escape "$REGISTRY_URL")"
    escaped_token="$(toml_escape "$REGISTRY_TOKEN")"
    tmp="$(mktemp "${file}.XXXXXX")"
    while IFS= read -r line || [ -n "$line" ]; do
        if [[ "$line" =~ ^[[:space:]]*\[[^]]+\][[:space:]]*$ ]]; then
            if [ "$section" -eq 1 ]; then
                [ "$have_url" -eq 1 ] || printf 'url = "%s"\n' "$escaped_url" >>"$tmp"
                [ "$have_token" -eq 1 ] || printf 'token = "%s"\n' "$escaped_token" >>"$tmp"
            fi
            section=0
            if [[ "$line" =~ ^[[:space:]]*\[registry\][[:space:]]*$ ]]; then section=1; saw_registry=1; fi
        fi
        if [ "$section" -eq 1 ] && [[ "$line" =~ ^[[:space:]]*url[[:space:]]*= ]]; then
            printf 'url = "%s"\n' "$escaped_url" >>"$tmp"; have_url=1; continue
        fi
        if [ "$section" -eq 1 ] && [[ "$line" =~ ^[[:space:]]*token[[:space:]]*= ]]; then
            printf 'token = "%s"\n' "$escaped_token" >>"$tmp"; have_token=1; continue
        fi
        printf '%s\n' "$line" >>"$tmp"
    done <"$file"
    if [ "$section" -eq 1 ]; then
        [ "$have_url" -eq 1 ] || printf 'url = "%s"\n' "$escaped_url" >>"$tmp"
        [ "$have_token" -eq 1 ] || printf 'token = "%s"\n' "$escaped_token" >>"$tmp"
    fi
    if [ "$saw_registry" -eq 0 ]; then
        printf '\n[registry]\nurl = "%s"\ntoken = "%s"\n' "$escaped_url" "$escaped_token" >>"$tmp"
    fi
    mv -f "$tmp" "$file"
    chmod 0640 "$file"
}

# --- бинарник ---
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -z "$BINARY" ]; then
    for cand in "$HERE/sharedd-node-agent" "$HERE/../dist/sharedd-node-agent" "/usr/local/bin/sharedd-node-agent"; do
        [ -f "$cand" ] && BINARY="$cand" && break
    done
fi
[ -n "$BINARY" ] && [ -f "$BINARY" ] || die "бинарник sharedd-node-agent не найден.
    Соберите: scripts/build_node.sh  (или передайте --binary /путь)"
IDENTITY="$("$BINARY" --version 2>/dev/null || true)"
[ "$IDENTITY" = "sharedd-node-agent" ] || die "передан неверный или устаревший бинарник (ожидался sharedd-node-agent, identity: ${IDENTITY:-не определён})"
say "бинарник: ${BOLD}${BINARY}${NC}"

# --- автоопределение конфига telemt ---

mtproxyl_cli() { command -v mtproxyl &>/dev/null; }

# Режим superexpert у MTProxyL активен = флаг в настройках + файл на месте
# (та же семантика, что _superexpert_active в MTProxyL). Читаем settings.conf
# напрямую — это дёшево и не дёргает CLI на каждое сравнение.
superexpert_active() {
    [ -f "$TELEMT_MTPROXYL" ] || return 1
    [ -f "$MTPROXYL_SETTINGS" ] || return 1
    grep -qE '^SUPEREXPERT_ENABLED="?true"?' "$MTPROXYL_SETTINGS" 2>/dev/null
}

# Гарантировать, что superexpert включён: если выключен — включаем через CLI
# (MTPROXYL_ASSUME_YES=1: read_line отвечает yes, редактор не запускается;
# если superexpert.toml ещё нет — 'on' создаст его копией рабочего config.toml).
ensure_superexpert_on() {
    if superexpert_active; then
        say "MTProxyL: режим супер-эксперта уже активен"
        return 0
    fi
    if ! mtproxyl_cli; then
        if [ -f "$TELEMT_MTPROXYL" ]; then
            warn "CLI mtproxyl не найден — включить режим не могу; считаю, что ${TELEMT_MTPROXYL} уже является источником конфига"
            return 0
        fi
        die "ни ванильного telemt (${TELEMT_CLASSIC}), ни MTProxyL (${MTPROXYL_DIR} + CLI) не найдено.
    Сначала установите telemt/MTProxyL, либо задайте путь явно: --telemt-config PATH"
    fi
    say "MTProxyL: включаю режим супер-эксперта (свой конфиг ${TELEMT_MTPROXYL})..."
    MTPROXYL_ASSUME_YES=1 mtproxyl superexpert on >>"$INSTALL_LOG" 2>&1 \
        || die "команда 'mtproxyl superexpert on' завершилась с ошибкой — см. лог выше"
    if superexpert_active \
        || mtproxyl superexpert status --json 2>/dev/null | grep -q '"active":true'; then
        ok "режим супер-эксперта активен (${TELEMT_MTPROXYL})"
    else
        die "режим супер-эксперта не включился — проверьте: mtproxyl superexpert status"
    fi
    if [ ! -f "$TELEMT_MTPROXYL" ]; then
        die "ожидался файл ${TELEMT_MTPROXYL} после включения superexpert — его нет"
    fi
}

if [ -z "$TELEMT_CONFIG" ]; then
    have_classic=0; have_superx=0
    [ -f "$TELEMT_CLASSIC" ] && have_classic=1
    [ -f "$TELEMT_MTPROXYL" ] && have_superx=1

    if [ "$have_classic" -eq 1 ] && [ "$have_superx" -eq 1 ]; then
        # оба мира существуют — выбор неоднозначен, спрашиваем (в не-TTY — fail-fast)
        if [ "$TTY" -eq 1 ] && [ "$NONINTERACTIVE" -eq 0 ]; then
            def=1; superexpert_active && def=2
            echo -e "  ${BOLD}Найдены два конфига telemt — какой патчить агенту?${NC}"
            echo -e "    ${BOLD}1${NC}) Классика (ванильный telemt): ${TELEMT_CLASSIC}"
            if superexpert_active; then
                sx_note="${GREEN}(режим активен)${NC}"
            else
                sx_note="${DIM}(superexpert выключен — включим)${NC}"
            fi
            echo -e "    ${BOLD}2${NC}) MTProxyL superexpert: ${TELEMT_MTPROXYL}   ${sx_note}"
            read -r -p "  ${SYM_ARROW} Выбор [${def}]: " choice </dev/tty || true
            case "${choice:-$def}" in
                1) TELEMT_CONFIG="$TELEMT_CLASSIC" ;;
                2) TELEMT_CONFIG="$TELEMT_MTPROXYL" ;;
                *) die "некорректный выбор: ${choice} (ожидалось 1/2)" ;;
            esac
            echo ""
        elif [ "$NONINTERACTIVE" -eq 1 ]; then
            def=1; superexpert_active && def=2
            [ "$def" -eq 1 ] && TELEMT_CONFIG="$TELEMT_CLASSIC" || TELEMT_CONFIG="$TELEMT_MTPROXYL"
            warn "найдены оба конфига telemt — неинтерактивно выбран: $TELEMT_CONFIG"
        else
            die "обнаружены оба конфига: $TELEMT_CLASSIC и $TELEMT_MTPROXYL — выбор неоднозначен.
    Укажите явно: --preset classic | --preset mtproxyl  (или --telemt-config PATH)"
        fi
    elif [ "$have_classic" -eq 1 ]; then
        TELEMT_CONFIG="$TELEMT_CLASSIC"
        if mtproxyl_cli || [ -d "$MTPROXYL_DIR" ]; then
            warn "обнаружен MTProxyL, но superexpert-конфига нет — беру классику."
            warn "если прокси на самом деле крутит MTProxyL (он перегенерирует свой config.toml),"
            warn "перезапустите установщик: --preset mtproxyl"
        fi
    else
        # superexpert.toml есть — берём его (включив режим); файла нет, но MTProxyL
        # установлен — тоже его мир: включаем superexpert, файл родится копией
        # текущего рабочего конфига.
        TELEMT_CONFIG="$TELEMT_MTPROXYL"
    fi
fi

# mtproxyl-режим — по фактическому пути (покрывает и --telemt-config, и --preset)
if [ "$TELEMT_CONFIG" = "$TELEMT_MTPROXYL" ]; then
    MTPROXYL_MODE=1
    ensure_superexpert_on
fi

# --- telemt должен существовать ---
if [ ! -f "$TELEMT_CONFIG" ]; then
    warn "конфиг telemt не найден: $TELEMT_CONFIG"
    if [ -d "$MTPROXYL_DIR" ] || mtproxyl_cli; then
        warn "обнаружен MTProxyL: прокси должен читать ${TELEMT_MTPROXYL} (режим superexpert) — перезапустите: --preset mtproxyl"
    fi
    [ -f "$TELEMT_CLASSIC" ] && [ "$TELEMT_CONFIG" != "$TELEMT_CLASSIC" ] && warn "найден классический конфиг: $TELEMT_CLASSIC — перезапустите: --preset classic"
    die "сначала установите telemt (или MTProxyL), затем повторите. Путь задаётся: --preset classic|mtproxyl, --telemt-config PATH"
fi
say "конфиг telemt: ${BOLD}${TELEMT_CONFIG}${NC}"

# Общий порт задаёт регистратор; несовпадение означает, что ноду нельзя
# безопасно допускать в пул или навешивать antiscan на локальный порт.
if command -v curl >/dev/null 2>&1; then
    SHARED_JSON="$(curl -fsSL --connect-timeout 15 -H "Authorization: Bearer $REGISTRY_TOKEN" "$REGISTRY_URL/config")" \
        || die "не удалось получить /config регистратора для проверки общего порта"
elif command -v wget >/dev/null 2>&1; then
    SHARED_JSON="$(wget -qO- --header="Authorization: Bearer $REGISTRY_TOKEN" "$REGISTRY_URL/config")" \
        || die "не удалось получить /config регистратора для проверки общего порта"
else
    die "для проверки общего порта нужен curl или wget"
fi
REGISTRY_PROXY_PORT="$(printf '%s' "$SHARED_JSON" | grep -oE '"proxy_port"[[:space:]]*:[[:space:]]*[0-9]+' | grep -oE '[0-9]+' | head -n1 || true)"
[ -n "$REGISTRY_PROXY_PORT" ] && [ "$REGISTRY_PROXY_PORT" -ge 1 ] && [ "$REGISTRY_PROXY_PORT" -le 65535 ] \
    || die "регистратор вернул некорректный proxy_port"
LOCAL_PROXY_PORT="$("$BINARY" --telemt-port="$TELEMT_CONFIG" 2>>"$INSTALL_LOG")" \
    || die "не удалось прочитать порт из $TELEMT_CONFIG — см. $INSTALL_LOG"
printf '%s' "$LOCAL_PROXY_PORT" | grep -qE '^[0-9]+$' || die "агент вернул некорректный локальный порт: $LOCAL_PROXY_PORT"
[ "$LOCAL_PROXY_PORT" = "$REGISTRY_PROXY_PORT" ] \
    || die "порт прокси $LOCAL_PROXY_PORT не совпадает с общим портом регистратора $REGISTRY_PROXY_PORT"
ok "общий порт прокси подтверждён регистратором: ${BOLD}${REGISTRY_PROXY_PORT}${NC}"

if ! command -v ipset >/dev/null 2>&1 || ! command -v iptables >/dev/null 2>&1; then
    say "Устанавливаю зависимости antiscan (ipset, iptables)..."
    if command -v apt-get >/dev/null 2>&1; then
        DEBIAN_FRONTEND=noninteractive apt-get update -qq >>"$INSTALL_LOG" 2>&1
        DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ipset iptables >>"$INSTALL_LOG" 2>&1
    elif command -v dnf >/dev/null 2>&1; then
        dnf install -y -q ipset iptables >>"$INSTALL_LOG" 2>&1
    elif command -v yum >/dev/null 2>&1; then
        yum install -y -q ipset iptables >>"$INSTALL_LOG" 2>&1
    else
        die "для antiscan нужны ipset и iptables; пакетный менеджер не найден"
    fi
fi
command -v ipset >/dev/null 2>&1 && command -v iptables >/dev/null 2>&1 \
    || die "не удалось установить ipset/iptables — см. $INSTALL_LOG"

# --- установка ---
say "Устанавливаю sharedd-node-agent..."
install -m 0755 "$BINARY" /usr/local/bin/sharedd-node-agent
install -d -m 0750 -o root -g root "$ETC_DIR"
install -d -m 0750 -o root -g root "$STATE_DIR"   # node_id; агент работает от root (правит telemt.toml)

if [ -n "$NODE_NAME" ]; then
    ID_STATE_FILE="$STATE_DIR/node_id"
    OLD_ID=""
    [ ! -s "$ID_STATE_FILE" ] || OLD_ID="$(tr -d '[:space:]' <"$ID_STATE_FILE")"
    old_suffix="${OLD_ID##*-}"
    if [ "${OLD_ID%-*}" = "$NODE_NAME" ] && printf '%s' "$old_suffix" | grep -qE '^[a-z0-9]{5}$'; then
        NODE_ID="$OLD_ID"
    else
        NODE_SUFFIX="$(LC_ALL=C tr -dc 'a-z0-9' </dev/urandom 2>/dev/null | head -c 5 || true)"
        [ "${#NODE_SUFFIX}" -eq 5 ] || die "не удалось сгенерировать суффикс имени ноды"
        NODE_ID="${NODE_NAME}-${NODE_SUFFIX}"
        if [ -n "$OLD_ID" ]; then
            warn "node id меняется: $OLD_ID -> $NODE_ID; позиция в очереди будет новой"
        fi
        printf '%s' "$NODE_ID" >"$ID_STATE_FILE"
        chmod 0600 "$ID_STATE_FILE"
    fi
    ok "имя ноды: ${BOLD}${NODE_ID}${NC}"
fi

NEED_CONFIG=0
[ ! -f "$ETC_DIR/node.toml" ] && NEED_CONFIG=1
[ "$RECONFIGURE" -eq 1 ] && NEED_CONFIG=1

APPLY_TOML="true"
[ "$APPLY" -eq 0 ] && APPLY_TOML="false"

if [ "$NEED_CONFIG" -eq 1 ]; then
    cat > "$ETC_DIR/node.toml" <<NODE_TOML
# sharedd node agent (сгенерировано install_node.sh $(date -Iseconds))
[registry]
url = "$(toml_escape "$REGISTRY_URL")"
token = "$(toml_escape "$REGISTRY_TOKEN")"

[telemt]
config_path = "${TELEMT_CONFIG}"

[sync]
# агент дописывает общий конфиг прямо в telemt.toml (пользователи, SNI,
# metrics_listen; mask_host не трогается); сохраняет права/владельца файла.
apply_to_telemt = ${APPLY_TOML}
NODE_TOML
    chmod 0640 "$ETC_DIR/node.toml"
    ok "конфиг записан: $ETC_DIR/node.toml"
else
    update_registry_config "$ETC_DIR/node.toml"
    ok "registry.url и обязательный registry.token обновлены в $ETC_DIR/node.toml"
fi

cat > /etc/systemd/system/sharedd-node-agent.service <<'NODE_UNIT'
[Unit]
Description=sharedd — агент HA-ноды (MTProto proxy pool)
After=network-online.target telemt.service
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/sharedd-node-agent -config /etc/sharedd/node.toml
Restart=always
RestartSec=5
# root нужен, чтобы перезаписывать telemt.toml с сохранением чужих owner/mode
User=root
StateDirectory=sharedd

[Install]
WantedBy=multi-user.target
NODE_UNIT
systemctl daemon-reload
systemctl enable sharedd-node-agent.service >>"$INSTALL_LOG" 2>&1

# --- 1. ПРОКСИ: конвейер применения конфига выполняет УСТАНОВЩИК ---
# Синхронный one-shot прогон агента (-apply-once) ДО старта демона:
#   стоп прокси → патч и сохранение telemt.toml → старт прокси → ожидание
#   подъёма по /metrics (это же и проверка, что конфиг принят) → при провале
#   откат файла; финал — прокси обязан работать: метрики отвечают, иначе
#   (пере)запускаем (юнит / mtproxyl CLI / вслепую telemt.service).
# Почему не «агент применит сам после запуска»: применение шло
# только при УСПЕШНОМ первом fetch /config — любой сбой сети/DNS/TLS оставлял
# остановленный установщиком прокси лежать мёртво, а первый globalping рисовал
# 0. Теперь прокси поднят и проверен ДО старта демона: регистрация, heartbeat
# и первый globalping уходят в живой прокси.
#
# Детект юнита — тремя уровнями (на Classic и MEKO-фиксе сервис всегда
# telemt.service; list-units его не видит, если юнит ещё не загружен):
detect_proxy_unit() {
    local unit d luf
    for unit in telemt.service mtproxy.service mtproxyl-telemt.service; do
        # 1) юнит известен менеджеру (cat читает и незагруженные юнит-файлы)
        if systemctl cat "$unit" >/dev/null 2>&1; then
            printf '%s' "$unit"; return 0
        fi
        # 2) индекс юнит-файлов с диска (свежая установка без daemon-reload)
        luf="$(systemctl list-unit-files "$unit" --no-legend --no-pager 2>/dev/null || true)"
        if [ -n "$luf" ]; then
            printf '%s' "$unit"; return 0
        fi
        # 3) последний рубеж — файлы на диске (дохлый dbus/частичный systemd:
        #    restart по имени файла всё равно обычно срабатывает)
        for d in /etc/systemd/system /run/systemd/system /usr/lib/systemd/system /lib/systemd/system /etc/systemd/system/*.wants; do
            [ -e "$d/$unit" ] && { printf '%s' "$unit"; return 0; }
        done
    done
    return 1
}

# rescue — вернуть прокси в работу, если one-shot не отработал (или был убит
# по таймауту между stop и start). Classic/MEKO: при неудаче детекта
# рестартуем telemt.service ВСЛЕПУЮ — он там всегда так называется.
rescue_proxy() {
    if [ "$MTPROXYL_MODE" -eq 1 ]; then
        if mtproxyl_cli; then
            say "восстановление прокси: mtproxyl restart..."
            MTPROXYL_ASSUME_YES=1 mtproxyl restart >>"$INSTALL_LOG" 2>&1 \
                && ok "прокси восстановлен (mtproxyl restart)" \
                || warn "mtproxyl restart тоже упал — смотрите: mtproxyl status"
        fi
        return 0
    fi
    local u="${PROXY_UNIT:-telemt.service}"
    say "восстановление прокси: systemctl restart $u ..."
    if systemctl restart "$u" >>"$INSTALL_LOG" 2>&1; then
        ok "прокси восстановлен ($u)"
    else
        warn "restart $u не удался — смотрите: journalctl -u $u -n 50 --no-pager"
    fi
}

# Эффективный apply_to_telemt — из ФАКТИЧЕСКОГО node.toml (при повторном запуске
# без --reconfigure мог остаться apply_to_telemt=false): тогда конфигом не
# управляем и прокси не трогаем вовсе.
APPLY_EFFECTIVE=1
[ "$APPLY" -eq 0 ] && APPLY_EFFECTIVE=0
if [ "$APPLY_EFFECTIVE" -eq 1 ] && grep -qE '^[[:space:]]*apply_to_telemt[[:space:]]*=[[:space:]]*false' "$ETC_DIR/node.toml" 2>/dev/null; then
    APPLY_EFFECTIVE=0
fi

PROXY_UNIT=""
if [ "$APPLY_EFFECTIVE" -eq 1 ]; then
    [ "$MTPROXYL_MODE" -eq 0 ] && PROXY_UNIT="$(detect_proxy_unit || true)"
    [ -n "$PROXY_UNIT" ] && say "юнит прокси: ${BOLD}${PROXY_UNIT}${NC}"

    if ! grep -aq 'APPLY-ONCE:' /usr/local/bin/sharedd-node-agent 2>/dev/null; then
        # Старая сборка агента проигнорировала бы -apply-once и висела бы
        # демоном-призраком до timeout: конвейера нет — прокси НЕ трогаем,
        # конфиг применит агент фоново после запуска (с рестартом прокси).
        warn "бинарник агента старой сборки (без one-shot конвейера) — конфиг синхронно не применяю."
        warn "пересоберите свежего агента (scripts/build_node.sh) и переподнимите установку;"
        warn "либо конфиг применится сам через ~1 мин после старта демона."
    else
        say "применяю конфиг: стоп прокси → патч → старт → проверка по метрикам (до ~3 мин)..."
        set +e
        APPLY_OUT="$(timeout 180 /usr/local/bin/sharedd-node-agent -config "$ETC_DIR/node.toml" -apply-once 2>&1)"
        APPLY_RC=$?
        set -e
        printf '%s\n' "$APPLY_OUT" >>"$INSTALL_LOG"
        FILTERED="$(printf '%s\n' "$APPLY_OUT" | grep -E 'APPLY-ONCE|applying|is up|restart|rollback|not ready|not detected' || true)"
        [ -n "$FILTERED" ] && { printf '%s\n' "$FILTERED" | tail -n 10 | sed 's/^/    /' || true; }
        case "$APPLY_RC" in
            0)
                ok "конфиг применён — прокси поднят, метрики отвечают" ;;
            3)
                warn "регистратор ${REGISTRY_URL} сейчас недоступен — конфиг пока не применён."
                warn "агент применит его сам, когда регистратор вернётся (sync каждую ~1 мин)."
                rescue_proxy ;;
            124)
                warn "one-shot прогон убит по таймауту 180 с (мог застрять между stop и start) — восстанавливаю..."
                rescue_proxy ;;
            *)
                warn "применение конфига не удалось (код ${APPLY_RC}) — восстанавливаю прокси..."
                rescue_proxy ;;
        esac
    fi
fi

# --- 2. Агент ---
say "запускаю sharedd-node-agent..."
systemctl restart sharedd-node-agent.service
sleep 2
if systemctl is-active --quiet sharedd-node-agent.service; then
    ok "агент запущен"
else
    die "агент не поднялся — смотрите: journalctl -u sharedd-node-agent -n 50 --no-pager"
fi

# --- 3. Финальная проверка: метрики должны отвечать ---
# One-shot на шаге 1 их уже дождался — тут обычно мгновенный успех; но если
# бинарник старый (шаг пропущен) или регистратор был недоступен — дожидаемся
# до 120 с и честно ругаемся. URL берём из самого конфига (умолчание
# 127.0.0.1:9090). Первая выдача сертификата маскировки бывает долгой.
if [ "$APPLY_EFFECTIVE" -eq 1 ]; then
    METRICS_URL="http://127.0.0.1:9090/metrics"
    ml=$(grep -E '^[[:space:]]*metrics_listen[[:space:]]*=' "$TELEMT_CONFIG" 2>/dev/null | tail -n1 | sed -E 's/.*=[[:space:]]*"([^"]+)".*/\1/' || true)
    mp=$(grep -E '^[[:space:]]*metrics_port[[:space:]]*=' "$TELEMT_CONFIG" 2>/dev/null | tail -n1 | sed -E 's/.*=[[:space:]]*([0-9]+).*/\1/' || true)
    if [ -n "$ml" ]; then
        mh="${ml%:*}"; mpo="${ml##*:}"
        case "$mh" in ""|0.0.0.0|"::") mh="127.0.0.1" ;; esac
        METRICS_URL="http://${mh}:${mpo}/metrics"
    elif [ -n "$mp" ]; then
        METRICS_URL="http://127.0.0.1:${mp}/metrics"
    fi
    say "жду подъёма прокси с новым конфигом (опрашиваю ${METRICS_URL}, до 120 с)..."
    UP=0
    for _ in $(seq 1 120); do
        if command -v curl &>/dev/null; then
            curl -fsS --max-time 3 "$METRICS_URL" >/dev/null 2>&1 && { UP=1; break; }
        else
            wget -q -T 3 -O /dev/null "$METRICS_URL" 2>/dev/null && { UP=1; break; }
        fi
        sleep 1
    done
    if [ "$UP" -eq 1 ]; then
        ok "прокси поднялся, метрики отвечают ($METRICS_URL)"
    else
        warn "прокси не поднялся за 120 с — диагностика:"
        warn "  journalctl -u sharedd-node-agent -n 50 --no-pager"
        [ -n "$PROXY_UNIT" ] && warn "  journalctl -u $PROXY_UNIT -n 50 --no-pager"
        [ "$MTPROXYL_MODE" -eq 1 ] && warn "  mtproxyl status"
    fi
fi

echo ""
hline
echo -e "  ${GREEN}${BOLD}Готово${NC}"
hline
echo -e "  Регистратор:     ${BOLD}${REGISTRY_URL}${NC}"
echo -e "  Конфиг telemt:   ${BOLD}${TELEMT_CONFIG}${NC}"
echo -e "  Конфиг ноды:     ${ETC_DIR}/node.toml"
echo -e "  Журнал:          journalctl -u sharedd-node-agent -f"
echo ""
echo -e "  ${DIM}Дальше всё автоматически: агент сам определит IP, зарегистрируется,${NC}"
echo -e "  ${DIM}подтянет SNI/пользователей/интервалы из регистратора и пропишет их${NC}"
echo -e "  ${DIM}в telemt.toml (mask_host не трогается; при mask=true — exclusive_mask).${NC}"
if [ "$MTPROXYL_MODE" -eq 1 ]; then
    echo ""
    echo -e "  ${DIM}MTProxyL superexpert: агент правит ${TELEMT_MTPROXYL} и сам делает${NC}"
    echo -e "  ${DIM}mtproxyl restart — рабочий config.toml пересобирается из superexpert.toml.${NC}"
    echo -e "  ${DIM}Все изменения из панели (юзеры/SNI) применяются автоматически с рестартом.${NC}"
fi
hline
