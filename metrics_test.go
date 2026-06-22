package main

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestAvailableNumberRejectsNonFiniteValues(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value float64
	}{
		{name: "nan", value: math.NaN()},
		{name: "positive infinity", value: math.Inf(1)},
		{name: "negative infinity", value: math.Inf(-1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := availableNumber(tc.value, "%")
			if got.Available {
				t.Fatalf("availableNumber(%v) returned available: %+v", tc.value, got)
			}
			if got.Unit != "%" {
				t.Fatalf("unit = %q, want %%", got.Unit)
			}
			if !strings.Contains(got.Error, "invalid") {
				t.Fatalf("error = %q, want invalid numeric value", got.Error)
			}
		})
	}
}

func TestRoundRejectsNonFiniteValues(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		got := round(value, 2)
		if got != 0 {
			t.Fatalf("round(%v) = %v, want 0", value, got)
		}
	}
}

func TestAvailableCapacity(t *testing.T) {
	got := availableCapacity(25, 100)
	if !got.Available {
		t.Fatalf("availableCapacity returned unavailable: %+v", got)
	}
	if got.UsedBytes != 25 || got.TotalBytes != 100 || got.Percent != 25 {
		t.Fatalf("capacity = %+v, want 25/100/25%%", got)
	}
}

func TestAvailableCapacityRejectsZeroTotal(t *testing.T) {
	got := availableCapacity(0, 0)
	if got.Available {
		t.Fatalf("availableCapacity accepted zero total: %+v", got)
	}
	if got.Error == "" {
		t.Fatalf("availableCapacity zero total missing error: %+v", got)
	}
}

func TestAvailableCapacityClampsUsedToTotal(t *testing.T) {
	got := availableCapacity(150, 100)
	if !got.Available {
		t.Fatalf("availableCapacity returned unavailable: %+v", got)
	}
	if got.UsedBytes != 100 || got.Percent != 100 {
		t.Fatalf("capacity = %+v, want used clamped to 100 and percent 100", got)
	}
}

func TestAvailableCapacityFromTotalFree(t *testing.T) {
	got := availableCapacityFromTotalFree(100, 25, "invalid counters")
	if !got.Available {
		t.Fatalf("availableCapacityFromTotalFree returned unavailable: %+v", got)
	}
	if got.UsedBytes != 75 || got.TotalBytes != 100 || got.Percent != 75 {
		t.Fatalf("capacity = %+v, want 75/100/75%%", got)
	}
}

func TestAvailableCapacityFromTotalFreeRejectsInvertedCounters(t *testing.T) {
	got := availableCapacityFromTotalFree(100, 125, "invalid counters")
	if got.Available {
		t.Fatalf("availableCapacityFromTotalFree accepted inverted counters: %+v", got)
	}
	if got.Error != "invalid counters" {
		t.Fatalf("error = %q, want invalid counters", got.Error)
	}
}

func TestKiBToBytes(t *testing.T) {
	got, ok := kibToBytes(4096)
	if !ok {
		t.Fatal("kibToBytes rejected valid value")
	}
	if got != 4*1024*1024 {
		t.Fatalf("bytes = %d, want 4194304", got)
	}
}

func TestKiBToBytesRejectsOverflow(t *testing.T) {
	if got, ok := kibToBytes(^uint64(0)); ok || got != 0 {
		t.Fatalf("kibToBytes overflow = %d, %v; want 0, false", got, ok)
	}
}

func TestSumUint64(t *testing.T) {
	got, ok := sumUint64(1, 2, 3)
	if !ok {
		t.Fatal("sumUint64 rejected valid values")
	}
	if got != 6 {
		t.Fatalf("sum = %d, want 6", got)
	}
}

func TestSumUint64RejectsOverflow(t *testing.T) {
	if got, ok := sumUint64(^uint64(0), 1); ok || got != 0 {
		t.Fatalf("sumUint64 overflow = %d, %v; want 0, false", got, ok)
	}
}

