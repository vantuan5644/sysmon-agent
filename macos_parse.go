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
