//go:build linux

package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type systemCollector struct {
	hostname    string
	mu          sync.Mutex
	prevCPU     cpuTimes
	prevNet     map[string]netCounter
	prevNetAt   time.Time
	prevRAPL    map[string]raplCounter
	prevRAPLAt  time.Time
	prevRAPLSet bool
}

type cpuTimes struct {
	idle  uint64
	total uint64
}

type netCounter struct {
	rxBytes uint64
	txBytes uint64
}

// raplCounter holds one Intel/AMD RAPL package energy reading plus the
// counter's maximum range, which is needed to handle the periodic wraparound
// of the energy_uj counter.
type raplCounter struct {
	energyUJ uint64
	maxUJ    uint64
}

func NewSystemCollector() MetricsCollector {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	c := &systemCollector{
		hostname:  hostname,
		prevNet:   map[string]netCounter{},
		prevNetAt: time.Now(),
	}
	if cpu, err := readCPUTimes(); err == nil {
		c.prevCPU = cpu
	}
	if netCounters, err := readNetCounters(); err == nil {
		c.prevNet = netCounters
	}
	if rapl, err := readRAPLCounters(powercapSysRoot); err == nil && len(rapl) > 0 {
		c.prevRAPL = rapl
		c.prevRAPLAt = time.Now()
		c.prevRAPLSet = true
	}
	return c
}

func (c *systemCollector) Collect(ctx context.Context) (Metrics, error) {
	started := time.Now()
	metrics := baseMetrics(c.hostname)
	metrics.Platform = readFirstLine("/proc/sys/kernel/osrelease")

	var wg sync.WaitGroup
	var cpu NumberMetric
	var cpuPower NumberMetric
	var cpuClock NumberMetric
	var cpuClockMax NumberMetric
	var memory CapacityMetric
	var disks []DiskMetric
	var network NetworkSet
	var temperatures TemperatureSet
	var gpu GPUSet

	collectMetricAsync(&wg, &cpu, func() NumberMetric {
		return c.collectCPU(ctx)
	}, func(recovered any) NumberMetric {
		return unavailableNumber("%", fmt.Sprintf("Linux CPU collector panicked: %v", recovered))
	})
	collectMetricAsync(&wg, &cpuPower, func() NumberMetric {
		return c.collectCPUPower(ctx)
	}, func(recovered any) NumberMetric {
		return unavailableNumber("W", fmt.Sprintf("Linux CPU power collector panicked: %v", recovered))
	})
	collectMetricAsync(&wg, &cpuClock, func() NumberMetric {
		cur, mx := collectCPUClocks()
		cpuClockMax = mx
		return cur
	}, func(recovered any) NumberMetric {
		cpuClockMax = unavailableNumber("MHz", fmt.Sprintf("Linux CPU clock collector panicked: %v", recovered))
		return unavailableNumber("MHz", fmt.Sprintf("Linux CPU clock collector panicked: %v", recovered))
	})
	collectMetricAsync(&wg, &memory, func() CapacityMetric {
		return collectMemory()
	}, func(recovered any) CapacityMetric {
		return unavailableCapacity(fmt.Sprintf("Linux memory collector panicked: %v", recovered))
	})
	collectMetricAsync(&wg, &disks, func() []DiskMetric {
		return collectDisks()
	}, func(recovered any) []DiskMetric {
		return unavailableDisk(fmt.Sprintf("Linux disk collector panicked: %v", recovered))
	})
	collectMetricAsync(&wg, &network, func() NetworkSet {
		return c.collectNetwork(ctx)
	}, func(recovered any) NetworkSet {
		return NetworkSet{Available: false, Error: fmt.Sprintf("Linux network collector panicked: %v", recovered)}
	})
	collectMetricAsync(&wg, &temperatures, func() TemperatureSet {
		return collectTemperatures()
	}, func(recovered any) TemperatureSet {
		return TemperatureSet{Available: false, Error: fmt.Sprintf("Linux temperature collector panicked: %v", recovered)}
	})
	collectMetricAsync(&wg, &gpu, func() GPUSet {
		return collectLinuxGPU(ctx)
	}, func(recovered any) GPUSet {
		return GPUSet{Available: false, Error: fmt.Sprintf("Linux GPU collector panicked: %v", recovered)}
	})
	wg.Wait()

	metrics.CPU = cpu
	metrics.CPUPower = cpuPower
	metrics.CPUClock = cpuClock
	metrics.CPUClockMax = cpuClockMax
	metrics.CPUTemperature = pickCPUTemperature(temperatures)
	metrics.PSUOutputPower = unavailableNumber("W", "no PSU output power sensor exposed on Linux")
	metrics.Memory = memory
	metrics.Disks = disks
	metrics.Network = network
	metrics.Temperatures = temperatures
	metrics.GPU = gpu
	return finishMetrics(metrics, started), nil
}