func TestEnsureDiskMetricsAddsUnavailablePlaceholder(t *testing.T) {
	got := ensureDiskMetrics(nil, "no disks")
	if len(got) != 1 {
		t.Fatalf("disk metrics length = %d, want 1", len(got))
	}
	if got[0].Name != "unavailable" || got[0].Capacity.Available || got[0].Capacity.Error != "no disks" {
		t.Fatalf("placeholder disk = %+v, want unavailable no disks row", got[0])
	}
}

func TestEnsureDiskMetricsPreservesExistingRows(t *testing.T) {
	disks := []DiskMetric{{
		Name:       "root",
		Mountpoint: "/",
		Capacity:   availableCapacity(1, 2),
	}}
	got := ensureDiskMetrics(disks, "unused")
	if len(got) != 1 || got[0].Name != "root" || !got[0].Capacity.Available {
		t.Fatalf("disk metrics = %+v, want original row", got)
	}
}

func TestSummarizeCollectionErrorsReportsTopLevelAndPartialFailures(t *testing.T) {
	metrics := baseMetrics("labbox")
	metrics.CPU = unavailableNumber("%", "cpu denied")
	metrics.Memory = availableCapacity(1, 2)
	metrics.Disks = []DiskMetric{
		{Name: "root", Mountpoint: "/", Capacity: availableCapacity(1, 2)},
		{Name: "backup", Mountpoint: "/backup", Capacity: unavailableCapacity("statfs denied")},
	}
	metrics.Network = NetworkSet{Available: false, Error: "no adapters"}
	metrics.Temperatures = TemperatureSet{Available: true, Sensors: []TemperatureMetric{
		{Name: "CPU", Celsius: availableNumber(42, "C")},
		{Name: "Mystery", Celsius: unavailableNumber("C", "sensor disappeared")},
	}}
	metrics.GPU = GPUSet{
		Available: true,
		Error:     "nvidia-smi not found",
		Devices: []GPUMetric{{
			Name:        "Intel GPU",
			Usage:       unavailableNumber("%", "usage not exposed"),
			Memory:      unavailableCapacity("VRAM not exposed"),
			Temperature: availableNumber(49, "C"),
		}},
	}

	got := summarizeCollectionErrors(metrics)
	want := []string{
		"cpu_percent: cpu denied",
		"disk /backup: statfs denied",
		"network: no adapters",
		"temperature Mystery: sensor disappeared",
		"gpu: nvidia-smi not found",
		"gpu Intel GPU usage: usage not exposed",
		"gpu Intel GPU memory: VRAM not exposed",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("collection errors = %#v, want %#v", got, want)
	}
}

func TestSummarizeCollectionErrorsReturnsNilWhenEverythingIsAvailable(t *testing.T) {
	metrics := baseMetrics("labbox")
	metrics.CPU = availableNumber(10, "%")
	metrics.Memory = availableCapacity(1, 2)
	metrics.Disks = []DiskMetric{{Name: "root", Mountpoint: "/", Capacity: availableCapacity(1, 2)}}
	metrics.Network = NetworkSet{Available: true, Interfaces: []NetworkInterfaceMetric{{
		Name:             "eth0",
		RXBytesPerSecond: availableNumber(1, "B/s"),
		TXBytesPerSecond: availableNumber(2, "B/s"),
	}}}
	metrics.Temperatures = TemperatureSet{Available: true, Sensors: []TemperatureMetric{{Name: "CPU", Celsius: availableNumber(42, "C")}}}
	metrics.GPU = GPUSet{Available: true, Devices: []GPUMetric{{
		Name:        "GPU",
		Usage:       availableNumber(1, "%"),
		Memory:      availableCapacity(1, 2),
		Temperature: availableNumber(42, "C"),
	}}}

	if got := summarizeCollectionErrors(metrics); got != nil {
		t.Fatalf("collection errors = %#v, want nil", got)
	}
}

