# Sysmon Agent

**Turn any old phone into a live system monitor for your PC.** One small binary serves a
beautiful dashboard — CPU, GPU, RAM, disk, network, temperatures, and power — as an
installable web app you add to your Home Screen. The host does all the work; the device
just paints the screen. Run it on several machines and one Home Screen app switches between
all of them.

<p align="center">
  <img src="assets/screenshot-linux.JPEG" alt="Sysmon Agent dashboard monitoring a Linux host" width="760">
  <br>
  <em>Linux host (hostname blurred)</em>
  <br><br>
  <img src="assets/screenshot-windows.png" alt="Sysmon Agent dashboard monitoring a Windows host" width="760">
  <br>
  <em>Windows host (hostname and Wi-Fi name blurred)</em>
</p>

---

## See it live

The dashboard below is running on an **iPhone 8 Plus (2017)** as a Home-Screen web app —
**no cable, no companion app to install, no GPU.** The phone is just on Wi-Fi painting
HTML; every sensor is read on the host PC and pushed over the network. That's why a
years-old handset with nothing plugged in makes a perfect, near-zero-power desk monitor.

| Idle | Under CPU load |
| :---: | :---: |
| <img src="assets/idle.gif" alt="Dashboard idle on an iPhone 8 Plus" width="420"> | <img src="assets/under-load.gif" alt="Dashboard under CPU load on an iPhone 8 Plus" width="420"> |
| Gauges tick live at a glance | Same phone, host under load — the CPU **clock ring fills** and the temperature crosses its threshold and turns **amber**, in real time |

---

## Highlights

- 📱 **Installable PWA** — add it to any phone, tablet, or browser Home Screen. Runs great
  on old hardware.
- 🪶 **Light on the device** — the host collects everything and streams it; the phone only
  renders, so there's **no GPU use and almost no battery/CPU cost** on the device. Old,
  wireless, and always-on is exactly the point.
- 🧊 **Single binary, zero dependencies** — stdlib-only Go with all assets embedded. One
  file per OS, nothing to install alongside it.
- 🖥️ **Full-system telemetry** — CPU, GPU, RAM, disk, network, temperatures, and power,
  refreshing live over Server-Sent Events.
- 🎛️ **At-a-glance gauges** — concentric rings: CPU load with the **core clock** inside,
  GPU load with VRAM, RAM, and network download with upload. Per-core load bars on the CPU
  card, a short live trend on the others, and amber/red warning thresholds.
- 📋 **Apps, drives and alerts** — a page of the busiest apps or processes (CPU, RAM, GPU
  memory, disk I/O; tap a column to sort), and a status page with each drive's capacity and
  temperature, threshold alerts, and any sensor that could not be read.
- 🌙 **Made for always-on** — dim mode, a slow screen shift against burn-in, keep-awake, and
  pause, one tap each.
- 🔘 **Quick controls** — mute the mic, turn local cameras off, play/pause media, mute the
  speaker, and lock the screen from a control row above every page, with live mic and camera
  activity indicators.