// Hostname exposes the resolved hostname so the sampler's warming snapshot can
// carry it before the first lane pass (see collectorHostname in sampler.go).
func (c *systemCollector) Hostname() string { return c.hostname }

// CollectFast gathers the cheap /proc metrics (CPU load + memory) the dashboard
// animates live, returning a patch the sampler applies to its shared snapshot
// (see the laneCollector contract in sampler.go). A panic degrades both fields
// rather than crashing the background goroutine.
func (c *systemCollector) CollectFast(ctx context.Context) (patch func(*Metrics)) {
	defer func() {
		if r := recover(); r != nil {
			cpu := unavailableNumber("%", fmt.Sprintf("Linux CPU collector panicked: %v", r))
			memory := unavailableCapacity(fmt.Sprintf("Linux memory collector panicked: %v", r))
			patch = func(m *Metrics) {
				m.CPU = cpu
				m.Memory = memory
			}
		}
	}()

	cpu := c.collectCPU(ctx)
	memory := collectMemory()
	return func(m *Metrics) {
		m.CPU = cpu
		m.Memory = memory
	}
}

// CollectSlow gathers the expensive metrics (platform, CPU power/clock, disks,
// network, temperatures, GPU) behind a concurrent fan-out, returning a patch the
// sampler applies. CPU and memory are owned by the fast lane and left untouched.
func (c *systemCollector) CollectSlow(ctx context.Context) (patch func(*Metrics)) {
	defer func() {
		if r := recover(); r != nil {
			patch = linuxDegradedSlowPatch(fmt.Sprintf("Linux slow collector panicked: %v", r))
		}
	}()

	platform := readFirstLine("/proc/sys/kernel/osrelease")

	var wg sync.WaitGroup
	var cpuPower NumberMetric
	var cpuClock NumberMetric
	var cpuClockMax NumberMetric
	var disks []DiskMetric
	var network NetworkSet
	var temperatures TemperatureSet
	var gpu GPUSet

	collectMetricAsync(&wg, &cpuPower, func() NumberMetric {
		return c.collectCPUPower(ctx)
	}, func(recovered any) NumberMetric {
		return unavailableNumber("W", fmt.Sprintf("Linux CPU power collector panicked: %v", recovered))
	})
	collectMetricAsync(&wg, &cpuClock, func() NumberMetric {
		cur, mx := collectCPUClocks()
		cpuClockMax = mx
		return cur
	}, func(recovered any) NumberMetric {
		cpuClockMax = unavailableNumber("MHz", fmt.Sprintf("Linux CPU clock collector panicked: %v", recovered))
		return unavailableNumber("MHz", fmt.Sprintf("Linux CPU clock collector panicked: %v", recovered))
	})
	collectMetricAsync(&wg, &disks, func() []DiskMetric {
		return collectDisks()
	}, func(recovered any) []DiskMetric {
		return unavailableDisk(fmt.Sprintf("Linux disk collector panicked: %v", recovered))
	})
	collectMetricAsync(&wg, &network, func() NetworkSet {
		return c.collectNetwork(ctx)
	}, func(recovered any) NetworkSet {
		return NetworkSet{Available: false, Error: fmt.Sprintf("Linux network collector panicked: %v", recovered)}
	})
	collectMetricAsync(&wg, &temperatures, func() TemperatureSet {
		return collectTemperatures()
	}, func(recovered any) TemperatureSet {
		return TemperatureSet{Available: false, Error: fmt.Sprintf("Linux temperature collector panicked: %v", recovered)}
	})
	collectMetricAsync(&wg, &gpu, func() GPUSet {
		return collectLinuxGPU(ctx)
	}, func(recovered any) GPUSet {
		return GPUSet{Available: false, Error: fmt.Sprintf("Linux GPU collector panicked: %v", recovered)}
	})
	wg.Wait()

	cpuTemperature := pickCPUTemperature(temperatures)
	return func(m *Metrics) {
		m.Platform = platform
		m.CPUPower = cpuPower
		m.CPUClock = cpuClock
		m.CPUClockMax = cpuClockMax
		m.CPUTemperature = cpuTemperature
		m.PSUOutputPower = unavailableNumber("W", "no PSU output power sensor exposed on Linux")
		m.Disks = disks
		m.Network = network
		m.Temperatures = temperatures
		m.GPU = gpu
	}
}