func TestFinishMetricsRecordsDurationAndRefreshesCollectionErrors(t *testing.T) {
	metrics := baseMetrics("labbox")
	metrics.CPU = unavailableNumber("%", "cpu denied")
	metrics.Memory = availableCapacity(1, 2)
	metrics.Disks = []DiskMetric{{Name: "root", Mountpoint: "/", Capacity: availableCapacity(1, 2)}}
	metrics.Network = NetworkSet{Available: false, Error: "no adapters"}
	metrics.Temperatures = TemperatureSet{Available: false, Error: "no sensors"}
	metrics.GPU = GPUSet{Available: false, Error: "no gpu"}
	metrics.CollectionErrors = []string{"stale"}

	got := finishMetrics(metrics, time.Now().Add(-25*time.Millisecond))
	if got.CollectionDurationMS < 0 {
		t.Fatalf("collection duration = %d, want non-negative", got.CollectionDurationMS)
	}
	want := []string{
		"cpu_percent: cpu denied",
		"network: no adapters",
		"temperatures: no sensors",
		"gpu: no gpu",
	}
	if strings.Join(got.CollectionErrors, "\n") != strings.Join(want, "\n") {
		t.Fatalf("collection errors = %#v, want %#v", got.CollectionErrors, want)
	}
}

func TestIsCPUTemperatureSensorClassifiesSensors(t *testing.T) {
	cases := map[string]bool{
		// CPU die readings (Linux hwmon + LibreHardwareMonitor naming)
		"AMD Ryzen 9 7950X Package":                 true,
		"AMD Ryzen 9 7950X Core (Tctl/Tdie)":        true,
		"k10temp Tctl":                              true,
		"coretemp Package id 0":                     true,
		"coretemp Core 0":                           true,
		"ASUS ROG CROSSHAIR X670E HERO Nuvoton CPU": true,
		"Intel Core i9-13900K":                      true,
		"CPU":                                       true,
		// Non-CPU sensors must be rejected
		"NVIDIA GeForce RTX 4090 GPU Core": false,
		"AMD Radeon Graphics GPU VR SoC":   false,
		"Kingston DIMM #1":                 false,
		"ASUS Motherboard":                 false,
		"Water In":                         false,
		"Ambient":                          false,
		"Samsung NVMe SSD":                 false,
		"Chipset":                          false,
	}
	for name, want := range cases {
		if got := isCPUTemperatureSensor(name); got != want {
			t.Errorf("isCPUTemperatureSensor(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestPickCPUTemperatureReturnsWarmestCpuDie(t *testing.T) {
	temps := TemperatureSet{
		Available: true,
		Sensors: []TemperatureMetric{
			{Name: "ASUS Motherboard", Celsius: availableNumber(40, "C")},
			{Name: "AMD Ryzen 9 7950X Core (Tctl/Tdie)", Celsius: availableNumber(48, "C")},
			{Name: "AMD Ryzen 9 7950X Package", Celsius: availableNumber(50, "C")},
			{Name: "NVIDIA GeForce RTX 4090 GPU Core", Celsius: availableNumber(55, "C")},
			{Name: "dead core", Celsius: unavailableNumber("C", "no reading")},
		},
	}
	got := pickCPUTemperature(temps)
	if !got.Available || got.Value != 50 {
		t.Fatalf("pickCPUTemperature = %+v, want 50 C (Package, warmest CPU die)", got)
	}
}

func TestPickCPUTemperatureUnavailableWhenNoCpuSensor(t *testing.T) {
	temps := TemperatureSet{
		Available: true,
		Sensors: []TemperatureMetric{
			{Name: "NVIDIA GPU Core", Celsius: availableNumber(55, "C")},
			{Name: "Motherboard", Celsius: availableNumber(40, "C")},
		},
	}
	got := pickCPUTemperature(temps)
	if got.Available {
		t.Fatalf("pickCPUTemperature = %+v, want unavailable when no CPU sensor present", got)
	}
}

func TestPickCPUTemperaturePropagatesUnavailableSet(t *testing.T) {
	got := pickCPUTemperature(TemperatureSet{Available: false, Error: "no sensors"})
	if got.Available || got.Error != "no sensors" {
		t.Fatalf("pickCPUTemperature = %+v, want unavailable with propagated error", got)
	}
}
