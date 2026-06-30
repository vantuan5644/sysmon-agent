package main

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// uplinkCacheTTL bounds how often the active-network identity (SSID / wired
// link) is re-resolved. The SSID rarely changes, and resolving it spawns
// platform tools (`iw`/`nmcli` on Linux, `netsh`/`Get-NetAdapter` on Windows),
// so the slow lane reuses a cached value within this window instead of spawning
// every pass.
const uplinkCacheTTL = 15 * time.Second

// cleanCPUModelName trims the marketing noise CPUs report (in /proc/cpuinfo or
// Win32_Processor.Name) -- "(R)"/"(TM)" marks, the "CPU @ x.xGHz" tail, and a
// trailing "N-Core Processor" -- leaving just the recognizable model. Shared by
// the Linux and Windows CPU-name readers.
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

// composeMemoryName joins the non-empty RAM identity parts (type, speed,
// channel) with the middle-dot separator the Linux dmidecode path established,
// e.g. "DDR5 · 6000 MT/s · Dual Channel". Empty parts are dropped so a missing
// field collapses cleanly (type-only -> "DDR5", speed-only -> "6000 MT/s").
func composeMemoryName(memType, speed, channel string) string {
	kept := make([]string, 0, 3)
	for _, part := range []string{memType, speed, channel} {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			kept = append(kept, trimmed)
		}
	}
	return strings.Join(kept, " · ")
}

// smbiosMemoryTypeLabel maps a Win32_PhysicalMemory.SMBIOSMemoryType code to a
// DDR-generation label, returning "" for unknown/unmapped codes so the type is
// simply omitted. Codes follow the SMBIOS spec (Memory Device, Type field).
func smbiosMemoryTypeLabel(code int) string {
	switch code {
	case 18:
		return "DDR"
	case 19:
		return "DDR2"
	case 24:
		return "DDR3"
	case 26:
		return "DDR4"
	case 34:
		return "DDR5"
	case 27:
		return "LPDDR"
	case 28:
		return "LPDDR2"
	case 29:
		return "LPDDR3"
	case 30:
		return "LPDDR4"
	case 35:
		return "LPDDR5"
	default:
		return ""
	}
}

// memorySpeedLabel renders a positive memory data rate (MT/s) as "<n> MT/s",
// returning "" for non-positive values so an unknown speed is omitted.
func memorySpeedLabel(mtps int) string {
	if mtps <= 0 {
		return ""
	}
	return strconv.Itoa(mtps) + " MT/s"
}

// channelLabel describes the memory channel population. It prefers the count of
// distinct memory channels parsed from the firmware's DIMM locators; when none
// were parseable it falls back to the populated-module count (a 2-DIMM desktop
// reads "Dual Channel"). Returns "" when nothing is known.
func channelLabel(distinctChannels, populatedModules int) string {
	switch {
	case distinctChannels >= 4:
		return "Quad Channel"
	case distinctChannels >= 2:
		return "Dual Channel"
	case distinctChannels == 1:
		return "Single Channel"
	}
	switch {
	case populatedModules >= 2:
		return "Dual Channel"
	case populatedModules == 1:
		return "Single Channel"
	default:
		return ""
	}
}

var (
	memoryChannelLocatorRe = regexp.MustCompile(`(?i)channel\s*([a-z0-9])`)
	memoryDimmLocatorRe    = regexp.MustCompile(`(?i)dimm[_ ]?([a-z])\d`)
)

// memoryChannelKey derives a normalized channel identifier from a DIMM locator
// string (Win32_PhysicalMemory DeviceLocator, falling back to BankLabel): e.g.
// "Controller0-ChannelA-DIMM0" -> "a", "P0 CHANNEL A" -> "a", "DIMM_A1" -> "a".
// Returns "" when no channel can be inferred, so the caller falls back to a
// module count.
func memoryChannelKey(locator string) string {
	loc := strings.TrimSpace(locator)
	if loc == "" {
		return ""
	}
	if m := memoryChannelLocatorRe.FindStringSubmatch(loc); m != nil {
		return strings.ToLower(m[1])
	}
	if m := memoryDimmLocatorRe.FindStringSubmatch(loc); m != nil {
		return strings.ToLower(m[1])
	}
	return ""
}

// memoryModuleInfo is a platform-neutral view of one RAM module used by
// buildMemoryName to compose the RAM identity string. A platform decodes its
// native inventory (Windows: Win32_PhysicalMemory) into this; fields it cannot
// read are left zero so they are simply omitted from the composed name.
type memoryModuleInfo struct {
	CapacityBytes uint64
	SpeedMTps     int
	SMBIOSType    int
	Locator       string
}

// buildMemoryName composes a RAM identity string ("DDR5 · 6000 MT/s · Dual
// Channel") from a host's memory modules. Only populated modules
// (CapacityBytes > 0) contribute; the type comes from the first populated
// module, the speed from the first populated module reporting one, and the
// channel config from the distinct channel keys (falling back to the populated
// count). Returns "" when nothing is populated, so the dashboard falls back to
// the total size.
func buildMemoryName(modules []memoryModuleInfo) string {
	populated := make([]memoryModuleInfo, 0, len(modules))
	for _, module := range modules {
		if module.CapacityBytes > 0 {
			populated = append(populated, module)
		}
	}
	if len(populated) == 0 {
		return ""
	}

	typeLabel := smbiosMemoryTypeLabel(populated[0].SMBIOSType)

	speed := 0
	for _, module := range populated {
		if module.SpeedMTps > 0 {
			speed = module.SpeedMTps
			break
		}
	}
	speedLabel := memorySpeedLabel(speed)

	channels := make(map[string]struct{})
	for _, module := range populated {
		if key := memoryChannelKey(module.Locator); key != "" {
			channels[key] = struct{}{}
		}
	}
	channel := channelLabel(len(channels), len(populated))

	return composeMemoryName(typeLabel, speedLabel, channel)
}