// linuxDegradedSlowPatch marks every slow-lane field unavailable with the same
// message. It is the panic safety net for CollectSlow (per-metric panics are
// already recovered by collectMetricAsync; this covers a panic before the
// fan-out).
func linuxDegradedSlowPatch(message string) func(*Metrics) {
	return func(m *Metrics) {
		m.CPUPower = unavailableNumber("W", message)
		m.CPUClock = unavailableNumber("MHz", message)
		m.CPUClockMax = unavailableNumber("MHz", message)
		m.CPUTemperature = unavailableNumber("C", message)
		m.PSUOutputPower = unavailableNumber("W", message)
		m.Disks = unavailableDisk(message)
		m.Network = NetworkSet{Available: false, Error: message}
		m.Temperatures = TemperatureSet{Available: false, Error: message}
		m.GPU = GPUSet{Available: false, Error: message}
	}
}

func (c *systemCollector) collectCPU(ctx context.Context) NumberMetric {
	now, err := readCPUTimes()
	if err != nil {
		return unavailableNumber("%", err.Error())
	}

	c.mu.Lock()
	prev := c.prevCPU
	c.prevCPU = now
	if prev.total == 0 || now.total <= prev.total {
		c.mu.Unlock()
		return c.sampleCPUAfterDelay(ctx, now, 100*time.Millisecond)
	}
	c.mu.Unlock()

	value, ok := cpuUsagePercent(prev, now)
	if !ok {
		return unavailableNumber("%", "CPU counters did not advance")
	}
	return availableNumber(value, "%")
}

func (c *systemCollector) sampleCPUAfterDelay(ctx context.Context, prev cpuTimes, delay time.Duration) NumberMetric {
	return c.sampleCPUAfterDelayWithReader(ctx, prev, delay, readCPUTimes)
}

func (c *systemCollector) sampleCPUAfterDelayWithReader(ctx context.Context, prev cpuTimes, delay time.Duration, read func() (cpuTimes, error)) NumberMetric {
	if err := waitForSample(ctx, delay); err != nil {
		return unavailableNumber("%", "CPU sampler canceled: "+err.Error())
	}
	later, err := read()
	if err != nil {
		return unavailableNumber("%", err.Error())
	}
	c.mu.Lock()
	c.prevCPU = later
	c.mu.Unlock()

	value, ok := cpuUsagePercent(prev, later)
	if !ok {
		return unavailableNumber("%", "CPU sampler is warming up")
	}
	return availableNumber(value, "%")
}

func cpuUsagePercent(prev, now cpuTimes) (float64, bool) {
	if prev.total == 0 || now.total <= prev.total {
		return 0, false
	}
	deltaTotal := now.total - prev.total
	deltaIdle := uint64(0)
	if now.idle > prev.idle {
		deltaIdle = now.idle - prev.idle
	}
	if deltaTotal == 0 || deltaIdle > deltaTotal {
		return 0, false
	}
	return (float64(deltaTotal-deltaIdle) / float64(deltaTotal)) * 100, true
}

const (
	powercapSysRoot      = "/sys/class/powercap"
	minRAPLSampleSeconds = 0.2
)

// collectCPUPower computes instantaneous CPU package power in watts from the
// Intel/AMD RAPL energy counters exposed under /sys/class/powercap. The
// energy_uj counters are monotonic and wrap periodically, so power is derived
// from the delta between two readings divided by the elapsed time. Without a
// previous reading (first sample after start) it waits briefly for a second
// sample so the dashboard's first refresh still shows a real value.
func (c *systemCollector) collectCPUPower(ctx context.Context) NumberMetric {
	now := time.Now()
	current, err := readRAPLCounters(powercapSysRoot)
	if err != nil {
		return unavailableNumber("W", err.Error())
	}

	c.mu.Lock()
	prev, prevAt, hasPrev := c.prevRAPL, c.prevRAPLAt, c.prevRAPLSet
	c.prevRAPL = current
	c.prevRAPLAt = now
	c.prevRAPLSet = true
	c.mu.Unlock()

	if !hasPrev || len(prev) == 0 {
		return c.sampleCPUPowerAfterDelay(ctx, current, now, 250*time.Millisecond)
	}
	elapsed := now.Sub(prevAt).Seconds()
	if elapsed <= 0 || elapsed < minRAPLSampleSeconds {
		return c.sampleCPUPowerAfterDelay(ctx, current, now, 250*time.Millisecond)
	}
	return computeRAPLPower(prev, current, elapsed)
}

