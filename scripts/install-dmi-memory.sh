#!/usr/bin/env bash
set -euo pipefail

# Lets sysmon-agent show the running RAM speed. dmidecode needs root to read the
# DMI table, and every agent unit sets NoNewPrivileges, so the agent cannot get
# root even through sudo. This installs a root oneshot unit that snapshots
# `dmidecode -t 17` to /run/sysmon-dmi/memory.txt at boot, which the agent reads.
# The rated EXPO/XMP speed needs none of this: it comes from the SPD EEPROMs,
# which are world-readable.

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
apply=false
unit_name=sysmon-dmi-memory.service
unit_source="$script_dir/systemd/$unit_name"

usage() {
    cat <<'EOF'
Usage: install-dmi-memory.sh [--apply]
Defaults to dry-run. Installs and starts sysmon-dmi-memory.service. Restart the
agent afterwards: it reads memory identity once at startup.
EOF
}

while (($#)); do
    case "$1" in
        --apply) apply=true ;;
        --help|-h) usage; exit 0 ;;
        *) usage; exit 2 ;;
    esac
    shift
done

[[ "$(uname -s)" == Linux ]] || { echo "Requires Linux" >&2; exit 1; }
[[ -f "$unit_source" ]] || { echo "Unit is missing: $unit_source" >&2; exit 1; }
if ! command -v dmidecode >/dev/null && [[ ! -x /usr/sbin/dmidecode ]]; then
    echo "dmidecode is not installed (Arch: pacman -S dmidecode; Debian/Ubuntu: apt install dmidecode)" >&2
    exit 1
fi

echo "Install unit: $unit_source -> /etc/systemd/system/$unit_name"
echo "Enable and start $unit_name; snapshot: /run/sysmon-dmi/memory.txt"
if ! $apply; then echo "Dry-run complete; use --apply to make these changes"; exit 0; fi
[[ $EUID -eq 0 ]] || { echo "Run the apply command with sudo" >&2; exit 1; }

install -o root -g root -m 0644 "$unit_source" "/etc/systemd/system/$unit_name"
systemctl daemon-reload
systemctl enable "$unit_name"
systemctl restart "$unit_name"
grep -m1 'Configured Memory Speed' /run/sysmon-dmi/memory.txt || echo "Snapshot has no configured speed; check: systemctl status $unit_name" >&2
echo "Then restart the agent: systemctl --user restart sysmon-agent (desktop) or sudo systemctl restart sysmon-agent (server)"
