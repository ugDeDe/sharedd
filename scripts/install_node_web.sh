#!/usr/bin/env bash


# Текущий релиз: повтор той же команды безопасно обновляет агент.
BINARY_URL="https://github.com/ugDeDe/sharedd/releases/latest/download/sharedd-node-agent"
REGISTRY_URL_DEFAULT="https://registrar.ddproxy.xyz"

set -euo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; CYAN='\033[0;36m'
BOLD='\033[1m'; DIM='\033[0m'; NC='\033[0m'

say()  { echo -e "  ${CYAN}→${NC} $*"; }
ok()   { echo -e "  ${GREEN}✓${NC} $*"; }
warn() { echo -e "  ${YELLOW}!${NC} $*"; }
die()  { echo -e "  ${RED}✗ $*${NC}" >&2; exit 1; }
hline(){ echo -e "  ${DIM}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"; }

INSTALL_LOG="/tmp/sharedd-node-install.log"
ETC_DIR="${SHAREDD_ETC_DIR:-/etc/sharedd}"            # env-точки — только для
STATE_DIR="${SHAREDD_STATE_DIR:-/var/lib/sharedd}"    # автотестов; в обычной
UNIT_DEST="${SHAREDD_UNIT_DEST:-/etc/systemd/system/sharedd-node-agent.service}" # установке
BIN_DEST="${SHAREDD_BIN_DEST:-/usr/local/bin/sharedd-node-agent}"                # не заданы

# ветки telemt: ваниль vs MTProxyL superexpert (как в install_node.sh)
TELEMT_CLASSIC="/etc/telemt/telemt.toml"
TELEMT_MTPROXYL="/opt/mtproxyl/superexpert.toml"
MTPROXYL_DIR="/opt/mtproxyl"
MTPROXYL_SETTINGS="${MTPROXYL_DIR}/settings.conf"
MTPROXYL_MODE=0

# ── параметры командной строки ──────────────────────────────────────────
#   --registry=URL   URL регистратора; заодно включает неинтерактивный режим
#                    (для автоматизации: ни одного вопроса с клавиатуры,
#                    спорные развилки решаются дефолтами)
#   --name=ИМЯ       имя ноды; итоговый id = ИМЯ-<5 символов a-z0-9>,
#                    например ddproxy-6an4o. Без --name id генерирует агент,
#                    как раньше (node-<16 hex>)
# То же можно задать переменными окружения REGISTRY_URL / NODE_NAME.
# Пример автоматизации:
#   curl -fsSL .../install_node_web.sh | sudo bash -s -- --registry=https://reg.example.com --name=ddproxy
NODE_NAME="${NODE_NAME:-}"
NONINTERACTIVE=0
usage() {
    cat <<USAGE
Использование: sudo bash $0 [--registry=URL] [--name=ИМЯ]

  --registry=URL   URL регистратора (без вопросов с клавиатуры — режим автоматизации)
  --name=ИМЯ       имя ноды; итоговый id: ИМЯ-xxxxx (5 символов a-z0-9), напр. ddproxy-6an4o
  -h, --help       эта справка

Переменные окружения: REGISTRY_URL, NODE_NAME (флаги имеют приоритет).
Через пайп: curl -fsSL ...install_node_web.sh | sudo bash -s -- --registry=URL --name=ИМЯ
USAGE
}
while [ $# -gt 0 ]; do
    case "$1" in
        --registry=*) REGISTRY_URL="${1#*=}"; NONINTERACTIVE=1 ;;
        --registry)   shift; [ $# -gt 0 ] || die "--registry требует URL"
                      REGISTRY_URL="$1"; NONINTERACTIVE=1 ;;
        --name=*)     NODE_NAME="${1#*=}" ;;
        --name)       shift; [ $# -gt 0 ] || die "--name требует значение"
                      NODE_NAME="$1" ;;
        -h|--help)    usage; exit 0 ;;
        *)            die "неизвестный параметр: $1 (справка: --help)" ;;
    esac
    shift
done
if [ -n "$NODE_NAME" ]; then
    printf '%s' "$NODE_NAME" | grep -qE '^[A-Za-z0-9]([A-Za-z0-9._-]{0,40}[A-Za-z0-9])?$' \
        || die "недопустимое имя '$NODE_NAME' — латиница/цифры/._- (до 42 символов, без ._- по краям)"
