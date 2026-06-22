#!/usr/bin/env bash
#
# run-linux.sh - rebuild sysmon-agent and rerun it. Linux counterpart to
# run-windows.ps1.
#
# Unlike Windows (which just kills the port owner and launches the exe), the
# Linux deployment runs the agent under systemd: a `systemctl --user` service
# on a desktop, or a system service on a headless host. This script rebuilds
# the Go binary, reinstalls it to /usr/local/bin, and restarts that service.
#
# Static assets and lhm-bridge.ps1 are //go:embed-ed, so a rebuild is required
# for any static/ or bridge change to take effect.
#
# Usage:
#   ./run-linux.sh                 # build + install + restart the service
#   ./run-linux.sh -n|--no-build   # reuse the existing built binary
#   ./run-linux.sh -f|--foreground # dev run: stop the service, run in the
#                                   #   foreground (Ctrl+C), then restart it
#   ./run-linux.sh -s|--system     # target the system service (sudo systemctl)
#   ./run-linux.sh -u|--user       # target the user service (default on desktop)
#   ./run-linux.sh -p 9100         # override port (default 9099)
#   ./run-linux.sh -b 0.0.0.0      # override bind (default 127.0.0.1)
#
# Scope auto-detects from the installed unit when -s/-u are not given.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUILT_BIN="$SCRIPT_DIR/sysmon-agent"
INSTALL_PATH="${SYSMON_INSTALL_PATH:-/usr/local/bin/sysmon-agent}"

BIND="${SYSMON_BIND:-127.0.0.1}"
PORT="${SYSMON_PORT:-9099}"
READY_TIMEOUT="${SYSMON_WAIT_READY:-15s}"
SCOPE=""
NO_BUILD=0
FOREGROUND=0

usage() {
    cat <<'EOF'
run-linux.sh - rebuild sysmon-agent and rerun it (Linux counterpart to run-windows.ps1).

The agent runs under systemd: a `systemctl --user` service on a desktop, or a
system service on a headless host. This rebuilds the Go binary, reinstalls it to
/usr/local/bin, and restarts that service. Static assets and lhm-bridge.ps1 are
//go:embed-ed, so a rebuild is required for those changes to take effect.

Usage:
  ./run-linux.sh             build + install + restart the service
  -n, --no-build             reuse the existing built binary (skip go build)
  -f, --foreground           dev run: stop service, run attached (Ctrl+C), then restart it
  -s, --system               target the system service (sudo systemctl)
  -u, --user                 target the user service (default on a desktop)
  -b, --bind ADDR            bind address (default 127.0.0.1)
  -p, --port PORT            port (default 9099)
  -h, --help                 show this help

Scope auto-detects from the installed unit when -s/-u are not given.
EOF
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        -n|--no-build)   NO_BUILD=1; shift ;;
        -f|--foreground) FOREGROUND=1; shift ;;
        -u|--user)       SCOPE=user; shift ;;
        -s|--system)     SCOPE=system; shift ;;
        -b|--bind)       BIND="${2:?--bind needs a value}"; shift 2 ;;
        -p|--port)       PORT="${2:?--port needs a value}"; shift 2 ;;
        -h|--help)       usage; exit 0 ;;
        *) echo "unknown argument: $1" >&2; usage; exit 2 ;;
    esac
done

# Resolve the systemd scope from the installed unit when not forced.
if [[ -z "$SCOPE" ]]; then
    if [[ -f "$HOME/.config/systemd/user/sysmon-agent.service" ]]; then
        SCOPE=user
    elif [[ -f /etc/systemd/system/sysmon-agent.service ]]; then
        SCOPE=system
    else
        SCOPE=none
    fi
fi

# sc runs systemctl in the resolved scope (sudo only for the system manager).
sc() {
    if [[ "$SCOPE" == user ]]; then
        systemctl --user "$@"
    else
        sudo systemctl "$@"
    fi
}

journal_hint() {
    if [[ "$SCOPE" == user ]]; then
        echo "journalctl --user -u sysmon-agent -n 50 --no-pager"
    else
        echo "sudo journalctl -u sysmon-agent -n 50 --no-pager"
    fi
}

# 1) Build (in the module dir so go:embed re-bakes static/ and lhm-bridge.ps1).
if [[ "$NO_BUILD" -eq 1 ]]; then
    [[ -x "$BUILT_BIN" ]] || { echo "no binary at $BUILT_BIN (drop --no-build to build it)" >&2; exit 1; }
    echo "==> Skipping build (reusing $BUILT_BIN)"
else
    echo "==> Building sysmon-agent"
    ( cd "$SCRIPT_DIR" && go build -o "$BUILT_BIN" . )
    echo "    built: $BUILT_BIN"
fi

# 2) Install to the stable path the service runs from. `install` swaps the
#    inode, so this is safe even while the old binary is executing.
echo "==> Installing to $INSTALL_PATH"
sudo install -m 0755 "$BUILT_BIN" "$INSTALL_PATH"

# 3a) Foreground dev run: stop the service, run attached, restore on exit.
if [[ "$FOREGROUND" -eq 1 ]]; then
    if [[ "$SCOPE" != none ]]; then
        echo "==> Stopping service ($SCOPE) to free :$PORT"
        sc stop sysmon-agent.service || true
        trap 'echo; echo "==> Restarting service ($SCOPE)"; sc restart sysmon-agent.service' EXIT
    fi
    echo "==> Foreground (Ctrl+C to stop): $INSTALL_PATH -bind $BIND -port $PORT"
    "$INSTALL_PATH" -bind "$BIND" -port "$PORT"
    exit 0
fi

# 3b) Service mode (default): restart and gate on readiness.
if [[ "$SCOPE" == none ]]; then
    echo "Binary updated at $INSTALL_PATH, but no sysmon-agent unit is installed."
    echo "Install one from deploy/ (system or user), or use --foreground."
    exit 0
fi

echo "==> Restarting service ($SCOPE)"
sc restart sysmon-agent.service

echo "==> Waiting for readiness (timeout $READY_TIMEOUT)"
if "$INSTALL_PATH" -wait-ready -bind "$BIND" -port "$PORT" -wait-ready-timeout "$READY_TIMEOUT"; then
    echo "    ready: http://$BIND:$PORT/"
else
    echo "    NOT ready - inspect with: $(journal_hint)" >&2
    exit 1
fi

sc status sysmon-agent.service --no-pager 2>/dev/null | head -n 4 || true
