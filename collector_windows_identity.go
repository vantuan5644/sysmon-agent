//go:build windows

package main

import (
	"context"
	"strings"
	"time"
)

// resolveHardwareNames returns the host's static identity strings -- the CPU
// model and the RAM type/speed/channel -- resolving them exactly once and
// caching the result. They never change at runtime and the lookups spawn CIM
// queries, so this keeps the cost off every slow pass and degrades to empty
// strings when a value cannot be read. It mirrors the Linux resolveHardwareNames.
func (c *systemCollector) resolveHardwareNames(ctx context.Context) (cpuName, memoryName string) {
	c.hardwareOnce.Do(func() {
		c.cpuName = windowsCPUModelName(ctx)
		c.memoryName = windowsMemoryName(ctx)
	})
	return c.cpuName, c.memoryName
}

// windowsCPUModelName returns a cleaned CPU model from Win32_Processor.Name
// (e.g. "AMD Ryzen 9 7950X"), or "" if it cannot be read. -First 1 collapses a
// multi-socket host to one object so ConvertTo-Json yields an object, not an
// array.
func windowsCPUModelName(ctx context.Context) string {
	var result struct {
		Name string
	}
	if err := runPowerShellJSON(ctx, `Get-CimInstance Win32_Processor | Select-Object -First 1 Name`, &result); err != nil {
		return ""
	}
	return cleanCPUModelName(result.Name)
}

// windowsMemoryModule models one Win32_PhysicalMemory row. Numeric fields are
// pointers so a null (older OS without ConfiguredClockSpeed, or a slot reporting
// no speed) decodes cleanly to "unknown" rather than a misleading zero.
type windowsMemoryModule struct {
	Capacity             *uint64
	Speed                *int
	ConfiguredClockSpeed *int
	SMBIOSMemoryType     *int
	DeviceLocator        string
	BankLabel            string
}

// windowsMemoryName returns the RAM identity ("DDR5 · 6000 MT/s · Dual Channel")
// from Win32_PhysicalMemory, or "" when it cannot be read. Unlike Linux's
// dmidecode (root-only), this class is readable by standard users, so even a
// user-session service populates it; on failure the dashboard falls back to the
// total size.
func windowsMemoryName(ctx context.Context) string {
	var rows []windowsMemoryModule
	if err := runPowerShellJSONArray(ctx, `Get-CimInstance Win32_PhysicalMemory | Select-Object Capacity,Speed,ConfiguredClockSpeed,SMBIOSMemoryType,DeviceLocator,BankLabel`, &rows); err != nil {
		return ""
	}
	modules := make([]memoryModuleInfo, 0, len(rows))
	for _, row := range rows {
		modules = append(modules, windowsMemoryModuleInfo(row))
	}
	return buildMemoryName(modules)
}

// windowsMemoryModuleInfo converts a decoded CIM row into the platform-neutral
// memoryModuleInfo. It prefers ConfiguredClockSpeed (the running speed) over the
// rated Speed, mirroring the Linux dmidecode path's preference for "Configured
// Memory Speed", and falls back from DeviceLocator to BankLabel for the channel
// key.
func windowsMemoryModuleInfo(row windowsMemoryModule) memoryModuleInfo {
	info := memoryModuleInfo{}
	if row.Capacity != nil {
		info.CapacityBytes = *row.Capacity
	}
	if row.ConfiguredClockSpeed != nil && *row.ConfiguredClockSpeed > 0 {
		info.SpeedMTps = *row.ConfiguredClockSpeed
	} else if row.Speed != nil && *row.Speed > 0 {
		info.SpeedMTps = *row.Speed
	}
	if row.SMBIOSMemoryType != nil {
		info.SMBIOSType = *row.SMBIOSMemoryType
	}
	info.Locator = strings.TrimSpace(row.DeviceLocator)
	if info.Locator == "" {
		info.Locator = strings.TrimSpace(row.BankLabel)
	}
	return info
}

// collectNetworkUplink resolves the active default-route network identity with a
// short TTL cache so the slow lane does not spawn Get-NetRoute/netsh every pass.
// It mirrors the Linux collectNetworkUplink.
func (c *systemCollector) collectNetworkUplink(ctx context.Context) NetworkUplink {
	c.mu.Lock()
	cached, at := c.uplink, c.uplinkAt
	c.mu.Unlock()
	if !at.IsZero() && time.Since(at) < uplinkCacheTTL {
		return cached
	}

	fresh := detectWindowsNetworkUplink(ctx)
	c.mu.Lock()
	c.uplink, c.uplinkAt = fresh, time.Now()
	c.mu.Unlock()
	return fresh
}

// windowsUplinkAdapter is the default-route adapter view emitted by spawn 1 of
// detectWindowsNetworkUplink.
type windowsUplinkAdapter struct {
	Name      string
	Media     string
	LinkSpeed string
	Status    string
}

