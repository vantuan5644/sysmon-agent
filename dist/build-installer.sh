#!/usr/bin/env bash
# dist/build-installer.sh — build the release exe, then the NSIS installer.
#
# Cross-compilable: makensis runs natively on Linux/macOS/Windows, so you can
# produce a Windows Setup .exe from any of them without wine.
#
# Usage:
#   ./dist/build-installer.sh                # version auto-detected from git, else 0.1.0
#   ./dist/build-installer.sh 1.2.3          # explicit version
#
# Requires: Go 1.22+, makensis (NSIS), and (optional) ImageMagick for the icon.
# Output: dist/out/SysmonAgent-Setup-<VERSION>.exe
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$ROOT"

VERSION="${1:-}"
if [[ -z "$VERSION" ]]; then
    if [[ -d "$ROOT/.git" ]] && command -v git >/dev/null 2>&1; then
        VERSION="$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || true)"
    fi
    [[ -z "$VERSION" ]] && VERSION="0.1.0"
fi

echo "==> Building release exe"
"$SCRIPT_DIR/build-windows.sh" "$VERSION"

if ! command -v makensis >/dev/null 2>&1; then
    echo "==> makensis not found; cannot build the installer." >&2
    echo "    Install NSIS:" >&2
    echo "      Debian/Ubuntu: sudo apt install nsis" >&2
    echo "      macOS:         brew install nsis" >&2
    echo "      Arch:          paru -S nsis   (AUR)" >&2
    echo "      Windows:       https://nsis.sourceforge.io/Download" >&2
    exit 1
fi

echo "==> Building NSIS installer (version $VERSION)"
( cd "$SCRIPT_DIR" && makensis -DVERSION="$VERSION" -NOCD installer.nsi )

OUT="$ROOT/dist/out/SysmonAgent-Setup-$VERSION.exe"
echo "==> Built: $OUT"
echo "    size:  $(du -h "$OUT" | awk '{print $1}')"
echo "    Test on a Windows host: double-click to install, then open"
echo "    http://localhost:9099/  (or your PC's LAN IP from a phone)."
