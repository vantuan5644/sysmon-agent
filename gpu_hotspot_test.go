package main

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestWindowsGPUHotspotIdentityAndCacheIsolation(t *testing.T) {
	gpu := GPUSet{Available: true, Devices: []GPUMetric{{Name: "NVIDIA GeForce RTX 4090", Temperature: availableNumber(45, "C")}, {Name: "NVIDIA GeForce RTX 3090"}}}
	readings := []gpuSensorReading{{Name: " NVIDIA  GeForce RTX 3090 ", Value: 77}, {Name: "nvidia geforce rtx 4090", Value: 66}}
	got := mergeWindowsGPUSensors(gpu, readings, nil, true)
	if got.Devices[0].HotspotTemperature.Value != 66 || got.Devices[1].HotspotTemperature.Value != 77 {
		t.Fatalf("wrong GPU association: %+v", got)
	}
	if gpu.Devices[0].HotspotTemperature.Available {
		t.Fatal("enrichment mutated cached device")
	}
	failed := mergeWindowsGPUSensors(got, readings, nil, false)
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
		readings []gpuSensorReading
	}{
		{"duplicate models", []GPUMetric{{Name: "RTX 3090"}, {Name: "RTX 3090"}}, []gpuSensorReading{{Name: "RTX 3090", Value: 70}}},
		{"duplicate sensors", []GPUMetric{{Name: "RTX 3090"}}, []gpuSensorReading{{Name: "RTX 3090", Value: 70}, {Name: "RTX 3090", Value: 72}}},
		{"nonfinite", []GPUMetric{{Name: "RTX 3090"}}, []gpuSensorReading{{Name: "RTX 3090", Value: math.NaN()}}},
		{"invalid", []GPUMetric{{Name: "RTX 3090"}}, []gpuSensorReading{{Name: "RTX 3090", Value: 150}}},
		{"missing", []GPUMetric{{Name: "RTX 3090"}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeWindowsGPUSensors(GPUSet{Available: true, Devices: tc.devices}, tc.readings, tc.readings, true)
			for _, device := range got.Devices {
				if device.HotspotTemperature.Available || device.HotspotTemperature.Unit != "C" {
					t.Fatalf("unexpected hotspot: %+v", device)
				}
				if device.VRAMTemperature.Available || device.VRAMTemperature.Unit != "C" {
					t.Fatalf("unexpected memory junction: %+v", device)
				}
			}
		})
	}
}

// The memory junction follows the hotspot's identity rules and is kept apart
// from it: a card reporting only one of the two gets only that one.
func TestWindowsGPUMemoryJunction(t *testing.T) {
	gpu := GPUSet{Available: true, Devices: []GPUMetric{{Name: "NVIDIA GeForce RTX 4090"}, {Name: "NVIDIA GeForce RTX 2080"}}}
	hotspots := []gpuSensorReading{{Name: "NVIDIA GeForce RTX 4090", Value: 66}, {Name: "NVIDIA GeForce RTX 2080", Value: 71}}
	vram := []gpuSensorReading{{Name: "nvidia geforce rtx 4090", Value: 96}}
	got := mergeWindowsGPUSensors(gpu, hotspots, vram, true)
	if v := got.Devices[0].VRAMTemperature; !v.Available || v.Value != 96 || got.Devices[0].HotspotTemperature.Value != 66 {
		t.Fatalf("RTX 4090: %+v", got.Devices[0])
	}
	if v := got.Devices[1].VRAMTemperature; v.Available || v.Error != vramNotReported || !got.Devices[1].HotspotTemperature.Available {
		t.Fatalf("RTX 2080 has no memory junction: %+v", got.Devices[1])
	}
	if gpu.Devices[0].VRAMTemperature.Available {
		t.Fatal("enrichment mutated cached device")
	}
	for _, tc := range []struct {
		name string
		vram []gpuSensorReading
	}{
		{"above the junction's range", []gpuSensorReading{{Name: "NVIDIA GeForce RTX 4090", Value: 127}}},
		{"two readings for one card", []gpuSensorReading{{Name: "NVIDIA GeForce RTX 4090", Value: 90}, {Name: "NVIDIA GeForce RTX 4090", Value: 92}}},
	} {
		got := mergeWindowsGPUSensors(gpu, hotspots, tc.vram, true)
		if got.Devices[0].VRAMTemperature.Available || !got.Devices[0].HotspotTemperature.Available {
			t.Fatalf("%s: %+v", tc.name, got.Devices[0])
		}
	}
	data, err := json.Marshal(mergeWindowsGPUSensors(gpu, hotspots, vram, true))
	if err != nil || !strings.Contains(string(data), `"vram_temperature_celsius":{"available":true,"value":96,"unit":"C"}`) {
		t.Fatalf("JSON: %s, %v", data, err)
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
