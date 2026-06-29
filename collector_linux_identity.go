//go:build linux

package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// uplinkCacheTTL bounds how often the active-network identity (SSID / wired
// link) is re-resolved. The SSID rarely changes, and resolving it spawns
// `iw`/`nmcli`, so the slow lane reuses a cached value within this window
// instead of spawning every pass.
const uplinkCacheTTL = 15 * time.Second

// resolveHardwareNames returns the host's static identity strings -- the CPU
// model and the RAM type/speed -- resolving them exactly once and caching the
// result. They never change at runtime, and the RAM lookup spawns dmidecode
// (root-only), so this keeps the cost off every slow pass and degrades to empty
// strings when a value cannot be read.
func (c *systemCollector) resolveHardwareNames(ctx context.Context) (cpuName, memoryName string) {
	c.hardwareOnce.Do(func() {
		c.cpuName = readCPUModelName()
		c.memoryName = readMemoryName(ctx)
	})
	return c.cpuName, c.memoryName
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

// cleanCPUModelName trims the marketing noise CPUs report in /proc/cpuinfo --
// "(R)"/"(TM)" marks, the "CPU @ x.xGHz" tail, and a trailing "N-Core
// Processor" -- leaving just the recognizable model.
func cleanCPUModelName(raw string) string {
	name := strings.TrimSpace(raw)
	for _, noise := range []string{"(R)", "(r)", "(TM)", "(tm)"} {
		name = strings.ReplaceAll(name, noise, "")
	}
	if idx := strings.Index(name, " CPU @"); idx >= 0 {
		name = name[:idx]
	}
	tokens := strings.Fields(name)
	kept := make([]string, 0, len(tokens))
	for _, token := range tokens {
		lower := strings.ToLower(token)
		if lower == "processor" || lower == "cpu" || strings.HasSuffix(lower, "-core") {
			continue
		}
		kept = append(kept, token)
	}
	return strings.Join(kept, " ")
}

// readMemoryName returns the RAM type + speed (e.g. "DDR5 · 6000 MT/s") from
// dmidecode, or "" when dmidecode is missing or unreadable (it needs root, so
// the user-session desktop service degrades to "" and the dashboard falls back
// to the total size).
func readMemoryName(ctx context.Context) string {
	path, err := exec.LookPath("dmidecode")
	if err != nil {
		return ""
	}
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(callCtx, path, "-t", "17").Output()
	if err != nil {
		return ""
	}
	return parseDmidecodeMemory(string(out))
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
	switch {
	case memType != "" && speed != "":
		return memType + " · " + speed
	case memType != "":
		return memType
	default:
		return speed
	}
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
		out, err := exec.CommandContext(callCtx, path, "dev", iface, "link").Output()
		cancel()
		if err == nil {
			if ssid, ok := parseIwSSID(string(out)); ok {
				return ssid, true
			}
		}
	}
	if path, err := exec.LookPath("nmcli"); err == nil {
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		out, err := exec.CommandContext(callCtx, path, "-t", "-f", "active,ssid", "dev", "wifi").Output()
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