fi

[ "$(id -u)" -eq 0 ] || die "запустите от root: sudo bash $0"
command -v systemctl &>/dev/null && [ -d /run/systemd/system ] || die "нужен systemd"
: > "$INSTALL_LOG"

echo ""; hline
echo -e "  ${BOLD}sharedd${NC} — установка агента ноды (web)"
hline; echo ""

# curl|bash: stdin — пайп, поэтому интерактивность ловим по любому потоку,
# а ответы читаем с /dev/tty — вопросы работают и в пайпе.
TTY=0; { [ -t 0 ] || [ -t 1 ] || [ -t 2 ]; } && TTY=1

# ── регистратор ──────────────────────────────────────────────────────────
REGISTRY_URL="${REGISTRY_URL:-}"
# При обновлении без параметров сохраняем уже выбранный URL регистратора,
# а не возвращаем ноду к демонстрационному значению по умолчанию.
if [ -z "$REGISTRY_URL" ] && [ -f "$ETC_DIR/node.toml" ]; then
    EXISTING_REGISTRY_URL="$(grep -E '^[[:space:]]*url[[:space:]]*=' "$ETC_DIR/node.toml" | head -n1 | sed -E 's/.*=[[:space:]]*"([^"]*)".*/\1/' || true)"
    if [ -n "$EXISTING_REGISTRY_URL" ]; then
        REGISTRY_URL="$EXISTING_REGISTRY_URL"
        say "найдена существующая установка — сохраняю URL регистратора: ${BOLD}${REGISTRY_URL}${NC}"
    fi
fi
if [ -z "$REGISTRY_URL" ]; then
    if [ "$TTY" -eq 1 ] && [ "$NONINTERACTIVE" -eq 0 ]; then
        read -r -p "  → URL регистратора [${REGISTRY_URL_DEFAULT}]: " ans </dev/tty || true
        REGISTRY_URL="${ans:-$REGISTRY_URL_DEFAULT}"
    else
        REGISTRY_URL="$REGISTRY_URL_DEFAULT"
    fi
fi
REGISTRY_URL="${REGISTRY_URL%/}"
# Нормализация: URL без схемы ("registrar.example.com") ломает Go-клиент
# агента — Post "registrar.../heartbeat": unsupported protocol scheme "".
case "$REGISTRY_URL" in
    https://*|http://*) ;;
    *://*) die "неподдерживаемая схема в URL регистратора: $REGISTRY_URL (нужен http:// или https://)" ;;
    *)  REGISTRY_URL="https://${REGISTRY_URL}"
        warn "URL регистратора без схемы — использую ${REGISTRY_URL}" ;;
esac

# ── имя ноды ─────────────────────────────────────────────────────────────
# Итоговый id: ИМЯ-<5 символов a-z0-9> (напр. ddproxy-6an4o). Установщик
# кладёт его в state-файл агента ($STATE_DIR/node_id) — агент видит готовый
# id и новый не генерирует. Без имени — пусто: агент сгенерирует случайный
# персистентный id сам, как раньше.
if [ -z "$NODE_NAME" ] && [ "$TTY" -eq 1 ] && [ "$NONINTERACTIVE" -eq 0 ]; then
    read -r -p "  → Имя ноды (итог: ИМЯ-xxxxx; пусто = случайный id): " ans </dev/tty || true
    NODE_NAME="${ans:-}"
    if [ -n "$NODE_NAME" ]; then
        printf '%s' "$NODE_NAME" | grep -qE '^[A-Za-z0-9]([A-Za-z0-9._-]{0,40}[A-Za-z0-9])?$' \
            || die "недопустимое имя '$NODE_NAME' — латиница/цифры/._- (до 42 символов, без ._- по краям)"
    fi
fi

# 5-символьный суффикс a-z0-9 (криптослучайный, /dev/urandom)
gen_suffix() {
    LC_ALL=C tr -dc 'a-z0-9' </dev/urandom 2>/dev/null | head -c 5 || true
}
NODE_ID=""
if [ -n "$NODE_NAME" ]; then
    sfx="$(gen_suffix)"
    [ "${#sfx}" -eq 5 ] || sfx="$(printf '%s%s' "$(date +%s%N)" "$$" | md5sum | tr -dc 'a-z0-9' | head -c 5 || true)"
    [ "${#sfx}" -eq 5 ] || die "не удалось сгенерировать суффикс имени"
    NODE_ID="${NODE_NAME}-${sfx}"
fi

# ── голый бинарник по ссылке ─────────────────────────────────────────────
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
case "$BINARY_URL" in
    http://*|https://*) ;;
    *) die "пропишите ссылку в BINARY_URL в начале скрипта" ;;
