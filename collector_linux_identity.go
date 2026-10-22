//go:build linux

package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// resolveHardwareNames returns the host's static identity -- the CPU model,
// the RAM type/speed label and the running-vs-rated RAM speed -- resolving it
// exactly once and caching the result. None of it changes at runtime, and the
// RAM lookup reads the DMI table (root-only; see runDmidecodeMemory), so this
// keeps the cost off every slow pass and degrades to empty values.
func (c *systemCollector) resolveHardwareNames(ctx context.Context) (cpuName, memoryName string, memorySpeed MemorySpeed) {
	c.hardwareOnce.Do(func() {
		// The result is cached for the life of the process, so the caller's
		// cancellation must not reach it: the first caller can be a /readyz
		// request whose client has already given up, and a lookup that fails on
		// a canceled context would stay failed until restart. Each lookup
		// carries its own timeout.
		ctx := context.WithoutCancel(ctx)
		c.cpuName = readCPUModelName()
		c.memoryName, c.memorySpeed = readMemoryIdentity(ctx)
	})
	return c.cpuName, c.memoryName, c.memorySpeed
}

// readCPUModelName returns a cleaned CPU model from /proc/cpuinfo (e.g.
// "AMD Ryzen 9 7950X"), or "" if it cannot be read.
func readCPUModelName() string {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "model name") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			return cleanCPUModelName(parts[1])
		}
	}
	return ""
}

// spdEEPROMGlob matches the SPD EEPROMs the spd5118 driver (DDR5) exposes.
// They are world-readable, unlike the DMI table, so the rated speeds need no
// privilege even when the running speed does.
const spdEEPROMGlob = "/sys/bus/i2c/drivers/spd5118/*/eeprom"

// dmidecodeCandidates are checked when dmidecode is not on PATH: Debian and
// Ubuntu install it to /usr/sbin, which a user service's PATH may not carry.
var dmidecodeCandidates = []string{"/usr/sbin/dmidecode", "/usr/bin/dmidecode"}

// readMemoryIdentity returns the RAM label (e.g. "DDR5 · 6000 MT/s") and the
// running-vs-rated speed. The label is "" when dmidecode is missing or
// unreadable, and the dashboard falls back to the total size.
func readMemoryIdentity(ctx context.Context) (string, MemorySpeed) {
	out, reason := runDmidecodeMemory(ctx)
	memType, configured, smbiosRated := parseDmidecodeSpeeds(out)
	speed := buildMemorySpeed(memType, configured, smbiosRated, readSPDRating(), reason)
	if out == "" {
		return "", speed
	}
	return parseDmidecodeMemory(out), speed
}

// dmiMemorySnapshotPath is where sysmon-dmi-memory.service (installed by
// scripts/install-dmi-memory.sh) leaves `dmidecode -t 17` output at boot. A var
// so tests can point it at a fixture.
var dmiMemorySnapshotPath = "/run/sysmon-dmi/memory.txt"

// runDmidecodeMemory returns `dmidecode -t 17` output, or "" plus the reason it
// could not be read. dmidecode needs root to read the DMI table, and the agent's
// units set NoNewPrivileges, so a non-root agent cannot escalate even through
// sudo: it reads the boot snapshot a root oneshot unit wrote instead. Only a
// root agent (the system unit) runs dmidecode itself.
func runDmidecodeMemory(ctx context.Context) (string, string) {
	if data, err := os.ReadFile(dmiMemorySnapshotPath); err == nil && parseDmiHasSpeed(string(data)) {
		return string(data), ""
	}
	if os.Geteuid() != 0 {
		return "", "DMI table is root-only: run scripts/install-dmi-memory.sh"
	}
	path := findDmidecode()
	if path == "" {
		return "", "dmidecode is not installed"
	}
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := trackedOutput(exec.CommandContext(callCtx, path, "-t", "17"))
	if err != nil || !parseDmiHasSpeed(string(out)) {
		return "", "dmidecode returned no memory speed"
	}
	return string(out), ""
}

