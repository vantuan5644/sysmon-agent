#!/usr/bin/env bash
#
# install-gpu-hotspot.sh - install the optional NVIDIA GPU hotspot reader (Linux).
#
# nvidia-smi does not report the GPU hotspot temperature on Linux. The
# sysmon-gpu-hotspot helper (cmd/gpu-hotspot) reads it from a GPU register
# through a read-only /dev/mem mapping, as root, and publishes
# /run/sysmon-gpu-hotspot/readings.json. The agent itself stays unprivileged: it
# only reads that file, and shows the value on the GPU card while it is fresh.
#
# Supported: RTX 3090 (10de:2204) and RTX 4090 (10de:2684). NVIDIA does not
# document the register, so compare the reading against another tool before
# relying on it. Windows needs none of this: the LibreHardwareMonitor bridge
# already reports the GPU hotspot there.
#
# Dry-run by default; nothing changes without --apply.
#
#   go build -trimpath -o sysmon-gpu-hotspot ./cmd/gpu-hotspot
#   ./install-gpu-hotspot.sh                            # checks + plan only
#   sudo ./install-gpu-hotspot.sh --apply               # install + start the service
#   sudo ./install-gpu-hotspot.sh --uninstall --apply   # remove it again
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
UNIT_NAME=sysmon-gpu-hotspot.service
UNIT_SOURCE="$SCRIPT_DIR/deploy/$UNIT_NAME"
UNIT_PATH="/etc/systemd/system/$UNIT_NAME"
INSTALL_PATH=/usr/local/bin/sysmon-gpu-hotspot
READINGS=/run/sysmon-gpu-hotspot/readings.json
LIMINE_DEFAULTS=/etc/default/limine
LIMINE_MARKER='# NVIDIA GPU hotspot register reader'
# NVIDIA (vendor 10de) device IDs the helper reads. Keep in sync with
# supportedDevice in internal/gpuhotspot/reader_linux.go.
SUPPORTED_IDS=(2204 2684)

HELPER_BINARY="$SCRIPT_DIR/sysmon-gpu-hotspot"
APPLY=0
UNINSTALL=0
REGISTER_ACCESS=0

usage() {
    cat <<'EOF'
install-gpu-hotspot.sh - install the optional NVIDIA GPU hotspot reader (Linux).

Installs a small root service that reads the RTX 3090 / RTX 4090 hotspot sensor
and publishes it for the agent. Dry-run by default.

Usage:
  go build -trimpath -o sysmon-gpu-hotspot ./cmd/gpu-hotspot
  ./install-gpu-hotspot.sh [options]

Options:
  --apply                    make the changes (run with sudo)
  --binary PATH              helper to install (default: ./sysmon-gpu-hotspot)
  --enable-register-access   Limine only: append iomem=relaxed to the default
                             kernel command line and run limine-update. Reboot
                             separately. Other bootloaders: see the error text.
  --uninstall                stop and remove the service and helper instead
  -h, --help                 show this help
EOF
}

note() { echo "    $*"; }
warn() { echo "warning: $*" >&2; }
die() { echo "error: $*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
    case "$1" in
        --apply)                  APPLY=1 ;;
        --uninstall)              UNINSTALL=1 ;;
        --enable-register-access) REGISTER_ACCESS=1 ;;
        --binary)                 HELPER_BINARY="${2:?--binary needs a path}"; shift ;;
        -h|--help)                usage; exit 0 ;;
        *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
    esac
    shift
done

[[ "$(uname -s)" == Linux && "$(uname -m)" == x86_64 ]] || die "requires Linux x86_64"
if (( UNINSTALL && REGISTER_ACCESS )); then
    die "--uninstall and --enable-register-access do not combine"
fi

require_root() {
    [[ $EUID -eq 0 ]] || die "--apply changes system files; rerun with sudo"
}

finish_dry_run() {
    if (( ! APPLY )); then
        echo "Dry-run complete; rerun with sudo and --apply to make these changes."
        exit 0
    fi
}

# Prints one line per supported GPU. Always returns 0: it runs inside $(...)
# under set -e, where a trailing failed test would abort the script.
find_supported_gpus() {
    local dev vendor device id
    for dev in /sys/bus/pci/devices/*; do
        [[ -r "$dev/vendor" && -r "$dev/device" ]] || continue
        vendor="$(<"$dev/vendor")"
        device="$(<"$dev/device")"
        [[ "$vendor" == 0x10de ]] || continue
        for id in "${SUPPORTED_IDS[@]}"; do
            if [[ "$device" == "0x$id" ]]; then
                echo "${dev##*/} (10de:$id)"
            fi
        done
    done
    return 0
}

# --- uninstall ---------------------------------------------------------------
if (( UNINSTALL )); then
    echo "==> Plan (uninstall)"
    note "stop and disable $UNIT_NAME"
    note "remove $UNIT_PATH and $INSTALL_PATH"
    if [[ -f $LIMINE_DEFAULTS ]] && grep -qF "$LIMINE_MARKER" "$LIMINE_DEFAULTS"; then
        note "the kernel command line is left alone: delete the '$LIMINE_MARKER'"
        note "entry from $LIMINE_DEFAULTS by hand, run limine-update, and reboot"
    fi
    finish_dry_run
    require_root
    if [[ -f $UNIT_PATH ]]; then
        systemctl disable --now "$UNIT_NAME"
    fi
    rm -f -- "$UNIT_PATH" "$INSTALL_PATH"
    systemctl daemon-reload
    echo "ok: removed. The dashboard stops showing the hotspot within a few seconds."
    exit 0
