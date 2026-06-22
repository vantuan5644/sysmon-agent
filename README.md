# Sysmon Agent

A single self-contained binary that turns **any** computer into a live system-monitoring
dashboard server — CPU, GPU, RAM, disk, network, temperatures, and power — rendered as an
installable Progressive Web App (PWA) on your phone, tablet, or any browser on the same
network. The device only renders the dashboard; every metric is collected on the host.

- **Stdlib-only Go.** Zero third-party dependencies (`go.mod` is empty). One binary per OS.
- **Cross-platform.** Linux (reads `/proc` + sysfs directly) and Windows (PowerShell + an
  embedded LibreHardwareMonitor bridge). Other platforms build and run with all sensors
  reported unavailable.
- **Embedded PWA.** All static assets are `//go:embed`-ed, so the dashboard ships inside the
  binary. Add it to your device Home Screen for a clean, always-on monitor.
- **Graceful degradation.** Every sensor degrades independently to an explicit
  `available: false` field with a reason — a missing sensor never fails the whole response.

---

## Table of contents

- [Quick start](#quick-start)
- [Build](#build)
- [Run](#run)
- [Device setup (PWA / Home Screen)](#device-setup-pwa--home-screen)
- [Deploy as a service](#deploy-as-a-service)
  - [Linux (systemd)](#linux-systemd)
  - [Windows (service)](#windows-service)
- [Publishing over HTTPS (Tailscale)](#publishing-over-https-tailscale)
- [Metrics & API](#metrics--api)
- [Dashboard](#dashboard)
- [Configuration](#configuration)
- [Platform notes](#platform-notes)
  - [Linux notes](#linux-notes)
  - [Windows notes](#windows-notes)
- [Troubleshooting](#troubleshooting)
- [Distribution — Windows installer](#distribution--windows-installer)
- [Verify & test](#verify--test)
- [Requirements](#requirements)
- [License](#license)

---

## Quick start

```bash
git clone <this-repo> system-monitor
cd system-monitor
./build.sh                 # produces ./sysmon-agent
./sysmon-agent             # listens on 0.0.0.0:9099
```

Then open from any device on the same network:

```
http://HOST_IP:9099/
```

For the best experience (PWA install, service worker, wake lock), serve it over HTTPS — see
[Publishing over HTTPS](#publishing-over-https-tailscale).

---

## Build

Requires **Go 1.22+** (only to build from source; a prebuilt binary has no Go requirement).

```bash
./build.sh                 # current OS/arch  -> ./sysmon-agent (or .exe on Windows)
./build.sh linux           # cross-compile for Linux
./build.sh windows         # cross-compile for Windows -> ./sysmon-agent.exe
go build -trimpath -o sysmon-agent .   # plain go build also works
```

> Any change under `static/` or to `lhm-bridge.ps1` requires a Go **rebuild** — those files
> are embedded into the binary at build time, not read from disk at runtime.

---

## Run

Defaults listen on all interfaces at port `9099` (convenient for a trusted LAN or a
Tailscale network).

```bash
./sysmon-agent
./sysmon-agent -bind 127.0.0.1 -port 9099
./sysmon-agent -settings ./settings.json
./sysmon-agent -tls -cert ./cert.pem -key ./key.pem     # optional direct TLS
```

Environment-variable equivalents (flags win when both are set):

```bash
SYSMON_BIND=127.0.0.1 SYSMON_PORT=9099 ./sysmon-agent
```

Endpoints:

```
http://HOST_IP:9099/                # the dashboard
http://HOST_IP:9099/healthz         # cheap process liveness
http://HOST_IP:9099/readyz          # proves metrics are actually collectable
http://HOST_IP:9099/api/status      # agent metadata + active display settings
http://HOST_IP:9099/api/metrics     # the live metrics payload
http://HOST_IP:9099/api/stream      # Server-Sent Events live metrics push
```

When the bind address is a wildcard such as `0.0.0.0`, startup logs print likely dashboard
URLs using non-loopback addresses, with Tailscale addresses listed first when available.
Common virtual/container interfaces (Docker bridges, veth links, Hyper-V/WSL, VirtualBox,
VMware, Npcap, Wi-Fi Direct) are skipped. If no non-loopback address is detected, the agent
logs that explicitly.

Direct TLS (`-tls -cert -key`) is the no-reverse-proxy fallback. Prefer Tailscale Serve or
another HTTPS reverse proxy when one is available.

On Windows PowerShell:

```powershell
go build -o sysmon-agent.exe .
.\sysmon-agent.exe -bind 0.0.0.0 -port 9099
```

Allow the port through Windows Firewall if the device connects over the LAN. On Linux, allow
TCP `9099` only from trusted local or Tailscale addresses.

---

## Device setup (PWA / Home Screen)

The dashboard is an installable PWA. The recommended path is HTTPS so the browser gets the
secure context a PWA / service worker needs:

1. Publish the agent over HTTPS (see [Publishing over HTTPS](#publishing-over-https-tailscale)).
2. Open the URL on the device.
3. **Add to Home Screen** — Safari "Add to Home Screen" on iOS, Chrome "Add to Home
   screen" / "Install app" on Android.
4. Keep the device on power for an always-on desk monitor. If the display still sleeps, set
   the device's auto-lock to Never while it is being used as the monitor.

Direct LAN (`http://HOST_IP:9099/`) also works for a quick test, but HTTPS is better for the
installed PWA path.

---

## Deploy as a service

### Linux (systemd)

Two path-agnostic units are shipped under `deploy/`. **Pick the one that matches your
host.**

#### User session unit (recommended for desktops)

[`deploy/sysmon-agent.user.service`](deploy/sysmon-agent.user.service) runs under the
per-user systemd manager. This is the right choice for a desktop/laptop with an
interactive session, because it inherits `XDG_RUNTIME_DIR` and `DBUS_SESSION_BUS_ADDRESS` —
which the **footer control buttons** (mic mute, media toggle, speaker mute, lock screen)
need: `wpctl`/`pactl` reach PipeWire/PulseAudio, `playerctl` reaches the media player, and
`loginctl lock-session` reaches the active session. Install it as your **desktop user, not
root**:

```bash
./build.sh
sudo install -m0755 sysmon-agent /usr/local/bin/sysmon-agent
mkdir -p ~/.config/systemd/user
install -m0644 deploy/sysmon-agent.user.service ~/.config/systemd/user/sysmon-agent.service
systemctl --user daemon-reload
systemctl --user enable --now sysmon-agent.service
sudo loginctl enable-linger "$USER"   # also run at boot, before/without login
```

State lives under `$XDG_STATE_HOME/sysmon-agent/` (~/.local/state/sysmon-agent) and the unit
is `WantedBy=default.target`. The controls also need `wireplumber` (`wpctl`) or
`pulseaudio` (`pactl`), `playerctl`, and a session that honors `loginctl lock-session`
(KDE/GNOME Wayland do).

#### System unit (headless hosts)

[`deploy/sysmon-agent.service`](deploy/sysmon-agent.service) runs as a system service — the
right choice for a headless host, or when you don't use the footer controls.

```bash
./build.sh
sudo install -m0755 sysmon-agent /usr/local/bin/sysmon-agent
sudo install -m0644 deploy/sysmon-agent.service /etc/systemd/system/
sudoedit /etc/systemd/system/sysmon-agent.service   # set User= for this host
sudo systemctl daemon-reload
sudo systemctl enable --now sysmon-agent.service
```

Both units:

- Run the binary at `/usr/local/bin/sysmon-agent`.
- Bind to `127.0.0.1:9099` by default (for use behind an HTTPS proxy); override
  `SYSMON_BIND`/`SYSMON_PORT` in the unit for a direct LAN listener.
- Perform a bounded `/readyz` readiness probe (`SYSMON_START_READY_TIMEOUT`, default `20s`)
  before startup is considered successful, and rate-limits repeated restart failures.
- Apply basic hardening while preserving access to `/proc`, sysfs, hwmon, and the RAPL
  powercap counters the Linux collector reads. (Do **not** add `ProtectSystem` /
  `PrivateDevices` / `ProtectKernelModules` — they would silently break those reads.)

The system unit additionally creates and owns `/var/lib/sysmon-agent` for state.

#### Rebuild / rerun helper

[`run-linux.sh`](run-linux.sh) rebuilds the Go binary, reinstalls it to `/usr/local/bin`,
and restarts the installed service — the Linux counterpart to `run-windows.ps1`. It
auto-detects whether the user unit or the system unit is installed and targets the right
manager:

```bash
./run-linux.sh                 # build + install + restart the service
./run-linux.sh -n              # reuse the existing binary (skip the build)
./run-linux.sh -f              # dev: stop service, run attached (Ctrl+C), then restart it
./run-linux.sh -u              # force the user service (systemctl --user)
./run-linux.sh -s              # force the system service (sudo systemctl)
```

Static assets and `lhm-bridge.ps1` are `//go:embed`-ed, so any `static/` or bridge change
requires the rebuild this script performs.

### Windows service

`install-windows.ps1` registers a native Windows service (`SysmonAgent`) using pure stdlib
SCM integration — the same binary is both a console app and a service. From an elevated
PowerShell session:

```powershell
go build -o sysmon-agent.exe .
.\install-windows.ps1 -Action Install
.\install-windows.ps1 -Action Status      # probes /readyz, reports active settings
.\install-windows.ps1 -Action Uninstall
```

The installer:

- Persists dashboard settings under `%ProgramData%\SysmonAgent\settings.json`.
- Adds an inbound TCP firewall rule for Domain/Private profiles, replacing any older rule.
- Configures service recovery to restart the agent after failures.
- Waits up to 20 s (override `-ReadinessTimeoutSeconds`) for `/readyz` after starting.

> The service runs as **LocalSystem**, which is *why* it can load the LibreHardwareMonitor
> kernel driver on its own every boot. Don't run the agent interactively under an
> unprivileged account for production — that account can't install the driver, so CPU power
> and board temps degrade. See [Windows notes](#windows-notes).

---

## Publishing over HTTPS (Tailscale)

[Tailscale Serve](https://tailscale.com/kb/1312/serve) is the easiest way to give the
dashboard the HTTPS context a PWA needs, without managing certificates in the agent.

```bash
# one published route, agent already listening on 127.0.0.1:9099
sudo tailscale serve --bg --https=9443 http://127.0.0.1:9099
```

Then open `https://TAILSCALE_HOST:9443/` from a device on your tailnet and add it to the
Home Screen. Any other HTTPS reverse proxy (Caddy, nginx, Traefik, Cloudflare Tunnel) works
equally well — just forward it to the agent's bind address.

---

## Metrics & API

`GET /api/metrics` returns:

- Hostname, OS, architecture, platform, timestamp, and collection duration.
- **CPU** usage percentage, package power (W) when exposed, clock speed (MHz) plus the
  advertised max/boost clock, and dedicated CPU die temperature.
- **PSU** total output power (W) when a USB-linked smart PSU is reported through the
  LibreHardwareMonitor bridge on Windows.
- **RAM** used, total, and percentage.
- **Disk** used, total, and percentage for mounted local filesystems.
- **Network** RX/TX byte rates per interface (after the sampler warms up).
- **Temperatures** from Linux hwmon/thermal sysfs or Windows ACPI/LibreHardwareMonitor.
- **GPU** telemetry from `nvidia-smi` (power, VRAM used/total, temperature) and AMD/Intel
  DRM sysfs on Linux.

Unavailable sensors are returned as explicit `available: false` fields with an error string,
rather than failing the whole request. `collection_errors` rolls all the unavailable fields
up into compact `name: reason` summaries for troubleshooting.

`GET /api/status` returns lightweight agent metadata: status, uptime, runtime OS/arch, the
active dashboard display settings, whether settings are persisted, and the latest
dashboard client-check observations (including the latest mobile/handheld-like client
separately). It does not collect sensors — use it to check whether the agent is responsive
and whether the installed device path has checked in recently.

`GET /readyz` performs a bounded metrics collection and returns `status: ok` only when the
dashboard metrics payload is structurally ready. Optional collector degradation does not
fail readiness; any `collection_errors` are still included.

`GET /api/stream` is a Server-Sent Events feed registered only when the resident sampler is
active — it pushes fresh snapshots as they are collected, with a `: keepalive` every 15 s.

Performance characteristics:

- A **resident sampler** keeps one warm `Metrics` snapshot refreshed by background lanes —
  the fast lane (CPU/RAM, ~5 Hz) and the slow lane (power/temps/disk/net/GPU, ~0.7 Hz) — so
  `/api/metrics` reads an in-memory snapshot instead of spawning a collection per request.
- Concurrent `/api/metrics` requests share one in-flight collection (single-flight), and
  successful samples are cached briefly (~750 ms) so 1-second always-on refreshes stay
  responsive.
- Within each Linux or Windows sample, independent metric groups are collected in parallel;
  if one group panics, it is reported as an unavailable field instead of crashing the
  response.

---

## Dashboard

The dashboard is an embedded PWA optimized for a phone-sized always-on display. It refreshes
every 250 ms, 500 ms, 1 s, or 2 s and has pause, dim, screen-shift, panel-focus, warning-threshold,
and wake-lock controls. The Wake preference is remembered locally on the device so the
installed dashboard retries the screen wake lock after reloads or visibility changes, but
the OS/browser support still controls whether the lock is actually granted. Screen-shift
mode subtly moves the dashboard a few pixels over time to reduce static-image retention
during long desk-monitor sessions; it is enabled by default for new settings and can be
toggled off from the device.

The primary metric cards are touch shortcuts: tap CPU or RAM to focus the performance view,
GPU for GPU details, TEMP for sensors; tap again to return to the full dashboard. A
horizontal swipe cycles the same panel focus order. Each primary card keeps a small 24-sample
trend strip. The CPU, GPU, and TEMP gauges are concentric double rings (primary reading
outer, correlated reading inner); RAM keeps a single utilization ring.

### Status strip & panels

The status strip shows live/offline/stale state, the latest metrics timestamp with sample
age, sample collection duration, agent uptime, whether settings are persisted, and the
current display mode (`app` for Home Screen standalone, `web` for browser). Tap the status
strip to refresh metrics, agent status, and the device client check immediately; it briefly
shows `Refreshing`, then `Client check sent (app)` when the verifier beacon is accepted from
Home Screen standalone mode, so the device tap has visible feedback during the deployed
verifier hold. Resuming from pause, returning from an iOS/browser page restore, or
reconnecting to the network also forces a fresh refresh when the page is visible.

If live metrics exceed the configured warning thresholds, an **Alerts** panel appears and
hides again when readings return under threshold. If collectors are degraded, an **Issues**
panel appears with the compact `collection_errors` summaries. If `/api/status` or
`/api/metrics` fail, those failures are also shown in the Issues panel and clear after the
next successful response.

### Stale dashboard shell

When served over HTTPS, the service worker caches the static dashboard shell, keeps API and
health-check calls network-only, and claims updated worker versions immediately. If the
dashboard shell is stale and its embedded build token no longer matches `/api/status`, the
device dashboard shows a stale-build entry in the Issues panel with the app and server build
IDs. Tap the status strip while that issue is visible to unregister the Sysmon service worker,
clear only `sysmon-static-*` caches, and reload the Home Screen dashboard once. If
iOS still keeps the old shell after that refresh, remove and re-add the Home Screen icon.

### Settings & client checks

`GET /api/settings` and `POST /api/settings` back the dim/screen-shift/refresh/panel/
threshold controls so the projected dashboard can be changed from the device.
Wake is not stored on the host because it represents per-device browser state; it is kept in the
device dashboard's local storage instead. These settings are intentionally limited to
display state and do not execute host commands.

Settings updates require an `Origin` header matching the public request host, including script-driven checks.
Reverse proxies should preserve the public `Host` header or, when
proxying from the same machine, send `X-Forwarded-Host` or a standard `Forwarded` header with `host=...`
so controls continue to work from the installed device dashboard. The agent
ignores spoofed forwarded host values from non-loopback clients. Settings read or update
failures are also shown in the Issues panel.

`POST /api/client-check` is a lightweight same-origin browser beacon used by the smoke
verifiers to prove a real browser opened the page. It records the last observed dashboard
build, optional interaction marker, viewport, display mode, visibility, user agent, and
timestamp in memory only. The dashboard sends passive checks on load, periodically while
visible, after resume, after an iOS page restore, after network reconnect, and after
viewport changes. Tapping the status strip sends the same client check with
`interaction=status_strip_tap`, which the deployed verifier uses as proof that the device
interaction path worked. Successful device control changes also send lightweight `settings_*`
interaction markers such as `settings_refresh`, `settings_panel`, and `settings_threshold`
so `/api/client-checks` shows recent interactive use without relaxing the final deployed verifier's `status_strip_tap` requirement. Client checks include the dashboard build token from `/api/status`,
so a stale cached dashboard build or passive page-open beacon cannot satisfy the final deployed gate. The backend accepts only the display-mode values the
dashboard can report: `standalone`, `fullscreen`, `minimal-ui`, `browser`, or `unknown`.

---

## Configuration

Display settings can be persisted with `-settings PATH` (or `SYSMON_SETTINGS=PATH`). The
refresh interval and warning thresholds are **host-side** config set via flags/env (not touch
controls) and written into the saved settings at startup via a host settings update; a `0`/
unset value keeps the saved/default.

| Flag / env | Range / default | Meaning |
| --- | --- | --- |
| `-bind` / `SYSMON_BIND` | `0.0.0.0` | HTTP bind address |
| `-port` / `SYSMON_PORT` | `9099` | HTTP listen port |
| `-fast-ms` / `SYSMON_FAST_MS` | min 100, default 200 | fast-lane (CPU/RAM) interval |
| `-slow-ms` / `SYSMON_SLOW_MS` | min 500, default 1500 | slow-lane (power/temps/disk/net/GPU) interval |
| `-refresh-ms` / `SYSMON_REFRESH_MS` | {250,500,1000,2000} | dashboard refresh interval |
| `-cpu-warn` / `SYSMON_CPU_WARN` | 50–90 | CPU utilization warn threshold % |
| `-mem-warn` / `SYSMON_MEM_WARN` | 50–90 | memory utilization warn threshold % |
| `-disk-warn` / `SYSMON_DISK_WARN` | 50–90 | disk utilization warn threshold % |
| `-gpu-warn` / `SYSMON_GPU_WARN` | 50–90 | GPU utilization warn threshold % |
| `-temp-warn` / `SYSMON_TEMP_WARN` | 50–90 (°C) | temperature warn threshold |
| `-settings` / `SYSMON_SETTINGS` | path | optional JSON file for persisted settings |
| `-tls` / `SYSMON_TLS` | bool | enable direct TLS (`-cert`/`-key`) |
| `-self-check` / `SYSMON_SELF_CHECK` | bool | run in-process endpoint checks and exit |
| `-wait-health` / `SYSMON_WAIT_HEALTH` | bool | wait for `/healthz` and exit (startup gate) |
| `-wait-ready` / `SYSMON_WAIT_READY` | bool | wait for `/readyz` and exit (startup gate) |

If the persisted settings file contains invalid JSON or unsupported values, the agent backs
it up with a `.bad-...` suffix and starts with defaults so the monitor still comes up.

---

## Platform notes

### Linux notes

- CPU, memory, local disks, and network are read from `/proc`.
- **CPU package power** is derived from Intel/AMD RAPL energy counters under
  `/sys/class/powercap/intel-rapl:*` (package domains only, with wraparound handling). On
  hosts that don't expose RAPL (some VMs, locked-down BIOS), it is reported unavailable.
- Network collection skips loopback plus common container/bridge interfaces (Docker bridges,
  veth links) while keeping physical, Wi-Fi, Tailscale, and WireGuard interfaces visible.
- Disk collection skips pseudo filesystems and known remote mounts (NFS, CIFS/SMB, SSHFS,
  rclone, WebDAV) so a stale share doesn't stall the refresh loop.
- Temperatures are read from `/sys/class/hwmon` and `/sys/class/thermal`, with duplicate
  sensor names collapsed.
- **NVIDIA GPU** requires `nvidia-smi` in `PATH`. **AMD GPU** (usage/VRAM/temp/power) is
  read from DRM sysfs via the `amdgpu` driver. **Intel iGPU** is best-effort through DRM
  sysfs identity + temperature (utilization/memory/power are usually unavailable).

### Windows notes

- CPU, memory, disks, and network use PowerShell/CIM queries. The agent prefers Windows
  PowerShell (`powershell.exe`) and falls back to PowerShell Core (`pwsh.exe`) when
  available.
- Independent Windows collectors run in parallel inside each metrics sample.
- Network collection skips loopback plus Hyper-V, WSL, Docker, VirtualBox, VMware, Npcap,
  and Wi-Fi Direct virtual adapters.
- **NVIDIA GPU** uses `nvidia-smi`. Non-NVIDIA GPUs are best-effort via
  `Win32_VideoController` (identity + total RAM) and the Windows `GPU Engine` performance
  counters (utilization).
- **CPU package power + CPU/motherboard/RAM temperatures** are not exposed by any native
  Windows API on consumer boards. The agent ships an embedded LibreHardwareMonitor bridge
  (a small PowerShell script that loads `LibreHardwareMonitorLib.dll` directly — no GUI, no
  WMI provider). Install once per host (elevated):

  ```powershell
  choco install librehardwaremonitor -y            # machine-wide (recommended)
  # or
  winget install LibreHardwareMonitor.LibreHardwareMonitor
  winget install Microsoft.PowerShell              # PowerShell 7+ (pwsh) — required for the bridge
  ```

  Install LibreHardwareMonitor **machine-wide** — a per-user `winget` install lands under
  the installing user's profile, which the LocalSystem service cannot read. Modern LHM builds
  target .NET 10, so the bridge must run under `pwsh` (PowerShell 7+); the agent resolves it
  automatically. The same bridge also reports PSU total output power when a USB-linked smart
  PSU (Corsair HXi/RMi, NZXT, Seasonic, etc.) is installed. See [Requirements](#requirements).

---

## Troubleshooting

- If a sensor shows unavailable, check `/api/metrics` first — each unavailable field carries
  an error string (missing kernel files, tools, or unsupported hardware) without checking logs.
- If **NVIDIA GPU** data is missing, install the NVIDIA driver and confirm `nvidia-smi` works
  from the same shell or service account that runs the agent.
- If **CPU package power** or **CPU/motherboard temperatures** are missing on Windows, install
  LibreHardwareMonitor (machine-wide) and PowerShell 7, then run one elevated sample after
  reboot (launch the LHM GUI once as admin). The agent picks the sensors up automatically; no
  restart is needed.
- On Linux, AMD/Intel GPU and temperature data depends on what the kernel exposes under
  `/sys/class/drm`, `/sys/class/hwmon`, and `/sys/class/thermal`.
- If the device can't connect from the LAN, verify the bind address, the machine IP, and the
  host firewall. Bind to `0.0.0.0` only on trusted LAN or Tailscale networks.
- For PWA install / service-worker behavior on iOS, serve over HTTPS (Tailscale Serve,
  another reverse proxy, or direct TLS).

---

## Distribution — Windows installer

The repo ships a complete packaging flow under `dist/` that turns the source into a
**double-clickable Windows installer** for non-technical users. It is fully
cross-compilable — you build the `.exe` installer from Linux, macOS, or Windows
without wine.

### One command

```bash
./dist/build-installer.sh 1.0.0      # or let the version default from `git describe`
```

This produces `dist/out/SysmonAgent-Setup-1.0.0.exe` (~3 MB). Ship that single file.

What it does, end to end:

1. **Builds a release exe** (`dist/build-windows.sh`): cross-compiles `GOOS=windows`,
   strips symbols (`-s -w`), injects the version into `-version`, and embeds a Win32
   version resource + the app icon (so Explorer/Properties show version + icon).
2. **Packages an NSIS installer** (`dist/installer.nsi`): a Modern-UI wizard that —

   - installs to `Program Files\Sysmon Agent`,
   - elevates automatically and runs the existing `install-windows.ps1` (the single
     source of truth for service registration, firewall, recovery actions, and the
     `/readyz` readiness gate),
   - adds **Start Menu** and (optional) **Desktop** shortcuts that open the dashboard,
   - registers a clean uninstaller in **Add/Remove Programs**, and on uninstall stops
     + removes the service, the firewall rule, the files, and the shortcuts.

### What the end user sees

Double-click `SysmonAgent-Setup-1.0.0.exe` → UAC prompt → wizard → Finish. The agent
runs as a background Windows service (`SysmonAgent`) that survives reboots. They open
the dashboard from the Start Menu shortcut, or from their phone at
`http://<PC-NAME>:9099/` on the same Wi-Fi (the installer opens the firewall for it).

### Requirements to build the installer

- **Go** 1.22+ (always).
- **NSIS** (`makensis`) — the only extra tool. Install it once:
  - Debian/Ubuntu: `sudo apt install nsis`
  - macOS: `brew install nsis`
  - Arch/CachyOS: `paru -S nsis` (AUR)
  - Windows: <https://nsis.sourceforge.io/Download>
- Optional: **ImageMagick** (`magick`/`convert`) regenerates `dist/packaging/sysmon.ico`
  from `static/icon-512.png`; a committed `.ico` ships as a fallback, so it's not
  required.

### Just the release binary (no installer)

If you only want a standalone optimized `.exe` (e.g. to run yourself, or to wrap in your
own deployment):

```bash
./dist/build-windows.sh 1.0.0       # -> dist/out/sysmon-agent.exe
```

Then register it manually from an elevated PowerShell:

```powershell
.\install-windows.ps1 -Action Install
```

### Enabling CPU power + board temperatures for end users

The zero-dependency install reports **CPU %, RAM, disks, network, and NVIDIA GPU** out of
the box. CPU package power and CPU/motherboard/RAM temperatures need
[LibreHardwareMonitor](https://github.com/LibreHardwareMonitor/LibreHardwareMonitor) +
PowerShell 7 (no native Windows API exposes them). For a non-technical user, have them
install both machine-wide once after the agent:

```powershell
winget install --scope machine LibreHardwareMonitor.LibreHardwareMonitor
winget install --scope machine Microsoft.PowerShell
```

Then reboot (or launch the LibreHardwareMonitor GUI once as admin) so its kernel driver
loads. The agent picks the sensors up automatically; no restart of the agent is needed.

### Signing (recommended before public release)

Windows SmartScreen will warn on an unsigned installer from a new publisher. For public
distribution, sign both the exe and the installer with a code-signing certificate
(`signtool sign /tr <timestamp-url> /fd sha256 /a <file>`), obtained from a CA or
[Azure Trusted Signing](https://learn.microsoft.com/azure/trusted-signing/). Without a
known reputation, expect a one-time "unrecognized app" prompt regardless of signing
until SmartScreen builds trust.

### Why NSIS (.exe) and not .msi?

NSIS was chosen because `makensis` runs **natively on Linux/macOS**, so the whole release
is cross-compilable from the same machine you develop on, and it can reuse the existing,
battle-tested `install-windows.ps1` instead of re-implementing service/firewall/readiness
logic in installer primitives. If you specifically need a `.msi` (e.g. for Group Policy
deployment), the same files work with [WiX Toolset v4](https://wixtoolset.org/)
(`dotnet tool install -g wix`, runs on Linux via .NET) or [`go-msi`](https://github.com/mhewedy/go-msi);
use `install-windows.ps1` as a deferred custom action just as the NSIS script does.

---

## Verify & test

```bash
go test ./...                                       # unit tests
go run . -self-check                                # in-process HTTP checks, no socket
./verify-no-listen.sh                               # gofmt + tests + vet + self-check + cross-compile + verifiers
node verify-api.mjs --sample                        # API schema vs a known-good sample
node verify-dashboard.mjs                           # dashboard JS against sample responses
node verify-render.mjs                              # portrait/landscape layout (+ optional headless screenshot)
./verify.sh                                         # real-host smoke: build, start on :19099, check API/PWA/settings, stop
./verify-deployed.sh                                # checks an already-running service + published device URL
```

`-self-check` exercises `/healthz`, `/api/status`, `/api/metrics`, `/api/settings`,
`/api/client-check`, and `/` through the same HTTP handler without opening a socket — useful
in sandboxes, but still do the curl/browser check on the real host.

`./verify.sh` builds a smoke binary under a temp directory (it doesn't overwrite your
`sysmon-agent`), starts it on `127.0.0.1:${SYSMON_VERIFY_PORT:-19099}`, checks the API,
dashboard, and settings round-trip, and writes a report to
`/tmp/sysmon-agent-verify-report.txt`.

`./verify-deployed.sh` validates the already-running service and the published device URL.
It detects the Tailscale Serve URL when `tailscale` + `jq` are available, falls back to a
direct LAN candidate, or accepts `SYSMON_DEPLOY_VERIFY_DEVICE_URL`. It waits for a fresh real
Home Screen status-strip tap. During the hold it polls the `/api/client-checks` history, falls back to `/api/client-check`
when history is unavailable, and also considers the `/api/status` `device_client_check`
and `client_check` fields so status-carried device evidence is not missed. By default it
requires both `standalone=true` and `display_mode=standalone`, proving the dashboard was opened from the installed Home Screen
app; set `SYSMON_DEPLOY_VERIFY_REQUIRE_STANDALONE=0` only for an ordinary browser smoke. It
also requires `interaction=status_strip_tap` by default; set
`SYSMON_DEPLOY_VERIFY_REQUIRE_INTERACTION=0` only when deliberately checking passive Home
Screen launch evidence.

The deployed verifier only passes the strict device gate when the client-check history, latest endpoint, or `/api/status` client-check fields have `standalone=true`,
`display_mode=standalone`, `interaction=status_strip_tap`, and a `dashboard_build` matching `/api/status`,
plus a `last_seen` timestamp from the hold window that is newer than any
client timestamp observed before the hold, so an old cached app, ordinary browser dashboard
visit, or passive Home Screen launch cannot satisfy the final report. Open Sysmon from the
device Home Screen icon during the hold, confirm the status strip shows `app`, and tap the
status strip once.

```bash
# final installed-service device check
sudo tailscale serve --bg --https=9443 http://127.0.0.1:9099
SYSMON_DEPLOY_VERIFY_HOLD=120 ./verify-deployed.sh
```

Windows equivalents: `.\verify-windows.ps1` (localhost smoke) and
`.\verify-deployed-windows.ps1` (installed-service device check), which applies the same fresh
Home Screen client-check gate requiring `standalone=true`, `display_mode=standalone`, and
`interaction=status_strip_tap`. Its report path defaults to
`%TEMP%\sysmon-agent-deployed-verify-report.txt`.

For an isolated smoke-agent device check, keep the smoke agent alive temporarily and bind it
to the LAN:

```bash
SYSMON_VERIFY_BIND=0.0.0.0 SYSMON_VERIFY_HOLD=120 SYSMON_VERIFY_REQUIRE_DEVICE=1 ./verify.sh
```

Open one of the printed candidate URLs from a mobile browser during the hold. Strict device
mode requires a fresh client check whose user agent contains a mobile-device token and
reports non-zero viewport dimensions. On Linux, if `qrencode` is installed, `verify.sh`
prints a terminal QR code for the first candidate URL.

---

## Requirements

Full host-side dependency matrix (Go version, runtime tools per platform, optional
PowerShell/LibreHardwareMonitor/Node) is in [REQUIREMENTS.md](REQUIREMENTS.md).

---

## License

Licensed under the Creative Commons Attribution-NonCommercial 4.0 International
License (CC-BY-NC 4.0). See [LICENSE](LICENSE) for the full text, or
<https://creativecommons.org/licenses/by-nc/4.0/>.

In short: you are free to use, share, and adapt this work **for non-commercial
purposes**, as long as you give appropriate credit. Commercial use requires a separate
license from the maintainer.
