#!/usr/bin/env bash
# Пересборка веб-интерфейса: ui/ -> registry/*.html
#
# Нужен только если правите ui/. Готовые страницы лежат в репозитории,
# поэтому обычная сборка бинарника от этого скрипта не зависит:
#   cd registry && go build .
#
# Node.js не требуется — esbuild подключён как Go-библиотека.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
echo "==> сборка интерфейса из ui/"
cd "$ROOT/tools/uigen"
go run . -root "$ROOT" "$@"
