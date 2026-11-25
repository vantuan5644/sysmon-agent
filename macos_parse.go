package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var macCPUIdle = regexp.MustCompile(`CPU usage:.*?([0-9]+(?:\.[0-9]+)?)% idle`)
var macPageSize = regexp.MustCompile(`page size of ([0-9]+) bytes`)

func parseMacCPU(output string) NumberMetric {
	matches := macCPUIdle.FindAllStringSubmatch(output, -1)
	// top's first sample is since boot; only the subsequent interval is useful.
	if len(matches) < 2 {
		return unavailableNumber("%", "macOS CPU interval sample unavailable")
	}
	idle, err := strconv.ParseFloat(matches[len(matches)-1][1], 64)
	if err != nil || idle > 100 {
		return unavailableNumber("%", "invalid macOS CPU sample")
	}
	return availableNumber(100-idle, "%")
}

var macMemoryRows = map[string]bool{
	"Anonymous pages":              true,
	"Pages purgeable":              true,
	"Pages wired down":             true,
	"Pages occupied by compressor": true,
}

// parseMacMemory adds up the parts Activity Monitor lists under Memory Used:
// app memory (anonymous minus purgeable), wired and compressed. Linux and
// Windows also leave reclaimable cache out of "used". Total minus free cannot do
// that here, because macOS fills idle memory with cache and keeps free pages
// near zero, so that figure sits around 99% on a healthy machine.
func parseMacMemory(output string, total uint64) CapacityMetric {
	pageMatch := macPageSize.FindStringSubmatch(output)
	if len(pageMatch) != 2 || total == 0 {
		return unavailableCapacity("macOS memory page size or total unavailable")
	}
	pageSize, _ := strconv.ParseUint(pageMatch[1], 10, 64)
	if pageSize == 0 {
		return unavailableCapacity("invalid macOS page size")
	}
	totalPages := total / pageSize
	counts := map[string]uint64{}
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(line, ":")
		key = strings.TrimSpace(key)
		if !ok || !macMemoryRows[key] {
			continue
		}
		pages, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(value), "."), 10, 64)
		if err != nil || pages > totalPages {
			return unavailableCapacity("invalid macOS memory counter: " + key)
		}
		counts[key] = pages
	}
	if len(counts) != len(macMemoryRows) {
		return unavailableCapacity("macOS memory counters incomplete")
	}
	app := counts["Anonymous pages"]
	if purgeable := counts["Pages purgeable"]; purgeable < app {
		app -= purgeable
	} else {
		app = 0
	}
	usedPages := app + counts["Pages wired down"] + counts["Pages occupied by compressor"]
	if usedPages > totalPages {
		return unavailableCapacity("macOS memory counters exceed physical memory")
	}
	return availableCapacity(usedPages*pageSize, total)
}

type macNetCounter struct{ rx, tx uint64 }

func parseMacNetwork(output string) (map[string]macNetCounter, error) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 0 {
		return nil, fmt.Errorf("empty macOS network counters")
	}
	header := strings.Fields(lines[0])
	columns := map[string]int{}
	for i, name := range header {
		columns[name] = i
	}
	for _, name := range []string{"Name", "Network", "Ibytes", "Obytes"} {
		if _, ok := columns[name]; !ok {
			return nil, fmt.Errorf("macOS network column %s missing", name)
		}
	}
	result := map[string]macNetCounter{}
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) < len(header) || !strings.HasPrefix(fields[columns["Network"]], "<Link#") {
			continue
		}
		name := strings.TrimSuffix(fields[columns["Name"]], "*")
		if strings.HasPrefix(name, "lo") {
			continue
		}
		rx, rxErr := strconv.ParseUint(fields[columns["Ibytes"]], 10, 64)
		tx, txErr := strconv.ParseUint(fields[columns["Obytes"]], 10, 64)
		if rxErr != nil || txErr != nil {
			continue
		}
		result[name] = macNetCounter{rx: rx, tx: tx}
	}
	return result, nil
}

// Apple GPU from the IORegistry.
//
// macOS publishes GPU load without privileges on each IOAccelerator service,
// which is what Activity Monitor's GPU History reads:
//
//	ioreg -r -d 1 -w 0 -c IOAccelerator
//
//	+-o AGXAcceleratorG13X  <class AGXAcceleratorG13X, id 0x1000004b2, registered, ...>
//	    {
//	      "IOClass" = "AGXAcceleratorG13X"
//	      "model" = "Apple M1 Pro"
//	      "gpu-core-count" = 16
//	      "PerformanceStatistics" = {"Device Utilization %"=12,"Renderer Utilization %"=10,"Tiler Utilization %"=11,"In use system memory"=583925760,"Alloc system memory"=1610612736,...}
//	    }
//
// "Device Utilization %" is the GPU's busy share; Renderer and Tiler are the
// two halves of Apple's tile-based pipeline and become engines. Apple silicon
// has unified memory, so "In use system memory" is reported against physical
// memory (hw.memsize) rather than against "Alloc system memory", which is only
// what the driver has reserved so far. Power and temperature need root
// (powermetrics) or private SMC keys, and stay unavailable.

var macIORegObject = regexp.MustCompile(`^\+-o\s+(\S+)\s+<class\s+([^,>]+)`)
var macIORegProperty = regexp.MustCompile(`^"([^"]+)"\s*=\s*(.*)$`)

