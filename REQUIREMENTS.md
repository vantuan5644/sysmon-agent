# Requirements

System dependencies needed to run the agent and to unlock every metric on each platform.
The agent itself is a single self-contained Go binary; the items below are host-side tools
the agent shells out to for sensor data.

## Build

- **Go** 1.22+ (only to build from source; a prebuilt binary has no Go runtime requirement).
  Install from <https://go.dev/dl/> or via a package manager:
  - Windows: `winget install GoLang.Go`
  - Linux (Debian/Ubuntu): `sudo apt install golang-go` or the official tarball
- **Git** to clone the repository.

## Runtime — Linux

Everything below is satisfied by a stock kernel. The agent reads `/proc` and sysfs directly,
so there are no required third-party tools.

- **CPU / RAM / disk / network**: `/proc/stat`, `/proc/meminfo`, `/proc/mounts`, `statfs(2)`,
  `/proc/net/dev`. Always available.
- **CPU package power**: Intel/AMD RAPL energy counters under
  `/sys/class/powercap/intel-rapl:*`. Present on most bare-metal kernels; unavailable (and
  reported as such) inside some VMs and on locked-down BIOS settings. On AMD boards make
  sure the `acpi_cpufreq` / `amd-pstate` driver and the RAPL module are loaded.
- **CPU / GPU / board temperatures**: `/sys/class/hwmon` and `/sys/class/thermal`. Available
  when the kernel has drivers for your super-IO chip (e.g. `nct6775`, `coretemp`, `k10temp`,
  `it87`) and GPU.
- **NVIDIA GPU** (usage / VRAM / temp / power): `nvidia-smi` in `PATH` (ships with the
  NVIDIA driver).
- **AMD GPU** (usage / VRAM / temp / power): DRM sysfs (`/sys/class/drm/card*/device/`) via
  the `amdgpu` kernel driver. AMD GPU power is read from the hwmon `power1_average` counter
  (microwatts).
- **Intel iGPU**: DRM sysfs identity + temperature. Utilization, memory, and power are
  usually unavailable because the kernel does not expose simple counters — they show as
  unavailable, which is expected.

Optional but recommended for production:

- **systemd** (for `sysmon-agent.service` auto-start — see `deploy/sysmon-agent.service`).
- **Tailscale** + `tailscale serve` (recommended) to publish the dashboard over HTTPS for the
  device PWA install path.

## Runtime — Windows

- **PowerShell 5.1** (`powershell.exe`) — ships with Windows. Used for CPU, RAM, disk,
  network, and ACPI queries.
- **PowerShell 7+** (`pwsh`) — required only for the LibreHardwareMonitor bridge. Install via
  `winget install Microsoft.PowerShell`. Without pwsh the bridge is skipped and CPU power +
  board/CPU temps stay unavailable. Install it **machine-wide**
  (`winget install --scope machine Microsoft.PowerShell`); a per-user pwsh is on your
  account's PATH but not the LocalSystem service's.

### GPU

- **NVIDIA GPU** (usage / VRAM / temp / power): `nvidia-smi` in `PATH` (ships with the
  NVIDIA driver).
- **Non-NVIDIA GPUs**: best-effort via `Win32_VideoController` (identity + total adapter RAM)
  and the Windows `GPU Engine` performance counters (utilization). VRAM use, power draw, and
  temperature require a vendor tool.

### CPU power + CPU/motherboard/RAM temperatures (LibreHardwareMonitor)

Windows exposes **no native API** for CPU package power or board/RAM temperatures on consumer
hardware. These metrics require
[LibreHardwareMonitor](https://github.com/LibreHardwareMonitor/LibreHardwareMonitor), which
the agent drives through a small embedded PowerShell bridge that loads
`LibreHardwareMonitorLib.dll` directly (no GUI, no WMI provider needed — modern LHM builds
removed the WMI provider).

Install once per host (elevated):

```powershell
# Option A: Chocolatey (stable canonical path, recommended)
choco install librehardwaremonitor -y

# Option B: WinGet (per-user install under %LOCALAPPDATA%)
winget install LibreHardwareMonitor.LibreHardwareMonitor
```

The bridge searches both install locations automatically. The `LibreHardwareMonitorLib.dll`
shipped by recent releases targets **.NET 10**, so the bridge must run under PowerShell 7
(`pwsh`); Windows PowerShell 5.1 cannot load it. The agent resolves pwsh automatically.

LibreHardwareMonitor loads a kernel driver for low-level sensor access, so the **first**
bridge call after install must happen from an elevated process once per boot session (the
driver then stays available for the rest of the session and subsequent non-elevated agent
calls keep working). The simplest way to satisfy this is to launch the LibreHardwareMonitor
GUI once as administrator after each reboot, or — the recommended production path — run the
agent as the **LocalSystem service** (`install-windows.ps1`), which has the privilege to
install/start the driver on its own every boot.

If LibreHardwareMonitor is not installed, `cpu_power` and `temperatures` degrade to explicit
`available: false` fields with an actionable error message; all other metrics (CPU %, RAM,
disks, network, NVIDIA GPU) keep working.

## Optional

- **Node.js** 18+ — only needed to run the `verify-*.mjs` dashboard/API smoke checks and the
  Node helpers in `verify-deployed.sh`. Not required for the agent.
- **A headless browser** (Chrome/Edge) — only for `verify-render.mjs` device layout
  screenshots. Falls back to deterministic static checks when absent.
