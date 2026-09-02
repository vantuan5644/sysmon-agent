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
		`collectMetricAsync(&wg, &storage`,
		`collectMetricAsync(&wg, &network`,
		`collectMetricAsync(&wg, &temperatures`,
		`collectMetricAsync(&wg, &gpu`,
		`wg.Wait()`,
		`metrics.CPU = cpu`,
		`metrics.Memory = memory`,
		`metrics.Disks = disks`,
		`metrics.Storage = storage`,
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
	total, available, swapTotal, swapFree, err := parseMemInfo(`MemTotal:       16384000 kB
MemFree:         1024000 kB
MemAvailable:    8192000 kB
Buffers:          100000 kB
Cached:           200000 kB
SwapTotal:       4194304 kB
SwapFree:        1048576 kB
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
	if swapTotal != 4194304*1024 {
		t.Fatalf("swapTotal = %d", swapTotal)
	}
	if swapFree != 1048576*1024 {
		t.Fatalf("swapFree = %d", swapFree)
	}
}

func TestParseMemInfoFallback(t *testing.T) {
	_, available, _, _, err := parseMemInfo(`MemTotal: 1000 kB
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
	_, _, _, _, err := parseMemInfo(`MemTotal: 18014398509481984 kB
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
	_, _, _, _, err := parseMemInfo(`MemTotal: 1000 kB
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
	_, err := readRAPLCounters(root)
	if err == nil {
		t.Fatal("expected error when no RAPL counters found")
	}
	if !strings.Contains(err.Error(), "no CPU package power counters found") {
		t.Fatalf("expected the absent-message for an empty powercap tree, got %v", err)
	}
}

func TestReadRAPLCountersUnreadableDistinguishesFromAbsent(t *testing.T) {
	root := t.TempDir()
	// A directory named energy_uj makes os.ReadFile fail (EISDIR) deterministically
	// regardless of the test uid, so this branch exercises on root CI too.
	pkg := filepath.Join(root, "intel-rapl:0")
	if err := os.MkdirAll(filepath.Join(pkg, "energy_uj"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := readRAPLCounters(root)
	if err == nil {
		t.Fatal("expected error when package energy_uj is present but unreadable")
	}
	if !strings.Contains(err.Error(), "not readable") {
		t.Fatalf("expected 'not readable' error, got %v", err)
	}
	if strings.Contains(err.Error(), "RAPL not exposed") {
		t.Fatalf("must not report the absent message for present-but-unreadable counters, got %v", err)
	}
}

func TestReadRAPLCountersPermissionDeniedSurfacesUdevHint(t *testing.T) {
	root := t.TempDir()
	pkg := filepath.Join(root, "intel-rapl:0")
	energy := filepath.Join(pkg, "energy_uj")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeTestFile(energy, "5000000"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(energy, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(energy, 0o644) }) // restore so t.TempDir cleanup can remove it

	_, err := readRAPLCounters(root)
	if err == nil {
		t.Skip("energy_uj stayed readable (running as root); permission branch not exercisable here")
	}
	if !strings.Contains(err.Error(), "not readable") {
		t.Fatalf("expected 'not readable' error, got %v", err)
	}
	if !strings.Contains(err.Error(), "99-powercap-rapl.rules") {
		t.Fatalf("expected udev rule hint in permission error, got %v", err)
	}
}

func TestReadProcCPUInfoClockAveragesMHz(t *testing.T) {
	data := "processor : 0\nvendor_id : AuthenticAMD\ncpu MHz : 3600.000\n\ncpu MHz : 4500.500\n"
	mhz, peak, ok := parseProcCPUInfoClocks(data)
	if !ok {
		t.Fatal("parseProcCPUInfoClocks returned ok=false on valid data")
	}
	// (3600 + 4500.5) / 2 = 4050.25
	if mhz < 4050.2 || mhz > 4050.3 {
		t.Fatalf("cpu MHz = %v, want ~4050.25", mhz)
	}
	if peak != 4500.5 {
		t.Fatalf("peak MHz = %v, want 4500.5 (the fastest core, not the average)", peak)
	}
}

func TestReadProcCPUInfoClockRejectsGarbage(t *testing.T) {
	if _, _, ok := parseProcCPUInfoClocks("processor : 0\nflags : fpu\n"); ok {
		t.Fatal("parseProcCPUInfoClocks should fail when no cpu MHz line is present")
	}
}

// An x86 /proc/cpuinfo carries a lowercase "bogomips" line alongside "cpu MHz",
// and BogoMIPS runs at roughly twice the clock there. Mixing the two would
// inflate the mean and -- far worse -- ratchet the peak-hold ceiling to a
// frequency the part can never reach, permanently, since the ceiling never
// falls. "cpu MHz" must win outright when present.
func TestReadProcCPUInfoClockPrefersCPUMHzOverBogoMIPS(t *testing.T) {
	data := "processor : 0\ncpu MHz : 3600.000\nBogoMIPS : 9000.61\n"
	mean, peak, ok := parseProcCPUInfoClocks(data)
	if !ok {
		t.Fatal("parseProcCPUInfoClocks returned ok=false on valid data")
	}
	if mean != 3600 || peak != 3600 {
		t.Fatalf("mean/peak = %v/%v, want 3600/3600 (BogoMIPS must not contribute)", mean, peak)
	}
}

// ARM exposes no "cpu MHz" line at all, so BogoMIPS remains the fallback there.
func TestReadProcCPUInfoClockFallsBackToBogoMIPS(t *testing.T) {
	data := "processor : 0\nBogoMIPS : 2000.00\n\nprocessor : 1\nBogoMIPS : 2400.00\n"
	mean, peak, ok := parseProcCPUInfoClocks(data)
	if !ok {
		t.Fatal("parseProcCPUInfoClocks should fall back to BogoMIPS when no cpu MHz line exists")
	}
	if mean != 2200 || peak != 2400 {
		t.Fatalf("mean/peak = %v/%v, want 2200/2400", mean, peak)
	}
}

func TestCollapseMountsByDeviceCollapsesBtrfsSubvolumes(t *testing.T) {
	// Six btrfs subvolume mounts of one device collapse to one row, keeping the
	// root-most mountpoint "/". A second device with two real mounts keeps the
	// shorter of the two.
	mounts := []mountInfo{
		{device: "/dev/nvme0n1p5", mountpoint: "/srv", fsType: "btrfs"},
		{device: "/dev/nvme0n1p5", mountpoint: "/var/cache", fsType: "btrfs"},
		{device: "/dev/nvme0n1p5", mountpoint: "/var/log", fsType: "btrfs"},
		{device: "/dev/nvme0n1p5", mountpoint: "/var/tmp", fsType: "btrfs"},
		{device: "/dev/nvme0n1p5", mountpoint: "/root", fsType: "btrfs"},
		{device: "/dev/nvme0n1p5", mountpoint: "/", fsType: "btrfs"},
		{device: "/dev/sda2", mountpoint: "/home", fsType: "ext4"},
		{device: "/dev/sda2", mountpoint: "/", fsType: "ext4"},
	}
	got := collapseMountsByDevice(mounts)
	if len(got) != 2 {
		t.Fatalf("collapsed %d mounts to %d rows, want 2", len(mounts), len(got))
	}
	// First-seen device order is preserved (nvme0n1p5 then sda2).
	if got[0].device != "/dev/nvme0n1p5" || got[0].mountpoint != "/" {
		t.Fatalf("nvme0n1p5 representative = %+v, want mountpoint %q", got[0], "/")
	}
	if got[1].device != "/dev/sda2" || got[1].mountpoint != "/" {
		t.Fatalf("sda2 representative = %+v, want mountpoint %q", got[1], "/")
	}
}

func TestAggregateDeviceCapacityCountsEachFilesystemOnce(t *testing.T) {
	// All mountpoints below are the same real directory, so every statfs returns
	// identical counters. That isolates the dedup rule: what varies between the
	// cases is only the backing DEVICE, which is the key.
	dir := t.TempDir()
	single := statfsCapacity(dir)
	if !single.Available {
		t.Skipf("statfs unavailable on %s: %s", dir, single.Error)
	}

	// btrfs shape: one device mounted at several subvolume paths. Each mount
	// reports the whole filesystem, so the device must be counted exactly once.
	subvolumes := []mountInfo{
		{device: "/dev/nvme0n1p5", mountpoint: dir, fsType: "btrfs"},
		{device: "/dev/nvme0n1p5", mountpoint: dir, fsType: "btrfs"},
		{device: "/dev/nvme0n1p5", mountpoint: dir, fsType: "btrfs"},
	}
	got := aggregateDeviceCapacity(subvolumes)
	if got.TotalBytes != single.TotalBytes || got.UsedBytes != single.UsedBytes {
		t.Fatalf("three subvolumes of one device = used %d / total %d, want one filesystem's %d / %d",
			got.UsedBytes, got.TotalBytes, single.UsedBytes, single.TotalBytes)
	}

	// Regression: two DISTINCT sibling partitions whose counters happen to match
	// exactly (a drive split into two equal, equally-used volumes) must both
	// count. Keying the dedup on the observed (total, free) pair collapsed them
	// and reported half the drive's capacity.
	siblings := []mountInfo{
		{device: "/dev/nvme0n1p1", mountpoint: dir, fsType: "ext4"},
		{device: "/dev/nvme0n1p2", mountpoint: dir, fsType: "ext4"},
	}
	got = aggregateDeviceCapacity(siblings)
	if got.TotalBytes != 2*single.TotalBytes || got.UsedBytes != 2*single.UsedBytes {
		t.Fatalf("two identical sibling partitions = used %d / total %d, want %d / %d (both counted)",
			got.UsedBytes, got.TotalBytes, 2*single.UsedBytes, 2*single.TotalBytes)
	}
}

func TestAggregateDeviceCapacityDegradesWithoutMounts(t *testing.T) {
	// No mounted filesystems -> unavailable with the honest error.
	empty := aggregateDeviceCapacity(nil)
	if empty.Available || empty.Error != "no mounted filesystems" {
		t.Fatalf("empty capacity = %+v, want unavailable \"no mounted filesystems\"", empty)
	}

	// A mountpoint that cannot be stat-ed degrades the same way rather than
	// reporting a bogus 0%.
	missing := aggregateDeviceCapacity([]mountInfo{
		{device: "/dev/nvme9n1p1", mountpoint: filepath.Join(t.TempDir(), "definitely-not-mounted"), fsType: "ext4"},
	})
	if missing.Available || missing.Error != "no mounted filesystems" {
		t.Fatalf("unstat-able mount = %+v, want unavailable", missing)
	}
}

func TestShouldIncludeStorageMountAcceptsRemovableMedia(t *testing.T) {
	// An external SSD auto-mounted under /run/media is rejected by the disk-row
	// filter (blanket "/run" prefix) but must count for per-device storage, or
	// its whole drive reports "no mounted filesystems".
	external := mountInfo{device: "/dev/nvme1n1p2", mountpoint: "/run/media/someone/EXT-short", fsType: "exfat"}
	if shouldIncludeMount(external) {
		t.Fatal("shouldIncludeMount should still reject /run/media (per-mountpoint disk rows)")
	}
	if !shouldIncludeStorageMount(external) {
		t.Fatal("shouldIncludeStorageMount should accept /run/media removable media")
	}

	// Ordinary mounts still pass both filters.
	root := mountInfo{device: "/dev/nvme0n1p5", mountpoint: "/", fsType: "btrfs"}
	if !shouldIncludeMount(root) || !shouldIncludeStorageMount(root) {
		t.Fatal("both filters should accept /")
	}

	// Runtime state under /run that is NOT removable media stays rejected by both.
	runtime := mountInfo{device: "/dev/loop0", mountpoint: "/run/something", fsType: "ext4"}
	if shouldIncludeMount(runtime) || shouldIncludeStorageMount(runtime) {
		t.Fatal("non-media /run mounts must stay excluded from both views")
	}

	// Pseudo/remote filesystems stay rejected even under /run/media.
	pseudo := mountInfo{device: "tmpfs", mountpoint: "/run/media/someone/ramdisk", fsType: "tmpfs"}
	if shouldIncludeStorageMount(pseudo) {
		t.Fatal("tmpfs under /run/media must stay excluded")
	}
}

func TestLinuxWholeDiskForNameMapsPartitionToWholeDisk(t *testing.T) {
	root := t.TempDir()
	classBlock := filepath.Join(root, "class", "block")

	// Partition nvme0n1p5: a real device dir backing it + a class-block symlink,
	// carrying a "partition" attribute. The symlink target is relative so the
	// resolved parent's base name is the whole disk (nvme0n1).
	realPart := filepath.Join(root, "devices", "nvme0", "nvme0n1", "nvme0n1p5")
	if err := writeTestFile(filepath.Join(realPart, "partition"), "5\n"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(classBlock, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../devices/nvme0/nvme0n1/nvme0n1p5", filepath.Join(classBlock, "nvme0n1p5")); err != nil {
		t.Fatal(err)
	}

	if got := linuxWholeDiskForName("nvme0n1p5", classBlock); got != "nvme0n1" {
		t.Fatalf("nvme0n1p5 -> %q, want nvme0n1", got)
	}
	// Whole disk (no partition attribute) maps to itself.
	if got := linuxWholeDiskForName("nvme0n1", classBlock); got != "nvme0n1" {
		t.Fatalf("nvme0n1 -> %q, want nvme0n1", got)
	}
	// Non-block name (tmpfs) is not a sysfs entry, so it passes through unchanged.
	if got := linuxWholeDiskForName("tmpfs", classBlock); got != "tmpfs" {
		t.Fatalf("tmpfs -> %q, want tmpfs", got)
	}
}

func TestNvmeControllerForDisk(t *testing.T) {
	cases := map[string]string{
		"nvme0n1":  "nvme0",
		"nvme12n0": "nvme12",
		"nvme3n1":  "nvme3",
	}
	for disk, want := range cases {
		got, ok := nvmeControllerForDisk(disk)
		if !ok || got != want {
			t.Fatalf("nvmeControllerForDisk(%q) = %q,%t, want %q,true", disk, got, ok, want)
		}
	}
	for _, disk := range []string{"sda", "mmcblk0", "nvme", "nvme0"} {
		if _, ok := nvmeControllerForDisk(disk); ok {
			t.Fatalf("nvmeControllerForDisk(%q) returned ok=true, want false", disk)
		}
	}
}

func TestLinuxBlockDeviceSizeMultipliesSectorsBy512(t *testing.T) {
	root := t.TempDir()
	blockRoot := filepath.Join(root, "block")
	// /sys/block/nvme0n1/size is in 512-byte sectors; size_bytes must be sectors*512.
	if err := writeTestFile(filepath.Join(blockRoot, "nvme0n1", "size"), "7814037168\n"); err != nil {
		t.Fatal(err)
	}
	got := linuxBlockDeviceSize("nvme0n1", blockRoot)
	if want := uint64(7814037168) * 512; got != want {
		t.Fatalf("linuxBlockDeviceSize = %d, want %d (sectors*512)", got, want)
	}
}

func TestNvmeStorageTemperaturePrefersComposite(t *testing.T) {
	root := t.TempDir()
	hwmon := filepath.Join(root, "class", "nvme", "nvme0", "hwmon2")
	files := map[string]string{
		filepath.Join(hwmon, "temp1_input"): "45850\n", // 45.85C
		filepath.Join(hwmon, "temp1_label"): "Composite\n",
		filepath.Join(hwmon, "temp2_input"): "60000\n", // 60C (should NOT win)
		filepath.Join(hwmon, "temp2_label"): "Sensor 2\n",
	}
	for path, value := range files {
		if err := writeTestFile(path, value); err != nil {
			t.Fatal(err)
		}
	}

	got := nvmeStorageTemperature("nvme0", filepath.Join(root, "class", "nvme"))
	if !got.Available || got.Value != 45.85 {
		t.Fatalf("Composite temperature = %+v, want 45.85C", got)
	}
}

func TestNvmeStorageTemperatureMissingHwmonIsUnavailableNotError(t *testing.T) {
	root := t.TempDir()
	got := nvmeStorageTemperature("nvme9", filepath.Join(root, "class", "nvme"))
	if got.Available {
		t.Fatalf("missing hwmon reported available: %+v", got)
	}
	if got.Unit != "C" {
		t.Fatalf("unit = %q, want C", got.Unit)
	}
	// An error STRING is fine (it explains why); the metric must still be a
	// well-formed unavailable NumberMetric, never a hard collector error.
	if got.Error == "" {
		t.Fatalf("missing hwmon reported available-without-error: %+v", got)
	}
}

func TestNvmeStorageTemperatureFallsBackToFirstSensor(t *testing.T) {
	// No Composite label: fall back to the first readable sensor.
	root := t.TempDir()
	hwmon := filepath.Join(root, "class", "nvme", "nvme0", "hwmon0")
	files := map[string]string{
		filepath.Join(hwmon, "temp1_input"): "52000\n",
		filepath.Join(hwmon, "temp1_label"): "Warning\n",
	}
	for path, value := range files {
		if err := writeTestFile(path, value); err != nil {
			t.Fatal(err)
		}
	}
	got := nvmeStorageTemperature("nvme0", filepath.Join(root, "class", "nvme"))
	if !got.Available || got.Value != 52 {
		t.Fatalf("fallback temperature = %+v, want 52C", got)
	}
}

// TestLinuxStorageTemperatureExplainsAUSBEnclosure covers the drive that
// prompted this: an NVMe SSD in a USB enclosure enumerates over UAS as sdX with
// no hwmon node, because the bridge does not answer the generic SMART query. The
// reading stays unavailable - we cannot read it without a vendor passthrough -
// but it must say why, rather than looking like a broken sensor.
func TestLinuxStorageTemperatureExplainsAUSBEnclosure(t *testing.T) {
	root := t.TempDir()
	blockRoot := filepath.Join(root, "block")
	if err := os.MkdirAll(blockRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	usbPath := "../devices/pci0000:00/0000:00:14.0/usb2/2-4/2-4:1.0/host6/target6:0:0/6:0:0:0/block/sdb"
	if err := os.Symlink(usbPath, filepath.Join(blockRoot, "sdb")); err != nil {
		t.Fatal(err)
	}

	got := linuxStorageTemperature("sdb", root)
	if got.Available {
		t.Fatalf("USB enclosure reported a temperature: %+v", got)
	}
	if got.Error != usbEnclosureTemperatureReason {
		t.Fatalf("error = %q, want %q", got.Error, usbEnclosureTemperatureReason)
	}
	if got.Unit != "C" {
		t.Fatalf("unit = %q, want C", got.Unit)
	}
}

// TestLinuxStorageTemperatureKeepsGenericMissForInternalDrives is the other half:
// a directly attached drive with no sensor must NOT be blamed on a bridge it
// does not have.
func TestLinuxStorageTemperatureKeepsGenericMissForInternalDrives(t *testing.T) {
	root := t.TempDir()
	blockRoot := filepath.Join(root, "block")
	if err := os.MkdirAll(blockRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	sataPath := "../devices/pci0000:00/0000:00:17.0/ata3/host2/target2:0:0/2:0:0:0/block/sda"
	if err := os.Symlink(sataPath, filepath.Join(blockRoot, "sda")); err != nil {
		t.Fatal(err)
	}

	got := linuxStorageTemperature("sda", root)
	if got.Available {
		t.Fatalf("empty sysfs reported a temperature: %+v", got)
	}
	if got.Error == usbEnclosureTemperatureReason {
		t.Fatalf("SATA drive blamed on a USB enclosure: %+v", got)
	}
	if got.Error == "" {
		t.Fatalf("missing sensor reported without an explanation: %+v", got)
	}
}

// TestLinuxStorageTemperaturePrefersARealReadingOverTheUSBExplanation: an
// enclosure that DOES publish an hwmon node (some SATA bridges do) must report
// the temperature, not the excuse.
func TestLinuxStorageTemperaturePrefersARealReadingOverTheUSBExplanation(t *testing.T) {
	// Faithful to real sysfs: /sys/block/<dev> IS the symlink into the device
	// tree, so the hwmon glob has to resolve through it.
	root := t.TempDir()
	blockRoot := filepath.Join(root, "block")
	usbPath := "../devices/pci0000:00/0000:00:14.0/usb2/2-4/2-4:1.0/host6/target6:0:0/6:0:0:0/block/sdc"
	hwmon := filepath.Join(blockRoot, filepath.FromSlash(usbPath), "device", "hwmon", "hwmon5")
	if err := writeTestFile(filepath.Join(hwmon, "temp1_input"), "38000\n"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(blockRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(usbPath, filepath.Join(blockRoot, "sdc")); err != nil {
		t.Fatal(err)
	}
	if !linuxBlockDeviceIsUSB("sdc", blockRoot) {
		t.Fatal("fixture is not recognized as USB-attached; the test would prove nothing")
	}

	got := linuxStorageTemperature("sdc", root)
	if !got.Available || got.Value != 38 {
		t.Fatalf("hwmon reading = %+v, want 38C", got)
	}
}