func (c *systemCollector) sampleCPUPowerAfterDelay(ctx context.Context, previous map[string]raplCounter, previousAt time.Time, delay time.Duration) NumberMetric {
	if err := waitForSample(ctx, delay); err != nil {
		return unavailableNumber("W", "CPU power sampler canceled: "+err.Error())
	}
	later, err := readRAPLCounters(powercapSysRoot)
	if err != nil {
		return unavailableNumber("W", err.Error())
	}
	laterAt := time.Now()

	c.mu.Lock()
	c.prevRAPL = later
	c.prevRAPLAt = laterAt
	c.prevRAPLSet = true
	c.mu.Unlock()

	return computeRAPLPower(previous, later, networkSampleElapsedSeconds(previousAt, laterAt, delay))
}

func computeRAPLPower(prev, current map[string]raplCounter, elapsed float64) NumberMetric {
	if elapsed <= 0 {
		return unavailableNumber("W", "invalid CPU power sample interval")
	}
	var totalDeltaUJ float64
	advanced := false
	for path, cur := range current {
		previous, ok := prev[path]
		if !ok {
			continue
		}
		delta, ok := raplEnergyDelta(previous, cur)
		if !ok {
			return unavailableNumber("W", "CPU energy counter wrapped without a known range")
		}
		totalDeltaUJ += delta
		if delta > 0 {
			advanced = true
		}
	}
	if !advanced {
		return unavailableNumber("W", "CPU energy counters did not advance")
	}
	watts := totalDeltaUJ / (elapsed * 1e6)
	if !isFinite(watts) || watts < 0 {
		return unavailableNumber("W", "invalid CPU power counters")
	}
	return availableNumber(watts, "W")
}

// raplEnergyDelta returns the microjoule delta between two RAPL readings,
// handling the periodic wraparound of the energy_uj counter when the maximum
// range is known.
func raplEnergyDelta(prev, cur raplCounter) (float64, bool) {
	if cur.energyUJ >= prev.energyUJ {
		return float64(cur.energyUJ - prev.energyUJ), true
	}
	if cur.maxUJ > prev.energyUJ {
		return float64(cur.maxUJ-prev.energyUJ) + float64(cur.energyUJ), true
	}
	return 0, false
}

func readRAPLCounters(root string) (map[string]raplCounter, error) {
	matches, err := filepath.Glob(filepath.Join(root, "intel-rapl:*"))
	if err != nil {
		return nil, err
	}
	counters := map[string]raplCounter{}
	for _, dir := range matches {
		if !isRAPLPackageEntry(filepath.Base(dir)) {
			continue
		}
		energyPath := filepath.Join(dir, "energy_uj")
		energy, ok := readUint64File(energyPath)
		if !ok {
			continue
		}
		maxUJ, _ := readUint64File(filepath.Join(dir, "max_energy_range_uj"))
		counters[energyPath] = raplCounter{energyUJ: energy, maxUJ: maxUJ}
	}
	if len(counters) == 0 {
		return nil, fmt.Errorf("no CPU package power counters found")
	}
	return counters, nil
}

