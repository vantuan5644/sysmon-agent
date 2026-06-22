//go:build linux

package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func collectLinuxGPU(ctx context.Context) GPUSet {
	nvidia := collectNVIDIAGPU(ctx)
	drm := collectLinuxDRMGPU("/sys")

	var devices []GPUMetric
	var errors []string
	if nvidia.Available {
		devices = append(devices, nvidia.Devices...)
	} else if nvidia.Error != "" {
		errors = append(errors, nvidia.Error)
	}
	if drm.Available {
		devices = append(devices, drm.Devices...)
	} else if drm.Error != "" {
		errors = append(errors, drm.Error)
	}

	if len(devices) == 0 {
		if len(errors) == 0 {
			errors = append(errors, "no GPU telemetry found")
		}
		return GPUSet{Available: false, Error: strings.Join(errors, "; ")}
	}
	return GPUSet{Available: true, Devices: devices, Error: strings.Join(errors, "; ")}
}

func collectLinuxDRMGPU(sysRoot string) GPUSet {
	matches, err := filepath.Glob(filepath.Join(sysRoot, "class", "drm", "card*", "device"))
	if err != nil {
		return GPUSet{Available: false, Error: err.Error()}
	}

	devices := make([]GPUMetric, 0, len(matches))
	for _, devicePath := range matches {
		cardName := filepath.Base(filepath.Dir(devicePath))
		if !isDRMCardName(cardName) {
			continue
		}
		vendorID := strings.ToLower(readFirstLine(filepath.Join(devicePath, "vendor")))
		if vendorID == "" || vendorID == "0x10de" {
			continue
		}
		devices = append(devices, collectLinuxDRMDevice(cardName, vendorID, devicePath))
	}

	sort.Slice(devices, func(i, j int) bool { return devices[i].Name < devices[j].Name })
	if len(devices) == 0 {
		return GPUSet{Available: false, Error: "no AMD/Intel DRM GPU telemetry found"}
	}
	return GPUSet{Available: true, Devices: devices}
}

func collectLinuxDRMDevice(cardName, vendorID, devicePath string) GPUMetric {
	return GPUMetric{
		Name:        linuxDRMGPUName(cardName, vendorID),
		Usage:       linuxDRMGPUUsage(devicePath),
		Power:       linuxDRMGPUPower(devicePath),
		Memory:      linuxDRMGPUVRAM(devicePath),
		Temperature: linuxDRMGPUTemperature(devicePath),
	}
}

func linuxDRMGPUName(cardName, vendorID string) string {
	switch strings.ToLower(vendorID) {
	case "0x1002":
		return "AMD GPU " + cardName
	case "0x8086":
		return "Intel GPU " + cardName
	default:
		return fmt.Sprintf("DRM GPU %s (%s)", cardName, vendorID)
	}
}

func linuxDRMGPUUsage(devicePath string) NumberMetric {
	value, ok := readUint64File(filepath.Join(devicePath, "gpu_busy_percent"))
	if !ok {
		return unavailableNumber("%", "DRM GPU usage not exposed")
	}
	return availableNumber(float64(value), "%")
}

func linuxDRMGPUVRAM(devicePath string) CapacityMetric {
	used, usedOK := readUint64File(filepath.Join(devicePath, "mem_info_vram_used"))
	total, totalOK := readUint64File(filepath.Join(devicePath, "mem_info_vram_total"))
	if !usedOK || !totalOK || total == 0 {
		return unavailableCapacity("DRM VRAM usage not exposed")
	}
	return availableCapacity(used, total)
}

func linuxDRMGPUTemperature(devicePath string) NumberMetric {
	paths, _ := filepath.Glob(filepath.Join(devicePath, "hwmon", "hwmon*", "temp*_input"))
	sort.Strings(paths)
	for _, path := range paths {
		value, ok := readTemperatureMilliC(path)
		if ok {
			return availableNumber(value, "C")
		}
	}
	return unavailableNumber("C", "DRM GPU temperature not exposed")
}

// linuxDRMGPUPower reads AMD GPU power from the hwmon power1_average counter
// (microwatts). Intel integrated graphics generally do not expose this counter,
// so power is reported as unavailable there instead of failing the device.
func linuxDRMGPUPower(devicePath string) NumberMetric {
	paths, _ := filepath.Glob(filepath.Join(devicePath, "hwmon", "hwmon*", "power1_average"))
	sort.Strings(paths)
	for _, path := range paths {
		microwatts, ok := readUint64File(path)
		if !ok || microwatts == 0 {
			continue
		}
		return availableNumber(float64(microwatts)/1e6, "W")
	}
	return unavailableNumber("W", "DRM GPU power not exposed")
}

func isDRMCardName(name string) bool {
	if !strings.HasPrefix(name, "card") || len(name) == len("card") {
		return false
	}
	for _, r := range strings.TrimPrefix(name, "card") {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func readUint64File(path string) (uint64, bool) {
	raw := strings.TrimSpace(readFirstLine(path))
	if raw == "" {
		return 0, false
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}
