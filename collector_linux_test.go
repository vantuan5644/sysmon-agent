//go:build linux

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCPUTimes(t *testing.T) {
	got, err := parseCPUTimes("cpu  4705 150 2252 136239 200 0 90 0 0 0\ncpu0 1 2 3 4\n")
	if err != nil {
		t.Fatal(err)
	}
	if got.total != 143636 {
		t.Fatalf("total = %d, want 143636", got.total)
	}
	if got.idle != 136439 {
		t.Fatalf("idle = %d, want 136439", got.idle)
	}
}

func TestLinuxCollectorRunsMetricGroupsConcurrently(t *testing.T) {
	data, err := os.ReadFile("collector_linux.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	for _, needle := range []string{
		`var wg sync.WaitGroup`,
		`collectMetricAsync(&wg, &cpu`,
		`collectMetricAsync(&wg, &memory`,
		`collectMetricAsync(&wg, &disks`,
		`collectMetricAsync(&wg, &network`,
		`collectMetricAsync(&wg, &temperatures`,
		`collectMetricAsync(&wg, &gpu`,
		`wg.Wait()`,
		`metrics.CPU = cpu`,
		`metrics.Memory = memory`,
		`metrics.Disks = disks`,
		`metrics.Network = network`,
		`metrics.Temperatures = temperatures`,
		`metrics.GPU = gpu`,
		`Linux CPU collector panicked`,
		`Linux GPU collector panicked`,
	} {
		if !strings.Contains(source, needle) {
			t.Fatalf("collector_linux.go missing concurrent Linux collection behavior %q", needle)
		}
	}
}

func TestParseCPUTimesRejectsOverflowingTotalCounters(t *testing.T) {
	_, err := parseCPUTimes("cpu  18446744073709551615 1 0 0 0\n")
	if err == nil {
		t.Fatal("parseCPUTimes accepted overflowing total counters")
	}
	if !strings.Contains(err.Error(), "cpu counters") {
		t.Fatalf("parseCPUTimes error = %v, want cpu counter context", err)
	}
}

func TestParseMemInfo(t *testing.T) {
	total, available, err := parseMemInfo(`MemTotal:       16384000 kB
MemFree:         1024000 kB
MemAvailable:    8192000 kB
Buffers:          100000 kB
Cached:           200000 kB
`)
	if err != nil {
		t.Fatal(err)
	}
	if total != 16384000*1024 {
		t.Fatalf("total = %d", total)
	}
	if available != 8192000*1024 {
		t.Fatalf("available = %d", available)
	}
}

func TestParseMemInfoFallback(t *testing.T) {
	_, available, err := parseMemInfo(`MemTotal: 1000 kB
MemFree: 100 kB
Buffers: 50 kB
Cached: 25 kB
`)
	if err != nil {
		t.Fatal(err)
	}
	if available != 175*1024 {
		t.Fatalf("available = %d, want %d", available, 175*1024)
	}
}

func TestParseMemInfoRejectsOverflowingKilobytes(t *testing.T) {
	_, _, err := parseMemInfo(`MemTotal: 18014398509481984 kB
MemAvailable: 1000 kB
`)
	if err == nil {
		t.Fatal("parseMemInfo accepted overflowing MemTotal")
	}
	if !strings.Contains(err.Error(), "MemTotal") {
		t.Fatalf("parseMemInfo error = %v, want MemTotal context", err)
	}
}

func TestParseMemInfoRejectsOverflowingFallbackCounters(t *testing.T) {
	_, _, err := parseMemInfo(`MemTotal: 1000 kB
MemFree: 9007199254740991 kB
Buffers: 9007199254740991 kB
Cached: 1000 kB
`)
	if err == nil {
		t.Fatal("parseMemInfo accepted overflowing fallback counters")
	}
	if !strings.Contains(err.Error(), "fallback counters") {
		t.Fatalf("parseMemInfo error = %v, want fallback counter context", err)
	}
}

func TestParseMountsUnescapesLinuxMountFields(t *testing.T) {
	got := parseMounts(`/dev/sda1 / ext4 rw 0 0
/dev/sdb1 /mnt/media\040drive ext4 rw 0 0
/dev/sdc1 /mnt/tab\011name ext4 rw 0 0
/dev/sdd1 /mnt/newline\012name ext4 rw 0 0
/dev/disk/by-label/data\134backup /mnt/backup ext4 rw 0 0
`)
	if len(got) != 5 {
		t.Fatalf("mounts = %+v, want 5 parsed rows", got)
	}
	if got[1].mountpoint != "/mnt/media drive" {
		t.Fatalf("space mountpoint = %q", got[1].mountpoint)
	}
	if got[2].mountpoint != "/mnt/tab\tname" {
		t.Fatalf("tab mountpoint = %q", got[2].mountpoint)
	}
	if got[3].mountpoint != "/mnt/newline\nname" {
		t.Fatalf("newline mountpoint = %q", got[3].mountpoint)
	}
	if got[4].device != `/dev/disk/by-label/data\backup` {
		t.Fatalf("backslash device = %q", got[4].device)
	}
}