func findDmidecode() string {
	if path, err := exec.LookPath("dmidecode"); err == nil {
		return path
	}
	for _, candidate := range dmidecodeCandidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

// parseDmiHasSpeed reports whether dmidecode output carries a module speed, so
// an empty or truncated snapshot falls through instead of counting as a read.
func parseDmiHasSpeed(out string) bool {
	_, configured, rated := parseDmidecodeSpeeds(out)
	return configured > 0 || rated > 0
}

// readSPDRating decodes every readable DDR5 SPD EEPROM and combines them. It
// returns the zero rating when the spd5118 driver is not loaded (DDR4 hosts,
// boards that hide the SMBus), and buildMemorySpeed then falls back to SMBIOS.
func readSPDRating() memoryRating {
	paths, _ := filepath.Glob(spdEEPROMGlob)
	ratings := make([]memoryRating, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if rating, ok := memoryRatingFromSPD(data); ok {
			ratings = append(ratings, rating)
		}
	}
	return combineMemoryRatings(ratings)
}

// parseDmidecodeSpeeds pulls the first populated module's type, configured
// (running) speed and rated speed from `dmidecode -t 17` output, in MT/s.
func parseDmidecodeSpeeds(out string) (memType string, configured, rated int) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case memType == "" && strings.HasPrefix(line, "Type:"):
			value := strings.TrimSpace(strings.TrimPrefix(line, "Type:"))
			if value != "" && !strings.EqualFold(value, "Unknown") {
				memType = value
			}
		case configured == 0 && strings.HasPrefix(line, "Configured Memory Speed:"):
			configured = parseDmiMTps(strings.TrimPrefix(line, "Configured Memory Speed:"))
		case rated == 0 && strings.HasPrefix(line, "Speed:"):
			rated = parseDmiMTps(strings.TrimPrefix(line, "Speed:"))
		}
	}
	return memType, configured, rated
}

func parseDmiMTps(raw string) int {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return 0
	}
	n, err := strconv.Atoi(fields[0])
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// parseDmidecodeMemory pulls the first populated module's type and speed from
// `dmidecode -t 17` output. Empty slots report "Unknown"/"No Module Installed",
// which are skipped, so the values come from a real stick. Configured Memory
// Speed (the running speed) is preferred over the rated Speed.
func parseDmidecodeMemory(out string) string {
	var memType, configured, base string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case memType == "" && strings.HasPrefix(line, "Type:"):
			value := strings.TrimSpace(strings.TrimPrefix(line, "Type:"))
			if value != "" && !strings.EqualFold(value, "Unknown") {
				memType = value
			}
		case configured == "" && strings.HasPrefix(line, "Configured Memory Speed:"):
			configured = parseDmiSpeed(strings.TrimPrefix(line, "Configured Memory Speed:"))
		case base == "" && strings.HasPrefix(line, "Speed:"):
			base = parseDmiSpeed(strings.TrimPrefix(line, "Speed:"))
		}
	}
	speed := configured
	if speed == "" {
		speed = base
	}
	return composeMemoryName(memType, speed, "")
}

// parseDmiSpeed normalizes a dmidecode speed field ("6000 MT/s", "3200 MHz") to
// a "<n> MT/s" label, returning "" for "Unknown"/unparseable values.
func parseDmiSpeed(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" || strings.EqualFold(value, "Unknown") {
		return ""
	}
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return ""
	}
	if _, err := strconv.Atoi(fields[0]); err != nil {
		return ""
	}
	return fields[0] + " MT/s"
}

// collectNetworkUplink resolves the active default-route network identity with a
// short TTL cache so the slow lane does not spawn `iw`/`nmcli` every pass.
func (c *systemCollector) collectNetworkUplink(ctx context.Context) NetworkUplink {
	c.mu.Lock()
	cached, at := c.uplink, c.uplinkAt
	c.mu.Unlock()
	if !at.IsZero() && time.Since(at) < uplinkCacheTTL {
		return cached
	}

	fresh := detectNetworkUplink(ctx)
	c.mu.Lock()
	c.uplink, c.uplinkAt = fresh, time.Now()
	c.mu.Unlock()
	return fresh
}

