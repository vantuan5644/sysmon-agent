package main

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestWindowsGPUHotspotIdentityAndCacheIsolation(t *testing.T) {
	gpu := GPUSet{Available: true, Devices: []GPUMetric{{Name: "NVIDIA GeForce RTX 4090", Temperature: availableNumber(45, "C")}, {Name: "NVIDIA GeForce RTX 3090"}}}
	readings := []gpuHotspotReading{{Name: " NVIDIA  GeForce RTX 3090 ", Value: 77}, {Name: "nvidia geforce rtx 4090", Value: 66}}
	got := mergeWindowsGPUHotspots(gpu, readings, true)
	if got.Devices[0].HotspotTemperature.Value != 66 || got.Devices[1].HotspotTemperature.Value != 77 {
		t.Fatalf("wrong GPU association: %+v", got)
	}
	if gpu.Devices[0].HotspotTemperature.Available {
		t.Fatal("enrichment mutated cached device")
	}
	failed := mergeWindowsGPUHotspots(got, readings, false)
	if failed.Devices[0].HotspotTemperature.Available || !failed.Devices[0].Temperature.Available {
		t.Fatal("failed bridge retained hotspot or lost core temperature")
	}
	data, err := json.Marshal(got)
	if err != nil || !strings.Contains(string(data), `"hotspot_temperature_celsius":{"available":true,"value":66,"unit":"C"}`) {
		t.Fatalf("JSON: %s, %v", data, err)
	}
}

func TestWindowsGPUHotspotRejectsAmbiguousAndInvalidReadings(t *testing.T) {
	for _, tc := range []struct {
		name     string
		devices  []GPUMetric
		readings []gpuHotspotReading
	}{
		{"duplicate models", []GPUMetric{{Name: "RTX 3090"}, {Name: "RTX 3090"}}, []gpuHotspotReading{{Name: "RTX 3090", Value: 70}}},
		{"duplicate sensors", []GPUMetric{{Name: "RTX 3090"}}, []gpuHotspotReading{{Name: "RTX 3090", Value: 70}, {Name: "RTX 3090", Value: 72}}},
		{"nonfinite", []GPUMetric{{Name: "RTX 3090"}}, []gpuHotspotReading{{Name: "RTX 3090", Value: math.NaN()}}},
		{"invalid", []GPUMetric{{Name: "RTX 3090"}}, []gpuHotspotReading{{Name: "RTX 3090", Value: 150}}},
		{"missing", []GPUMetric{{Name: "RTX 3090"}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeWindowsGPUHotspots(GPUSet{Available: true, Devices: tc.devices}, tc.readings, true)
			for _, device := range got.Devices {
				if device.HotspotTemperature.Available || device.HotspotTemperature.Unit != "C" {
					t.Fatalf("unexpected hotspot: %+v", device)
				}
			}
		})
	}
}

func TestNVIDIAPCIIdentityColumn(t *testing.T) {
	got := parseNVIDIAGPUCSV("NVIDIA GeForce RTX 4090, 5, 1024, 24564, 48, 65, 00000000:01:00.0\n")
	if len(got.Devices) != 1 || got.Devices[0].PCIBusID != "00000000:01:00.0" || got.Devices[0].Power.Value != 65 {
		t.Fatalf("parsed: %+v", got)
	}
	if got.Devices[0].HotspotTemperature.Available || got.Devices[0].HotspotTemperature.Unit != "C" {
		t.Fatal("NVIDIA CSV should initialize unavailable hotspot")
	}
}
