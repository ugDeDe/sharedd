#!/usr/bin/env bash
# Следующая версия релиза по последнему тегу vX.Y.Z / VX.Y.Z и коммитам после него.
# Правила (Conventional Commits): `type!:` или BREAKING CHANGE → major,
# `feat` → minor, остальное → patch. Нет тегов — отсчёт от 0.0.0.
#
#   scripts/next_version.sh          → 1.1.0
#   scripts/next_version.sh --dev    → 1.1.0-dev.abc1234 (коротким хэшем HEAD)
#   scripts/next_version.sh --last   → последняя версия без префикса (или 0.0.0)
set -euo pipefail

MODE="${1:-}"
case "$MODE" in ""|--dev|--last) ;; *) echo "usage: $0 [--dev|--last]" >&2; exit 2 ;; esac

tag=$(git tag --list 'v[0-9]*' 'V[0-9]*' | grep -E '^[vV][0-9]+\.[0-9]+\.[0-9]+$' \
    | sed -E 's/^([vV])(.*)$/\2 \1\2/' | sort -V -k1,1 | tail -n1 | cut -d' ' -f2 || true)
last="${tag#[vV]}"
[ -n "$tag" ] || last="0.0.0"

if [ "$MODE" = "--last" ]; then echo "$last"; exit 0; fi

IFS=. read -r major minor patch <<<"$last"
if [ -n "$tag" ]; then range="$tag..HEAD"; else range="HEAD"; fi
msgs=$(git log --format='%s%n%b%n--END--' "$range" 2>/dev/null || true)

bump="patch"
if grep -qE '^[a-z]+(\([^)]*\))?!:|^BREAKING[ -]CHANGE' <<<"$msgs"; then
    bump=major
elif grep -qE '^feat(\([^)]*\))?:' <<<"$msgs"; then
    bump=minor
fi
case "$bump" in
    major) major=$((major + 1)); minor=0; patch=0 ;;
    minor) minor=$((minor + 1)); patch=0 ;;
    patch) patch=$((patch + 1)) ;;
esac
next="$major.$minor.$patch"

if [ "$MODE" = "--dev" ]; then
    echo "$next-dev.$(git rev-parse --short=7 HEAD)"
else
    echo "$next"
fi
