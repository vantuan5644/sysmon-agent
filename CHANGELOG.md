# Changelog

Versions are plain numbers. What each one changes is written here, not in its name.
The full notes of a published release are on its
[GitHub release page](https://github.com/vantuan5644/sysmon-agent/releases).

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
