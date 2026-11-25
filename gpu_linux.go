//go:build linux

package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// collectLinuxGPU merges NVIDIA (nvidia-smi) with AMD/Intel DRM sysfs. usage
// returns the DRM fdinfo sample the pass started alongside (drm_fdinfo_linux.go);
// it is only waited for after the sysfs reads.
func collectLinuxGPU(ctx context.Context, usage func() drmUsageSummary) GPUSet {
	nvidia := mergeLinuxGPUHotspots(collectNVIDIAGPU(ctx), "/run/sysmon-gpu-hotspot/readings.json", time.Now())
	drm := collectLinuxDRMGPUWithUsage("/sys", usage)

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
	return collectLinuxDRMGPUWithUsage(sysRoot, nil)
}

// collectLinuxDRMGPUWithUsage reads every AMD/Intel card under sysRoot and
// fills in what DRM fdinfo measured: the engines of each card, and the load
// of a card without gpu_busy_percent (every Intel GPU).
func collectLinuxDRMGPUWithUsage(sysRoot string, usage func() drmUsageSummary) GPUSet {
	matches, err := filepath.Glob(filepath.Join(sysRoot, "class", "drm", "card*", "device"))
	if err != nil {
		return GPUSet{Available: false, Error: err.Error()}
	}

	cards := make([]linuxDRMDevice, 0, len(matches))
	for _, devicePath := range matches {
		cardName := filepath.Base(filepath.Dir(devicePath))
		if !isDRMCardName(cardName) {
			continue
		}
		vendorID := strings.ToLower(readFirstLine(filepath.Join(devicePath, "vendor")))
		if vendorID == "" || vendorID == "0x10de" {
			continue
		}
		cards = append(cards, collectLinuxDRMCard(filepath.Dir(devicePath), cardName, vendorID))
	}
	if len(cards) == 0 {
		return GPUSet{Available: false, Error: "no AMD/Intel DRM GPU telemetry found"}
	}

	if usage != nil {
		summary := usage()
		for i := range cards {
			applyDRMDeviceUsage(&cards[i], summary)
		}
	}
	sortLinuxDRMDevices(cards)
	devices := make([]GPUMetric, 0, len(cards))
	for _, card := range cards {
		devices = append(devices, card.metric)
	}
	return GPUSet{Available: true, Devices: devices}
}

// applyDRMDeviceUsage attaches a card's fdinfo engines, and takes its load
// as the card's usage when sysfs has no gpu_busy_percent.
func applyDRMDeviceUsage(card *linuxDRMDevice, summary drmUsageSummary) {
	if card.suspended || card.pdev == "" {
		return
	}
	device, ok := summary.Devices[card.pdev]
	if !summary.Available || !ok {
		if !card.metric.Usage.Available && summary.Error != "" {
			card.metric.Usage = unavailableNumber("%", "DRM GPU usage not exposed; "+summary.Error)
		}
		return
	}
	if len(device.Engines) > 0 {
		card.metric.Engines = device.Engines
	}
	if !card.metric.Usage.Available {
		card.metric.Usage = device.Load
	}
}

// collectLinuxDRMCard reads one AMD/Intel card. A runtime-suspended card is
// idle and is otherwise left alone.
func collectLinuxDRMCard(cardPath, cardName, vendorID string) linuxDRMDevice {
	devicePath := filepath.Join(cardPath, "device")
	name := linuxDRMGPUNameFor(cardName, vendorID, devicePath)
	card := linuxDRMDevice{card: cardName, pdev: linuxDRMPCISlot(devicePath)}
	metric := GPUMetric{Name: name}

	if linuxDRMRuntimeSuspended(devicePath) {
		const asleep = "GPU is runtime-suspended"
		card.suspended = true
		metric.Integrated = strings.EqualFold(vendorID, "0x8086") && !intelDiscretePattern.MatchString(name)
		metric.Usage = availableNumber(0, "%")
		metric.Memory = unavailableCapacity(asleep)
		metric.Power = unavailableNumber("W", asleep)
		metric.Temperature = unavailableNumber("C", asleep)
		metric.HotspotTemperature = unavailableNumber("C", asleep)
		metric.VRAMTemperature = unavailableNumber("C", asleep)
		card.metric = metric
		return card
	}

	hwmon := readLinuxGPUHwmon(devicePath)
	metric.Integrated = linuxDRMGPUIntegrated(vendorID, devicePath, name, hwmon)
	metric.Usage = linuxDRMGPUUsage(devicePath)
	metric.Memory, metric.MemoryKind = linuxDRMGPUMemory(devicePath, metric.Integrated)

	switch {
	case hwmon.edge.Available:
		metric.Temperature = hwmon.edge
	case hwmon.firstTemp.Available:
		metric.Temperature = hwmon.firstTemp
	default:
		metric.Temperature = unavailableNumber("C", "DRM GPU temperature not exposed")
	}
	metric.HotspotTemperature = hwmon.junction
	if !metric.HotspotTemperature.Available {
		metric.HotspotTemperature = unavailableNumber("C", "no junction (hotspot) sensor exposed")
	}
	metric.VRAMTemperature = hwmon.memory
	if !metric.VRAMTemperature.Available {
		metric.VRAMTemperature = unavailableNumber("C", "no memory temperature sensor exposed")
	}
	switch {
	case hwmon.powerAverage.Available:
		metric.Power = hwmon.powerAverage
	case hwmon.powerInput.Available:
		metric.Power = hwmon.powerInput
	default:
		metric.Power = unavailableNumber("W", "DRM GPU power not exposed")
	}
	metric.PowerLimit = numberPtr(hwmon.powerCap)
	if clock, ok := linuxDRMGPUClock(cardPath, devicePath, vendorID, hwmon); ok {
		metric.Clock = &clock
	}
	metric.FanRPM = numberPtr(hwmon.fanRPM)
	metric.HotspotCritical = numberPtr(hwmon.junctionCrit)
	metric.VRAMCritical = numberPtr(hwmon.memoryCrit)
	card.metric = metric
	return card
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
