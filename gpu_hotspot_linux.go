//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"sysmon-agent/internal/gpuhotspot"
)

func mergeLinuxGPUHotspots(gpu GPUSet, path string, now time.Time) GPUSet {
	gpu.Devices = append([]GPUMetric(nil), gpu.Devices...)
	snapshot, err := readLinuxGPUHotspots(path, now)
	byPCI := make(map[string][]gpuhotspot.Device)
	for _, reading := range snapshot.Devices {
		id, idErr := gpuhotspot.NormalizePCIBusID(reading.PCIBusID)
		if idErr == nil {
			byPCI[id] = append(byPCI[id], reading)
		}
	}
	for i := range gpu.Devices {
		device := &gpu.Devices[i]
		device.HotspotTemperature = unavailableNumber("C", "NVIDIA hotspot sensor not reported")
		if err != nil {
			device.HotspotTemperature.Error = err.Error()
			continue
		}
		id, idErr := gpuhotspot.NormalizePCIBusID(device.PCIBusID)
		if idErr != nil {
			continue
		}
		readings := byPCI[id]
		if len(readings) > 1 {
			device.HotspotTemperature.Error = "ambiguous GPU hotspot PCI identity"
		} else if len(readings) == 1 {
			reading := readings[0]
			if reading.Error != "" {
				device.HotspotTemperature.Error = reading.Error
			} else if reading.Celsius != nil && validGPUHotspot(*reading.Celsius) {
				device.HotspotTemperature = availableNumber(*reading.Celsius, "C")
			}
		}
	}
	return gpu
}

func readLinuxGPUHotspots(path string, now time.Time) (gpuhotspot.Snapshot, error) {
	var snapshot gpuhotspot.Snapshot
	file, err := os.Open(path)
	if err != nil {
		return snapshot, fmt.Errorf("GPU hotspot helper unavailable: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, gpuhotspot.MaxBytes+1))
	if err != nil || len(data) > gpuhotspot.MaxBytes {
		return snapshot, fmt.Errorf("GPU hotspot helper file unreadable or oversized")
	}
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return snapshot, fmt.Errorf("invalid GPU hotspot helper JSON: %w", err)
	}
	if len(snapshot.Devices) > gpuhotspot.MaxDevices {
		return snapshot, fmt.Errorf("too many GPU hotspot devices")
	}
	age := now.Sub(snapshot.Timestamp)
	if snapshot.Timestamp.IsZero() || age < -time.Second || age > gpuhotspot.FreshFor {
		return snapshot, fmt.Errorf("GPU hotspot helper sample unavailable or stale")
	}
	if snapshot.Error != "" {
		return snapshot, fmt.Errorf("GPU hotspot helper: %s", snapshot.Error)
	}
	return snapshot, nil
}