// detectWindowsNetworkUplink finds the lowest-metric default-route adapter and
// labels it: a Wi-Fi SSID for wireless links, or "Ethernet" (with link speed
// when known) for wired ones. No default route, or a route via an excluded
// virtual adapter, degrades to Available:false.
func detectWindowsNetworkUplink(ctx context.Context) NetworkUplink {
	adapter, ok := windowsDefaultRouteAdapter(ctx)
	if !ok {
		return NetworkUplink{Available: false, Error: "no default route"}
	}
	name := strings.TrimSpace(adapter.Name)
	if name != "" && !shouldIncludeWindowsNetworkInterface(name) {
		return NetworkUplink{Available: false, Error: "default route via excluded virtual adapter"}
	}
	media := strings.ToLower(adapter.Media)
	switch {
	case strings.Contains(media, "802.11") || strings.Contains(media, "wireless"):
		if ssid, ok := windowsWifiSSID(ctx); ok {
			return NetworkUplink{Available: true, Kind: "wifi", Name: ssid}
		}
		return NetworkUplink{Available: true, Kind: "wifi", Name: "Wi-Fi"}
	case strings.Contains(media, "802.3") || strings.Contains(media, "ethernet"):
		return NetworkUplink{Available: true, Kind: "ethernet", Name: windowsWiredUplinkName(adapter.LinkSpeed)}
	default:
		// Unknown PhysicalMediaType: only call it a wired link when the adapter is
		// up and its name does not look wireless, otherwise degrade rather than
		// mislabel.
		if strings.EqualFold(strings.TrimSpace(adapter.Status), "Up") && !looksWirelessName(name) {
			return NetworkUplink{Available: true, Kind: "ethernet", Name: windowsWiredUplinkName(adapter.LinkSpeed)}
		}
		return NetworkUplink{Available: false, Error: "could not classify default-route adapter"}
	}
}

// windowsDefaultRouteAdapter returns the adapter owning the lowest-metric IPv4
// default route. It is a single PowerShell pipeline (Get-NetRoute sorted by
// metric -> Get-NetAdapter), emitting a flat object the runPowerShellJSON
// wrapper renders to JSON. No default route -> empty stdout -> (zero, false).
func windowsDefaultRouteAdapter(ctx context.Context) (windowsUplinkAdapter, bool) {
	const script = `$r = Get-NetRoute -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue | Sort-Object RouteMetric,ifMetric | Select-Object -First 1; if ($null -eq $r) { return }; $a = Get-NetAdapter -InterfaceIndex $r.ifIndex -ErrorAction SilentlyContinue; if ($null -eq $a) { return }; [pscustomobject]@{ Name = $a.Name; Media = [string]$a.PhysicalMediaType; LinkSpeed = [string]$a.LinkSpeed; Status = [string]$a.Status }`
	var result windowsUplinkAdapter
	if err := runPowerShellJSON(ctx, script, &result); err != nil {
		return windowsUplinkAdapter{}, false
	}
	if strings.TrimSpace(result.Name) == "" {
		return windowsUplinkAdapter{}, false
	}
	return result, true
}

// windowsWifiSSID returns the SSID the host is associated with, parsed from
// `netsh wlan show interfaces`. WLAN AutoConfig being off, a localized SSID
// label, or unparseable output degrades to (",", false) and the caller falls
// back to the generic "Wi-Fi" name.
func windowsWifiSSID(ctx context.Context) (string, bool) {
	out, err := runPowerShell(ctx, `netsh wlan show interfaces`)
	if err != nil {
		return "", false
	}
	return parseNetshSSID(string(out))
}

// parseNetshSSID extracts the connected SSID from `netsh wlan show interfaces`
// output: the line whose key (before the first colon) is exactly "SSID" -- never
// "BSSID" -- with its first non-empty value. The label is localized on non-
// English Windows, so a miss returns (",", false).
func parseNetshSSID(out string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		if !strings.EqualFold(key, "SSID") {
			continue
		}
		if value := strings.TrimSpace(line[idx+1:]); value != "" {
			return value, true
		}
	}
	return "", false
}

// windowsWiredUplinkName labels a wired uplink "Ethernet", appending the
// negotiated link speed when Get-NetAdapter reported a usable one.
func windowsWiredUplinkName(linkSpeed string) string {
	if speed := normalizeWindowsLinkSpeed(linkSpeed); speed != "" {
		return "Ethernet · " + speed
	}
	return "Ethernet"
}

// normalizeWindowsLinkSpeed converts Get-NetAdapter's formatted LinkSpeed
// ("2.5 Gbps", "1 Gbps", "100 Mbps") to the "<n> <unit>b/s" style the Linux
// path uses, returning "" for an empty or zero value. Replacing "bps" with
// "b/s" handles every SI prefix in one pass ("Gbps" -> "Gb/s").
func normalizeWindowsLinkSpeed(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" || s == "0" || strings.HasPrefix(s, "0 ") {
		return ""
	}
	return strings.ReplaceAll(s, "bps", "b/s")
}

func looksWirelessName(name string) bool {
	lower := strings.ToLower(name)
	for _, marker := range []string{"wi-fi", "wifi", "wireless", "wlan"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