// isRAPLPackageEntry matches package-level RAPL directories such as
// "intel-rapl:0" and "intel-rapl:1" while excluding sub-domains such as
// "intel-rapl:0:0" (core), "intel-rapl:0:1" (uncore), or "intel-rapl:0:2" (dram)
// so only whole-package energy is summed.
func isRAPLPackageEntry(name string) bool {
	const prefix = "intel-rapl:"
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	rest := strings.TrimPrefix(name, prefix)
	if rest == "" {
		return false
	}
	for _, r := range rest {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func readCPUTimes() (cpuTimes, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuTimes{}, err
	}
	return parseCPUTimes(string(data))
}

// collectCPUClock reports the average current CPU clock across all cores in
// megahertz. It prefers the cpufreq sysfs interface (scaling_cur_freq, in kHz)
// because it reflects live frequency, and falls back to the "cpu MHz" lines in
// /proc/cpuinfo on kernels that do not expose cpufreq (some VMs/servers).
// collectCPUClocks reports the average current CPU clock and the advertised
// maximum (boost) clock so the dashboard can render a clock ring relative to
// peak. Current prefers cpufreq scaling_cur_freq (averaged across cores) with a
// /proc/cpuinfo fallback; max prefers cpuinfo_max_freq (real boost) and falls
// back to scaling_max_freq.
func collectCPUClocks() (current, max NumberMetric) {
	if mhz, ok := readCPUFreqClock(); ok {
		current = availableNumber(mhz, "MHz")
	} else if mhz, ok := readProcCPUInfoClock(); ok {
		current = availableNumber(mhz, "MHz")
	} else {
		current = unavailableNumber("MHz", "CPU clock frequency not exposed")
	}
	if mhz, ok := readCPUFreqMaxClock(); ok {
		max = availableNumber(mhz, "MHz")
	} else {
		max = unavailableNumber("MHz", "CPU max clock frequency not exposed")
	}
	return current, max
}

func readCPUFreqMaxClock() (float64, bool) {
	for _, name := range []string{"cpuinfo_max_freq", "scaling_max_freq"} {
		mhz, ok := readCPUFreqValue(name)
		if ok {
			return mhz, true
		}
	}
	return 0, false
}

// readCPUFreqValue reads a single per-package cpufreq value (kHz -> MHz) from
// /sys/devices/system/cpu/cpu0/cpufreq/<name>. cpu0 is representative for max
// frequencies on homogeneous CPUs.
func readCPUFreqValue(name string) (float64, bool) {
	kHz, ok := readUint64File("/sys/devices/system/cpu/cpu0/cpufreq/" + name)
	if !ok || kHz == 0 {
		return 0, false
	}
	return float64(kHz) / 1000.0, true
}

func readCPUFreqClock() (float64, bool) {
	pattern := "/sys/devices/system/cpu/cpu[0-9]*/cpufreq/scaling_cur_freq"
	paths, err := filepath.Glob(pattern)
	if err != nil || len(paths) == 0 {
		return 0, false
	}
	var total float64
	count := 0
	for _, path := range paths {
		kHz, ok := readUint64File(path)
		if !ok {
			continue
		}
		total += float64(kHz) / 1000.0
		count++
	}
	if count == 0 {
		return 0, false
	}
	return total / float64(count), true
}

func readProcCPUInfoClock() (float64, bool) {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return 0, false
	}
	return parseProcCPUInfoClock(string(data))
}

func parseProcCPUInfoClock(data string) (float64, bool) {
	var total float64
	count := 0
	for _, line := range strings.Split(data, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "cpu MHz") && !strings.HasPrefix(trimmed, "BogoMIPS") {
			continue
		}
		parts := strings.SplitN(trimmed, ":", 2)
		if len(parts) != 2 {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if err != nil || !isFinite(value) || value <= 0 {
			continue
		}
		total += value
		count++
	}
	if count == 0 {
		return 0, false
	}
	return total / float64(count), true
}

func parseCPUTimes(data string) (cpuTimes, error) {
	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			return cpuTimes{}, fmt.Errorf("invalid /proc/stat cpu line")
		}

		var total uint64
		values := make([]uint64, 0, len(fields)-1)
		for _, field := range fields[1:] {
			value, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				return cpuTimes{}, fmt.Errorf("invalid /proc/stat value %q", field)
			}
			values = append(values, value)
			nextTotal, ok := sumUint64(total, value)
			if !ok {
				return cpuTimes{}, fmt.Errorf("invalid /proc/stat cpu counters")
			}
			total = nextTotal
		}
		idle := values[3]
		if len(values) > 4 {
			combinedIdle, ok := sumUint64(idle, values[4])
			if !ok {
				return cpuTimes{}, fmt.Errorf("invalid /proc/stat idle counters")
			}
			idle = combinedIdle
		}
		return cpuTimes{idle: idle, total: total}, nil
	}
	if err := scanner.Err(); err != nil {
		return cpuTimes{}, err
	}
	return cpuTimes{}, fmt.Errorf("missing cpu line in /proc/stat")
}

