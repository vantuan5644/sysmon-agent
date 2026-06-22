package main

import (
	"math"
	"strings"
	"testing"
)

func TestParseFloatRejectsNonFiniteValues(t *testing.T) {
	for _, raw := range []string{"NaN", "+Inf", "-Inf", "Inf"} {
		if value, ok := parseFloat(raw); ok {
			t.Fatalf("parseFloat(%q) = %v, true; want unavailable", raw, value)
		}
	}
}

func TestParseNonNegativeFloatRejectsNegativeValues(t *testing.T) {
	if value, ok := parseNonNegativeFloat("-1"); ok {
		t.Fatalf("parseNonNegativeFloat accepted %v, want unavailable", value)
	}
	if value, ok := parseNonNegativeFloat("1.5"); !ok || value != 1.5 {
		t.Fatalf("parseNonNegativeFloat valid value = %v/%v, want 1.5/true", value, ok)
	}
}

func TestMiBFloatToBytes(t *testing.T) {
	got, ok := mibFloatToBytes(1.5)
	if !ok {
		t.Fatal("mibFloatToBytes rejected valid value")
	}
	if got != 1572864 {
		t.Fatalf("bytes = %d, want 1572864", got)
	}
}

func TestMiBFloatToBytesRejectsInvalidValues(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), -1, math.MaxFloat64} {
		if got, ok := mibFloatToBytes(value); ok || got != 0 {
			t.Fatalf("mibFloatToBytes(%v) = %d, %v; want 0, false", value, got, ok)
		}
	}
}

func TestNVIDIAMemoryCapacityRejectsInvalidCounters(t *testing.T) {
	for _, tc := range []struct {
		name     string
		usedMiB  float64
		totalMiB float64
	}{
		{name: "zero total", usedMiB: 0, totalMiB: 0},
		{name: "used exceeds total", usedMiB: 4096, totalMiB: 1024},
		{name: "used overflow", usedMiB: math.MaxFloat64, totalMiB: 1024},
		{name: "total overflow", usedMiB: 1024, totalMiB: math.MaxFloat64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := nvidiaMemoryCapacity(tc.usedMiB, tc.totalMiB)
			if got.Available {
				t.Fatalf("nvidiaMemoryCapacity = %+v, want unavailable", got)
			}
			if got.Error != "invalid nvidia-smi memory counters" {
				t.Fatalf("error = %q, want invalid nvidia-smi memory counters", got.Error)
			}
		})
	}
}

func TestParseNVIDIAGPUCSVRejectsNonFiniteValues(t *testing.T) {
	got := parseNVIDIAGPUCSV("GPU 0, NaN, Inf, 4096, -Inf\n")
	if !got.Available || len(got.Devices) != 1 {
		t.Fatalf("GPU set = %+v, want one degraded device", got)
	}
	device := got.Devices[0]
	if device.Usage.Available {
		t.Fatalf("usage unexpectedly available: %+v", device.Usage)
	}
	if device.Memory.Available {
		t.Fatalf("memory unexpectedly available: %+v", device.Memory)
	}
	if device.Temperature.Available {
		t.Fatalf("temperature unexpectedly available: %+v", device.Temperature)
	}
}

func TestParseNVIDIAGPUCSVRejectsNegativeMemoryValues(t *testing.T) {
	got := parseNVIDIAGPUCSV("GPU 0, 25, -1, 4096, 60\nGPU 1, 25, 1024, -1, 60\n")
	if !got.Available || len(got.Devices) != 2 {
		t.Fatalf("GPU set = %+v, want two degraded devices", got)
	}
	for _, device := range got.Devices {
		if device.Memory.Available {
			t.Fatalf("%s memory unexpectedly available: %+v", device.Name, device.Memory)
		}
		if !device.Usage.Available || !device.Temperature.Available {
			t.Fatalf("%s usage/temperature should remain available: %+v", device.Name, device)
		}
	}
}

func TestParseNVIDIAGPUCSVRejectsInvalidMemoryCounters(t *testing.T) {
	got := parseNVIDIAGPUCSV("GPU 0, 25, 4096, 1024, 60\nGPU 1, 25, 1e40, 4096, 60\n")
	if !got.Available || len(got.Devices) != 2 {
		t.Fatalf("GPU set = %+v, want two degraded devices", got)
	}
	for _, device := range got.Devices {
		if device.Memory.Available {
			t.Fatalf("%s memory unexpectedly available: %+v", device.Name, device.Memory)
		}
		if !strings.Contains(device.Memory.Error, "invalid nvidia-smi memory counters") {
			t.Fatalf("%s memory error = %q, want invalid counters", device.Name, device.Memory.Error)
		}
	}
}

func TestParseNVIDIAGPUCSVParsesValidValues(t *testing.T) {
	got := parseNVIDIAGPUCSV("GPU 0, 25, 1024, 4096, 60\n")
	if !got.Available || len(got.Devices) != 1 {
		t.Fatalf("GPU set = %+v, want one available device", got)
	}
	device := got.Devices[0]
	if !device.Usage.Available || device.Usage.Value != 25 {
		t.Fatalf("usage = %+v, want 25%%", device.Usage)
	}
	if !device.Memory.Available || device.Memory.Percent != 25 {
		t.Fatalf("memory = %+v, want 25%%", device.Memory)
	}
	if !device.Temperature.Available || device.Temperature.Value != 60 {
		t.Fatalf("temperature = %+v, want 60C", device.Temperature)
	}
	if device.Power.Available {
		t.Fatalf("power = %+v, want unavailable when column omitted", device.Power)
	}
}

func TestParseNVIDIAGPUCSVParsesPowerDraw(t *testing.T) {
	got := parseNVIDIAGPUCSV("GPU 0, 25, 1024, 4096, 60, 245.5\n")
	if !got.Available || len(got.Devices) != 1 {
		t.Fatalf("GPU set = %+v, want one available device", got)
	}
	device := got.Devices[0]
	if !device.Power.Available || device.Power.Value != 245.5 || device.Power.Unit != "W" {
		t.Fatalf("power = %+v, want 245.5 W", device.Power)
	}
}

func TestNVIDIAPowerMetricHandlesMissingAndInvalidColumns(t *testing.T) {
	for _, tc := range []struct {
		name    string
		columns []string
	}{
		{name: "nil", columns: nil},
		{name: "blank", columns: []string{""}},
		{name: "not supported", columns: []string{"[Not Supported]"}},
		{name: "na", columns: []string{"N/A"}},
		{name: "negative", columns: []string{"-5"}},
		{name: "non-numeric", columns: []string{"abc"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := nvidiaPowerMetric(tc.columns); got.Available {
				t.Fatalf("nvidiaPowerMetric(%v) = %+v, want unavailable", tc.columns, got)
			}
		})
	}
	got := nvidiaPowerMetric([]string{"120.25"})
	if !got.Available || got.Value != 120.25 || got.Unit != "W" {
		t.Fatalf("nvidiaPowerMetric([120.25]) = %+v, want 120.25 W", got)
	}
}
