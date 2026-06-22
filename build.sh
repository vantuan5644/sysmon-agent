#!/usr/bin/env bash
set -euo pipefail

# ============================================================
# Build sysmon-agent — a single self-contained Go binary that
# serves a system-monitoring PWA dashboard.
#
# Usage:
#   ./build.sh              # build for the current OS/arch
#   ./build.sh linux        # cross-compile (linux|windows|darwin)
#   GOOS=windows GOARCH=amd64 ./build.sh
#
# Output:
#   ./sysmon-agent          (Linux / macOS)
#   ./sysmon-agent.exe      (Windows)
# ============================================================

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

GOOS_TARGET="${1:-${GOOS:-}}"
OUTPUT="sysmon-agent"
case "${GOOS_TARGET}" in
    windows) OUTPUT="sysmon-agent.exe" ;;
esac

if [[ -n "${GOOS_TARGET}" && "${GOOS_TARGET}" != "${GOOS:-}" ]]; then
    export GOOS="${GOOS_TARGET}"
fi

GREEN='\033[0;32m'
NC='\033[0m'

# Fall back to a writable temp cache when the default Go cache is not writable
# (common in read-only mounts or restricted containers).
if [[ -z "${GOCACHE:-}" ]]; then
    cache="$(go env GOCACHE 2>/dev/null || true)"
    if [[ -n "$cache" ]]; then
        probe="$cache/.sysmon-build-write-test.$$"
        if ! { mkdir -p "$cache" 2>/dev/null && : > "$probe"; } 2>/dev/null; then
            export GOCACHE="${TMPDIR:-/tmp}/go-build-cache"
            mkdir -p "$GOCACHE"
        fi
        rm -f "$probe" 2>/dev/null || true
    fi
fi

echo "Building ${OUTPUT} ($(go env GOOS)/$(go env GOARCH))..."
go build -trimpath -o "$OUTPUT" .
printf "${GREEN}ok:${NC} %s\n" "$OUTPUT"