esac
say "скачиваю: ${BOLD}${BINARY_URL}${NC}"
file="$TMP/sharedd-node-agent"
if command -v curl &>/dev/null; then
    curl -fsSL --connect-timeout 15 -o "$file" "$BINARY_URL" || die "скачивание не удалось"
elif command -v wget &>/dev/null; then
    wget -q -O "$file" "$BINARY_URL" || die "скачивание не удалось"
else
    die "нужен curl или wget (apt install curl)"
fi
[ -s "$file" ] || die "скачался пустой файл — проверьте BINARY_URL"
install -m 0755 "$file" "$BIN_DEST"
ok "бинарник: ${BOLD}${BIN_DEST}${NC}"

# ── конфиг telemt: автоопределение ───────────────────────────────────────
superexpert_active() {  # флаг в настройках + файл на месте (семантика MTProxyL)
    [ -f "$TELEMT_MTPROXYL" ] && [ -f "$MTPROXYL_SETTINGS" ] \
        && grep -qE '^SUPEREXPERT_ENABLED="?true"?' "$MTPROXYL_SETTINGS" 2>/dev/null
}
mtproxyl_cli() { command -v mtproxyl &>/dev/null; }

ensure_superexpert_on() {
    if superexpert_active; then
        say "MTProxyL: режим супер-эксперта уже активен"; return 0
    fi
    if ! mtproxyl_cli; then
        if [ -f "$TELEMT_MTPROXYL" ]; then
            warn "CLI mtproxyl не найден — считаю, что $TELEMT_MTPROXYL уже источник конфига"
            return 0
        fi
        die "ни ванильного telemt ($TELEMT_CLASSIC), ни MTProxyL не найдено — сначала поставьте прокси"
    fi
    say "MTProxyL: включаю режим супер-эксперта..."
    # ASSUME_YES: read_line отвечает yes; файла нет — 'on' создаст его копией рабочего конфига
    MTPROXYL_ASSUME_YES=1 mtproxyl superexpert on >>"$INSTALL_LOG" 2>&1 \
        || die "'mtproxyl superexpert on' упал — см. $INSTALL_LOG"
    if superexpert_active \
        || mtproxyl superexpert status --json 2>/dev/null | grep -q '"active":true'; then
        ok "режим супер-эксперта активен"
    else
        die "superexpert не включился — проверьте: mtproxyl superexpert status"
    fi
}

TELEMT_CONFIG="${TELEMT_CONFIG:-}"   # пустая = автоопределение
if [ -z "$TELEMT_CONFIG" ]; then
    have_classic=0; have_superx=0
    [ -f "$TELEMT_CLASSIC" ] && have_classic=1
    [ -f "$TELEMT_MTPROXYL" ] && have_superx=1
    if [ "$have_classic" -eq 1 ] && [ "$have_superx" -eq 1 ]; then
        def=1; superexpert_active && def=2
        if [ "$TTY" -eq 1 ] && [ "$NONINTERACTIVE" -eq 0 ]; then
            echo -e "  ${BOLD}Найдены два конфига telemt — какой патчить?${NC}"
            echo -e "    ${BOLD}1${NC}) Классика: $TELEMT_CLASSIC"
            echo -e "    ${BOLD}2${NC}) MTProxyL superexpert: $TELEMT_MTPROXYL"
            read -r -p "  → Выбор [${def}]: " choice </dev/tty || true
            case "${choice:-$def}" in
                1) TELEMT_CONFIG="$TELEMT_CLASSIC" ;;
                2) TELEMT_CONFIG="$TELEMT_MTPROXYL" ;;
                *) die "ожидалось 1 или 2" ;;
            esac; echo ""
        elif [ "$NONINTERACTIVE" -eq 1 ]; then
            # автоматизация (--registry=...): спорную развилку решаем дефолтом
            case "$def" in
                1) TELEMT_CONFIG="$TELEMT_CLASSIC" ;;
                2) TELEMT_CONFIG="$TELEMT_MTPROXYL" ;;
            esac
            warn "найдены оба конфига telemt — неинтерактивно выбран: $TELEMT_CONFIG"
        else
            die "оба конфига на месте, а спросить не у кого (не-TTY) — оставьте один из них"
        fi
    elif [ "$have_classic" -eq 1 ]; then
        TELEMT_CONFIG="$TELEMT_CLASSIC"
    else
        TELEMT_CONFIG="$TELEMT_MTPROXYL"   # superexpert есть или MTProxyL стоит — его мир
    fi