- 🔀 **One app, every host** — tap the hostname to switch between every machine running the
  agent, found on your tailnet automatically or listed with `-peers`. One Home Screen app
  covers them all, with no SSH and nothing extra on the phone. See
  [Watch several hosts](#watch-several-hosts).
- 🧮 **AI usage page** — an optional fourth page with Claude Code plan usage (5-hour
  session, weekly, per-model weeklies, usage credits), Codex quota, and a seven-day token
  chart for each. Reads the local files both tools already write; hidden unless you point
  the agent at a config directory.
- ♻️ **Graceful degradation** — a sensor that can't be read shows `unavailable` with a
  reason; it never breaks the rest of the dashboard.
- 🐧 🪟 **Cross-platform** — Linux (`/proc` + sysfs + RAPL) and Windows (native Win32 APIs +
  an embedded LibreHardwareMonitor bridge for CPU power, board temps, and the GPU hotspot).

---

## Get started

### Windows — one click

Download the latest **`SysmonAgent-Setup-<version>.exe`** from the
[**Releases page**](https://github.com/vantuan5644/sysmon-agent/releases/latest) and
double-click it. The wizard installs a background service, opens the firewall, and adds a
dashboard shortcut. The build is unsigned, so Windows SmartScreen shows a one-time
**"Windows protected your PC" → More info → Run anyway**.

### Linux / build from source

Needs **Go 1.22+** (only to build — a prebuilt binary has no Go requirement).

```bash
git clone https://github.com/vantuan5644/sysmon-agent system-monitor
cd system-monitor
./build.sh                 # -> ./sysmon-agent
./sysmon-agent             # serves on 0.0.0.0:9099
```

Open `http://HOST_IP:9099/` from any device on the same network.

### Add it to your phone

The dashboard is an installable PWA. Browsers only allow "install" over HTTPS, and the
easiest way to get that is [Tailscale Serve](https://tailscale.com/kb/1312/serve) (no
certificates to manage):

```bash
# agent already listening on 127.0.0.1:9099
sudo tailscale serve --bg --https=9443 http://127.0.0.1:9099
```

Open `https://TAILSCALE_HOST:9443/` on the device → **Add to Home Screen** (Safari) or
**Install app** (Chrome) → set the device's auto-lock to *Never* for an always-on monitor.
Any other HTTPS reverse proxy (Caddy, nginx, Cloudflare Tunnel) works too. On Windows,
`install-windows.ps1 -Action Install -TailscaleServe` sets up the same Serve route; after the
one-click setup, run it from an elevated PowerShell as
`& "C:\Program Files\Sysmon Agent\install-windows.ps1" -Action Install -TailscaleServe`.
Watching several machines? Add just one of them; the others are a tap away inside the app.

---

## Watch several hosts

Run the agent on each machine you want to watch, then add **one** of them to your Home
Screen. Tap the hostname at the top of the dashboard (a `⌄` appears once there is another
host) and pick a machine; the device remembers the choice. While another host is in view,
its name and the control row are tinted and the line under the name reads `via <agent>`:
the mic, camera, media, speaker and lock buttons act on the host in view, and Lock's confirm
prompt names it.

<p align="center">
  <img src="assets/host-picker.png" alt="The host list open over the dashboard: three agents, the serving one marked 'this agent' and checked" width="300">
  &nbsp;&nbsp;
  <img src="assets/host-remote.png" alt="Another machine in view: its name and the control row tinted, and 'via' plus the serving agent under the name" width="300">
  <br>
  <em>Left: the host list. Right: another machine in view, relayed by the agent the app was
  installed from. Hostnames blurred.</em>
</p>

### How it works

There is no SSH and nothing to install on the phone. Every machine's own agent collects its
own metrics, exactly as it does for a single host. The agent you installed the app from
forwards the dashboard's requests to the others over ordinary HTTP(S):

```
iPhone app ──HTTPS──▶ agent on desk-pc    (the one the app was installed from)
                          │  GET https://nas.<tailnet>.ts.net:9443/api/stream
                          ▼
                      agent on nas         (collects nas's own metrics)
```

1. **Listing hosts.** The serving agent runs `tailscale status --json` on its own machine to
   see which machines are online, then sends one `GET /api/status` to each of them on `:9443`
   (Tailscale Serve) and `:9099`. Machines that answer as a Sysmon agent are listed, along with
   any you pinned with `-peers`.
2. **Viewing a host.** The dashboard asks its own agent for `/api/hosts/<id>/stream`. That
   agent opens a connection to the other agent's `/api/stream`, the same live feed that
   agent's own dashboard reads, and copies it back as it arrives. Status, AI usage and
   metrics polling travel the same way.
3. **Buttons.** Lock, mute and the rest go the same route, as a `POST` to the other agent's
   `/api/control`.

The app stays on the agent it was installed from because of how iPhone Home Screen apps
work: one that opens another address drops into a Safari sheet, and an HTTPS page may not
fetch a plain-HTTP agent at all. Forwarding needs nothing new on the other machines, so
agents on older releases still appear; a feature an older build lacks only blanks that panel.

### Where hosts come from

- **Tailscale discovery**, on by default (`-peer-discovery=false` turns it off). Phones,
  offline machines and nodes shared in from other tailnets are skipped, and only replies from
  a Sysmon agent are listed.
- **A pinned list**: `-peers "nas=https://nas.example.ts.net:9443,http://10.0.0.5:9099"`
  (env `SYSMON_PEERS`), for machines outside Tailscale. The same list can go on every host;
  an entry pointing at the agent itself is dropped.

### Network and safety

- The dashboard names a host only by id. The agent looks the address up in its own list and
  answers anything else with 404, so it cannot be used to reach other addresses.
- Dim, screen shift, client checks and self-update stay with the agent serving the app:
  they describe the viewing device or replace that agent's own binary.
- A forwarded button press must pass the serving agent's same-origin check first, exactly
  like a local one.
- Between agents, a tailnet address keeps traffic inside Tailscale's encrypted tunnel. A
  pinned `http://` LAN address is plain HTTP on your LAN, the same as opening that agent
  directly.
- Discovery runs only when a dashboard asks for the host list (page load, opening the list,
  or a list over five minutes old), at most once every 10 seconds, with a 2.5-second timeout
  per request.
- A forwarded live stream adds about 230 KB/s through the serving agent while another host
  is in view. Install the app from a machine that stays on: if it sleeps, every host view goes
  with it.

---

<details>
<summary><b>The dashboard: pages and buttons</b></summary>

Swipe sideways between pages; the dots at the bottom show where you are.

1. **Live metrics** — the CPU, GPU, RAM and NET gauges.
2. **More status** — threshold alerts, every drive with its capacity and temperature, and an
   Issues list of sensors that could not be read (with the reason). "All clear" when empty.
3. **App details** — the busiest apps, or individual processes, by CPU, RAM, GPU memory and
   disk I/O. Tap a column header to sort.
4. **AI usage** — optional, see below. Hidden unless configured.

Above every page:

- **Hostname** — tap it to switch hosts when there is more than one.
- **Status strip** — connection state, agent version, uptime and sample age. Tap anywhere in
  the header to refresh now.
- **Alert chip** (`⚠ 3`) — appears when a threshold is crossed; tap it to jump to the alerts.
- **◐ ⇄ ☀ Ⅱ** — dim mode, screen shift (moves the layout a few pixels over time against
  burn-in), keep the screen awake, and pause updates.
- **Control row** — Mic, Camera, Media, Speaker and Lock for the host in view.
</details>

<details>
<summary><b>Run options &amp; endpoints</b></summary>

Defaults bind all interfaces on port `9099` (good for a trusted LAN or a Tailscale network).

```bash
./sysmon-agent                                       # 0.0.0.0:9099
./sysmon-agent -bind 127.0.0.1 -port 9099
./sysmon-agent -settings ./settings.json
./sysmon-agent -tls -cert ./cert.pem -key ./key.pem  # optional direct TLS (proxy is preferred)
SYSMON_BIND=127.0.0.1 SYSMON_PORT=9099 ./sysmon-agent
```

| Endpoint | Purpose |
| --- | --- |
| `/` | the dashboard |
| `/healthz` | cheap process liveness |
| `/readyz` | proves metrics are actually collectable |
| `/api/status` | agent metadata + active display settings |
| `/api/metrics` | the live metrics payload |
| `/api/stream` | Server-Sent Events live metrics push |
| `/api/osd` | small cached CPU/GPU payload for an on-screen display overlay |
| `/api/quota` | Claude Code quota + token history (AI usage page) |
| `/api/codex-usage` | Codex quota + token history (AI usage page) |
| `/api/hosts` | the host list for the host picker (`?refresh=1` asks for a rescan) |
| `/api/hosts/{id}/…` | `metrics`, `stream`, `status`, `quota`, `codex-usage` and `control`, forwarded to another agent |
| `/api/settings` | GET or POST display settings (dim, screen shift, update check) |
| `/api/control` | POST a host control: mic, camera, media, speaker, lock |
| `/api/update` | POST to self-update (Windows service only) |

When bound to a wildcard like `0.0.0.0`, startup logs print likely dashboard URLs
(Tailscale addresses first), skipping virtual/container interfaces. Open the port through
the firewall only on trusted LAN or Tailscale networks.
</details>

<details>
<summary><b>Configuration (flags / env)</b></summary>

Display settings persist with `-settings PATH` (or `SYSMON_SETTINGS`). Refresh interval and
warning thresholds are **host-side** config (flags/env, not touch controls); a `0`/unset
value keeps the saved default.

| Flag / env | Range / default | Meaning |
| --- | --- | --- |
| `-bind` / `SYSMON_BIND` | `0.0.0.0` | HTTP bind address |
| `-port` / `SYSMON_PORT` | `9099` | HTTP listen port |
| `-fast-ms` / `SYSMON_FAST_MS` | min 100, default 200 | fast-lane (CPU/RAM) interval |
| `-slow-ms` / `SYSMON_SLOW_MS` | min 500, default 1500 | slow-lane (power/temps/disk/net/GPU) interval |
| `-refresh-ms` / `SYSMON_REFRESH_MS` | {250,500,1000,2000} | dashboard refresh interval |
| `-cpu-warn` / `-mem-warn` / `-disk-warn` / `-gpu-warn` (`SYSMON_CPU_WARN`, …) | 50–90 | utilization warn thresholds (%) |
| `-temp-warn` / `SYSMON_TEMP_WARN` | 50–90 (°C) | temperature warn threshold |
| `-settings` / `SYSMON_SETTINGS` | path | optional JSON file for persisted settings |
| `-claude-config-dir` / `SYSMON_CLAUDE_CONFIG_DIR` | path | Claude Code config dir for the AI usage page (default `$CLAUDE_CONFIG_DIR`, then `$HOME/.claude`; missing = Claude section hidden) |
| `-claude-quota-poll` / `SYSMON_CLAUDE_QUOTA_POLL` | bool, off | also poll `api.anthropic.com` for quota instead of only reading local files |
| `-codex-config-dir` / `SYSMON_CODEX_CONFIG_DIR` | path | Codex config dir for the AI usage page (default `$CODEX_HOME`, then `$HOME/.codex`; missing = Codex section hidden) |
| `-codex-quota-poll` / `SYSMON_CODEX_QUOTA_POLL` | bool, off | refresh Codex quota every 5 min through the installed Codex CLI |
| `-codex-binary` / `SYSMON_CODEX_BINARY` | path | Codex CLI used by the poll (default: `codex` on `PATH`) |
| `-peers` / `SYSMON_PEERS` | `[name=]URL,…` | other agents to list in the host picker |
| `-peer-discovery` / `SYSMON_PEER_DISCOVERY` | bool, on | find agents on the tailnet when the host list is opened |
| `-no-update-check` / `SYSMON_UPDATE_CHECK=0` | off | turn off the update check and self-update |
| `-tls` / `SYSMON_TLS` | bool | enable direct TLS with `-cert`/`-key` (`SYSMON_CERT`/`SYSMON_KEY`) |
| `-self-check` / `-wait-health` / `-wait-ready` | bool | run in-process checks, or wait for a running agent's health or readiness, then exit |
| `-version` | | print the version and exit |
| `SYSMON_LHM_DAEMON` (Windows) | on | `0` uses the one-shot LibreHardwareMonitor bridge: slower, and no drive temperatures |
| `SYSMON_LHM_STORAGE_MS` (Windows) | 15000 | how often each drive's temperature is refreshed; `0` turns drive temperatures off |

Invalid persisted settings are backed up with a `.bad-...` suffix and the agent starts with
defaults so the monitor still comes up.
</details>

<details>
<summary><b>Deploy as a service (Linux systemd / Windows service)</b></summary>

**Linux.** Two units ship under `deploy/`:

- [`deploy/sysmon-agent.user.service`](deploy/sysmon-agent.user.service) (recommended for
  desktops) runs under your per-user systemd manager so the **host controls** (mic/media/
  speaker/lock) can reach PipeWire/PulseAudio, `playerctl`, and `loginctl lock-session`.
- [`deploy/sysmon-agent.service`](deploy/sysmon-agent.service) runs as a system service for
  headless hosts.

```bash
./build.sh
sudo install -m0755 sysmon-agent /usr/local/bin/sysmon-agent
# user unit:
install -m0644 deploy/sysmon-agent.user.service ~/.config/systemd/user/sysmon-agent.service
systemctl --user enable --now sysmon-agent.service
sudo loginctl enable-linger "$USER"
```

[`run-linux.sh`](run-linux.sh) rebuilds, reinstalls to `/usr/local/bin`, and restarts the
right unit. Don't add `ProtectSystem`/`PrivateDevices`/`ProtectKernelModules` — they break
the `/proc`, sysfs, hwmon, and RAPL reads.

**CPU package power (recommended).** The kernel ships the RAPL `energy_uj` counter as
`0400` root-only, and both units run the agent unprivileged with `NoNewPrivileges=true`, so
without this rule CPU power reports "present but not readable". The rule makes the counter
world-readable (power readings are low-sensitivity) and is a no-op on hosts without RAPL:

```bash
sudo install -m0644 scripts/udev/99-powercap-rapl.rules /etc/udev/rules.d/
sudo udevadm trigger --subsystem-match=powercap
```

It matches udev's `change` action as well as `add`, so the trigger applies it immediately,
with no reboot or agent restart.

**Camera button (optional).** Both units run the agent as a regular user, and Linux camera
control writes each USB video interface's `authorized` file plus `/sys/bus/usb/drivers_probe`,
which are root-only by default. A udev rule and a tmpfiles entry hand them to a
`sysmon-camera` group:

```bash
sudo groupadd -r sysmon-camera && sudo usermod -aG sysmon-camera "$USER"
sudo install -m0644 scripts/udev/70-sysmon-camera.rules /etc/udev/rules.d/
sudo install -m0644 scripts/tmpfiles/sysmon-camera.conf /etc/tmpfiles.d/
sudo systemd-tmpfiles --create /etc/tmpfiles.d/sysmon-camera.conf
sudo udevadm trigger --subsystem-match=usb --action=change
```

Log out and back in (or reboot) so the user manager picks up the group. Until then, Camera
reports that control requires root or the `sysmon-camera` group.

**Windows.** `install-windows.ps1` registers a native `SysmonAgent` service (pure stdlib SCM
integration — the same binary is both console app and service). From an elevated PowerShell:

```powershell
.\install-windows.ps1 -Action Install
.\install-windows.ps1 -Action Install -TempWarn 85   # AMD: Tctl idles hot, 70 C warns constantly
.\install-windows.ps1 -Action Install -TailscaleServe  # also publish https://<machine>.<tailnet>.ts.net:9443
.\install-windows.ps1 -Action Status      # checks /readyz, reports settings, AI usage sources, Serve
.\install-windows.ps1 -Action Update      # download + verify + swap + rollback
.\install-windows.ps1 -Action Uninstall
```

The service runs as **LocalSystem**, which is why it can load the LibreHardwareMonitor
kernel driver every boot. Don't run it interactively under an unprivileged account for
production, or CPU power and board temps degrade.

**Updating.** Three paths, in order of convenience:

1. **In-dashboard.** The agent checks for a newer release once at startup and every 24 h,
   and the dashboard shows a `vX.Y.Z available - Update` banner. One tap downloads the new
   binary, **SHA-256 verifies it against the release's `SHA256SUMS.txt`**, then hands it to
   a detached helper that stops the service, swaps the binary, restarts, polls `/readyz`,
   and **rolls back to the previous binary if the new one fails readiness**. The dashboard
   reloads itself once the new build is up. No SmartScreen prompt — that only fires on an
   interactive double-click, not when the service replaces its own binary.
2. **`-Action Update`** — the same download/verify/swap/rollback driven entirely from the
   script, with no in-app network calls. Good for a weekly elevated Scheduled Task:
   ```powershell
   .\install-windows.ps1 -Action Update -DryRun                 # report only
   .\install-windows.ps1 -Action Update -Force                  # reapply same version
   .\install-windows.ps1 -Action Update -UpdateVersion v1.2.3   # pin or downgrade
   ```
3. **Re-run the installer.** `SysmonAgent-Setup-<ver>.exe` stops the running service and
   waits for it to release the binary before overwriting, so upgrading in place works
   without uninstalling first.

Self-update is confined to the **LocalSystem Windows service** — console runs and Linux
hosts refuse it (HTTP 501) and should use the installer or the systemd/package path. Every
release **must** publish `SHA256SUMS.txt`: it is what authenticates the download before a
SYSTEM-privileged process executes it, and both engines refuse to update without it.

**Turning the check off.** It is on by default. Toggle `update_check_enabled` via
`POST /api/settings`, or hard-disable it host-side with `-no-update-check` /
`SYSMON_UPDATE_CHECK=0` (flag/env wins over the setting). Disabled means no update calls at
all. With the AI usage page left in its default files-only mode (no `-claude-quota-poll`, no
`-codex-quota-poll`), `api.github.com` is the only internet endpoint the agent contacts.
Inside your own network it also asks other agents for their status when a dashboard opens
the host list (see [Watch several hosts](#watch-several-hosts); `-peer-discovery=false`
turns that off).
</details>

<details>
<summary><b>Metrics &amp; API</b></summary>

`GET /api/metrics` returns hostname/OS/arch/timestamp plus:

- **CPU** load overall and per core; package power (W) with core/SoC rails where exposed; the
  average and fastest-core clock, the highest clock observed since start, and the rated and
  base clocks; and the CPU temperature with the name of the sensor it came from.
- **GPU** usage/VRAM/temp/power (NVIDIA via `nvidia-smi`; AMD/Intel via DRM sysfs on Linux),
  plus the NVIDIA hotspot where it can be read (see below).
- **RAM** used/total/% and swap; **disks** per mounted local filesystem; **storage** per
  physical drive (model, size, capacity, temperature); **network** RX/TX per interface, the
  active uplink (Wi-Fi SSID or Ethernet link speed) and Tailscale state.
- **Processes**: the busiest apps and processes by CPU, RAM, GPU memory and disk I/O.
- **Device activity**: whether the microphone and camera are in use, for the control row.
- **Temperatures** from Linux hwmon/thermal or Windows ACPI/LibreHardwareMonitor, and **PSU**
  total output power when a USB-linked smart PSU is present (Windows LHM bridge).

Unavailable sensors come back as `available: false` with an error string instead of failing
the request; `collection_errors` rolls them up into compact `name: reason` summaries.

A **resident sampler** keeps one warm snapshot refreshed by a fast lane (CPU/RAM, ~5 Hz) and
a slow lane (power/temps/disk/net/GPU, ~0.7 Hz), so `/api/metrics` reads memory instead of
spawning a collection per request. Concurrent requests share one in-flight collection, and
`/api/stream` pushes fresh snapshots over SSE with a keepalive every 15 s.

`GET /api/osd` serves a small cached payload (CPU usage and package temperature, GPU usage
and temperature) for an on-screen display such as a game-streaming overlay, without waking
the full dashboard collector. On Windows an independent one-second loop asks the resident
LibreHardwareMonitor bridge for CPU/GPU sensors only, and only while an OSD client is
active; hardware readings older than 3.5 s are suppressed. Other platforms currently serve
CPU usage there and report the hardware fields unavailable.
</details>

<details>
<summary><b>Host controls &amp; capture activity</b></summary>

Mic, Camera, Media, Speaker, and Lock stay above every dashboard page. Capture buttons show
a compact `On · Idle` summary: control state first, usage second. For Mic, On means unmuted
and Off means muted; Camera uses On/Off/Mix for supported local devices. A green border means
On, independently of usage. **Busy** means capture use was detected and gets amber text and an
animated indicator; **Idle** means no use was detected within the available coverage; a dash
means unknown. Paused, disconnected, or observations older than ten seconds show unknown usage
without animation, and reduced-motion preferences keep indicators static.

Mute state is unknown until a control action returns; after that it shows the agent's last
action, so a mute toggled in another app does not appear. A muted microphone can still have an
open capture session: mute silences audio without closing the app's session. The animated bars
show that capture is in use; they carry no sound-level information.

**Camera** toggles supported local devices: Windows Camera-class devices and older USB video
devices, or Linux USB video interfaces. Windows needs Administrator or LocalSystem (the
installed service runs as LocalSystem); Linux needs root or the `sysmon-camera` group set up
in the deploy section above. Many virtual and network cameras are outside this control's
coverage, and disabling a camera can interrupt an ongoing call or camera-based sign-in.
Device failures and partial results are shown explicitly, and cameras that were already
disabled are left alone.

Camera control requires a persistent `-settings` path. Before changing devices the agent saves
recovery records at `<settings-path>.cameras.json`; the next Camera action restores only the
recorded devices, including after an agent restart. Missing devices keep their entries until
reconnected, so do not delete that file while cameras need restoring. On Linux only the video
interfaces are disabled, which keeps the microphone of a combined USB webcam working.

Activity is collected in the background, at most once every two seconds. Windows uses Core
Audio capture sessions plus best-effort microphone and camera privacy records from loaded user
profiles. Linux uses `pw-dump`, or PulseAudio recording streams via `pactl`, supplemented by
ALSA capture state and V4L2 device holders; a system service may not see the desktop session,
in which case usage stays unknown. A V4L2 handle means the device is open, which need not mean
frames are streaming. Treat the indicators as an activity aid; they cannot promise that every
capture path was inspected.
</details>

<details>
<summary><b>AI usage page (Claude Code + Codex)</b></summary>

An optional fourth swipe page: Claude Code plan usage (the 5-hour session window, the shared
weekly, any per-model weeklies, usage credits), Codex quota windows, and a rolling seven-day
token chart for each tool. It is **hidden unless at least one provider is configured**, so if
you use neither, the dashboard is exactly the three pages it has always been. A missing
directory disables only that provider.

```bash
./sysmon-agent -claude-config-dir ~/.claude -codex-config-dir ~/.codex
```

Claude defaults to `$CLAUDE_CONFIG_DIR`, then `$HOME/.claude`; Codex to `$CODEX_HOME`, then
`$HOME/.codex`. On the **Windows service** pass them explicitly (the installer defaults both
from the installing user's profile): the service runs as LocalSystem, whose home is the
systemprofile directory, so it can never discover your profile on its own.

**Token charts** are file-only for both tools and never read credentials. Claude totals come
from the response usage records under `projects/**/*.jsonl`, deduplicated per response;
Codex totals from `sessions/**/*.jsonl`. Each bar is one local calendar day and counts
input, cache, and output tokens once. Both trees are rescanned every 30 seconds.

**Claude quota.** By default the agent makes *no network calls for this at
all* — it reads two files Claude Code and its quota widget already maintain, `quota.json`
and `widgets/quota-live-cache.json`, and merges them: freshest source wins per window, and
rows only the API knows about (per-model weeklies, credits) are carried over labelled with
their own age rather than dropped. Files-first is deliberate — the usage endpoint
rate-limits hard, so a second poller would just fight whatever else is already polling.

`-claude-quota-poll` opts a host with no such widget into polling
`api.anthropic.com/api/oauth/usage` directly, every 5 minutes (backing off to 15 after a
rate-limit). The OAuth token Claude Code already stores is read per poll, sent only to
`api.anthropic.com`, and is never logged, persisted, or included in any response the agent
serves.

On the installed Windows service, pass `-ClaudeQuotaPoll` to `install-windows.ps1`
rather than hand-editing the service command line: `Get-BinaryPath` rebuilds that line
from the parameters on every install, so a manual edit is dropped by the next one.
Polling is what lets the page stay fresh with no Claude Code session open and no desktop
widget running — bounded by the OAuth token’s lifetime, since only Claude Code itself
refreshes it. Run one poller per account: the agent or the desktop widget, not both.

**Codex quota.** By default the quota windows come from the newest rate-limit event Codex
wrote to its session files, so they move only when Codex runs on this host. For current
numbers between conversations, add `-codex-quota-poll`: at startup and every five minutes
the agent starts a hidden, temporary `codex app-server --stdio` and calls
`account/rateLimits/read`. No conversation or model turn is started; Codex itself handles
authentication and the outbound request, using the configured directory as `CODEX_HOME`, and
the agent never parses, logs, or serves Codex credentials. Each lookup has a 20-second
timeout, and a failure keeps the last quota with an error and its real age. On the Windows
service use `install-windows.ps1 -CodexQuotaPoll` (optionally
`-CodexBinary C:\path\to\codex.exe`): the installer saves the CLI's absolute path, because
LocalSystem does not share your `PATH`.
</details>

<details>
<summary><b>NVIDIA GPU hotspot temperature</b></summary>

The GPU card adds `Hotspot NN°C` to its detail line when the hotspot sensor can be read
(`gpu.devices[].hotspot_temperature_celsius` in `/api/metrics`). It has no warning threshold
yet.

**Windows** needs nothing extra: the LibreHardwareMonitor bridge already reads `GPU Hot Spot`.
When two GPUs share a model name the reading stays unavailable, rather than risk showing it
on the wrong card.

**Linux** has no supported interface for it (`nvidia-smi` does not report it), so an optional
root helper reads it from a GPU register through a read-only `/dev/mem` mapping. Supported:
**RTX 3090** (`10de:2204`) and **RTX 4090** (`10de:2684`). The helper never writes to the GPU
and opens no socket; it publishes `/run/sysmon-gpu-hotspot/readings.json` every two seconds,
and the agent stays unprivileged and ignores readings older than six seconds. NVIDIA does not
document the register (the offset and decoding follow
[gddr6-core-junction-vram-temps](https://github.com/ThomasBaruzier/gddr6-core-junction-vram-temps/blob/6d8c5ecf633a8658d205fb2c24531bf87164912f/src/sensor.c)),
so compare the number against another tool before relying on it.

```bash
go build -trimpath -o sysmon-gpu-hotspot ./cmd/gpu-hotspot
./install-gpu-hotspot.sh                  # dry-run: supported GPU? lockdown? iomem=?
sudo ./install-gpu-hotspot.sh --apply     # installs deploy/sysmon-gpu-hotspot.service, starts it
cat /run/sysmon-gpu-hotspot/readings.json
sudo ./install-gpu-hotspot.sh --uninstall --apply
```

Two kernel settings can stand in the way, and the dry-run reports both:

- Kernels built with `CONFIG_IO_STRICT_DEVMEM` refuse `/dev/mem` reads of memory the nvidia
  driver has claimed unless `iomem=relaxed` is on the kernel command line. That widens
  `/dev/mem` access system-wide, so it is opt-in: `--enable-register-access` adds it on
  Limine (backing up `/etc/default/limine` first); for GRUB, systemd-boot and others the
  script prints the change to make by hand. Either way, reboot to activate it.
- Kernel lockdown, usually turned on by Secure Boot, blocks `/dev/mem` outright. The script
  warns about it and leaves it alone.
</details>

<details>
<summary><b>Platform notes</b></summary>

**Linux** — CPU/memory/disk/network from `/proc`; CPU package power from Intel/AMD RAPL
(`/sys/class/powercap`), unavailable on hosts without it and, for a non-root agent, until
`scripts/udev/99-powercap-rapl.rules` is installed (see the deploy section); temperatures from `/sys/class/hwmon`
and `/sys/class/thermal`. NVIDIA needs `nvidia-smi` in `PATH`; AMD via the `amdgpu` DRM
sysfs; Intel iGPU is best-effort. Container/bridge and remote-mount interfaces are skipped.

**Windows** — live CPU/memory/process/network/disk-capacity numbers come from native Win32
APIs, so an open dashboard does not spawn a PowerShell per sample; bounded PowerShell/CIM
queries remain only for slow-changing data (hardware identity, pagefile, physical-disk
discovery) and the ACPI fallback. CPU package power and CPU/board/RAM
temperatures aren't exposed by any native Windows API, so the agent ships an embedded
**LibreHardwareMonitor bridge** (loads `LibreHardwareMonitorLib.dll` directly — no GUI, no
WMI). Install it **machine-wide** plus **PowerShell 7+** once, elevated:

```powershell
choco install librehardwaremonitor -y                # machine-wide (recommended)
winget install --scope machine Microsoft.PowerShell  # pwsh — required for the bridge
```

LibreHardwareMonitor 0.9.5 and later read the CPU through the separate
[**PawnIO**](https://github.com/namazso/PawnIO.Setup) driver, which the LibreHardwareMonitor
download does not include. Without it CPU package power, the CPU temperature and the board's
sensors stay unavailable while GPU and PSU readings work. Install PawnIO once, then restart
the service.

A per-user install lands in a profile the LocalSystem service can't read, so those sensors
silently go unavailable. The same bridge also reports PSU output power for USB-linked smart
PSUs (Corsair HXi/RMi, NZXT, Seasonic, …).

**USB-attached drives** — an NVMe or SATA drive in a USB enclosure usually reports no
temperature on either OS, because a generic SMART query does not pass through the USB
bridge chip. The storage panel says so (`USB enclosure: bridge does not expose live SMART
temperature`) instead of reporting a broken sensor.
</details>

<details>
<summary><b>Build the Windows installer</b></summary>

The `dist/` flow turns the source into a double-clickable installer, cross-compilable from
Linux/macOS/Windows (no wine):

```bash
./dist/build-installer.sh 1.0.0     # -> dist/out/SysmonAgent-Setup-1.0.0.exe (~3 MB)
./dist/build-windows.sh   1.0.0     # just the standalone exe, no installer
```

It cross-compiles a stripped, version-stamped exe with an embedded icon, then wraps it in an
NSIS Modern-UI wizard that runs the existing `install-windows.ps1` (service + firewall +
recovery + `/readyz` readiness check) and registers a clean uninstaller. Needs **Go 1.22+** and
**NSIS** (`makensis`; `apt install nsis` / `brew install nsis` / AUR `nsis`); ImageMagick is
optional for icon regeneration. For public distribution, sign the exe + installer to avoid
the SmartScreen warning.
</details>

<details>
<summary><b>Verify &amp; test</b></summary>

```bash
go test ./...                  # unit tests
go run . -self-check           # in-process HTTP checks, no socket
./verify-no-listen.sh          # gofmt + tests + vet + self-check + cross-compile + JS verifiers
./verify.sh                    # real-host smoke: build, start on :19099, check API/PWA/settings, stop
./verify-deployed.sh           # checks an already-running service + published device URL
```

`verify-deployed.sh` waits for a fresh real Home-Screen status-strip tap, proving the
installed PWA path works. Windows equivalents: `verify-windows.ps1`,
`verify-deployed-windows.ps1`.
</details>

---

## Requirements

Full host-side dependency matrix (Go, per-platform runtime tools, optional PowerShell /
LibreHardwareMonitor / Node) is in [REQUIREMENTS.md](REQUIREMENTS.md).

## License

[Creative Commons Attribution-NonCommercial 4.0 (CC-BY-NC 4.0)](LICENSE) — free to use,
share, and adapt for **non-commercial** purposes with credit. Commercial use requires a
separate license from the maintainer.
