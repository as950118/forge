#!/usr/bin/env bash
# Static documentation check (links, forge subcommands/flags that really exist,
# invisible characters, local-only paths). See scripts/docs_check.py for the
# exact rules and the limits: it does NOT run the documented commands or check
# their output. Use `make docs-check` (builds the binary first), or:
#
#   FORGE_BIN=dist/forge bash scripts/docs-check.sh [--root DIR]
#
# Reads and writes nothing outside the repository; runs only `forge ... --help`.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
command -v python3 >/dev/null 2>&1 || { echo "docs-check: python3 가 필요합니다" >&2; exit 2; }
exec python3 -I "$HERE/docs_check.py" "$@"