func TestStatfsBytes(t *testing.T) {
	got, ok := statfsBytes(256, 4096)
	if !ok {
		t.Fatal("statfsBytes rejected valid counters")
	}
	if got != 1024*1024 {
		t.Fatalf("bytes = %d, want 1048576", got)
	}
}

func TestStatfsBytesRejectsInvalidBlockSize(t *testing.T) {
	for _, blockSize := range []int64{0, -4096} {
		if got, ok := statfsBytes(1, blockSize); ok || got != 0 {
			t.Fatalf("statfsBytes(1, %d) = %d, %v; want 0, false", blockSize, got, ok)
		}
	}
}

func TestStatfsBytesRejectsOverflow(t *testing.T) {
	if got, ok := statfsBytes(^uint64(0), 2); ok || got != 0 {
		t.Fatalf("statfsBytes overflow = %d, %v; want 0, false", got, ok)
	}
}

func TestParseNetDev(t *testing.T) {
	got, err := parseNetDev(`Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
  eth0: 1234 1 0 0 0 0 0 0 5678 2 0 0 0 0 0 0
    lo: 100 1 0 0 0 0 0 0 100 1 0 0 0 0 0 0
`)
	if err != nil {
		t.Fatal(err)
	}
	if got["eth0"].rxBytes != 1234 || got["eth0"].txBytes != 5678 {
		t.Fatalf("eth0 counters = %+v", got["eth0"])
	}
}

func TestBuildLinuxNetworkSet(t *testing.T) {
	got := buildLinuxNetworkSet(
		map[string]netCounter{
			"eth0":    {rxBytes: 1000, txBytes: 2000},
			"docker0": {rxBytes: 9000, txBytes: 9000},
			"lo":      {rxBytes: 1, txBytes: 1},
		},
		map[string]netCounter{
			"eth0":            {rxBytes: 1500, txBytes: 2600},
			"wlan0":           {rxBytes: 100, txBytes: 200},
			"docker0":         {rxBytes: 9500, txBytes: 9500},
			"vethabcdef":      {rxBytes: 1000, txBytes: 1000},
			"br-1234567890ab": {rxBytes: 1000, txBytes: 1000},
			"lo":              {rxBytes: 2, txBytes: 2},
		},
		0.5,
	)
	if !got.Available {
		t.Fatalf("network unavailable: %s", got.Error)
	}
	if len(got.Interfaces) != 2 {
		t.Fatalf("interfaces = %+v, want eth0 and wlan0", got.Interfaces)
	}
	if got.Interfaces[0].Name != "eth0" {
		t.Fatalf("first interface = %q, want eth0", got.Interfaces[0].Name)
	}
	if got.Interfaces[0].RXBytesPerSecond.Value != 1000 || got.Interfaces[0].TXBytesPerSecond.Value != 1200 {
		t.Fatalf("eth0 rates = %+v/%+v, want 1000/1200", got.Interfaces[0].RXBytesPerSecond, got.Interfaces[0].TXBytesPerSecond)
	}
	if got.Interfaces[1].Name != "wlan0" || got.Interfaces[1].RXBytesPerSecond.Available {
		t.Fatalf("new interface = %+v, want warming wlan0", got.Interfaces[1])
	}
}