fi
if [ "$TELEMT_CONFIG" = "$TELEMT_MTPROXYL" ]; then
    MTPROXYL_MODE=1
    ensure_superexpert_on
fi
[ -f "$TELEMT_CONFIG" ] || die "конфиг telemt не найден: $TELEMT_CONFIG — сначала поставьте telemt/MTProxyL"
say "конфиг telemt: ${BOLD}${TELEMT_CONFIG}${NC}"

# ── конфиг ноды + systemd ────────────────────────────────────────────────
install -d -m 0750 -o root -g root "$ETC_DIR"
install -d -m 0750 -o root -g root "$STATE_DIR"

# Имя ноды: кладём готовый id в state-файл агента ДО его первого запуска —
# resolveNodeID() читает существующий файл и ничего не перегенерирует.
if [ -n "$NODE_ID" ]; then
    ID_STATE_FILE="$STATE_DIR/node_id"
    if [ -s "$ID_STATE_FILE" ]; then
        OLD_ID="$(tr -d '[:space:]' <"$ID_STATE_FILE")"
        if [ "$OLD_ID" != "$NODE_ID" ]; then
            warn "нода уже имеет id '${OLD_ID}' — меняю на '${NODE_ID}'."
            warn "для регистратора это НОВАЯ нода (место в очереди обнулится)."
        fi
    fi
    printf '%s' "$NODE_ID" > "$ID_STATE_FILE"
    chmod 0600 "$ID_STATE_FILE"
    ok "имя ноды: ${BOLD}${NODE_ID}${NC}"
