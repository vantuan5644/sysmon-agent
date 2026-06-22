#!/usr/bin/env bash
# dist/build-windows.sh — produce a release Windows .exe for sysmon-agent.
#
# Cross-buildable from Linux/macOS/Windows. Stamps the version, strips the
# symbol table, and embeds a Win32 version resource + icon (so the exe shows
# version + icon in Explorer / Properties). Output: dist/out/sysmon-agent.exe.
#
# Usage:
#   ./dist/build-windows.sh                # version auto-detected from git, else 0.1.0
#   ./dist/build-windows.sh 1.2.3          # explicit version
#   VERSION=1.2.3 ./dist/build-windows.sh
#
# Requires: Go 1.22+. Optional: ImageMagick (regenerates dist/packaging/sysmon.ico
# from static/icon-512.png) and goversioninfo (auto-installed to a temp dir for
# the version-resource syso if `go` can reach the module proxy).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$ROOT"

VERSION="${1:-${VERSION:-}}"
if [[ -z "$VERSION" ]]; then
    if [[ -d "$ROOT/.git" ]] && command -v git >/dev/null 2>&1; then
        VERSION="$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || true)"
    fi
    [[ -z "$VERSION" ]] && VERSION="0.1.0"
fi

OUT_DIR="$ROOT/dist/out"
mkdir -p "$OUT_DIR"
OUT="$OUT_DIR/sysmon-agent.exe"

# Split "1.2.3" / "1.2.3-4-gabc" into major.minor.patch for the version resource.
IFS='.-' read -r MAJOR MINOR PATCH _rest <<< "$VERSION"
MAJOR="${MAJOR:-0}"; MINOR="${MINOR:-0}"; PATCH="${PATCH:-0}"

echo "==> Building sysmon-agent $VERSION for windows/amd64"

# 1) (Re)generate the icon from the PWA asset when ImageMagick is present.
PKG_DIR="$ROOT/dist/packaging"
if command -v magick >/dev/null 2>&1; then
    magick "$ROOT/static/icon-512.png" \
        \( -clone 0 -resize 256x256 \) \
        \( -clone 0 -resize 128x128 \) \
        \( -clone 0 -resize 64x64 \) \
        \( -clone 0 -resize 48x48 \) \
        \( -clone 0 -resize 32x32 \) \
        \( -clone 0 -resize 16x16 \) \
        -delete 0 "$PKG_DIR/sysmon.ico" 2>/dev/null || true
elif command -v convert >/dev/null 2>&1; then
    convert "$ROOT/static/icon-512.png" \
        \( -clone 0 -resize 256x256 \) \
        \( -clone 0 -resize 128x128 \) \
        \( -clone 0 -resize 64x64 \) \
        \( -clone 0 -resize 48x48 \) \
        \( -clone 0 -resize 32x32 \) \
        \( -clone 0 -resize 16x16 \) \
        -delete 0 "$PKG_DIR/sysmon.ico" 2>/dev/null || true
fi

# 2) Generate the Win32 version resource + icon syso via goversioninfo, when
#    available. This is build-time only (does not add a go.mod dependency);
#    if it can't be reached, the build proceeds without the resource.
GVI="$(command -v goversioninfo 2>/dev/null || true)"
if [[ -z "$GVI" ]]; then
    TMP_GOBIN="$(mktemp -d)"
    if GOBIN="$TMP_GOBIN" go install github.com/josephspurrier/goversioninfo/cmd/goversioninfo@latest >/dev/null 2>&1; then
        GVI="$TMP_GOBIN/goversioninfo"
    fi
fi
if [[ -n "$GVI" ]] && [[ -f "$PKG_DIR/version.json" ]]; then
    ( cd "$PKG_DIR" && "$GVI" -major "$MAJOR" -minor "$MINOR" -patch "$PATCH" \
        -product-version "$VERSION.0" -ver "$VERSION.0" \
        -icon "$PKG_DIR/sysmon.ico" -o "$PKG_DIR/resource.syso" 2>/dev/null ) || true
    [[ -f "$PKG_DIR/resource.syso" ]] && cp "$PKG_DIR/resource.syso" "$ROOT/resource.syso" && SYSO_FLAG=1
fi

# 3) Cross-compile. -s -w strips symbols for a smaller binary; the version is
#    injected into the main.version var. No -H=windowsgui: the binary is a
#    Windows service and a console diagnostic tool, so keep the console subsys.
LDFLAGS="-s -w -X main.version=$VERSION"
if GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="$LDFLAGS" -o "$OUT" .; then
    :
else
    # Resource syso occasionally breaks a clean build on some hosts; retry without it.
    rm -f "$ROOT/resource.syso"
    GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="$LDFLAGS" -o "$OUT" .
fi
rm -f "$ROOT/resource.syso"

echo "==> Built: $OUT"
echo "    version: $VERSION"
echo "    size:    $(du -h "$OUT" | awk '{print $1}') ($(stat -c%s "$OUT") bytes)"
echo "    verify on a Windows host (or via wine):"
echo "      $OUT -version"
