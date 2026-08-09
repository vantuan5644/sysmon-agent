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

func TestSummarizeCollectionErrorsOmitsNoSwapConfiguredButReportsFailures(t *testing.T) {
	available := func() Metrics {
		metrics := baseMetrics("labbox")
		metrics.CPU = availableNumber(10, "%")
		metrics.Memory = availableCapacity(1, 2)
		metrics.MemorySwap = unavailableCapacity("no swap configured")
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
		return metrics
	}

	// "no swap configured" is a normal state and must not surface as an error...
	if got := summarizeCollectionErrors(available()); got != nil {
		t.Fatalf("collection errors for no-swap host = %#v, want nil", got)
	}
	// ...nor must an unset (zero-value) swap field ...
	zeroSwap := available()
	zeroSwap.MemorySwap = CapacityMetric{}
	if got := summarizeCollectionErrors(zeroSwap); got != nil {
		t.Fatalf("collection errors for unset swap = %#v, want nil", got)
	}
	// ...but a genuine swap read failure still rolls up.
	failed := available()
	failed.MemorySwap = unavailableCapacity("/proc/meminfo read denied")
	got := summarizeCollectionErrors(failed)
	want := []string{"swap: /proc/meminfo read denied"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("collection errors = %#v, want %#v", got, want)
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

// TestSummarizeCollectionErrorsOmitsTailscale locks in the intentional design
// that an unavailable Tailscale status never becomes a collection error.
// Tailscale is an optional status indicator, not a sensor expected on every
// host: surfacing "tailscale CLI not found" would permanently flag every
// non-Tailscale machine. Its state is shown only via the NET card status pill.
func TestSummarizeCollectionErrorsOmitsTailscale(t *testing.T) {
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
	// Tailscale not installed / unavailable with a descriptive reason.
	metrics.Tailscale = TailscaleStatus{Available: false, Error: "tailscale CLI not found"}

	got := summarizeCollectionErrors(metrics)
	for _, msg := range got {
		if strings.Contains(msg, "tailscale") {
			t.Fatalf("collection errors surfaced optional Tailscale status as an issue: %q\nfull: %#v", msg, got)
		}
	}
	if len(got) != 0 {
		t.Fatalf("collection errors = %#v, want empty when only optional Tailscale is unavailable", got)
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

func TestPickCPUTemperaturePrefersTctlOverWarmerPackage(t *testing.T) {
	// Tctl must win even though Package reads hotter: they are distinct AMD
	// registers, and letting the warmer one win is what made cpu_temperature
	// mean different things on Linux and Windows.
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
	if !got.Available || got.Value != 48 {
		t.Fatalf("pickCPUTemperature = %+v, want 48 C (Tctl outranks a warmer Package)", got)
	}
}

// TestPickCPUTemperatureRejectsHotterNonDieSensors uses the exact sensor set
// measured on BBLWIN (ASUS X670E Hero + 7950X, LibreHardwareMonitor) at idle.
// Every rejected candidate here previously outranked or could outrank the die
// reading purely by being hot. See docs/windows-idle-tuning.md.
func TestPickCPUTemperatureRejectsHotterNonDieSensors(t *testing.T) {
	temps := TemperatureSet{
		Available: true,
		Sensors: []TemperatureMetric{
			{Name: "AMD Ryzen 9 7950X CCD1 (Tdie)", Celsius: availableNumber(37, "C")},
			{Name: "AMD Ryzen 9 7950X CCD2 (Tdie)", Celsius: availableNumber(36.75, "C")},
			{Name: "AMD Ryzen 9 7950X CCDs Average (Tdie)", Celsius: availableNumber(36.88, "C")},
			{Name: "AMD Ryzen 9 7950X CCDs Max (Tdie)", Celsius: availableNumber(37, "C")},
			{Name: "AMD Ryzen 9 7950X Core (Tctl/Tdie)", Celsius: availableNumber(59.5, "C")},
			{Name: "AMD Ryzen 9 7950X IOD Hotspot", Celsius: availableNumber(43.5, "C")},
			{Name: "AMD Ryzen 9 7950X L3 (CCD1)", Celsius: availableNumber(32.75, "C")},
			{Name: "AMD Ryzen 9 7950X Package", Celsius: availableNumber(43.34, "C")},
			{Name: "ASUS ROG CROSSHAIR X670E HERO Nuvoton NCT6799D CPU", Celsius: availableNumber(52, "C")},
		},
	}
	got := pickCPUTemperature(temps)
	if !got.Available || got.Value != 59.5 {
		t.Fatalf("pickCPUTemperature = %+v, want 59.5 C (Core (Tctl/Tdie))", got)
	}
}

// The board super-IO sensor is named "... CPU" and can read hotter than the die.
// It must never win, or the reported CPU temperature silently becomes a socket
// reading on some boards and a die reading on others.
func TestPickCPUTemperatureIgnoresHotterBoardSocketSensor(t *testing.T) {
	temps := TemperatureSet{
		Available: true,
		Sensors: []TemperatureMetric{
			{Name: "k10temp Tctl", Celsius: availableNumber(45, "C")},
			{Name: "nct6799 CPU", Celsius: availableNumber(61, "C")},
		},
	}
	got := pickCPUTemperature(temps)
	if !got.Available || got.Value != 45 {
		t.Fatalf("pickCPUTemperature = %+v, want 45 C (k10temp Tctl, not the board sensor)", got)
	}
}

func TestPickCPUTemperaturePrefersIntelPackageOverPerCore(t *testing.T) {
	temps := TemperatureSet{
		Available: true,
		Sensors: []TemperatureMetric{
			{Name: "coretemp Core 0", Celsius: availableNumber(71, "C")},
			{Name: "coretemp Core 3", Celsius: availableNumber(74, "C")},
			{Name: "coretemp Package id 0", Celsius: availableNumber(70, "C")},
		},
	}
	got := pickCPUTemperature(temps)
	if !got.Available || got.Value != 70 {
		t.Fatalf("pickCPUTemperature = %+v, want 70 C (Package id 0 outranks a hotter core)", got)
	}
}

// A host exposing only narrower-than-die sensors must still report something
// rather than degrade to unavailable -- the per-metric degradation invariant.
func TestPickCPUTemperatureFallsBackWhenNoDieSensorExists(t *testing.T) {
	temps := TemperatureSet{
		Available: true,
		Sensors: []TemperatureMetric{
			{Name: "AMD Ryzen 9 7950X CCD1 (Tdie)", Celsius: availableNumber(37, "C")},
			{Name: "AMD Ryzen 9 7950X CCD2 (Tdie)", Celsius: availableNumber(39, "C")},
		},
	}
	got := pickCPUTemperature(temps)
	if !got.Available || got.Value != 39 {
		t.Fatalf("pickCPUTemperature = %+v, want 39 C (warmest CCD as last resort)", got)
	}
}

// The canonical table must win over the ranking, and must report which sensor
// it used. Tctl is cooler than CCD1 here, exactly as measured on BBLWIN under
// load, so a temperature-driven pick would get this wrong.
func TestPickCPUTemperatureSensorUsesCanonicalTable(t *testing.T) {
	temps := TemperatureSet{
		Available: true,
		Sensors: []TemperatureMetric{
			{Name: "AMD Ryzen 9 7950X CCD1 (Tdie)", Celsius: availableNumber(66, "C")},
			{Name: "AMD Ryzen 9 7950X Core (Tctl/Tdie)", Celsius: availableNumber(62.38, "C")},
			{Name: "AMD Ryzen 9 7950X Package", Celsius: availableNumber(50.2, "C")},
			{Name: "ASUS ROG CROSSHAIR X670E HERO Nuvoton NCT6799D CPU", Celsius: availableNumber(55.5, "C")},
		},
	}
	value, sensor := pickCPUTemperatureSensor(temps)
	if !value.Available || value.Value != 62.38 {
		t.Fatalf("value = %+v, want 62.38 C (canonical Core (Tctl/Tdie))", value)
	}
	if sensor != "AMD Ryzen 9 7950X Core (Tctl/Tdie)" {
		t.Fatalf("sensor = %q, want the canonical AMD LHM sensor", sensor)
	}
}

// The LHM name is model-prefixed, so the table matches by suffix. A different
// AMD part must resolve without a table edit.
func TestPickCPUTemperatureSensorMatchesAnyModelPrefix(t *testing.T) {
	for _, name := range []string{
		"AMD Ryzen 9 7950X Core (Tctl/Tdie)",
		"AMD Ryzen 5 5600X Core (Tctl/Tdie)",
		"AMD Ryzen Threadripper PRO 7995WX Core (Tctl/Tdie)",
	} {
		temps := TemperatureSet{Available: true, Sensors: []TemperatureMetric{
			{Name: name, Celsius: availableNumber(51, "C")},
			{Name: "some board CPU", Celsius: availableNumber(80, "C")},
		}}
		value, sensor := pickCPUTemperatureSensor(temps)
		if !value.Available || value.Value != 51 || sensor != name {
			t.Errorf("for %q: value = %+v sensor = %q, want 51 C from that sensor", name, value, sensor)
		}
	}
}

// Table order decides, not temperature: a host exposing both the AMD LHM sensor
// and a hwmon k10temp must not have the choice flip when one runs hotter.
func TestPickCPUTemperatureSensorTableOrderBeatsTemperature(t *testing.T) {
	temps := TemperatureSet{
		Available: true,
		Sensors: []TemperatureMetric{
			{Name: "k10temp Tctl", Celsius: availableNumber(70, "C")},
			{Name: "AMD Ryzen 9 7950X Core (Tctl/Tdie)", Celsius: availableNumber(44, "C")},
		},
	}
	_, sensor := pickCPUTemperatureSensor(temps)
	if sensor != "AMD Ryzen 9 7950X Core (Tctl/Tdie)" {
		t.Fatalf("sensor = %q, want the first table entry regardless of temperature", sensor)
	}
}

func TestPickCPUTemperatureSensorFallsBackToRankOffTable(t *testing.T) {
	temps := TemperatureSet{
		Available: true,
		Sensors: []TemperatureMetric{
			{Name: "some-soc CPU", Celsius: availableNumber(58, "C")},
			{Name: "some-soc CPU Package", Celsius: availableNumber(55, "C")},
		},
	}
	value, sensor := pickCPUTemperatureSensor(temps)
	// "some-soc CPU Package" ends with the canonical Intel LHM suffix, so the
	// table claims it before the ranking runs.
	if !value.Available || value.Value != 55 || sensor != "some-soc CPU Package" {
		t.Fatalf("value = %+v sensor = %q, want the canonical package sensor", value, sensor)
	}
}

func TestPickCPUTemperatureSensorReportsEmptyNameWhenUnresolved(t *testing.T) {
	_, sensor := pickCPUTemperatureSensor(TemperatureSet{Available: false, Error: "no sensors"})
	if sensor != "" {
		t.Fatalf("sensor = %q, want empty when nothing was resolved", sensor)
	}
}

// Guards the substring traps called out in cpuDieTemperatureRank: "socket"
// contains "soc", "diode" contains "iod", and "junction" contains "nct".
func TestCPUDieTemperatureRankSubstringTraps(t *testing.T) {
	for _, name := range []string{"CPU Socket", "CPU Diode", "CPU Junction"} {
		if got := cpuDieTemperatureRank(name); got == cpuTempRankNone {
			t.Errorf("cpuDieTemperatureRank(%q) = none, want a real rank", name)
		}
	}
	for _, name := range []string{
		"AMD Ryzen 9 7950X CCD1 (Tdie)",
		"AMD Ryzen 9 7950X L3 (CCD1)",
		"AMD Ryzen 9 7950X IOD Hotspot",
		"AMD Ryzen 9 7950X CCDs Average (Tdie)",
		"ASUS ROG CROSSHAIR X670E HERO Nuvoton NCT6799D CPU",
		"CPU VRM",
	} {
		if got := cpuDieTemperatureRank(name); got != cpuTempRankNone {
			t.Errorf("cpuDieTemperatureRank(%q) = %d, want none", name, got)
		}
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