func TestShouldIncludeLinuxNetworkInterface(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"", false},
		{"lo", false},
		{"docker0", false},
		{"br-1234567890ab", false},
		{"vethabcdef", false},
		{"virbr0", false},
		{"cni0", false},
		{"flannel.1", false},
		{"eth0", true},
		{"enp3s0", true},
		{"wlan0", true},
		{"tailscale0", true},
		{"wg0", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldIncludeLinuxNetworkInterface(tc.name); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBuildLinuxNetworkSetRejectsLoopbackOnly(t *testing.T) {
	got := buildLinuxNetworkSet(
		map[string]netCounter{"lo": {rxBytes: 1, txBytes: 1}},
		map[string]netCounter{"lo": {rxBytes: 2, txBytes: 2}},
		1,
	)
	if got.Available || got.Error == "" {
		t.Fatalf("network = %+v, want unavailable loopback-only set", got)
	}
}

func TestBuildLinuxNetworkSetReportsCounterReset(t *testing.T) {
	got := buildLinuxNetworkSet(
		map[string]netCounter{"eth0": {rxBytes: 500, txBytes: 1000}},
		map[string]netCounter{"eth0": {rxBytes: 400, txBytes: 1200}},
		1,
	)
	if !got.Available || len(got.Interfaces) != 1 {
		t.Fatalf("network = %+v, want one degraded interface", got)
	}
	if got.Interfaces[0].RXBytesPerSecond.Available || !strings.Contains(got.Interfaces[0].RXBytesPerSecond.Error, "counter reset") {
		t.Fatalf("rx rate = %+v, want counter reset", got.Interfaces[0].RXBytesPerSecond)
	}
	if !got.Interfaces[0].TXBytesPerSecond.Available || got.Interfaces[0].TXBytesPerSecond.Value != 200 {
		t.Fatalf("tx rate = %+v, want 200 B/s", got.Interfaces[0].TXBytesPerSecond)
	}
}

func TestAppendUniqueTemperatureSensorsMergesThermalZones(t *testing.T) {
	hwmon := []TemperatureMetric{
		{Name: "k10temp Tctl", Celsius: availableNumber(52.2, "C")},
	}
	thermalZones := []TemperatureMetric{
		{Name: " K10TEMP   Tctl ", Celsius: availableNumber(52.2, "C")},
		{Name: "x86_pkg_temp", Celsius: availableNumber(61.3, "C")},
	}

	got := appendUniqueTemperatureSensors(hwmon, thermalZones...)
	if len(got) != 2 {
		t.Fatalf("sensors = %+v, want original hwmon sensor plus distinct thermal zone", got)
	}
	if got[0].Name != "k10temp Tctl" {
		t.Fatalf("first sensor name = %q, want original hwmon display name", got[0].Name)
	}
	if got[1].Name != "x86_pkg_temp" {
		t.Fatalf("second sensor name = %q, want distinct thermal-zone sensor", got[1].Name)
	}
}

func TestCPUUsagePercent(t *testing.T) {
	got, ok := cpuUsagePercent(cpuTimes{idle: 100, total: 1000}, cpuTimes{idle: 150, total: 1200})
	if !ok {
		t.Fatal("cpuUsagePercent returned unavailable")
	}
	if got != 75 {
		t.Fatalf("usage = %v, want 75", got)
	}
}

func TestCPUUsagePercentRejectsNonAdvancingCounters(t *testing.T) {
	if _, ok := cpuUsagePercent(cpuTimes{idle: 100, total: 1000}, cpuTimes{idle: 100, total: 1000}); ok {
		t.Fatal("cpuUsagePercent returned available for non-advancing counters")
	}
}

func TestCPUUsagePercentRejectsIdleDeltaExceedingTotal(t *testing.T) {
	if _, ok := cpuUsagePercent(cpuTimes{idle: 100, total: 1000}, cpuTimes{idle: 400, total: 1100}); ok {
		t.Fatal("cpuUsagePercent returned available for impossible idle delta")
	}
}

func TestSampleCPUAfterDelayStoresLaterBaseline(t *testing.T) {
	collector := &systemCollector{}
	later := cpuTimes{idle: 120, total: 200}
	metric := collector.sampleCPUAfterDelayWithReader(
		context.Background(),
		cpuTimes{idle: 100, total: 100},
		0,
		func() (cpuTimes, error) { return later, nil },
	)
	if !metric.Available || metric.Value != 80 {
		t.Fatalf("CPU metric = %+v, want available 80%%", metric)
	}
	if collector.prevCPU != later {
		t.Fatalf("prevCPU = %+v, want later sample %+v", collector.prevCPU, later)
	}
}

func TestShouldIncludeMount(t *testing.T) {
	cases := []struct {
		name  string
		mount mountInfo
		want  bool
	}{
		{"root ext4", mountInfo{device: "/dev/nvme0n1p2", mountpoint: "/", fsType: "ext4"}, true},
		{"proc", mountInfo{device: "proc", mountpoint: "/proc", fsType: "proc"}, false},
		{"run tmpfs", mountInfo{device: "tmpfs", mountpoint: "/run", fsType: "tmpfs"}, false},
		{"nfs share", mountInfo{device: "nas:/data", mountpoint: "/mnt/nas", fsType: "nfs4"}, false},
		{"cifs share", mountInfo{device: "//nas/shared", mountpoint: "/mnt/shared", fsType: "cifs"}, false},
		{"sshfs share", mountInfo{device: "sshfs#nas:/srv", mountpoint: "/mnt/ssh", fsType: "fuse.sshfs"}, false},
		{"rclone remote", mountInfo{device: "rclone", mountpoint: "/mnt/cloud", fsType: "fuse.rclone"}, false},
		{"docker overlay", mountInfo{device: "overlay", mountpoint: "/var/lib/docker/overlay2/x", fsType: "overlay"}, false},
		{"root overlay", mountInfo{device: "overlay", mountpoint: "/", fsType: "overlay"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldIncludeMount(tc.mount); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsRAPLPackageEntry(t *testing.T) {
	cases := map[string]bool{
		"intel-rapl:0":   true,
		"intel-rapl:1":   true,
		"intel-rapl:0:0": false,
		"intel-rapl:0:2": false,
		"intel-rapl":     false,
		"intel-rapl:abc": false,
		"intel-rapl:":    false,
		"amd-rapl:0":     false,
		"":               false,
	}
	for name, want := range cases {
		if got := isRAPLPackageEntry(name); got != want {
			t.Errorf("isRAPLPackageEntry(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestRAPLEnergyDelta(t *testing.T) {
	if d, ok := raplEnergyDelta(raplCounter{energyUJ: 100}, raplCounter{energyUJ: 300}); !ok || d != 200 {
		t.Fatalf("plain delta = %v/%v, want 200/true", d, ok)
	}
	if _, ok := raplEnergyDelta(raplCounter{energyUJ: 300}, raplCounter{energyUJ: 300}); !ok {
		t.Fatal("zero advance should still report ok with delta 0")
	}
	if d, ok := raplEnergyDelta(raplCounter{energyUJ: 500, maxUJ: 1000}, raplCounter{energyUJ: 100, maxUJ: 1000}); !ok || d != 600 {
		t.Fatalf("wrap delta = %v/%v, want 600/true", d, ok)
	}
	if _, ok := raplEnergyDelta(raplCounter{energyUJ: 500}, raplCounter{energyUJ: 100}); ok {
		t.Fatal("wrap without max should be unavailable")
	}
}

func TestComputeRAPLPower(t *testing.T) {
	prev := map[string]raplCounter{"/sys/class/powercap/intel-rapl:0/energy_uj": {energyUJ: 1_000_000}}
	cur := map[string]raplCounter{"/sys/class/powercap/intel-rapl:0/energy_uj": {energyUJ: 2_000_000}}
	got := computeRAPLPower(prev, cur, 1.0)
	if !got.Available || got.Value != 1.0 || got.Unit != "W" {
		t.Fatalf("power = %+v, want 1 W", got)
	}
	if got := computeRAPLPower(cur, cur, 1.0); got.Available {
		t.Fatalf("no-advance = %+v, want unavailable", got)
	}
	if got := computeRAPLPower(prev, cur, 0); got.Available {
		t.Fatalf("zero elapsed = %+v, want unavailable", got)
	}
}

func TestReadRAPLCountersFiltersSubDomains(t *testing.T) {
	root := t.TempDir()
	pkg := filepath.Join(root, "intel-rapl:0")
	sub := filepath.Join(root, "intel-rapl:0:0")
	for path, value := range map[string]string{
		filepath.Join(pkg, "energy_uj"):           "5000000",
		filepath.Join(pkg, "max_energy_range_uj"): "262143999999999",
		filepath.Join(sub, "energy_uj"):           "1000000",
	} {
		if err := writeTestFile(path, value); err != nil {
			t.Fatal(err)
		}
	}
	got, err := readRAPLCounters(root)
	if err != nil {
		t.Fatalf("readRAPLCounters err = %v", err)
	}
	counter, ok := got[filepath.Join(pkg, "energy_uj")]
	if !ok {
		t.Fatalf("expected package counter, got %v", got)
	}
	if counter.energyUJ != 5000000 || counter.maxUJ != 262143999999999 {
		t.Fatalf("package counter = %+v", counter)
	}
	if _, ok := got[filepath.Join(sub, "energy_uj")]; ok {
		t.Fatalf("sub-domain core counter must be excluded, got %v", got)
	}
}

func TestReadRAPLCountersEmptyWhenMissing(t *testing.T) {
	root := t.TempDir()
	if _, err := readRAPLCounters(root); err == nil {
		t.Fatal("expected error when no RAPL counters found")
	}
}

func TestReadProcCPUInfoClockAveragesMHz(t *testing.T) {
	data := "processor : 0\nvendor_id : AuthenticAMD\ncpu MHz : 3600.000\n\ncpu MHz : 4500.500\n"
	mhz, ok := parseProcCPUInfoClock(data)
	if !ok {
		t.Fatal("parseProcCPUInfoClock returned ok=false on valid data")
	}
	// (3600 + 4500.5) / 2 = 4050.25
	if mhz < 4050.2 || mhz > 4050.3 {
		t.Fatalf("cpu MHz = %v, want ~4050.25", mhz)
	}
}

func TestReadProcCPUInfoClockRejectsGarbage(t *testing.T) {
	if _, ok := parseProcCPUInfoClock("processor : 0\nflags : fpu\n"); ok {
		t.Fatal("parseProcCPUInfoClock should fail when no cpu MHz line is present")
	}
}