// macIORegEntry is one registry object: its class and raw property values.
type macIORegEntry struct {
	class string
	props map[string]string
}

// parseMacIORegEntries splits `ioreg -r -d 1 -w 0` output into objects. Values
// stay raw text; nested dictionaries are parsed on demand.
func parseMacIORegEntries(output string) []macIORegEntry {
	var entries []macIORegEntry
	for _, line := range strings.Split(output, "\n") {
		text := strings.TrimLeft(line, " |\t")
		if match := macIORegObject.FindStringSubmatch(text); match != nil {
			entries = append(entries, macIORegEntry{class: strings.TrimSpace(match[2]), props: map[string]string{}})
			continue
		}
		if len(entries) == 0 {
			continue
		}
		if match := macIORegProperty.FindStringSubmatch(strings.TrimSpace(text)); match != nil {
			entries[len(entries)-1].props[match[1]] = strings.TrimSpace(match[2])
		}
	}
	return entries
}

// parseMacIORegDict reads one dictionary value, {"key"=value,...}, into raw
// value text by key. Commas and '=' inside quotes or nested {} and () do not
// split.
func parseMacIORegDict(value string) map[string]string {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "{") || !strings.HasSuffix(value, "}") {
		return nil
	}
	body := value[1 : len(value)-1]
	result := map[string]string{}
	var items []string
	depth, quoted, start := 0, false, 0
	for i := 0; i < len(body); i++ {
		switch c := body[i]; {
		case c == '"':
			quoted = !quoted
		case quoted:
		case c == '{' || c == '(' || c == '<':
			depth++
		case c == '}' || c == ')' || c == '>':
			depth--
		case c == ',' && depth == 0:
			items = append(items, body[start:i])
			start = i + 1
		}
	}
	items = append(items, body[start:])
	for _, item := range items {
		quoted, split := false, -1
		for i := 0; i < len(item) && split < 0; i++ {
			if item[i] == '"' {
				quoted = !quoted
			} else if item[i] == '=' && !quoted {
				split = i
			}
		}
		if split < 0 {
			continue
		}
		key := strings.Trim(strings.TrimSpace(item[:split]), `"`)
		if key != "" {
			result[key] = strings.TrimSpace(item[split+1:])
		}
	}
	return result
}

func macIORegNumber(values map[string]string, key string) (float64, bool) {
	text, ok := values[key]
	if !ok {
		return 0, false
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil || !isFinite(value) || value < 0 {
		return 0, false
	}
	return value, true
}

// macGPUName prefers the registry's model ("Apple M1 Pro"); an Apple GPU
// without one reads "Apple GPU", and anything else its driver class.
func macGPUName(entry macIORegEntry) string {
	if model := strings.Trim(entry.props["model"], `"`); strings.TrimSpace(model) != "" {
		return strings.TrimSpace(model)
	}
	if strings.HasPrefix(entry.class, "AGXAccelerator") {
		return "Apple GPU"
	}
	return entry.class
}

// parseMacGPU turns IOAccelerator objects into GPU devices. physicalMemory is
// hw.memsize, the pool Apple silicon's GPU allocates from; 0 leaves memory
// unavailable.
func parseMacGPU(output string, physicalMemory uint64) GPUSet {
	const unexposed = "not exposed by the unprivileged macOS collector"
	var devices []GPUMetric
	for _, entry := range parseMacIORegEntries(output) {
		device := GPUMetric{
			Name:               macGPUName(entry),
			Power:              unavailableNumber("W", unexposed),
			Temperature:        unavailableNumber("C", unexposed),
			HotspotTemperature: unavailableNumber("C", unexposed),
			VRAMTemperature:    unavailableNumber("C", unexposed),
		}
		stats := parseMacIORegDict(entry.props["PerformanceStatistics"])
		if stats == nil {
			device.Usage = unavailableNumber("%", "IOAccelerator reports no PerformanceStatistics")
			device.Memory = unavailableCapacity("IOAccelerator reports no PerformanceStatistics")
			devices = append(devices, device)
			continue
		}
		if usage, ok := macIORegNumber(stats, "Device Utilization %"); ok {
			device.Usage = availableNumber(clampEnginePercent(usage), "%")
		} else {
			device.Usage = unavailableNumber("%", "IOAccelerator reports no Device Utilization %")
		}
		for _, engine := range []struct{ key, name string }{
			{"Renderer Utilization %", "Renderer"},
			{"Tiler Utilization %", "Tiler"},
		} {
			if value, ok := macIORegNumber(stats, engine.key); ok {
				device.Engines = append(device.Engines, GPUEngine{Name: engine.name, Usage: availableNumber(clampEnginePercent(value), "%")})
			}
		}
		sortGPUEngines(device.Engines)
		used, ok := macIORegNumber(stats, "In use system memory")
		if !ok {
			used, ok = macIORegNumber(stats, "Alloc system memory")
		}
		switch {
		case !ok:
			device.Memory = unavailableCapacity("IOAccelerator reports no system memory use")
		case physicalMemory == 0:
			device.Memory = unavailableCapacity("macOS physical memory size unavailable")
		default:
			device.Memory = availableCapacity(uint64(used), physicalMemory)
		}
		devices = append(devices, device)
	}
	if len(devices) == 0 {
		return GPUSet{Error: "no IOAccelerator found in the IORegistry"}
	}
	return GPUSet{Available: true, Devices: devices}
}
