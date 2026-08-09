//go:build linux

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
	prevPerCore []cpuTimes
	prevNet     map[string]netCounter
	prevNetAt   time.Time
	prevRAPL    map[string]raplCounter
	prevRAPLAt  time.Time
	prevRAPLSet bool
	// prevProc holds the previous per-PID cumulative CPU-second and disk-byte
	// counters used to derive per-process CPU% and disk rates as a delta. It is
	// replaced wholesale each slow pass, so dead PIDs are pruned automatically.
	// Guarded by mu. See collectProcesses.
	prevProc map[int]procSample

	// clockPeak ratchets the observed CPU boost ceiling reported as
	// cpu_clock_max. Shared, untagged implementation so the field means the same
	// thing here as on Windows. Carries its own lock.
	clockPeak cpuClockPeakTracker

	// powerSmooth rolling-averages cpu_power. RAPL is already an integrated
	// energy delta and needs no denoising, but smoothing only on Windows would
	// make cpu_power a different statistic on each platform -- the defect this
	// codebase has now hit three times. Shared, untagged so both stay symmetric.
	// Carries its own lock.
	powerSmooth cpuPowerSmoother

	// hardwareOnce resolves the static identity strings (CPU model, RAM
	// type/speed) exactly once -- they never change at runtime and RAM needs a
	// dmidecode spawn -- so the slow lane reuses the cached values every pass.
	hardwareOnce sync.Once
	cpuName      string
	memoryName   string

	// uplink caches the active network identity (Wi-Fi SSID / wired link) with a
	// short TTL so the slow lane does not spawn `iw`/`nmcli` on every pass.
	uplink   NetworkUplink
	uplinkAt time.Time
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

