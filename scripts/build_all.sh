#!/usr/bin/env bash
# Сборка обоих компонентов.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

"$HERE/build_registry.sh"
"$HERE/build_node.sh"