func collectMemory() CapacityMetric {
	total, available, err := readMemInfo()
	if err != nil {
		return unavailableCapacity(err.Error())
	}
	if total == 0 || available > total {
		return unavailableCapacity("invalid memory counters")
	}
	return availableCapacity(total-available, total)
}

func readMemInfo() (total uint64, available uint64, err error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	return parseMemInfo(string(data))
}

func parseMemInfo(data string) (total uint64, available uint64, err error) {
	values := map[string]uint64{}
	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(strings.TrimSuffix(scanner.Text(), ":"))
		if len(fields) < 2 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		value, parseErr := strconv.ParseUint(fields[1], 10, 64)
		if parseErr != nil {
			continue
		}
		bytes, ok := kibToBytes(value)
		if !ok {
			return 0, 0, fmt.Errorf("invalid /proc/meminfo value for %s", key)
		}
		values[key] = bytes
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, err
	}

	total = values["MemTotal"]
	available = values["MemAvailable"]
	if available == 0 {
		var ok bool
		available, ok = sumUint64(values["MemFree"], values["Buffers"], values["Cached"])
		if !ok {
			return 0, 0, fmt.Errorf("invalid /proc/meminfo fallback counters")
		}
	}
	if total == 0 {
		return 0, 0, fmt.Errorf("MemTotal missing from /proc/meminfo")
	}
	return total, available, nil
}

type mountInfo struct {
	device     string
	mountpoint string
	fsType     string
}

func collectDisks() []DiskMetric {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return unavailableDisk(err.Error())
	}

	mounts := parseMounts(string(data))
	disks := make([]DiskMetric, 0, len(mounts))
	seen := map[string]bool{}
	for _, mount := range mounts {
		if !shouldIncludeMount(mount) || seen[mount.mountpoint] {
			continue
		}
		seen[mount.mountpoint] = true

		var stat syscall.Statfs_t
		if err := syscall.Statfs(mount.mountpoint, &stat); err != nil {
			disks = append(disks, DiskMetric{
				Name:       diskName(mount.device),
				Mountpoint: mount.mountpoint,
				FSType:     mount.fsType,
				Capacity:   unavailableCapacity(err.Error()),
			})
			continue
		}
		total, totalOK := statfsBytes(stat.Blocks, stat.Bsize)
		free, freeOK := statfsBytes(stat.Bavail, stat.Bsize)
		capacity := unavailableCapacity("invalid disk capacity counters")
		if totalOK && freeOK {
			capacity = availableCapacityFromTotalFree(total, free, "invalid disk capacity counters")
		}
		disks = append(disks, DiskMetric{
			Name:       diskName(mount.device),
			Mountpoint: mount.mountpoint,
			FSType:     mount.fsType,
			Capacity:   capacity,
		})
	}

	sort.Slice(disks, func(i, j int) bool {
		if disks[i].Mountpoint == "/" {
			return true
		}
		if disks[j].Mountpoint == "/" {
			return false
		}
		return disks[i].Mountpoint < disks[j].Mountpoint
	})
	return ensureDiskMetrics(disks, "no local filesystems found")
}

func statfsBytes(blocks uint64, blockSize int64) (uint64, bool) {
	if blockSize <= 0 {
		return 0, false
	}
	size := uint64(blockSize)
	if blocks > ^uint64(0)/size {
		return 0, false
	}
	return blocks * size, true
}

func parseMounts(data string) []mountInfo {
	var mounts []mountInfo
	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		mounts = append(mounts, mountInfo{
			device:     unescapeMountField(fields[0]),
			mountpoint: unescapeMountField(fields[1]),
			fsType:     fields[2],
		})
	}
	return mounts
}

func shouldIncludeMount(mount mountInfo) bool {
	if mount.mountpoint == "" {
		return false
	}
	if skippedLinuxFSType[mount.fsType] {
		return false
	}
	if remoteLinuxFSType(mount.fsType) {
		return false
	}
	if mount.fsType == "overlay" && mount.mountpoint != "/" {
		return false
	}
	for _, prefix := range []string{"/proc", "/sys", "/dev", "/run"} {
		if mount.mountpoint == prefix || strings.HasPrefix(mount.mountpoint, prefix+"/") {
			return false
		}
	}
	if strings.Contains(mount.mountpoint, "/var/lib/docker/overlay2/") {
		return false
	}
	return true
}

