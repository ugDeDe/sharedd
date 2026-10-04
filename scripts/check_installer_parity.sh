#!/usr/bin/env bash
# Парность установщиков ноды: критичные функции конфиг-конвейера обязаны быть
# БАЙТ-В-БАЙТ одинаковыми в интерактивном и web-варианте. Расползание копий —
# классический источник «на тестовом VPS работало»: правка одного экземпляра
# без второго ловится этой проверкой (CI, job scripts).
#
# Интерактивно различающиеся части (prompts, read_registry_token и т.п.)
# сюда сознательно не входят.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
A="$ROOT/scripts/install_node.sh"
B="$ROOT/scripts/install_node_web.sh"

extract() { # extract <file> <fn>
    awk -v fn="$2" '
        $0 ~ "^"fn"\\(\\)" { f=1 }
        f { print }
        f && /^\}/ { exit }
    ' "$1"
}

status=0
for fn in toml_escape update_registry_config detect_proxy_unit rescue_proxy; do
    if diff -q <(extract "$A" "$fn") <(extract "$B" "$fn") >/dev/null; then
        echo "parity ok: $fn"
    else
        echo "DRIFT: ${fn}() differs between install_node.sh and install_node_web.sh"
        diff <(extract "$A" "$fn") <(extract "$B" "$fn") | head -20 || true
        status=1
    fi
done

exit $status
