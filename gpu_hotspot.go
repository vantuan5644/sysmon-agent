package main

import "strings"

// gpuSensorReading is one NVIDIA temperature from the LibreHardwareMonitor
// bridge: the hotspot (gpu_hotspots) or the memory junction (gpu_vram).
type gpuSensorReading struct {
	Name       string  `json:"name"`
	Identifier string  `json:"identifier"`
	Value      float64 `json:"value"`
}

// mergeWindowsGPUSensors attaches the bridge's hotspot and memory junction
// readings to the nvidia-smi devices. Match unique names only: LHM and
// nvidia-smi enumeration indices need not agree. Copy the slice before
// enrichment because the original may belong to a cache.
func mergeWindowsGPUSensors(gpu GPUSet, hotspots, vram []gpuSensorReading, bridgeAvailable bool) GPUSet {
	gpu.Devices = append([]GPUMetric(nil), gpu.Devices...)
	counts := make(map[string]int)
	for _, device := range gpu.Devices {
		counts[normalizeGPUName(device.Name)]++
	}
	hotspotsByName := gpuReadingsByName(hotspots)
	vramByName := gpuReadingsByName(vram)
	for i := range gpu.Devices {
		device := &gpu.Devices[i]
		device.HotspotTemperature = unavailableNumber("C", "NVIDIA hotspot sensor not reported")
		device.VRAMTemperature = unavailableNumber("C", vramNotReported)
		if !bridgeAvailable {
			continue
		}
		key := normalizeGPUName(device.Name)
		unique := counts[key] == 1
		matchGPUReading(&device.HotspotTemperature, hotspotsByName[key], unique, validGPUHotspot, "ambiguous GPU hotspot identity")
		matchGPUReading(&device.VRAMTemperature, vramByName[key], unique, validGPUVRAM, "ambiguous GPU memory junction identity")
	}
	return gpu
}

func gpuReadingsByName(readings []gpuSensorReading) map[string][]gpuSensorReading {
	byName := make(map[string][]gpuSensorReading)
	for _, reading := range readings {
		key := normalizeGPUName(reading.Name)
		byName[key] = append(byName[key], reading)
	}
	return byName
}

// matchGPUReading sets metric from the one reading named like its device. Two
// devices or two readings under one name are ambiguous, and nothing is taken.
func matchGPUReading(metric *NumberMetric, matches []gpuSensorReading, unique bool, valid func(float64) bool, ambiguous string) {
	if !unique || len(matches) > 1 {
		*metric = unavailableNumber("C", ambiguous)
		return
	}
	if len(matches) == 1 && valid(matches[0].Value) {
		*metric = availableNumber(matches[0].Value, "C")
	}
}

func normalizeGPUName(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(name), " "))
}

func validGPUHotspot(value float64) bool { return isFinite(value) && value > 0 && value < 150 }

// The memory junction exists only on cards with GDDR6X or GDDR7 memory. Linux
// reads it with the hotspot helper (docs/nvidia-hotspot.md), Windows from
// LibreHardwareMonitor's "GPU Memory Junction" sensor.
const vramNotReported = "GPU memory junction not reported"

func validGPUVRAM(value float64) bool { return isFinite(value) && value > 0 && value < 127 }