var skippedLinuxFSType = map[string]bool{
	"autofs":      true,
	"binfmt_misc": true,
	"bpf":         true,
	"cgroup":      true,
	"cgroup2":     true,
	"configfs":    true,
	"debugfs":     true,
	"devpts":      true,
	"devtmpfs":    true,
	"fusectl":     true,
	"hugetlbfs":   true,
	"mqueue":      true,
	"nsfs":        true,
	"proc":        true,
	"pstore":      true,
	"ramfs":       true,
	"securityfs":  true,
	"selinuxfs":   true,
	"squashfs":    true,
	"sysfs":       true,
	"tmpfs":       true,
	"tracefs":     true,
}

var skippedRemoteLinuxFSType = map[string]bool{
	"9p":        true,
	"afs":       true,
	"ceph":      true,
	"cifs":      true,
	"davfs":     true,
	"glusterfs": true,
	"lustre":    true,
	"nfs":       true,
	"nfs4":      true,
	"smb3":      true,
	"smbfs":     true,
	"sshfs":     true,
	"virtiofs":  true,
	"webdav":    true,
}

func remoteLinuxFSType(fsType string) bool {
	if skippedRemoteLinuxFSType[fsType] {
		return true
	}
	return strings.HasPrefix(fsType, "fuse.sshfs") ||
		strings.HasPrefix(fsType, "fuse.rclone") ||
		strings.HasPrefix(fsType, "fuse.davfs")
}

func unescapeMountField(value string) string {
	replacer := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return replacer.Replace(value)
}

func diskName(device string) string {
	if device == "" {
		return "unknown"
	}
	if strings.HasPrefix(device, "/dev/") {
		return filepath.Base(device)
	}
	return device
}

func (c *systemCollector) collectNetwork(ctx context.Context) NetworkSet {
	nowCounters, err := readNetCounters()
	if err != nil {
		return NetworkSet{Available: false, Error: err.Error()}
	}
	now := time.Now()

	c.mu.Lock()
	prevCounters := c.prevNet
	prevAt := c.prevNetAt
	c.prevNet = nowCounters
	c.prevNetAt = now
	c.mu.Unlock()

	elapsed := now.Sub(prevAt).Seconds()
	if elapsed <= 0 || elapsed < minNetworkSampleSeconds || len(prevCounters) == 0 {
		return c.sampleNetworkAfterDelay(ctx, nowCounters, now, 250*time.Millisecond)
	}
	return buildLinuxNetworkSet(prevCounters, nowCounters, elapsed)
}

func (c *systemCollector) sampleNetworkAfterDelay(ctx context.Context, previous map[string]netCounter, previousAt time.Time, delay time.Duration) NetworkSet {
	if err := waitForSample(ctx, delay); err != nil {
		return NetworkSet{Available: false, Error: "network sampler canceled: " + err.Error()}
	}
	laterCounters, err := readNetCounters()
	if err != nil {
		return NetworkSet{Available: false, Error: err.Error()}
	}
	laterAt := time.Now()

	c.mu.Lock()
	c.prevNet = laterCounters
	c.prevNetAt = laterAt
	c.mu.Unlock()

	return buildLinuxNetworkSet(previous, laterCounters, networkSampleElapsedSeconds(previousAt, laterAt, delay))
}

