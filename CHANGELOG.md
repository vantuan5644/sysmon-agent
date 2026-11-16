# Changelog

Versions are plain numbers. What each one changes is written here, not in its name.
The full notes of a published release are on its
[GitHub release page](https://github.com/vantuan5644/sysmon-agent/releases).

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
