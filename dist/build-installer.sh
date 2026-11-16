#!/usr/bin/env bash
# dist/build-installer.sh — build the release exe, the Linux binaries, then the
# NSIS installer and the checksum manifest that covers all of them.
#
# Cross-compilable: makensis runs natively on Linux/macOS/Windows, so you can
# produce a Windows Setup .exe from any of them without wine.
#
# Usage:
#   ./dist/build-installer.sh                # version auto-detected from git, else 0.1.0
#   ./dist/build-installer.sh 1.2.3          # explicit version
#
# Requires: Go 1.22+, makensis (NSIS), and (optional) ImageMagick for the icon.
# Output: dist/out/SysmonAgent-Setup-<VERSION>.exe, sysmon-agent.exe,
#         sysmon-agent-linux-amd64, sysmon-agent-linux-arm64, SHA256SUMS.txt
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

# The Linux root updater (`sudo sysmon-agent -update`) installs
# sysmon-agent-linux-<GOARCH> from the latest release, after checking it
# against SHA256SUMS.txt and against its own -version. Static, so one binary
# runs on any distribution.
LINUX_ARCHES=(amd64 arm64)
for arch in "${LINUX_ARCHES[@]}"; do
    echo "==> Building sysmon-agent $VERSION for linux/$arch"
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath \
        -ldflags="-s -w -X main.version=$VERSION" \
        -o "$ROOT/dist/out/sysmon-agent-linux-$arch" .
done

MAKENSIS="$(command -v makensis 2>/dev/null || true)"
if [[ -z "$MAKENSIS" ]]; then
    # The Windows NSIS installer does not put makensis on PATH, so a Git Bash
    # release build fails here even though NSIS is installed. Look in the
    # standard install locations before giving up.
    # Note: no ${PROGRAMFILES(X86)} here — parentheses are not legal in a bash
    # variable name, so referencing that environment variable is a parse error.
    for candidate in \
        "/c/Program Files (x86)/NSIS/makensis.exe" \
        "/c/Program Files/NSIS/makensis.exe" \
        "${PROGRAMFILES:-/c/Program Files}/NSIS/makensis.exe"; do
        if [[ -x "$candidate" ]]; then
            MAKENSIS="$candidate"
            echo "==> Using NSIS at: $MAKENSIS"
            break
        fi
    done
fi
if [[ -z "$MAKENSIS" ]]; then
    echo "==> makensis not found; cannot build the installer." >&2
    echo "    Install NSIS:" >&2
    echo "      Debian/Ubuntu: sudo apt install nsis" >&2
    echo "      macOS:         brew install nsis" >&2
    echo "      Arch:          paru -S nsis   (AUR)" >&2
    echo "      Windows:       https://nsis.sourceforge.io/Download" >&2
    echo "                     (installs to Program Files without touching PATH)" >&2
    exit 1
fi

echo "==> Building NSIS installer (version $VERSION)"
( cd "$SCRIPT_DIR" && "$MAKENSIS" -DVERSION="$VERSION" -NOCD installer.nsi )

OUT="$ROOT/dist/out/SysmonAgent-Setup-$VERSION.exe"

# --- checksum manifest -------------------------------------------------------
# SHA256SUMS.txt is a REQUIRED release asset, not a nicety. Both update engines
# (the agent's in-dashboard self-update in update.go, and
# install-windows.ps1 -Action Update) download it and verify the binary against
# it before executing or swapping it in. The agent service runs as LocalSystem,
# so an unverified swap would be a SYSTEM-level RCE vector — both engines refuse
# to proceed when this asset is missing from the release.
echo "==> Writing SHA256SUMS.txt"
SUMS="$ROOT/dist/out/SHA256SUMS.txt"
ASSETS=("sysmon-agent.exe" "SysmonAgent-Setup-$VERSION.exe")
for arch in "${LINUX_ARCHES[@]}"; do
    ASSETS+=("sysmon-agent-linux-$arch")
done
(
    # Run from the output dir so the manifest carries bare file names: both
    # engines match the release asset name exactly, with no directory part.
    cd "$ROOT/dist/out"
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "${ASSETS[@]}"
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "${ASSETS[@]}"
    elif command -v certutil >/dev/null 2>&1; then
        # Windows fallback: reshape certutil's multi-line output into the
        # coreutils "<hex>  <name>" form the engines parse.
        for f in "${ASSETS[@]}"; do
            hash="$(certutil -hashfile "$f" SHA256 | sed -n '2p' | tr -d ' \r')"
            printf '%s  %s\n' "$hash" "$f"
        done
    else
        echo "ERROR: no sha256 tool found (need sha256sum, shasum, or certutil)." >&2
        echo "       Refusing to build a release without a checksum manifest." >&2
        exit 1
    fi
) > "$SUMS"

# Fail loudly rather than shipping an empty or short manifest.
if [[ "$(grep -c . "$SUMS")" -ne "${#ASSETS[@]}" ]]; then
    echo "ERROR: $SUMS does not contain exactly ${#ASSETS[@]} entries:" >&2
    cat "$SUMS" >&2
    exit 1
fi

echo "==> Built: $OUT"
echo "    size:  $(du -h "$OUT" | awk '{print $1}')"
echo "    sums:  $SUMS"
sed 's/^/           /' "$SUMS"
echo
echo "    Test on a Windows host: double-click to install, then open"
echo "    http://localhost:9099/  (from a phone: https://<PC's LAN IP>:9099/ and the PIN the install log ends with)."
echo
echo "    Publish EVERY asset - the update engines refuse a release without"
echo "    SHA256SUMS.txt, and Linux hosts update from the linux binaries:"
echo "      gh release create v$VERSION \\"
echo "        \"$ROOT/dist/out/sysmon-agent.exe\" \\"
for arch in "${LINUX_ARCHES[@]}"; do
    echo "        \"$ROOT/dist/out/sysmon-agent-linux-$arch\" \\"
done
echo "        \"$OUT\" \\"
echo "        \"$SUMS\""