// procSample holds the previous cumulative counters for one PID so the slow
// lane can derive per-process CPU% and Disk I/O rates as deltas between passes.
type procSample struct {
	cpuSeconds float64
	ts         time.Time
	diskRead   uint64
	diskWrite  uint64
	diskAvail  bool
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
		prevProc:  map[int]procSample{},
	}
	if cpu, err := readCPUTimes(); err == nil {
		c.prevCPU = cpu
	}
	if perCore, err := readPerCoreCPUTimes(); err == nil {
		c.prevPerCore = perCore
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
	metrics.CPUName, metrics.MemoryName = c.resolveHardwareNames(ctx)

	var wg sync.WaitGroup
	var cpu NumberMetric
	var cpuPower NumberMetric
	var cpuClocks cpuClockSet
	var memory CapacityMetric
	var swap CapacityMetric
	var disks []DiskMetric
	var storage StorageSet
	var network NetworkSet
	var temperatures TemperatureSet
	var gpu GPUSet
	var tailscale TailscaleStatus
	var processes ProcessSet

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
	collectMetricAsync(&wg, &cpuClocks, func() cpuClockSet {
		return c.collectCPUClockSet()
	}, func(recovered any) cpuClockSet {
		return degradedCPUClockSet(fmt.Sprintf("Linux CPU clock collector panicked: %v", recovered))
	})
	collectMetricAsync(&wg, &memory, func() CapacityMetric {
		m, s := collectMemoryAndSwap()
		swap = s
		return m
	}, func(recovered any) CapacityMetric {
		swap = unavailableCapacity(fmt.Sprintf("Linux memory collector panicked: %v", recovered))
		return unavailableCapacity(fmt.Sprintf("Linux memory collector panicked: %v", recovered))
	})
	collectMetricAsync(&wg, &disks, func() []DiskMetric {
		return collectDisks()
	}, func(recovered any) []DiskMetric {
		return unavailableDisk(fmt.Sprintf("Linux disk collector panicked: %v", recovered))
	})
	collectMetricAsync(&wg, &storage, func() StorageSet {
		return collectStorage()
	}, func(recovered any) StorageSet {
		return unavailableStorage(fmt.Sprintf("Linux storage collector panicked: %v", recovered))
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
	collectMetricAsync(&wg, &tailscale, func() TailscaleStatus {
		return readTailscaleStatus(ctx)
	}, func(recovered any) TailscaleStatus {
		return TailscaleStatus{Available: false, Error: fmt.Sprintf("Linux Tailscale collector panicked: %v", recovered)}
	})
	collectMetricAsync(&wg, &processes, func() ProcessSet {
		return c.collectProcesses(ctx)
	}, func(recovered any) ProcessSet {
		return unavailableProcessSet(fmt.Sprintf("Linux process collector panicked: %v", recovered))
	})
	wg.Wait()

	power := c.powerSmooth.observe(cpuPowerSample{
		Package: cpuPower,
		Core:    unavailableNumber("W", linuxNoPowerRailsMessage),
		Soc:     unavailableNumber("W", linuxNoPowerRailsMessage),
		Misc:    unavailableNumber("W", linuxNoPowerRailsMessage),
		PSUOut:  unavailableNumber("W", "no PSU output power sensor exposed on Linux"),
	})

	metrics.CPU = cpu
	metrics.CPUCores = c.collectCPUCores(ctx)
	metrics.CPUPower = power.Package
	unavailableCPUPowerRails(&metrics, linuxNoPowerRailsMessage)
	cpuClocks.applyTo(&metrics)
	metrics.CPUTemperature, metrics.CPUTemperatureSensor = pickCPUTemperatureSensor(temperatures)
	metrics.PSUOutputPower = unavailableNumber("W", "no PSU output power sensor exposed on Linux")
	metrics.Memory = memory
	metrics.MemorySwap = swap
	metrics.Disks = disks
	metrics.Storage = storage
	metrics.Network = network
	metrics.Tailscale = tailscale
	metrics.Temperatures = temperatures
	metrics.GPU = gpu
	metrics.Processes = processes
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
			cores := unavailableCPUCores(fmt.Sprintf("Linux CPU collector panicked: %v", r))
			memory := unavailableCapacity(fmt.Sprintf("Linux memory collector panicked: %v", r))
			swap := unavailableCapacity(fmt.Sprintf("Linux memory collector panicked: %v", r))
			patch = func(m *Metrics) {
				m.CPU = cpu
				m.CPUCores = cores
				m.Memory = memory
				m.MemorySwap = swap
			}
		}
	}()

	cpu := c.collectCPU(ctx)
	cores := c.collectCPUCores(ctx)
	memory, swap := collectMemoryAndSwap()
	return func(m *Metrics) {
		m.CPU = cpu
		m.CPUCores = cores
		m.Memory = memory
		m.MemorySwap = swap
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
	cpuName, memoryName := c.resolveHardwareNames(ctx)

	var wg sync.WaitGroup
	var cpuPower NumberMetric
	var cpuClocks cpuClockSet
	var disks []DiskMetric
	var storage StorageSet
	var network NetworkSet
	var temperatures TemperatureSet
	var gpu GPUSet
	var tailscale TailscaleStatus
	var processes ProcessSet

	collectMetricAsync(&wg, &cpuPower, func() NumberMetric {
		return c.collectCPUPower(ctx)
	}, func(recovered any) NumberMetric {
		return unavailableNumber("W", fmt.Sprintf("Linux CPU power collector panicked: %v", recovered))
	})
	collectMetricAsync(&wg, &cpuClocks, func() cpuClockSet {
		return c.collectCPUClockSet()
	}, func(recovered any) cpuClockSet {
		return degradedCPUClockSet(fmt.Sprintf("Linux CPU clock collector panicked: %v", recovered))
	})
	collectMetricAsync(&wg, &disks, func() []DiskMetric {
		return collectDisks()
	}, func(recovered any) []DiskMetric {
		return unavailableDisk(fmt.Sprintf("Linux disk collector panicked: %v", recovered))
	})
	collectMetricAsync(&wg, &storage, func() StorageSet {
		return collectStorage()
	}, func(recovered any) StorageSet {
		return unavailableStorage(fmt.Sprintf("Linux storage collector panicked: %v", recovered))
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
	collectMetricAsync(&wg, &tailscale, func() TailscaleStatus {
		return readTailscaleStatus(ctx)
	}, func(recovered any) TailscaleStatus {
		return TailscaleStatus{Available: false, Error: fmt.Sprintf("Linux Tailscale collector panicked: %v", recovered)}
	})
	collectMetricAsync(&wg, &processes, func() ProcessSet {
		return c.collectProcesses(ctx)
	}, func(recovered any) ProcessSet {
		return unavailableProcessSet(fmt.Sprintf("Linux process collector panicked: %v", recovered))
	})
	wg.Wait()

	cpuTemperature, cpuTemperatureSensor := pickCPUTemperatureSensor(temperatures)
	power := c.powerSmooth.observe(cpuPowerSample{
		Package: cpuPower,
		Core:    unavailableNumber("W", linuxNoPowerRailsMessage),
		Soc:     unavailableNumber("W", linuxNoPowerRailsMessage),
		Misc:    unavailableNumber("W", linuxNoPowerRailsMessage),
		PSUOut:  unavailableNumber("W", "no PSU output power sensor exposed on Linux"),
	})
	return func(m *Metrics) {
		m.Platform = platform
		m.CPUName = cpuName
		m.MemoryName = memoryName
		m.CPUPower = power.Package
		unavailableCPUPowerRails(m, linuxNoPowerRailsMessage)
		cpuClocks.applyTo(m)
		m.CPUTemperature = cpuTemperature
		m.CPUTemperatureSensor = cpuTemperatureSensor
		m.PSUOutputPower = unavailableNumber("W", "no PSU output power sensor exposed on Linux")
		m.Disks = disks
		m.Storage = storage
		m.Network = network
		m.Tailscale = tailscale
		m.Temperatures = temperatures
		m.GPU = gpu
		m.Processes = processes
	}
}

// linuxNoPowerRailsMessage explains the permanently-absent per-rail CPU power
// breakdown on Linux. RAPL (/sys/class/powercap) exposes package and, on some
// parts, a core subdomain, but not the AMD SMU core/SoC/misc split the Windows
// LibreHardwareMonitor bridge reads out of the SMU power table.
const linuxNoPowerRailsMessage = "no per-rail CPU power breakdown on Linux (RAPL exposes package energy only, not the AMD SMU core/SoC/misc rails)"

// linuxDegradedSlowPatch marks every slow-lane field unavailable with the same
// message. It is the panic safety net for CollectSlow (per-metric panics are
// already recovered by collectMetricAsync; this covers a panic before the
// fan-out).
func linuxDegradedSlowPatch(message string) func(*Metrics) {
	return func(m *Metrics) {
		m.CPUPower = unavailableNumber("W", message)
		unavailableCPUPowerRails(m, message)
		degradedCPUClockSet(message).applyTo(m)
		m.CPUTemperature = unavailableNumber("C", message)
		m.CPUTemperatureSensor = ""
		m.PSUOutputPower = unavailableNumber("W", message)
		m.Disks = unavailableDisk(message)
		m.Storage = unavailableStorage(message)
		m.Network = NetworkSet{Available: false, Error: message}
		m.Tailscale = TailscaleStatus{Available: false, Error: message}
		m.Temperatures = TemperatureSet{Available: false, Error: message}
		m.GPU = GPUSet{Available: false, Error: message}
		m.Processes = unavailableProcessSet(message)
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

// collectCPUCores reports per-core utilization by deltaing the per-core jiffy
// counters against the previous sample. It mirrors collectCPU: with no prior
// per-core sample (first call) or a changed core count (CPU hot-plug) it takes a
// brief second read so the first snapshot still carries real values, then
// degrades the whole set to unavailable rather than returning a hard error.
func (c *systemCollector) collectCPUCores(ctx context.Context) CPUCoreSet {
	now, err := readPerCoreCPUTimes()
	if err != nil {
		return unavailableCPUCores(err.Error())
	}

	c.mu.Lock()
	prev := c.prevPerCore
	c.prevPerCore = now
	c.mu.Unlock()

	if len(prev) == 0 || len(prev) != len(now) {
		return c.samplePerCoreAfterDelay(ctx, now, 100*time.Millisecond)
	}
	return perCoreUsage(prev, now)
}

func (c *systemCollector) samplePerCoreAfterDelay(ctx context.Context, prev []cpuTimes, delay time.Duration) CPUCoreSet {
	if err := waitForSample(ctx, delay); err != nil {
		return unavailableCPUCores("per-core sampler canceled: " + err.Error())
	}
	later, err := readPerCoreCPUTimes()
	if err != nil {
		return unavailableCPUCores(err.Error())
	}
	c.mu.Lock()
	c.prevPerCore = later
	c.mu.Unlock()

	if len(prev) == 0 || len(prev) != len(later) {
		return unavailableCPUCores("per-core sampler is warming up")
	}
	return perCoreUsage(prev, later)
}

// perCoreUsage turns two equal-length per-core samples into busy percentages
// (one per core, rounded to one decimal). A core whose counters did not advance
// is reported as idle rather than dropped, so Cores stays index-aligned.
func perCoreUsage(prev, now []cpuTimes) CPUCoreSet {
	cores := make([]float64, len(now))
	for i := range now {
		value, ok := cpuUsagePercent(prev[i], now[i])
		if !ok {
			value = 0
		}
		cores[i] = round(value, 1)
	}
	return availableCPUCores(cores)
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
		// Only count the counter as advanced when energy actually accrued. A
		// matched package with a zero delta means the counter is stuck (the CPU
		// is always drawing some power), so it must degrade rather than report 0 W.
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
	packageEntries := 0
	var firstEnergyErr error
	for _, dir := range matches {
		if !isRAPLPackageEntry(filepath.Base(dir)) {
			continue
		}
		packageEntries++
		energyPath := filepath.Join(dir, "energy_uj")
		energy, readErr := readRAPLEnergyUJ(energyPath)
		if readErr != nil {
			if firstEnergyErr == nil {
				firstEnergyErr = readErr
			}
			continue
		}
		maxUJ, _ := readUint64File(filepath.Join(dir, "max_energy_range_uj"))
		counters[energyPath] = raplCounter{energyUJ: energy, maxUJ: maxUJ}
	}
	if len(counters) == 0 {
		// Distinguish "no RAPL interface exposed" (VMs, locked BIOS) from
		// "interface present but unreadable". The latter is almost always the
		// kernel's default 0400 root-only mode on energy_uj hitting an
		// unprivileged agent; surface that cause and the udev fix explicitly so
		// the error string is actionable instead of the misleading "no counters".
		if packageEntries > 0 && firstEnergyErr != nil {
			msg := fmt.Sprintf("RAPL package energy counters present but not readable: %v", firstEnergyErr)
			if errors.Is(firstEnergyErr, os.ErrPermission) {
				msg += " (check access to /sys/class/powercap/intel-rapl:*/energy_uj; install scripts/udev/99-powercap-rapl.rules for unprivileged access)"
			}
			return nil, errors.New(msg)
		}
		return nil, errors.New("no CPU package power counters found (RAPL not exposed; common inside VMs or with a locked-down BIOS)")
	}
	return counters, nil
}

// readRAPLEnergyUJ reads a RAPL energy_uj counter, returning the underlying
// error (e.g. EACCES) so readRAPLCounters can tell "present but unreadable"
// from "absent". Unlike readUint64File, it does not swallow the cause.
func readRAPLEnergyUJ(path string) (uint64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	value, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0, err
	}
	return value, nil
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
// collectCPUClocks reports the live cross-core average clock, the live clock of
// the fastest single core, the firmware-declared ceiling, and the rated base
// clock. See the Metrics clock-field comment for what each one means; the short
// version is that the average and the per-core peak differ by ~1 GHz on a
// many-core part, and the declared ceiling is not an achievable clock on
// amd-pstate.
//
// Current and peak come from one pass over the cpufreq scaling_cur_freq nodes
// (with a /proc/cpuinfo fallback for kernels that expose no cpufreq); rated
// prefers cpuinfo_max_freq and falls back to scaling_max_freq; base prefers the
// Intel-only base_frequency node and falls back to the CPPC nominal frequency,
// which is what AMD parts expose instead.
// collectCPUClockSet reads the clocks and folds the per-core peak into this
// collector's peak-hold ceiling, producing the full set the sampler publishes.
func (c *systemCollector) collectCPUClockSet() cpuClockSet {
	current, peakCore, rated, base := collectCPUClocks()
	baseMHz, baseOK := 0.0, false
	if base.Available {
		baseMHz, baseOK = base.Value, true
	}
	return cpuClockSet{
		Current:  current,
		PeakCore: peakCore,
		Max:      c.clockPeak.observe(peakCore, current, baseMHz, baseOK),
		Rated:    rated,
		Base:     base,
	}
}

func collectCPUClocks() (current, peakCore, rated, base NumberMetric) {
	avgMHz, peakMHz, ok := readCPUFreqClocks()
	if !ok {
		avgMHz, peakMHz, ok = readProcCPUInfoClocks()
	}
	if ok {
		current = availableNumber(avgMHz, "MHz")
		peakCore = availableNumber(peakMHz, "MHz")
	} else {
		current = unavailableNumber("MHz", "CPU clock frequency not exposed")
		peakCore = unavailableNumber("MHz", "per-core CPU clock frequency not exposed")
	}
	if mhz, ok := readCPUFreqMaxClock(); ok {
		rated = availableNumber(mhz, "MHz")
	} else {
		rated = unavailableNumber("MHz", "CPU rated max clock frequency not exposed")
	}
	if mhz, ok := readCPUFreqBaseClock(); ok {
		base = availableNumber(mhz, "MHz")
	} else {
		base = unavailableNumber("MHz", "CPU base clock frequency not exposed")
	}
	return current, peakCore, rated, base
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

// readCPUFreqBaseClock reads the rated base (non-turbo) frequency. cpufreq's
// base_frequency node is Intel-only, so AMD parts fall back to the ACPI CPPC
// nominal frequency, which is the same figure: on a 7950X acpi_cppc/nominal_freq
// reads 4501, matching the rated 4.5 GHz base. Without this fallback base clock
// was permanently unavailable on AMD, which cost the dashboard ring its lower
// bound and the peak-hold tracker its seed.
func readCPUFreqBaseClock() (float64, bool) {
	if mhz, ok := readCPUFreqValue("base_frequency"); ok {
		return mhz, true
	}
	return readCPPCNominalFreq()
}

// readCPPCNominalFreq reads the CPPC nominal (base) frequency. Note the unit:
// unlike every cpufreq node, which reports kHz, the acpi_cppc nodes report MHz
// already -- so this deliberately does not reuse readCPUFreqValue, which divides
// by 1000.
func readCPPCNominalFreq() (float64, bool) {
	mhz, ok := readUint64File("/sys/devices/system/cpu/cpu0/acpi_cppc/nominal_freq")
	if !ok || mhz == 0 {
		return 0, false
	}
	return float64(mhz), true
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

// readCPUFreqClocks returns the mean and the maximum live core clock in MHz from
// one pass over every CPU's cpufreq scaling_cur_freq node. Both come from the
// same pass so the pair is always self-consistent (peak >= mean) rather than
// straddling two samples taken microseconds apart.
//
// SMT siblings each expose their own node reporting the same physical core's
// frequency. That double-counts hyperthreaded cores in the mean, which is
// harmless (every core is counted the same number of times, so the mean is
// unchanged) and irrelevant to the max.
//
// This lives on the SLOW sampler lane on purpose. On amd-pstate each read is
// serviced by an IPI to the target CPU to sample APERF/MPERF, so polling 32
// nodes at the fast-lane rate would wake every idle core five times a second --
// the monitor would measurably degrade the deep C-state residency that keeps the
// host cool, i.e. it would change the thing it is trying to measure.
func readCPUFreqClocks() (mean, peak float64, ok bool) {
	pattern := "/sys/devices/system/cpu/cpu[0-9]*/cpufreq/scaling_cur_freq"
	paths, err := filepath.Glob(pattern)
	if err != nil || len(paths) == 0 {
		return 0, 0, false
	}
	var total float64
	count := 0
	for _, path := range paths {
		kHz, ok := readUint64File(path)
		if !ok {
			continue
		}
		mhz := float64(kHz) / 1000.0
		total += mhz
		if mhz > peak {
			peak = mhz
		}
		count++
	}
	if count == 0 {
		return 0, 0, false
	}
	return total / float64(count), peak, true
}

func readProcCPUInfoClocks() (mean, peak float64, ok bool) {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return 0, 0, false
	}
	return parseProcCPUInfoClocks(string(data))
}

// parseProcCPUInfoClocks averages and peaks the per-CPU clock lines in
// /proc/cpuinfo, the fallback for kernels that expose no cpufreq interface (some
// VMs and servers).
//
// x86 reports "cpu MHz" per CPU; ARM reports no such line and "BogoMIPS" is the
// nearest usable stand-in. The two are read as alternatives, never mixed: an x86
// /proc/cpuinfo carries a lowercase "bogomips" line too, and averaging a real
// frequency together with a BogoMIPS figure (~2x the clock on x86) would inflate
// both the mean and, far worse, the peak -- which then ratchets the peak-hold
// ceiling somewhere it can never come back down from.
func parseProcCPUInfoClocks(data string) (mean, peak float64, ok bool) {
	for _, prefix := range []string{"cpu MHz", "BogoMIPS"} {
		var total float64
		var high float64
		count := 0
		for _, line := range strings.Split(data, "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, prefix) {
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
			if value > high {
				high = value
			}
			count++
		}
		if count > 0 {
			return total / float64(count), high, true
		}
	}
	return 0, 0, false
}

func parseCPUTimes(data string) (cpuTimes, error) {
	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		return parseCPULineFields(strings.Fields(line)[1:])
	}
	if err := scanner.Err(); err != nil {
		return cpuTimes{}, err
	}
	return cpuTimes{}, fmt.Errorf("missing cpu line in /proc/stat")
}

// parseCPULineFields converts the numeric jiffy counters of a /proc/stat cpu
// line (the fields after the "cpu"/"cpuN" label) into an idle+total pair. It is
// shared by the aggregate (parseCPUTimes) and per-core (parsePerCoreCPUTimes)
// paths so both apply the same overflow checks and idle = idle+iowait rule.
func parseCPULineFields(numbers []string) (cpuTimes, error) {
	if len(numbers) < 4 {
		return cpuTimes{}, fmt.Errorf("invalid /proc/stat cpu line")
	}
	var total uint64
	values := make([]uint64, 0, len(numbers))
	for _, field := range numbers {
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

func readPerCoreCPUTimes() ([]cpuTimes, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return nil, err
	}
	return parsePerCoreCPUTimes(string(data))
}

// parsePerCoreCPUTimes returns one cpuTimes per logical core from the "cpu0",
// "cpu1", ... lines of /proc/stat, ordered by their appearance (core index).
// The aggregate "cpu " line and any non-"cpuN" line are skipped.
func parsePerCoreCPUTimes(data string) ([]cpuTimes, error) {
	var cores []cpuTimes
	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(strings.TrimSpace(scanner.Text()))
		if len(fields) == 0 {
			continue
		}
		label := fields[0]
		if len(label) <= 3 || label[:3] != "cpu" || !isAllDigits(label[3:]) {
			continue
		}
		core, err := parseCPULineFields(fields[1:])
		if err != nil {
			return nil, err
		}
		cores = append(cores, core)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(cores) == 0 {
		return nil, fmt.Errorf("no per-core cpu lines in /proc/stat")
	}
	return cores, nil
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func collectMemory() CapacityMetric {
	total, available, _, _, err := readMemInfo()
	if err != nil {
		return unavailableCapacity(err.Error())
	}
	if total == 0 || available > total {
		return unavailableCapacity("invalid memory counters")
	}
	return availableCapacity(total-available, total)
}

// collectMemoryAndSwap parses /proc/meminfo once and returns both the physical
// memory and swap capacity metrics, so the fast lane does not read the file
// twice. Swap used = SwapTotal - SwapFree; a host with no swap (SwapTotal == 0)
// reports unavailable so the dashboard shows a graceful "no swap".
func collectMemoryAndSwap() (memory, swap CapacityMetric) {
	total, available, swapTotal, swapFree, err := readMemInfo()
	if err != nil {
		msg := err.Error()
		return unavailableCapacity(msg), unavailableCapacity(msg)
	}
	if total == 0 || available > total {
		memory = unavailableCapacity("invalid memory counters")
	} else {
		memory = availableCapacity(total-available, total)
	}
	if swapTotal == 0 {
		swap = unavailableCapacity("no swap configured")
	} else if swapFree > swapTotal {
		swap = unavailableCapacity("invalid swap counters")
	} else {
		swap = availableCapacity(swapTotal-swapFree, swapTotal)
	}
	return memory, swap
}

func readMemInfo() (total, available, swapTotal, swapFree uint64, err error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, 0, 0, err
	}
	return parseMemInfo(string(data))
}

func parseMemInfo(data string) (total, available, swapTotal, swapFree uint64, err error) {
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
			return 0, 0, 0, 0, fmt.Errorf("invalid /proc/meminfo value for %s", key)
		}
		values[key] = bytes
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, 0, 0, err
	}

	total = values["MemTotal"]
	available = values["MemAvailable"]
	if available == 0 {
		var ok bool
		available, ok = sumUint64(values["MemFree"], values["Buffers"], values["Cached"])
		if !ok {
			return 0, 0, 0, 0, fmt.Errorf("invalid /proc/meminfo fallback counters")
		}
	}
	if total == 0 {
		return 0, 0, 0, 0, fmt.Errorf("MemTotal missing from /proc/meminfo")
	}
	return total, available, values["SwapTotal"], values["SwapFree"], nil
}

type mountInfo struct {
	device     string
	mountpoint string
	fsType     string
}

func collectDisks() []DiskMetric {
	mounts, err := readIncludedMounts()
	if err != nil {
		return unavailableDisk(err.Error())
	}

	// Collapse to one row per backing device (see collapseMountsByDevice) so a
	// btrfs device mounted at several subvolumes emits a single row instead of
	// N identical capacity readings (and N identical alerts).
	representatives := collapseMountsByDevice(mounts)

	disks := make([]DiskMetric, 0, len(representatives))
	for _, mount := range representatives {
		disks = append(disks, DiskMetric{
			Name:       diskName(mount.device),
			Mountpoint: mount.mountpoint,
			FSType:     mount.fsType,
			Capacity:   statfsCapacity(mount.mountpoint),
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

// collapseMountsByDevice returns one representative mount per backing device,
// keeping the shortest (root-most) mountpoint. Keying on the device rather than
// the mountpoint is what turns the five btrfs subvolume rows (/root, /srv,
// /var/cache, ...) into a single "/" row. Pure (no I/O) so it is unit-testable
// against synthetic mounts, and the order of devices is stable (first-seen).
func collapseMountsByDevice(mounts []mountInfo) []mountInfo {
	best := map[string]mountInfo{}
	order := []string{}
	for _, mount := range mounts {
		existing, ok := best[mount.device]
		if !ok {
			best[mount.device] = mount
			order = append(order, mount.device)
			continue
		}
		if isMoreRootMountpoint(mount.mountpoint, existing.mountpoint) {
			best[mount.device] = mount
		}
	}
	out := make([]mountInfo, 0, len(order))
	for _, device := range order {
		out = append(out, best[device])
	}
	return out
}

// readIncludedMounts returns the local (non-pseudo, non-remote) mounts from
// /proc/mounts that back the per-mountpoint disk rows.
func readIncludedMounts() ([]mountInfo, error) {
	return readMounts(shouldIncludeMount)
}

// readStorageMounts returns the mounts used for per-device storage grouping. It
// is readIncludedMounts plus removable media (see shouldIncludeStorageMount), so
// an external drive still reports its capacity on the storage panel.
func readStorageMounts() ([]mountInfo, error) {
	return readMounts(shouldIncludeStorageMount)
}

// readMounts parses /proc/mounts and keeps the entries the filter accepts.
func readMounts(include func(mountInfo) bool) ([]mountInfo, error) {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return nil, err
	}
	var out []mountInfo
	for _, mount := range parseMounts(string(data)) {
		if include(mount) {
			out = append(out, mount)
		}
	}
	return out, nil
}

// statfsCapacity stats one mountpoint and returns its used/total capacity. It is
// the shared per-mount statfs path for collectDisks and collectStorage.
func statfsCapacity(mountpoint string) CapacityMetric {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(mountpoint, &stat); err != nil {
		return unavailableCapacity(err.Error())
	}
	total, totalOK := statfsBytes(stat.Blocks, stat.Bsize)
	free, freeOK := statfsBytes(stat.Bavail, stat.Bsize)
	if !totalOK || !freeOK {
		return unavailableCapacity("invalid disk capacity counters")
	}
	return availableCapacityFromTotalFree(total, free, "invalid disk capacity counters")
}

// isMoreRootMountpoint reports whether a is a more root-level mountpoint than b
// ("/" wins; otherwise the shorter path wins). It is the tie-breaker that keeps
// "/" as the representative mountpoint when a btrfs device has several
// subvolume mounts.
func isMoreRootMountpoint(a, b string) bool {
	if a == "/" {
		return true
	}
	if b == "/" {
		return false
	}
	return len(a) < len(b)
}

// collectStorage builds the per-physical-drive storage set: each whole block
// device (NVMe/SATA SSD, HDD) with its physical size, aggregated capacity over
// the mounted filesystems it backs, and its temperature. It is generic over
// /sys/block/* (skipping loop/zram/dm-/sr/ram) so SATA and NVMe both work. A
// drive with nothing mounted (e.g. an unmounted NTFS drive) degrades Capacity to
// unavailable with "no mounted filesystems" rather than reporting a bogus
// 0%-of-physical; Temperature degrades independently. Mirrors the GPUSet pattern.
func collectStorage() StorageSet {
	return collectStorageFrom("/sys")
}

// collectStorageFrom builds the per-physical-drive storage set rooted at sysRoot
// ("/sys" in production), so it is unit-testable against a fake sysfs tree. Each
// whole block device (NVMe/SATA SSD, HDD) carries its physical size, aggregated
// capacity over the mounted filesystems it backs, and its temperature. A drive
// with nothing mounted (e.g. an unmounted NTFS drive) degrades Capacity to
// unavailable with "no mounted filesystems" rather than reporting a bogus
// 0%-of-physical; Temperature degrades independently. Mirrors the GPUSet pattern.
func collectStorageFrom(sysRoot string) StorageSet {
	blockRoot := filepath.Join(sysRoot, "block")
	entries, err := os.ReadDir(blockRoot)
	if err != nil {
		return unavailableStorage("read " + blockRoot + ": " + err.Error())
	}

	// Group included mountpoints onto their whole-disk name so each device's
	// capacity can aggregate the filesystems it actually backs. A partition
	// (nvme0n1p5) is mapped to its whole disk (nvme0n1); a whole disk maps to
	// itself. Non-block mounts (tmpfs/overlay) pass through unchanged and never
	// match a sysRoot/block device, so they are ignored.
	classBlockRoot := filepath.Join(sysRoot, "class", "block")
	mountsByDevice := map[string][]mountInfo{}
	if mounts, err := readStorageMounts(); err == nil {
		for _, mount := range mounts {
			whole := linuxWholeDiskForName(diskName(mount.device), classBlockRoot)
			mountsByDevice[whole] = append(mountsByDevice[whole], mount)
		}
	}

	var devices []StorageDevice
	for _, entry := range entries {
		name := entry.Name()
		if shouldSkipBlockDevice(name) {
			continue
		}
		mounts := mountsByDevice[name]
		devices = append(devices, StorageDevice{
			Name:        name,
			Model:       strings.TrimSpace(readFirstLine(filepath.Join(blockRoot, name, "device", "model"))),
			SizeBytes:   linuxBlockDeviceSize(name, blockRoot),
			Mountpoints: mountpointsOf(mounts),
			Capacity:    aggregateDeviceCapacity(mounts),
			Temperature: linuxStorageTemperature(name, sysRoot),
		})
	}
	if len(devices) == 0 {
		return unavailableStorage("no block devices found")
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].Name < devices[j].Name })
	return StorageSet{Available: true, Devices: devices}
}

// shouldSkipBlockDevice names the /sys/block entries that are not physical
// drives: loop devices, compressed RAM (zram), device-mapper (dm-), optical
// (sr), and legacy RAM disks (ram).
func shouldSkipBlockDevice(name string) bool {
	for _, prefix := range []string{"loop", "zram", "dm-", "sr", "ram"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// linuxBlockDeviceSize returns a whole block device's capacity in bytes from
// <blockRoot>/<name>/size (512-byte sectors). The multiply is overflow-checked
// (sumUint64 adds, so it cannot be reused here).
func linuxBlockDeviceSize(name, blockRoot string) uint64 {
	sectors, ok := readUint64File(filepath.Join(blockRoot, name, "size"))
	if !ok {
		return 0
	}
	const sectorSize = 512
	if sectors > ^uint64(0)/sectorSize {
		return 0
	}
	return sectors * sectorSize
}

// mountpointsOf projects the mountpoint paths out of a device's mount list, for
// the storage row's caption. Every mountpoint is kept (they are distinct paths)
// even where several share one filesystem.
func mountpointsOf(mounts []mountInfo) []string {
	if len(mounts) == 0 {
		return nil
	}
	out := make([]string, 0, len(mounts))
	for _, mount := range mounts {
		out = append(out, mount.mountpoint)
	}
	return out
}

// aggregateDeviceCapacity sums used/total across the distinct filesystems
// backing one device. Several mountpoints can share a single filesystem (btrfs
// subvolumes: /, /root, /srv, /var/log ... are all one filesystem on one
// partition), and each reports that filesystem's full size, so counting every
// mountpoint would report a multi-subvolume device at N× its real capacity.
//
// The dedup key is the backing device from /proc/mounts, which is exact: two
// mounts share a filesystem iff they share a device. An earlier version keyed on
// the observed (total, free) pair instead, which silently collapsed two genuinely
// distinct sibling partitions whenever they happened to match -- e.g. a drive
// split into two equal, equally-used volumes reported half its capacity.
//
// Deduping before the statfs call also saves the redundant syscalls. A device
// with no mountpoints (or none stat-able) degrades to unavailable rather than a
// misleading 0%.
func aggregateDeviceCapacity(mounts []mountInfo) CapacityMetric {
	seen := map[string]bool{}
	var totalSum, usedSum uint64
	anyAvailable := false
	for _, mount := range mounts {
		if seen[mount.device] {
			continue
		}
		seen[mount.device] = true
		capacity := statfsCapacity(mount.mountpoint)
		if !capacity.Available {
			continue
		}
		anyAvailable = true
		total, totalOK := sumUint64(totalSum, capacity.TotalBytes)
		used, usedOK := sumUint64(usedSum, capacity.UsedBytes)
		if !totalOK || !usedOK {
			return unavailableCapacity("capacity counters overflow")
		}
		totalSum = total
		usedSum = used
	}
	if !anyAvailable {
		return unavailableCapacity("no mounted filesystems")
	}
	return availableCapacity(usedSum, totalSum)
}

// linuxWholeDiskForName maps a block-device name (a whole disk or a partition)
// to its whole-disk name using sysfs rooted at classBlockRoot: a partition
// carries a "partition" attribute and its parent directory in sysfs is the whole
// disk. Whole disks map to themselves. Falls back to the input unchanged when
// sysfs is unavailable or the name is not a known block device (e.g. "tmpfs"),
// so non-block mounts never match a real device and are simply ignored by the
// grouping.
func linuxWholeDiskForName(name, classBlockRoot string) string {
	if name == "" {
		return name
	}
	link := filepath.Join(classBlockRoot, name)
	if _, err := os.Stat(filepath.Join(link, "partition")); err != nil {
		return name
	}
	target, err := os.Readlink(link)
	if err != nil {
		return name
	}
	resolved := filepath.Join(classBlockRoot, target)
	return filepath.Base(filepath.Dir(filepath.Clean(resolved)))
}

// linuxStorageTemperature reads one block device's temperature from the sysfs
// tree rooted at sysRoot. NVMe drives expose their Composite sensor under
// <sysRoot>/class/nvme/<controller>/hwmon; other devices (SATA SSD/HDD) may
// expose one under <sysRoot>/block/<dev>/device/hwmon.
func linuxStorageTemperature(diskName, sysRoot string) NumberMetric {
	if controller, ok := nvmeControllerForDisk(diskName); ok {
		return nvmeStorageTemperature(controller, filepath.Join(sysRoot, "class", "nvme"))
	}
	return blockDeviceStorageTemperature(diskName, filepath.Join(sysRoot, "block"))
}

// nvmeControllerForDisk maps an NVMe whole-disk name to its controller: nvme0n1
// -> nvme0, nvme12n0 -> nvme12. Returns false for non-NVMe names.
func nvmeControllerForDisk(disk string) (string, bool) {
	if !strings.HasPrefix(disk, "nvme") {
		return "", false
	}
	rest := strings.TrimPrefix(disk, "nvme")
	idx := strings.IndexByte(rest, 'n')
	if idx <= 0 {
		return "", false
	}
	return "nvme" + rest[:idx], true
}

// nvmeStorageTemperature reads the controller's Composite temperature from
// <classRoot>/<controller>/hwmon*, falling back to the first available sensor if
// none is labelled Composite. Returns unavailable (never an error) when the
// hwmon is absent.
func nvmeStorageTemperature(controller, classRoot string) NumberMetric {
	paths, _ := filepath.Glob(filepath.Join(classRoot, controller, "hwmon*", "temp*_input"))
	sort.Strings(paths)
	var fallback NumberMetric
	fallbackOK := false
	for _, p := range paths {
		value, ok := readTemperatureMilliC(p)
		if !ok {
			continue
		}
		base := strings.TrimSuffix(filepath.Base(p), "_input")
		label := strings.TrimSpace(readFirstLine(filepath.Join(filepath.Dir(p), base+"_label")))
		if strings.EqualFold(label, "Composite") {
			return availableNumber(value, "C")
		}
		if !fallbackOK {
			fallback = availableNumber(value, "C")
			fallbackOK = true
		}
	}
	if fallbackOK {
		return fallback
	}
	return unavailableNumber("C", "no NVMe temperature sensor exposed")
}

// blockDeviceStorageTemperature reads a non-NVMe block device's temperature
// from <blockRoot>/<disk>/device/hwmon, the path SATA/SAS HBA temperature
// sensors use.
func blockDeviceStorageTemperature(disk, blockRoot string) NumberMetric {
	paths, _ := filepath.Glob(filepath.Join(blockRoot, disk, "device", "hwmon", "hwmon*", "temp*_input"))
	sort.Strings(paths)
	for _, p := range paths {
		if value, ok := readTemperatureMilliC(p); ok {
			return availableNumber(value, "C")
		}
	}
	return unavailableNumber("C", "no block-device temperature sensor exposed")
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
	if !isLocalFilesystemMount(mount) {
		return false
	}
	for _, prefix := range []string{"/proc", "/sys", "/dev", "/run"} {
		if mount.mountpoint == prefix || strings.HasPrefix(mount.mountpoint, prefix+"/") {
			return false
		}
	}
	return true
}

// isLocalFilesystemMount is the filesystem-type half of the mount filter: it
// rejects pseudo, remote, non-root overlay, and docker-layer mounts, but says
// nothing about the mountpoint's location. shouldIncludeMount adds the
// system-directory prefix rejection on top; shouldIncludeStorageMount reuses
// this half so removable media can opt back in.
func isLocalFilesystemMount(mount mountInfo) bool {
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
	if strings.Contains(mount.mountpoint, "/var/lib/docker/overlay2/") {
		return false
	}
	return true
}

// shouldIncludeStorageMount is the mount filter for per-device storage grouping.
// It accepts everything shouldIncludeMount does, plus removable media mounted
// under /run/media -- which shouldIncludeMount rejects via its blanket "/run"
// prefix rule. That rule exists to keep tmpfs/runtime state out of the
// per-mountpoint disk rows, but an external SSD auto-mounted at
// /run/media/<user>/<label> is a real filesystem on a real drive, and excluding
// it makes its whole drive report "no mounted filesystems" on the storage panel.
// (/media and /mnt already pass shouldIncludeMount, so they need no special case.)
func shouldIncludeStorageMount(mount mountInfo) bool {
	if shouldIncludeMount(mount) {
		return true
	}
	if !isLocalFilesystemMount(mount) {
		return false
	}
	return strings.HasPrefix(mount.mountpoint, "/run/media/")
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

// collectNetwork builds the per-interface throughput set and attaches the
// active-network identity (Wi-Fi SSID / wired link) for the NET card.
func (c *systemCollector) collectNetwork(ctx context.Context) NetworkSet {
	set := c.collectNetworkRates(ctx)
	set.Uplink = c.collectNetworkUplink(ctx)
	return set
}

func (c *systemCollector) collectNetworkRates(ctx context.Context) NetworkSet {
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

// linuxClockTicksPerSecond is the USER_HZ clock tick rate used by the
// /proc/[pid]/stat utime+stime fields. It has been 100 Hz on essentially every
// Linux/x86 and Linux/arm kernel for decades; the stdlib does not expose
// sysconf(_SC_CLK_TCK), so this constant converts jiffies to seconds. A host
// with a non-standard rate (extremely rare, mostly embedded) would mis-scale
// per-process CPU% by a constant factor without failing the response.
const linuxClockTicksPerSecond = 100.0

// collectProcesses enumerates /proc/[pid] once per slow pass and derives the
// per-process CPU% (delta of utime+stime jiffies, whole-host normalized so it
// is comparable to the aggregated CPU gauge), RSS from /proc/[pid]/status,
// Disk I/O rates (delta of /proc/[pid]/io read_bytes/write_bytes), and joins
// NVIDIA per-process GPU memory by PID. Each field degrades independently: a
// PID the agent lacks privilege to read (another user's PID under a --user
// service) degrades its disk/memory fields rather than the row. New PIDs with
// no prior sample report CPU as unavailable for one cycle, then fill next pass.
// Dead PIDs are pruned automatically because prevProc is rebuilt each pass.
func (c *systemCollector) collectProcesses(ctx context.Context) ProcessSet {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return unavailableProcessSet("read /proc: " + err.Error())
	}
	numCPU := runtime.NumCPU()
	if numCPU < 1 {
		numCPU = 1
	}
	now := time.Now()
	gpuMem := nvidiaProcessGPUMemory(ctx)

	current := make(map[int]procSample, len(entries))
	raw := make([]ProcessMetric, 0, 64)
	total := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, ok := parsePID(entry.Name())
		if !ok {
			continue
		}
		total++
		pr, readOK := readLinuxProcess(entry.Name())
		if !readOK {
			continue
		}
		pm := ProcessMetric{PID: pr.pid, Name: pr.name}
		if pr.rssAvail {
			pm.Memory = availableNumber(float64(pr.rssBytes), "B")
		} else {
			pm.Memory = unavailableNumber("B", "resident memory unavailable")
		}

		c.mu.Lock()
		prev, hadPrev := c.prevProc[pid]
		c.mu.Unlock()

		elapsed := now.Sub(prev.ts).Seconds()
		if hadPrev && elapsed > 0 {
			pm.CPU = processCPUPercent(pr.cpuSeconds, prev.cpuSeconds, elapsed, numCPU)
			pm.DiskRead = processDiskRate(pr.diskRead, prev.diskRead, pr.diskAvail, prev.diskAvail, elapsed)
			pm.DiskWrite = processDiskRate(pr.diskWrite, prev.diskWrite, pr.diskAvail, prev.diskAvail, elapsed)
		}
		if !pm.CPU.Available {
			pm.CPU = unavailableNumber("%", "process sampler is warming up")
		}
		if !pm.DiskRead.Available {
			pm.DiskRead = unavailableNumber("B/s", "insufficient privilege or warming up")
		}
		if !pm.DiskWrite.Available {
			pm.DiskWrite = unavailableNumber("B/s", "insufficient privilege or warming up")
		}

		if bytes, onGPU := gpuMem[pid]; onGPU {
			pm.GPUMemory = availableNumber(float64(bytes), "B")
		} else {
			pm.GPUMemory = unavailableNumber("B", "not a CUDA/compute process")
		}

		current[pid] = procSample{
			cpuSeconds: pr.cpuSeconds,
			ts:         now,
			diskRead:   pr.diskRead,
			diskWrite:  pr.diskWrite,
			diskAvail:  pr.diskAvail,
		}
		raw = append(raw, pm)
	}

	c.mu.Lock()
	c.prevProc = current
	c.mu.Unlock()
	return buildProcessSet(raw, total)
}

// linuxProcessRaw is the cumulative-counter snapshot read from /proc/[pid] for
// one PID, before delta math turns it into rates.
type linuxProcessRaw struct {
	pid        int
	name       string
	cpuSeconds float64
	rssBytes   uint64
	rssAvail   bool
	diskRead   uint64
	diskWrite  uint64
	diskAvail  bool
}

func parsePID(name string) (int, bool) {
	pid, err := strconv.Atoi(name)
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

func readLinuxProcess(pidStr string) (linuxProcessRaw, bool) {
	pid, ok := parsePID(pidStr)
	if !ok {
		return linuxProcessRaw{}, false
	}
	stat, err := os.ReadFile("/proc/" + pidStr + "/stat")
	if err != nil {
		return linuxProcessRaw{}, false
	}
	utime, stime, name, ok := parseLinuxProcStat(string(stat))
	if !ok {
		return linuxProcessRaw{}, false
	}
	pr := linuxProcessRaw{
		pid:        pid,
		name:       name,
		cpuSeconds: float64(utime+stime) / linuxClockTicksPerSecond,
	}
	pr.rssBytes, pr.rssAvail = readLinuxProcRSS(pidStr)
	pr.diskRead, pr.diskWrite, pr.diskAvail = readLinuxProcIO(pidStr)
	return pr, true
}

// parseLinuxProcStat extracts the process name (between the first '(' and the
// last ')', which may itself contain spaces/parens) plus the utime (field 14)
// and stime (field 15) jiffy counters from a /proc/[pid]/stat line. The fields
// after the comm are indexed relative to the slice following the last ')'.
func parseLinuxProcStat(stat string) (utime, stime uint64, name string, ok bool) {
	rp := strings.LastIndexByte(stat, ')')
	if rp < 0 {
		return 0, 0, "", false
	}
	lp := strings.IndexByte(stat, '(')
	if lp >= 0 && lp < rp {
		name = stat[lp+1 : rp]
	}
	rest := strings.Fields(stat[rp+1:])
	// state(0) ppid(1) pgrp(2) session(3) tty_nr(4) tpgid(5) flags(6)
	// minflt(7) cminflt(8) majflt(9) cmajflt(10) utime(11) stime(12)
	if len(rest) < 13 {
		return 0, 0, name, false
	}
	var err error
	utime, err = strconv.ParseUint(rest[11], 10, 64)
	if err != nil {
		return 0, 0, name, false
	}
	stime, err = strconv.ParseUint(rest[12], 10, 64)
	if err != nil {
		return 0, 0, name, false
	}
	return utime, stime, name, true
}

// readLinuxProcRSS returns the resident set size in bytes from the VmRSS line
// of /proc/[pid]/status. It degrades to unavailable when the file is absent or
// unreadable (e.g. another user's PID under a --user service).
func readLinuxProcRSS(pidStr string) (uint64, bool) {
	data, err := os.ReadFile("/proc/" + pidStr + "/status")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "VmRSS:" {
			continue
		}
		kib, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, false
		}
		bytes, ok := kibToBytes(kib)
		return bytes, ok
	}
	return 0, false
}

// readLinuxProcIO returns the cumulative read_bytes/write_bytes counters from
// /proc/[pid]/io. The file is root-only (0400), so an unprivileged reader gets
// EACCES for other users' PIDs; callers report disk I/O as unavailable for
// those rows instead of failing the whole set.
func readLinuxProcIO(pidStr string) (read, write uint64, ok bool) {
	data, err := os.ReadFile("/proc/" + pidStr + "/io")
	if err != nil {
		return 0, 0, false
	}
	gotR, gotW := false, false
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		val, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "read_bytes:":
			read = val
			gotR = true
		case "write_bytes:":
			write = val
			gotW = true
		}
	}
	return read, write, gotR && gotW
}

// processDiskRate turns two cumulative byte counters into a bytes/sec rate for
// one process. A counter reset (process restart) or missing privilege degrades
// to unavailable; warming up (no prior sample) degrades the same way for one
// cycle.
func processDiskRate(current, previous uint64, currentOK, previousOK bool, elapsedSeconds float64) NumberMetric {
	if elapsedSeconds <= 0 || !currentOK || !previousOK {
		return unavailableNumber("B/s", "process disk sampler is warming up")
	}
	if current < previous {
		return unavailableNumber("B/s", "process disk counter reset")
	}
	return availableNumber(float64(current-previous)/elapsedSeconds, "B/s")
}
