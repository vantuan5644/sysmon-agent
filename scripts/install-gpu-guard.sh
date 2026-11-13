#!/usr/bin/env bash
#
# install-gpu-guard.sh - install the GPU overheat guard's root helper (Linux).
#
# The agent runs unprivileged, so on Linux it can neither lower the GPU power
# limit nor end other users' processes. The sysmon-gpu-guard helper
# (cmd/gpu-guard) runs the guard as root instead. It reads the NVIDIA core
# temperature from nvidia-smi, and the hotspot from sysmon-gpu-hotspot when that
# helper is installed (install-gpu-hotspot.sh), and writes
# /run/sysmon-gpu-guard/state.json. The agent shows that state on the dashboard
# and raises desktop notifications from it.
#
# warn (the default) raises the alarms and logs what act would do. act also
# lowers the power limit to the card's minimum at the critical hotspot, then
# ends GPU apps if it stays critical. See "GPU overheat guard" in README.md.
#
# Dry-run by default; nothing changes without --apply.
#
#   GOOS=linux GOARCH=amd64 go build -trimpath -o sysmon-gpu-guard ./cmd/gpu-guard
#   scripts/install-gpu-guard.sh                            # plan only
#   sudo scripts/install-gpu-guard.sh --mode warn --apply   # install + start
#   sudo scripts/install-gpu-guard.sh --uninstall --apply   # remove it again
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_dir="$(cd -- "$script_dir/.." && pwd)"
apply=false
uninstall=false
helper_binary="$repo_dir/sysmon-gpu-guard"
mode=""
unit_name=sysmon-gpu-guard.service
unit_source="$script_dir/systemd/$unit_name"
unit_path="/etc/systemd/system/$unit_name"
install_path=/usr/local/bin/sysmon-gpu-guard
env_file=/etc/default/sysmon-gpu-guard
# While the guard holds the power limit lowered, the limit to put back is here.
record=/run/sysmon-gpu-guard/power-limit

usage() {
    cat <<'EOF'
Usage: install-gpu-guard.sh [--binary PATH] [--mode warn|act] [--apply]
       install-gpu-guard.sh --uninstall [--apply]
Defaults to dry-run. Installs the GPU overheat guard as a root service.

  --binary PATH  helper to install (default: sysmon-gpu-guard in the repo root)
  --mode MODE    warn raises the alarms and logs what act would do; act also
                 lowers the power limit at the critical hotspot and then ends
                 GPU apps. Kept in /etc/default/sysmon-gpu-guard; without
                 --mode an existing file is left alone and a new one starts
                 in warn.
  --uninstall    stop and remove the service, the helper and its settings
  --apply        make the changes (run with sudo)

The hotspot comes from sysmon-gpu-hotspot.service (install-gpu-hotspot.sh);
without it the guard judges the core temperature.
Build first: GOOS=linux GOARCH=amd64 go build -trimpath -o sysmon-gpu-guard ./cmd/gpu-guard
EOF
}

while (($#)); do
    case "$1" in
        --apply) apply=true ;;
        --uninstall) uninstall=true ;;
        --binary) [[ $# -ge 2 ]] || { usage; exit 2; }; helper_binary="$2"; shift ;;
        --mode) [[ $# -ge 2 ]] || { usage; exit 2; }; mode="$2"; shift ;;
        --help|-h) usage; exit 0 ;;
        *) usage; exit 2 ;;
    esac
    shift
done
[[ "$(uname -s)" == Linux ]] || { echo "Requires Linux" >&2; exit 1; }

if $uninstall; then
    echo "Stop and disable $unit_name"
    echo "Remove $unit_path, $install_path and $env_file"
    if [[ -f "$record" ]]; then
        echo "An episode holds the power limit lowered; put back $(cat "$record") W with nvidia-smi"
    fi
    if ! $apply; then echo "Dry-run complete; use --apply to make these changes"; exit 0; fi
    [[ $EUID -eq 0 ]] || { echo "Run the apply command with sudo" >&2; exit 1; }
    systemctl disable --now "$unit_name" 2>/dev/null || true
    if [[ -f "$record" ]]; then
        watts="$(tr -d '[:space:]' < "$record")"
        if [[ "$watts" =~ ^[0-9]+([.][0-9]+)?$ ]] && command -v nvidia-smi >/dev/null; then
            nvidia-smi -pl "$watts" || echo "Could not put back $watts W; set it with nvidia-smi -pl" >&2
        fi
    fi
    rm -f -- "$unit_path" "$install_path" "$env_file"
    rm -rf -- /run/sysmon-gpu-guard
    systemctl daemon-reload
    echo "Removed. The agent shows no guard state once its file is gone."
    exit 0
fi

[[ "$(uname -m)" == x86_64 ]] || { echo "Requires Linux x86_64" >&2; exit 1; }
[[ -f "$helper_binary" ]] || { echo "No helper at $helper_binary; build it from the repo root with: GOOS=linux GOARCH=amd64 go build -trimpath -o sysmon-gpu-guard ./cmd/gpu-guard" >&2; exit 1; }
case "$mode" in
    ""|warn|act) ;;
    *) echo "--mode must be warn or act" >&2; exit 2 ;;
esac
[[ -f "$unit_source" ]] || { echo "Guard unit is missing: $unit_source" >&2; exit 1; }
command -v nvidia-smi >/dev/null || { echo "nvidia-smi is missing: nothing to guard" >&2; exit 1; }

echo "Install helper: $helper_binary -> $install_path (root-owned)"
echo "Install unit: $unit_source -> $unit_path"
if [[ -n "$mode" ]]; then
    echo "Write $env_file: SYSMON_GPU_GUARD=$mode"
elif [[ -f "$env_file" ]]; then
    echo "Keep $env_file: $(grep -E '^SYSMON_GPU_GUARD=' "$env_file" || echo 'no mode line, so warn')"
else
    echo "Write $env_file: SYSMON_GPU_GUARD=warn"
fi
if ! systemctl is-active --quiet sysmon-gpu-hotspot.service; then
    echo "Note: sysmon-gpu-hotspot.service is not running, so the guard judges the core temperature (critical at 88 C)"
fi
echo "Enable and (re)start $unit_name; state: /run/sysmon-gpu-guard/state.json"
if ! $apply; then echo "Dry-run complete; use --apply to make these changes"; exit 0; fi
[[ $EUID -eq 0 ]] || { echo "Run the apply command with sudo" >&2; exit 1; }

install -o root -g root -m 0755 "$helper_binary" "$install_path"
install -o root -g root -m 0644 "$unit_source" "$unit_path"
if [[ -n "$mode" || ! -f "$env_file" ]]; then
    printf '# sysmon-gpu-guard: warn or act; thresholds SYSMON_GPU_HOTSPOT_WARN, SYSMON_GPU_HOTSPOT_CRIT,\n# SYSMON_GPU_DELTA_WARN and SYSMON_GPU_GUARD_SPARE (see sysmon-gpu-guard -h)\nSYSMON_GPU_GUARD=%s\n' "${mode:-warn}" > "$env_file.tmp"
    chmod 0644 "$env_file.tmp"
    mv -f "$env_file.tmp" "$env_file"
fi
systemctl daemon-reload
systemctl enable "$unit_name"
systemctl restart "$unit_name"
echo "Check: systemctl status $unit_name; journalctl -u sysmon-gpu-guard -n 20; cat /run/sysmon-gpu-guard/state.json"
