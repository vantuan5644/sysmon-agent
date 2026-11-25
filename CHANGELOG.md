# Changelog

Versions are plain numbers. What each one changes is written here, not in its name.
The full notes of a published release are on its
[GitHub release page](https://github.com/vantuan5644/sysmon-agent/releases).

## 0.2.9 (2026-11-25)

Host alerts that reach you with no dashboard open, Docker containers, internet
latency, much richer AMD, Intel and Apple GPU readings, and a guard against DNS
rebinding. Dashboard build v178 to v179.

### New

- **Host alerts and push notifications.** The agent can notify you when something
  stays wrong while no dashboard is open: CPU or memory use, GPU or CPU temperature,
  any temperature sensor, free space on a mount, or a new agent release. Each rule has
  a hold time (15 s), a release time with hysteresis (4 s), a cooldown (10 min) and an
  optional resolved notice. Notices go to ntfy (with an optional token), to a JSON
  webhook, or to paired browsers through Web Push, implemented with the standard
  library only (RFC 8291 and 8292). Configure it with `-alert-*`, `-ntfy-url`,
  `-alert-webhook-url` and `-web-push` (or their `SYSMON_*` variables), or with
  `alerts.json` beside the settings file. It is off by default. `GET /api/alerts`
  shows rules, active alerts and delivery without secrets, and page 2 of the dashboard
  gains "Notify this device" and "Send test".
- **Docker containers.** `-docker` (`SYSMON_DOCKER`) adds a Containers panel with each
  container's state, health, uptime, CPU and memory, read from the Docker Engine API
  every 15 s while a dashboard is open. The agent only reads: it never starts, stops
  or executes anything. The endpoint is `-docker-host` (`SYSMON_DOCKER_HOST`), else
  `DOCKER_HOST`, else the platform's default socket; Windows needs a `tcp://`
  endpoint. Reading the Docker socket needs the `docker` group, which is equivalent to
  root on that host, so grant it knowingly.
- **Internet latency.** The NET card shows the internet round trip beside the uplink
  name: a TCP connect to `-latency-target` (`SYSMON_LATENCY_TARGET`, default
  `1.1.1.1:443`, `off` disables) every 5 s while someone is watching.
  `metrics.network.latency` reports the last, average, minimum and maximum round trip
  and the loss over the last five minutes.
- **Quota pace forecast.** The AI page forecasts each 5-hour and weekly window beside
  its reset countdown: "full in 1h20m" when the pace so far would fill it before it
  resets, "limit reached" at 100%, otherwise "on pace" or "plenty left". It stays quiet
  for the first 4% of a window and under 1% used. Quota rows gain `window_seconds`.
- **Linux AMD and Intel GPUs report much more.** A real model name (from
  `product_name` or pci.ids), power with its limit, edge, hotspot and VRAM temperatures
  with their critical values, the shader clock, fan speed, and shared VRAM plus GTT
  memory on APUs. Discrete cards are listed before integrated ones, and a sleeping card
  reads idle without being woken. A bounded DRM fdinfo sampler, with no subprocesses,
  gives Intel GPUs a usage figure, shows per-engine load, and fills the process table's
  GPU column on Linux. New optional fields: `power_limit_watts`, `clock_mhz`, `fan_rpm`,
  `hotspot_critical_celsius`, `vram_critical_celsius`, `memory_kind`, `integrated`.
- **Apple silicon GPU on macOS.** Usage, Renderer and Tiler load, and in-use memory
  (out of physical memory, which the GPU shares), read from the IORegistry without
  privileges. GPU power and temperature remain unavailable.
- **The mic and camera indicators name the app using them.** `device_activity.*.apps`
  lists the holding processes by name (never command lines), up to five. On Linux they
  come from PipeWire or PulseAudio capture clients, ALSA capture owners and
  `/dev/video*` holders; when only the sound server is found it is reported as such
  instead of a guessed client. On Windows they come from Core Audio capture sessions
  and the privacy records. The dashboard shows the first name in place of "Busy".

### Fixed

- **The agent answers only host names it knows**, a guard against DNS rebinding.
  Allowed without configuration: IP addresses, `localhost`, `*.local`, `*.ts.net`, the
  machine's hostname, a `-bind` name and the `-mdns-name` label. Any other name gets
  421 Misdirected Request naming the new `-allowed-hosts` flag (`SYSMON_ALLOWED_HOSTS`;
  comma-separated, `*.example.org` allows every name below it). **If you reach an agent
  through another DNS name, add it with `-allowed-hosts` before upgrading.**
- **A stalled response body no longer freezes the dashboard.** Requests now time out on
  the body as well as the headers, so a body that stops arriving no longer stops
  metrics polling until a reload.
- **A failed update check is retried within minutes**: 15 minutes, backing off to at
  most 4 hours, instead of waiting a full day.
- **The dashboard says when its own script fails.** A "Dashboard script error … —
  reload" banner appears when the script fails to load, fails to parse, throws, or does
  not start within 10 seconds.
- **The live quota pollers are gentler and survive restarts.** The Claude poller backs
  off 1, 2, 4, 8, then 15 minutes after failures, never faster than its 5-minute
  cadence, and honours a 429's `Retry-After` up to an hour. After a 401 or 403 it does
  not re-send the rejected token: it re-reads the credentials every minute, sends a new
  token at once, and retries the same one only after 30 minutes. The Codex poller backs
  off 2, 4, 8, 16, then 30 minutes. Both keep their schedule and last good reading
  (never a token) in a private `quota-state.json`, so a restart neither polls straight
  away nor shows an empty page.

## 0.2.8 (2026-11-18)

The AI usage charts stop costing a CPU core every 30 seconds. The dashboard is
unchanged (build v178).

### Fixed

- **The token charts read only what is new in each transcript.** Every 30 seconds the
  agent used to read and decode every Claude Code and Codex transcript written in the
  last seven days, from the start. With a few large Codex sessions that came to about
  900 MB a pass: a full CPU core for several seconds, every 30 seconds, enough to make
  the fans rise and fall on that rhythm. Each pass now resumes every file where the
  last one stopped and parses only the lines added since, so a quiet pass takes
  milliseconds and reads almost nothing. A transcript that was rewritten, not appended
  to, is noticed and read again from the start. The charts show the same numbers as
  before.

## 0.2.7 (2026-11-16)

Checking for an update no longer waits a day. Dashboard build v177 to v178.

### New

- **Tapping the status strip checks for an update.** The tap that refreshes the
  dashboard now also asks the agent to look for a newer release right away, on the
  agent that served the dashboard, running a release, from a paired device (or any
  device when pairing is off). The strip says `Checking for updates`, then
  `Up to date (0.2.7)`, `Update 0.2.8 available` (with the banner) or
  `Update check failed`. The agent asks GitHub at most once a minute and answers a tap
  inside that minute with the last result, its own daily check included. Nothing
  changes where the update check is turned off, on a development build, or while
  another host is in view. The route is `POST /api/update/check`.
- **Closing the update banner no longer hides that release for good.** Its × used to
  hide a version in that browser until a newer one came out, with no way back. A tap
  that finds the release now shows its banner again.

## 0.2.6 (2026-11-16)

Updates on Linux, and three fixes to how older installs update. Dashboard build v176 to
v177.

### New

- **Linux agents update themselves.** `sudo sysmon-agent -update` installs the latest
  release: it downloads `sysmon-agent-linux-<arch>`, checks it against
  `SHA256SUMS.txt` and against its own `-version`, stops the systemd units running that
  binary (system units and `systemctl --user` ones), swaps it, starts them again, and
  rolls back if a unit does not stay up. Run `sudo sysmon-agent -update-setup` once and
  the dashboard's Update button does the same: the agent drops a request file in a
  folder its own account owns, and a root `sysmon-agent-update.path` unit starts the
  update. The request carries nothing the updater trusts; it fetches and verifies the
  release itself. The result shows in the dashboard, a refusal included.
- **The update banner says what to run where the dashboard cannot update.** A Linux
  host before `-update-setup` shows the command (`sudo .../sysmon-agent -update`)
  in place of the Update button.

### Fixes

- **An upgrade from before pairing shows the PIN on the host's screen.** 0.2.1
  turned pairing on by default, so a host upgraded from 0.2.0 or older came up asking
  every dashboard for a PIN, with nothing on the host saying where to get one. On its
  first start with no paired device, the agent now opens a pairing window and shows
  the PIN on the host: a message box on Windows, a desktop notification on Linux. It
  does this once, never when `-pairing` or `SYSMON_PAIRING` is set, and a host with no
  screen logs the `-pair` command instead.
- **Windows "Installed apps" shows the version that runs.** An update from the
  dashboard replaced the binary but left the installer's version in Installed apps,
  so it kept showing the old one. The service now corrects the record at start.
- **Development builds no longer check GitHub.** A build without a version could never
  be updated, yet it asked GitHub for a new release every day.
- **A Start search finds Pair a device (Windows).** The Start menu shortcut is now
  "Pair a device - Sysmon Agent (System Monitor)", since Start search matches only the
  words of a shortcut's name: typing `sysmon`, `system monitor` or `pair` finds it.
  Before, `sysmon` found only the dashboard. Setup removes the old shortcut on upgrade.

## 0.2.5 (2026-11-15)

The memory junction on Windows. Dashboard build v176, unchanged.

### New

- **VRAM temperature on Windows.** The GPU card shows the memory junction (VRAM) after
  the hotspot, and the GPU overheat guard judges it at the same lines as on Linux (warn
  100 °C, critical 106 °C), on NVIDIA cards with GDDR6X or GDDR7 memory. It comes from
  LibreHardwareMonitor's `GPU Memory Junction` sensor, in the same reading as the
  hotspot, so the guard's 2-second check costs nothing more. Cards with older memory
  have no such sensor and show none.

## 0.2.4 (2026-11-15)

The Windows network total counts each adapter once. Dashboard build v176, unchanged.

### Fixes

- **The network total counts each adapter once (Windows).** Windows lists every
  network filter driver bound to an adapter (QoS Packet Scheduler, the WFP filters, a
  virtual switch extension) as an interface of its own with the adapter's own byte
  counts, and the agent added them all up. A download at 133 MB/s on one Ethernet port
  showed as about 520 MB/s, four times over. Filter interfaces are now skipped.

## 0.2.3 (2026-11-15)

Dashboard build v173 to v176.

### New

- **No PIN to watch the host from the host.** A browser on the host itself, at
  `http://localhost:9099`, shows the dashboard without pairing, view only: the agent
  already answered its reads. Its controls and settings still need a PIN, since the
  agent cannot tell that browser from any other program on the machine; using one
  opens the PIN form, with a way back to the dashboard.
- **One `.local` name for a machine that boots two systems.** `-mdns-name <label>` (or
  `SYSMON_MDNS_NAME`) gives the agent a `.local` name other than its hostname's, for
  example the name the other system on the same machine uses. The agent answers mDNS
  for that name itself: with this host's IPv4 address on the asker's network, never
  while another host answers for the name, and with a goodbye when it stops. The name
  is added to the agent's certificate and opened by pairing QR codes, so a Home Screen
  app keeps working whichever system the machine booted.
- **The GPU guard judges the memory junction too (Linux).** Where the hotspot helper is
  installed, it also reads the GDDR6X memory junction temperature, which `nvidia-smi`
  does not report on GeForce cards. The guard treats it as a second sensor: warning at
  100 °C, critical at 106 °C, with the same staged action as the hotspot, before the
  card's own memory throttling at about 110 °C. The dashboard shows VRAM after the
  hotspot.

### Fixes

- **A lost memory reading is no evidence of cooling.** While an episode runs, a junction
  reading that goes missing keeps the lowered power limit and the repeated alarm, as a
  lost hotspot reading already did. A helper that cannot read the register says so on
  the dashboard (`vram_error`).
- **The host's system shows in full on a phone.** The line under the host name is now
  two lines, the system and architecture, then the kernel or Windows build, and the
  name takes the width they need. On one line, a phone cut the build off.
- **A smaller status strip.** The version badge shows just the number (`0.2.3`); its
  colour already tells a release from a development build. The dashboard build badge
  (`v175`) shows only when it tells something: on a development build, on an agent too
  old to report its version, or on a host whose build differs from the dashboard's.

## 0.2.2 (2026-11-13)

Clearer pairing directions. The PIN form on the host says where its PIN comes from, and
the directions to Paired devices name the second page.

## 0.2.1 (2026-11-13)

Device pairing: the agent shows its data and runs its controls only for devices paired
with a PIN, and serves network clients over HTTPS with a certificate of its own.

## 0.2.0 (2026-11-13)

GPU overheat guard, host switching, the AI usage page and camera control.

## 0.1.6 (2026-07-21)

Fix for an update banner that stayed after the update.

## 0.1.5 (2026-07-21)

Auto-update.

## 0.1.4 (2026-07-21)

CPU power that matches AMD Adrenalin.

## 0.1.3 (2026-07-01)

Windows media and lock controls fixed.

## 0.1.1 (2026-06-29)

The CPU gauge shows the core clock.

## 0.1.0 (2026-06-22)

Windows installer you can double-click.