fi
if [ -f "$ETC_DIR/node.toml" ]; then
    # Конфиг уже есть — оставляем, но registry.url приводим к выбранному
    # (и заодно чиним старый битый url без схемы: unsupported protocol scheme).
    CUR_URL="$(grep -E '^[[:space:]]*url[[:space:]]*=' "$ETC_DIR/node.toml" | head -n1 | sed -E 's/.*=[[:space:]]*"([^"]*)".*/\1/' || true)"
    if [ "$CUR_URL" = "$REGISTRY_URL" ]; then
        say "$ETC_DIR/node.toml существует — оставляю как есть"
    else
        warn "registry.url в конфиге: '${CUR_URL:-<не найден>}' — обновляю на '${REGISTRY_URL}'"
        if grep -qE '^[[:space:]]*url[[:space:]]*=' "$ETC_DIR/node.toml"; then
            sed -i -E "0,/^[[:space:]]*url[[:space:]]*=.*/s||url = \"${REGISTRY_URL}\"|" "$ETC_DIR/node.toml"
        else
            printf '\n[registry]\nurl = "%s"\n' "$REGISTRY_URL" >> "$ETC_DIR/node.toml"
        fi
        ok "registry.url обновлён в $ETC_DIR/node.toml (остальное не тронуто)"
    fi
else
    cat > "$ETC_DIR/node.toml" <<TOML
# sharedd node agent (установлено $(date -Iseconds))
[registry]
url = "${REGISTRY_URL}"

[telemt]
config_path = "${TELEMT_CONFIG}"

[sync]
apply_to_telemt = true
TOML
    chmod 0640 "$ETC_DIR/node.toml"
    ok "конфиг записан: $ETC_DIR/node.toml"
fi

cat > "$UNIT_DEST" <<UNIT
[Unit]
Description=sharedd — агент HA-ноды (MTProto proxy pool)
After=network-online.target telemt.service
Wants=network-online.target

[Service]
ExecStart=${BIN_DEST} -config ${ETC_DIR}/node.toml
Restart=always
RestartSec=5
User=root
StateDirectory=sharedd

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable sharedd-node-agent.service >>"$INSTALL_LOG" 2>&1

# ── прокси: конвейер применения конфига выполняет УСТАНОВЩИК ────────────────
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

# Эффективный apply_to_telemt — из ФАКТИЧЕСКОГО node.toml (вдруг остался
# apply_to_telemt=false): тогда конфигом не управляем и прокси не трогаем.
APPLY_EFFECTIVE=1
if grep -qE '^[[:space:]]*apply_to_telemt[[:space:]]*=[[:space:]]*false' "$ETC_DIR/node.toml" 2>/dev/null; then
    APPLY_EFFECTIVE=0
fi

PROXY_UNIT=""
if [ "$APPLY_EFFECTIVE" -eq 1 ]; then
    [ "$MTPROXYL_MODE" -eq 0 ] && PROXY_UNIT="$(detect_proxy_unit || true)"
    [ -n "$PROXY_UNIT" ] && say "юнит прокси: ${BOLD}${PROXY_UNIT}${NC}"

    if ! grep -aq 'APPLY-ONCE:' "$BIN_DEST" 2>/dev/null; then
        # Старая сборка агента проигнорировала бы -apply-once и висела бы
        # демоном-призраком до timeout: конвейера нет — прокси НЕ трогаем,
        # конфиг применит агент фоново после запуска (с рестартом прокси).
        warn "скачанный бинарник старой сборки (без one-shot конвейера) — конфиг синхронно не применяю."
        warn "обновите бинарник по ссылке BINARY_URL в начале скрипта;"
        warn "либо конфиг применится сам через ~1 мин после старта демона."
    else
        say "применяю конфиг: стоп прокси → патч → старт → проверка по метрикам (до ~3 мин)..."
        set +e
        APPLY_OUT="$(timeout 180 "$BIN_DEST" -config "$ETC_DIR/node.toml" -apply-once 2>&1)"
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

# ── теперь агент ──────────────────────────────────────────────────────────
systemctl restart sharedd-node-agent.service
sleep 2
systemctl is-active --quiet sharedd-node-agent.service \
    || die "не стартовал: journalctl -u sharedd-node-agent -n 50 (хвост лога установки: $INSTALL_LOG)"
ok "sharedd-node-agent запущен"

sleep 3
if journalctl -u sharedd-node-agent --since "-30s" 2>/dev/null | grep -q register; then
    ok "нода регистрируется на ${REGISTRY_URL}"
else
    warn "регистрация не подтверждена за 30 сек — журнал: journalctl -u sharedd-node-agent -f"
fi

# ── финальная проверка: метрики должны отвечать ─────────────────────────────
# One-shot на шаге выше их уже дождался — тут обычно мгновенный успех; но если
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
    say "финальная проверка: жду ответа метрик прокси (${METRICS_URL}, до 120 с)..."
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
        ok "прокси работает, метрики отвечают ($METRICS_URL)"
    else
        warn "метрики не отвечают за 120 с — диагностика:"
        warn "  journalctl -u sharedd-node-agent -n 50 --no-pager"
        [ -n "$PROXY_UNIT" ] && warn "  journalctl -u $PROXY_UNIT -n 50 --no-pager"
        [ "$MTPROXYL_MODE" -eq 1 ] && warn "  mtproxyl status"
    fi
fi

echo ""; hline
echo -e "  ${GREEN}${BOLD}Готово${NC}"
hline
echo -e "  Регистратор:    ${BOLD}${REGISTRY_URL}${NC}"
[ -n "$NODE_ID" ] && echo -e "  Имя ноды:       ${BOLD}${NODE_ID}${NC}"
echo -e "  telemt:         ${BOLD}${TELEMT_CONFIG}${NC}"
echo -e "  Журнал:         journalctl -u sharedd-node-agent -f"
echo -e "  ${DIM}Пользователи/SNI приедут сами из регистратора и допишутся в telemt-конфиг.${NC}"
hline