fi

# --- install -----------------------------------------------------------------
[[ -f "$UNIT_SOURCE" ]] || die "missing $UNIT_SOURCE"
if [[ ! -f "$HELPER_BINARY" ]]; then
    die "no helper at $HELPER_BINARY. Build it first:
  go build -trimpath -o sysmon-gpu-hotspot ./cmd/gpu-hotspot
or pass --binary PATH."
fi

echo "==> Checks"
gpus="$(find_supported_gpus)"
if [[ -n "$gpus" ]]; then
    while IFS= read -r line; do note "supported GPU: $line"; done <<<"$gpus"
else
    warn "no RTX 3090 / RTX 4090 found; the helper would publish an empty device list"
fi
lockdown=/sys/kernel/security/lockdown
if [[ -r $lockdown ]] && ! grep -qF '[none]' "$lockdown"; then
    warn "kernel lockdown is active ($(<"$lockdown")). It blocks /dev/mem outright,"
    warn "usually because Secure Boot is on; this script does not change that."
fi
if grep -qw 'iomem=relaxed' /proc/cmdline; then
    note "iomem=relaxed is active on the running kernel"
else
    note "iomem=relaxed is not on the running kernel command line. Kernels built with"
    note "CONFIG_IO_STRICT_DEVMEM refuse /dev/mem reads of memory the nvidia driver owns;"
    note "if the reading then says 'operation not permitted', add iomem=relaxed"
    note "(--enable-register-access does it on Limine)."
fi

boot_change=0
if (( REGISTER_ACCESS )); then
    if [[ ! -f $LIMINE_DEFAULTS ]] || ! command -v limine-update >/dev/null; then
        cat >&2 <<EOF
error: --enable-register-access edits the kernel command line only for Limine
($LIMINE_DEFAULTS plus limine-update). Add iomem=relaxed yourself instead, e.g.
  GRUB:          append it to GRUB_CMDLINE_LINUX_DEFAULT in /etc/default/grub, then
                 sudo update-grub   (or: sudo grub-mkconfig -o /boot/grub/grub.cfg)
  Fedora/RHEL:   sudo grubby --update-kernel=ALL --args=iomem=relaxed
  systemd-boot:  append it to the options line of your loader entry
Then reboot and rerun this script without --enable-register-access.
EOF
        exit 1
    fi
    if grep -Eq '^[^#]*iomem=strict' "$LIMINE_DEFAULTS"; then
        die "$LIMINE_DEFAULTS sets iomem=strict explicitly; resolve that by hand first"
    fi
    if grep -Eq '^[^#]*iomem=relaxed' "$LIMINE_DEFAULTS"; then
        note "iomem=relaxed is already in $LIMINE_DEFAULTS"
    else
        boot_change=1
    fi
fi

echo "==> Plan"
note "install $HELPER_BINARY -> $INSTALL_PATH (root-owned)"
note "install $UNIT_SOURCE -> $UNIT_PATH"
note "enable and (re)start $UNIT_NAME; readings: $READINGS"
if (( boot_change )); then
    note "back up $LIMINE_DEFAULTS, append iomem=relaxed, run limine-update"
    note "iomem=relaxed relaxes /dev/mem protection for every driver-owned range"
    note "system-wide, not just this GPU. Rebooting is left to you."
fi
finish_dry_run
require_root

install -o root -g root -m 0755 "$HELPER_BINARY" "$INSTALL_PATH"
install -o root -g root -m 0644 "$UNIT_SOURCE" "$UNIT_PATH"
if (( boot_change )); then
    backup="$LIMINE_DEFAULTS.before-gpu-hotspot.$(date -u +%Y%m%dT%H%M%SZ)"
    cp -p -- "$LIMINE_DEFAULTS" "$backup"
    printf '\n%s\nKERNEL_CMDLINE[default]+=" iomem=relaxed"\n' "$LIMINE_MARKER" >> "$LIMINE_DEFAULTS"
    if ! limine-update; then
        cp -p -- "$backup" "$LIMINE_DEFAULTS"
        die "limine-update failed; restored $backup. Inspect the boot entries before rebooting."
    fi
    note "boot configuration backup: $backup"
fi
systemctl daemon-reload
systemctl enable "$UNIT_NAME"
# restart, not start: on a reinstall the old helper would keep running.
systemctl restart "$UNIT_NAME"

for _ in 1 2 3 4 5; do
    [[ -s $READINGS ]] && break
    sleep 1
done
if [[ -s $READINGS ]]; then
    echo "==> $READINGS"
    cat "$READINGS"
    echo
else
    warn "no readings yet; inspect: journalctl -u $UNIT_NAME -n 50 --no-pager"
fi
if (( REGISTER_ACCESS )) && ! grep -qw 'iomem=relaxed' /proc/cmdline; then
    echo "Reboot when convenient to activate iomem=relaxed; until then the reading may be unavailable."
fi
echo "ok: an agent built from this checkout reads it on its next slow-lane pass; no restart needed."