// detectNetworkUplink finds the default-route interface and labels it: a Wi-Fi
// SSID for wireless links, or "Ethernet" (with link speed when known) for wired
// ones. No default route degrades to Available:false.
func detectNetworkUplink(ctx context.Context) NetworkUplink {
	iface, ok := readDefaultRouteIface()
	if !ok {
		return NetworkUplink{Available: false, Error: "no default route"}
	}
	if isWirelessIface(iface) {
		if ssid, ok := readWifiSSID(ctx, iface); ok {
			return NetworkUplink{Available: true, Kind: "wifi", Name: ssid}
		}
		return NetworkUplink{Available: true, Kind: "wifi", Name: "Wi-Fi"}
	}
	return NetworkUplink{Available: true, Kind: "ethernet", Name: wiredUplinkName(iface)}
}

// readDefaultRouteIface returns the interface owning the lowest-metric default
// route from /proc/net/route, ignoring loopback and the Tailscale tunnel so the
// physical uplink is reported. The route table is in hex with a header row.
func readDefaultRouteIface() (string, bool) {
	file, err := os.Open("/proc/net/route")
	if err != nil {
		return "", false
	}
	defer file.Close()

	best := ""
	bestMetric := ^uint64(0)
	scanner := bufio.NewScanner(file)
	scanner.Scan() // header
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 11 {
			continue
		}
		iface := fields[0]
		if iface == "lo" || iface == "tailscale0" {
			continue
		}
		if fields[1] != "00000000" { // not a default route
			continue
		}
		metric, err := strconv.ParseUint(fields[6], 10, 64)
		if err != nil {
			continue
		}
		if metric < bestMetric {
			bestMetric = metric
			best = iface
		}
	}
	if best == "" {
		return "", false
	}
	return best, true
}

func isWirelessIface(iface string) bool {
	for _, marker := range []string{"/wireless", "/phy80211"} {
		if _, err := os.Stat("/sys/class/net/" + iface + marker); err == nil {
			return true
		}
	}
	return false
}

// readWifiSSID returns the SSID the interface is associated with, preferring
// `iw dev <iface> link` and falling back to `nmcli`. Either tool missing or the
// link being down degrades to (",", false).
func readWifiSSID(ctx context.Context, iface string) (string, bool) {
	if path, err := exec.LookPath("iw"); err == nil {
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		out, err := trackedOutput(exec.CommandContext(callCtx, path, "dev", iface, "link"))
		cancel()
		if err == nil {
			if ssid, ok := parseIwSSID(string(out)); ok {
				return ssid, true
			}
		}
	}
	if path, err := exec.LookPath("nmcli"); err == nil {
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		out, err := trackedOutput(exec.CommandContext(callCtx, path, "-t", "-f", "active,ssid", "dev", "wifi"))
		cancel()
		if err == nil {
			if ssid, ok := parseNmcliSSID(string(out)); ok {
				return ssid, true
			}
		}
	}
	return "", false
}

func parseIwSSID(out string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "SSID:") {
			ssid := strings.TrimSpace(strings.TrimPrefix(line, "SSID:"))
			if ssid != "" {
				return ssid, true
			}
		}
	}
	return "", false
}

func parseNmcliSSID(out string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "yes:") {
			ssid := strings.TrimSpace(strings.TrimPrefix(line, "yes:"))
			if ssid != "" {
				return ssid, true
			}
		}
	}
	return "", false
}

// wiredUplinkName labels a wired uplink "Ethernet", appending the negotiated
// link speed from sysfs when it is a sane value.
func wiredUplinkName(iface string) string {
	if mbps, ok := readUint64File("/sys/class/net/" + iface + "/speed"); ok && mbps > 0 && mbps < 1_000_000 {
		return "Ethernet · " + formatLinkSpeed(mbps)
	}
	return "Ethernet"
}

func formatLinkSpeed(mbps uint64) string {
	if mbps >= 1000 {
		if mbps%1000 == 0 {
			return fmt.Sprintf("%d Gb/s", mbps/1000)
		}
		return fmt.Sprintf("%.1f Gb/s", float64(mbps)/1000)
	}
	return fmt.Sprintf("%d Mb/s", mbps)
}