func buildLinuxNetworkSet(prevCounters, nowCounters map[string]netCounter, elapsed float64) NetworkSet {
	names := make([]string, 0, len(nowCounters))
	for name := range nowCounters {
		if shouldIncludeLinuxNetworkInterface(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	interfaces := make([]NetworkInterfaceMetric, 0, len(names))
	for _, name := range names {
		current := nowCounters[name]
		previous, ok := prevCounters[name]
		if !ok {
			interfaces = append(interfaces, NetworkInterfaceMetric{
				Name:             name,
				RXBytesTotal:     current.rxBytes,
				TXBytesTotal:     current.txBytes,
				RXBytesPerSecond: unavailableNumber("B/s", "interface is warming up"),
				TXBytesPerSecond: unavailableNumber("B/s", "interface is warming up"),
			})
			continue
		}

		interfaces = append(interfaces, NetworkInterfaceMetric{
			Name:             name,
			RXBytesTotal:     current.rxBytes,
			TXBytesTotal:     current.txBytes,
			RXBytesPerSecond: networkCounterRate(previous.rxBytes, current.rxBytes, elapsed),
			TXBytesPerSecond: networkCounterRate(previous.txBytes, current.txBytes, elapsed),
		})
	}
	if len(interfaces) == 0 {
		return NetworkSet{Available: false, Error: "no non-loopback network interfaces found"}
	}
	sortNetworkInterfacesByActivity(interfaces)
	return NetworkSet{Available: true, Interfaces: interfaces}
}

func shouldIncludeLinuxNetworkInterface(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || name == "lo" {
		return false
	}
	for _, prefix := range skippedLinuxNetworkInterfacePrefixes {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	return true
}

var skippedLinuxNetworkInterfacePrefixes = []string{
	"br-",
	"cni",
	"docker",
	"flannel",
	"kube-ipvs",
	"nerdctl",
	"podman",
	"veth",
	"virbr",
}

func readNetCounters() (map[string]netCounter, error) {
	data, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return nil, err
	}
	return parseNetDev(string(data))
}

func parseNetDev(data string) (map[string]netCounter, error) {
	counters := map[string]netCounter{}
	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.Contains(line, ":") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		name := strings.TrimSpace(parts[0])
		fields := strings.Fields(parts[1])
		if len(fields) < 16 {
			continue
		}
		rxBytes, rxErr := strconv.ParseUint(fields[0], 10, 64)
		txBytes, txErr := strconv.ParseUint(fields[8], 10, 64)
		if rxErr != nil || txErr != nil {
			continue
		}
		counters[name] = netCounter{rxBytes: rxBytes, txBytes: txBytes}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return counters, nil
}

func collectTemperatures() TemperatureSet {
	sensors := collectHWMONTemperatures()
	sensors = appendUniqueTemperatureSensors(sensors, collectThermalZoneTemperatures()...)
	if len(sensors) == 0 {
		return TemperatureSet{Available: false, Error: "no supported temperature sensors found"}
	}
	sort.Slice(sensors, func(i, j int) bool { return sensors[i].Name < sensors[j].Name })
	return TemperatureSet{Available: true, Sensors: sensors}
}

func appendUniqueTemperatureSensors(sensors []TemperatureMetric, candidates ...TemperatureMetric) []TemperatureMetric {
	seen := make(map[string]struct{}, len(sensors)+len(candidates))
	for _, sensor := range sensors {
		key := normalizedTemperatureSensorName(sensor.Name)
		if key != "" {
			seen[key] = struct{}{}
		}
	}
	for _, candidate := range candidates {
		key := normalizedTemperatureSensorName(candidate.Name)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		sensors = append(sensors, candidate)
		seen[key] = struct{}{}
	}
	return sensors
}

func normalizedTemperatureSensorName(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(name), " "))
}

func collectHWMONTemperatures() []TemperatureMetric {
	var sensors []TemperatureMetric
	paths, _ := filepath.Glob("/sys/class/hwmon/hwmon*/temp*_input")
	for _, input := range paths {
		value, ok := readTemperatureMilliC(input)
		if !ok {
			continue
		}
		base := strings.TrimSuffix(filepath.Base(input), "_input")
		dir := filepath.Dir(input)

		chip := readFirstLine(filepath.Join(dir, "name"))
		label := readFirstLine(filepath.Join(dir, base+"_label"))
		name := strings.TrimSpace(strings.Join([]string{chip, label}, " "))
		if name == "" {
			name = filepath.Base(dir) + " " + base
		}
		sensors = append(sensors, TemperatureMetric{
			Name:    name,
			Celsius: availableNumber(value, "C"),
		})
	}
	return sensors
}

func collectThermalZoneTemperatures() []TemperatureMetric {
	var sensors []TemperatureMetric
	paths, _ := filepath.Glob("/sys/class/thermal/thermal_zone*/temp")
	for _, input := range paths {
		value, ok := readTemperatureMilliC(input)
		if !ok {
			continue
		}
		dir := filepath.Dir(input)
		name := readFirstLine(filepath.Join(dir, "type"))
		if name == "" {
			name = filepath.Base(dir)
		}
		sensors = append(sensors, TemperatureMetric{
			Name:    name,
			Celsius: availableNumber(value, "C"),
		})
	}
	return sensors
}

func readTemperatureMilliC(path string) (float64, bool) {
	raw := strings.TrimSpace(readFirstLine(path))
	if raw == "" {
		return 0, false
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false
	}
	celsius := value / 1000
	if celsius < -50 || celsius > 150 {
		return 0, false
	}
	return celsius, true
}

func readFirstLine(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(data), "\n")
	return strings.TrimSpace(line)
}
