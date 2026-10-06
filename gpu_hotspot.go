package main

import "strings"

type gpuHotspotReading struct {
	Name       string  `json:"name"`
	Identifier string  `json:"identifier"`
	Value      float64 `json:"value"`
}

// Match unique names only: LHM and nvidia-smi enumeration indices need not agree.
// Copy the slice before enrichment because the original may belong to a cache.
func mergeWindowsGPUHotspots(gpu GPUSet, readings []gpuHotspotReading, bridgeAvailable bool) GPUSet {
	gpu.Devices = append([]GPUMetric(nil), gpu.Devices...)
	counts := make(map[string]int)
	for _, device := range gpu.Devices {
		counts[normalizeGPUName(device.Name)]++
	}
	byName := make(map[string][]gpuHotspotReading)
	for _, reading := range readings {
		key := normalizeGPUName(reading.Name)
		byName[key] = append(byName[key], reading)
	}
	for i := range gpu.Devices {
		device := &gpu.Devices[i]
		device.HotspotTemperature = unavailableNumber("C", "NVIDIA hotspot sensor not reported")
		if !bridgeAvailable {
			continue
		}
		key := normalizeGPUName(device.Name)
		if counts[key] != 1 || len(byName[key]) > 1 {
			device.HotspotTemperature = unavailableNumber("C", "ambiguous GPU hotspot identity")
			continue
		}
		if matches := byName[key]; len(matches) == 1 && validGPUHotspot(matches[0].Value) {
			device.HotspotTemperature = availableNumber(matches[0].Value, "C")
		}
	}
	return gpu
}

func normalizeGPUName(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(name), " "))
}

func validGPUHotspot(value float64) bool { return isFinite(value) && value > 0 && value < 150 }
