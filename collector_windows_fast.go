//go:build windows

package main

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Fast-lane Windows collectors. These read CPU load and physical memory through
// direct kernel32 syscalls (GetSystemTimes / GlobalMemoryStatusEx) instead of
// spawning PowerShell, so the resident sampler (see sampler.go) can refresh them
// at ~5 Hz with no process-spawn cost. The slow lane keeps the PowerShell/LHM
// path for everything else. The cold Collect() in collector_windows.go is left
// intact as the fallback used before the first sample and during -self-check.

var (
	procGetSystemTimes           = kernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx     = kernel32.NewProc("GlobalMemoryStatusEx")
	ntdll                        = syscall.NewLazyDLL("ntdll.dll")
	procNtQuerySystemInformation = ntdll.NewProc("NtQuerySystemInformation")
)

// cpuFastWarmup is the brief delay between the two GetSystemTimes reads on the
// very first fast pass (when there is no previous sample to delta against), so
// the first published snapshot already carries a real CPU figure. It mirrors the
// Linux fast lane's warmup read.
const cpuFastWarmup = 100 * time.Millisecond

// cpuFastSample holds the three cumulative 100 ns tick counters GetSystemTimes
// reports. kernel time already includes idle time, so busy = (kernel+user)-idle.
type cpuFastSample struct {
	idle   uint64
	kernel uint64
	user   uint64
}

// memoryStatusEx mirrors the Win32 MEMORYSTATUSEX struct passed to
// GlobalMemoryStatusEx. dwLength must be set to the struct size before the call.
type memoryStatusEx struct {
	dwLength                uint32
	dwMemoryLoad            uint32
	ullTotalPhys            uint64
	ullAvailPhys            uint64
	ullTotalPageFile        uint64
	ullAvailPageFile        uint64
	ullTotalVirtual         uint64
	ullAvailVirtual         uint64
	ullAvailExtendedVirtual uint64
}

// CollectFast gathers the syscall-only metrics (CPU load + physical memory) the
// dashboard animates live. It performs the reads here and returns a patch that
// assigns them onto the sampler's shared snapshot (see the laneCollector
// contract in sampler.go). A panic degrades both fields rather than crashing the
// background goroutine.
func (c *systemCollector) CollectFast(ctx context.Context) (patch func(*Metrics)) {
	defer func() {
		if r := recover(); r != nil {
			cpu := unavailableNumber("%", fmt.Sprintf("Windows CPU collector panicked: %v", r))
			cores := unavailableCPUCores(fmt.Sprintf("Windows CPU cores collector panicked: %v", r))
			memory := unavailableCapacity(fmt.Sprintf("Windows memory collector panicked: %v", r))
			patch = func(m *Metrics) {
				m.CPU = cpu
				m.CPUCores = cores
				m.Memory = memory
			}
		}
	}()

	cpu := c.windowsCPUFast(ctx)
	cores := c.windowsCPUCores(ctx)
	memory := windowsMemoryFast()
	return func(m *Metrics) {
		m.CPU = cpu
		m.CPUCores = cores
		m.Memory = memory
	}
}

// SystemProcessorPerformanceInformation is NtQuerySystemInformation class 8; it
// returns one record per logical processor with the same cumulative 100 ns
// counters GetSystemTimes exposes for the whole machine.
const systemProcessorPerformanceInformationClass = 8

// statusInfoLengthMismatch (STATUS_INFO_LENGTH_MISMATCH) means the supplied
// buffer was too small for all processor records; we grow and retry.
const statusInfoLengthMismatch = uintptr(0xC0000004)

// systemProcessorPerformanceInformation mirrors the Win32
// SYSTEM_PROCESSOR_PERFORMANCE_INFORMATION struct. KernelTime includes IdleTime,
// matching GetSystemTimes semantics, so the shared cpuUsagePercent delta math
// applies unchanged. The trailing pad keeps the struct at its native 48-byte
// size (8-byte aligned because of the int64 members).
type systemProcessorPerformanceInformation struct {
	IdleTime       int64
	KernelTime     int64
	UserTime       int64
	DpcTime        int64
	InterruptTime  int64
	InterruptCount uint32
	_              uint32
}

// windowsCPUCores reports per-logical-core utilization by deltaing the per-core
// NtQuerySystemInformation counters against the previous fast-lane sample. It
// mirrors windowsCPUFast/collectCPUCores: with no prior sample (first call) or a
// changed core count (CPU hot-plug) it takes a brief second read so the first
// snapshot still carries real values, and degrades the whole set to unavailable
// rather than returning a hard error.
func (c *systemCollector) windowsCPUCores(ctx context.Context) CPUCoreSet {
	cur, err := readPerCoreSystemTimes()
	if err != nil {
		return unavailableCPUCores(err.Error())
	}

	c.mu.Lock()
	prev := c.prevCPUCores
	c.prevCPUCores = cur
	c.mu.Unlock()

	if len(prev) == 0 || len(prev) != len(cur) {
		return c.windowsCPUCoresAfterDelay(ctx, cur, cpuFastWarmup)
	}
	return windowsPerCoreUsage(prev, cur)
}

func (c *systemCollector) windowsCPUCoresAfterDelay(ctx context.Context, prev []cpuFastSample, delay time.Duration) CPUCoreSet {
	if err := waitForSample(ctx, delay); err != nil {
		return unavailableCPUCores("per-core sampler canceled: " + err.Error())
	}
	later, err := readPerCoreSystemTimes()
	if err != nil {
		return unavailableCPUCores(err.Error())
	}
	c.mu.Lock()
	c.prevCPUCores = later
	c.mu.Unlock()
	if len(prev) == 0 || len(prev) != len(later) {
		return unavailableCPUCores("per-core sampler is warming up")
	}
	return windowsPerCoreUsage(prev, later)
}

// windowsPerCoreUsage turns two equal-length per-core samples into busy
// percentages (one per core, rounded to one decimal). A core whose counters did
// not advance is reported as idle rather than dropped, so Cores stays
// index-aligned with the logical core numbering.
func windowsPerCoreUsage(prev, cur []cpuFastSample) CPUCoreSet {
	cores := make([]float64, len(cur))
	for i := range cur {
		value, ok := cpuUsagePercent(prev[i], cur[i])
		if !ok {
			value = 0
		}
		cores[i] = round(value, 1)
	}
	return availableCPUCores(cores)
}

// readPerCoreSystemTimes returns one cpuFastSample per logical processor from
// NtQuerySystemInformation. It sizes the buffer from runtime.NumCPU() and grows
// on STATUS_INFO_LENGTH_MISMATCH (more processors than NumCPU reported, e.g.
// processor-group affinity), using the returned length to determine the real
// core count.
func readPerCoreSystemTimes() ([]cpuFastSample, error) {
	n := runtime.NumCPU()
	if n < 1 {
		n = 1
	}
	recordSize := int(unsafe.Sizeof(systemProcessorPerformanceInformation{}))
	for attempts := 0; attempts < 4; attempts++ {
		buf := make([]systemProcessorPerformanceInformation, n)
		var returnLen uint32
		status, _, _ := procNtQuerySystemInformation.Call(
			uintptr(systemProcessorPerformanceInformationClass),
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(len(buf))*unsafe.Sizeof(buf[0]),
			uintptr(unsafe.Pointer(&returnLen)),
		)
		if status == statusInfoLengthMismatch {
			needed := int(returnLen) / recordSize
			if needed <= n {
				needed = n * 2
			}
			n = needed
			continue
		}
		if status != 0 {
			return nil, fmt.Errorf("NtQuerySystemInformation(SystemProcessorPerformanceInformation) failed: 0x%X", status)
		}
		count := int(returnLen) / recordSize
		if count < 1 {
			return nil, fmt.Errorf("NtQuerySystemInformation returned no processor records")
		}
		if count > len(buf) {
			count = len(buf)
		}
		samples := make([]cpuFastSample, count)
		for i := 0; i < count; i++ {
			samples[i] = cpuFastSample{
				idle:   uint64(buf[i].IdleTime),
				kernel: uint64(buf[i].KernelTime),
				user:   uint64(buf[i].UserTime),
			}
		}
		return samples, nil
	}
	return nil, fmt.Errorf("NtQuerySystemInformation processor buffer kept growing")
}

// CollectSlow gathers the expensive metrics (platform string, CPU power/clock,
// PSU, disks, network, temperatures, GPU) the same way Collect() does, behind a
// concurrent fan-out and the shared LibreHardwareMonitor bridge. It returns a
// patch that assigns them onto the sampler's snapshot. CPU and memory are owned
// by the fast lane and deliberately left untouched here.
func (c *systemCollector) CollectSlow(ctx context.Context) (patch func(*Metrics)) {
	defer func() {
		if r := recover(); r != nil {
			patch = windowsDegradedSlowPatch(fmt.Sprintf("Windows slow collector panicked: %v", r))
		}
	}()

	platform := c.platformCache.Get(func() string {
		return windowsPlatform(ctx)
	})

	var wg sync.WaitGroup
	var cpuPower NumberMetric
	var cpuClock NumberMetric
	var cpuClockMax NumberMetric
	var psuOutputPower NumberMetric
	var swap CapacityMetric
	var disks []DiskMetric
	var network NetworkSet
	var temperatures TemperatureSet
	var gpu GPUSet
	var tailscale TailscaleStatus

	// One bridge invocation per slow pass feeds CPU power, clock, PSU and temps;
	// LHM's kernel driver does not tolerate concurrent Computer.Open() calls.
	bridgeResult, bridgeErr := c.fetchLhmBridge(ctx)
	collectMetricAsync(&wg, &cpuPower, func() NumberMetric {
		return windowsCPUPowerFromBridge(bridgeResult, bridgeErr)
	}, func(recovered any) NumberMetric {
		return unavailableNumber("W", fmt.Sprintf("Windows CPU power collector panicked: %v", recovered))
	})
	collectMetricAsync(&wg, &psuOutputPower, func() NumberMetric {
		return windowsPSUOutputPowerFromBridge(bridgeResult, bridgeErr)
	}, func(recovered any) NumberMetric {
		return unavailableNumber("W", fmt.Sprintf("Windows PSU output power collector panicked: %v", recovered))
	})
	collectMetricAsync(&wg, &cpuClock, func() NumberMetric {
		cur, mx := windowsCPUClocks(ctx, bridgeResult, bridgeErr)
		cpuClockMax = mx
		return cur
	}, func(recovered any) NumberMetric {
		cpuClockMax = unavailableNumber("MHz", fmt.Sprintf("Windows CPU clock collector panicked: %v", recovered))
		return unavailableNumber("MHz", fmt.Sprintf("Windows CPU clock collector panicked: %v", recovered))
	})
	collectMetricAsync(&wg, &disks, func() []DiskMetric {
		return windowsDisks(ctx)
	}, func(recovered any) []DiskMetric {
		return unavailableDisk(fmt.Sprintf("Windows disk collector panicked: %v", recovered))
	})
	collectMetricAsync(&wg, &network, func() NetworkSet {
		return c.windowsNetwork(ctx)
	}, func(recovered any) NetworkSet {
		return NetworkSet{Available: false, Error: fmt.Sprintf("Windows network collector panicked: %v", recovered)}
	})
	collectMetricAsync(&wg, &temperatures, func() TemperatureSet {
		return windowsTemperaturesFromBridge(ctx, bridgeResult, bridgeErr)
	}, func(recovered any) TemperatureSet {
		return TemperatureSet{Available: false, Error: fmt.Sprintf("Windows temperature collector panicked: %v", recovered)}
	})
	collectMetricAsync(&wg, &gpu, func() GPUSet {
		return c.windowsGPU(ctx)
	}, func(recovered any) GPUSet {
		return GPUSet{Available: false, Error: fmt.Sprintf("Windows GPU collector panicked: %v", recovered)}
	})
	collectMetricAsync(&wg, &tailscale, func() TailscaleStatus {
		return readTailscaleStatus(ctx)
	}, func(recovered any) TailscaleStatus {
		return TailscaleStatus{Available: false, Error: fmt.Sprintf("Windows Tailscale collector panicked: %v", recovered)}
	})
	collectMetricAsync(&wg, &swap, func() CapacityMetric {
		return windowsSwap(ctx)
	}, func(recovered any) CapacityMetric {
		return unavailableCapacity(fmt.Sprintf("Windows swap collector panicked: %v", recovered))
	})
	wg.Wait()

	cpuTemperature := pickCPUTemperature(temperatures)
	return func(m *Metrics) {
		m.Platform = platform
		m.CPUPower = cpuPower
		m.CPUClock = cpuClock
		m.CPUClockMax = cpuClockMax
		m.CPUTemperature = cpuTemperature
		m.PSUOutputPower = psuOutputPower
		m.MemorySwap = swap
		m.Disks = disks
		m.Network = network
		m.Tailscale = tailscale
		m.Temperatures = temperatures
		m.GPU = gpu
	}
}

// windowsDegradedSlowPatch marks every slow-lane field unavailable with the same
// message. It is the panic safety net for CollectSlow (per-metric panics are
// already recovered by collectMetricAsync; this covers a panic before the
// fan-out, e.g. in the bridge fetch or platform cache).
func windowsDegradedSlowPatch(message string) func(*Metrics) {
	return func(m *Metrics) {
		m.CPUPower = unavailableNumber("W", message)
		m.CPUClock = unavailableNumber("MHz", message)
		m.CPUClockMax = unavailableNumber("MHz", message)
		m.CPUTemperature = unavailableNumber("C", message)
		m.PSUOutputPower = unavailableNumber("W", message)
		m.MemorySwap = unavailableCapacity(message)
		m.Disks = unavailableDisk(message)
		m.Network = NetworkSet{Available: false, Error: message}
		m.Tailscale = TailscaleStatus{Available: false, Error: message}
		m.Temperatures = TemperatureSet{Available: false, Error: message}
		m.GPU = GPUSet{Available: false, Error: message}
	}
}

// windowsCPUFast reports total CPU utilization from GetSystemTimes. The counters
// are cumulative, so it deltas against the previous fast-lane sample. The very
// first call (no prior sample) takes a brief second read so the first snapshot
// is not blank.
func (c *systemCollector) windowsCPUFast(ctx context.Context) NumberMetric {
	cur, err := readSystemTimes()
	if err != nil {
		return unavailableNumber("%", err.Error())
	}

	c.mu.Lock()
	prev := c.prevCPUFast
	hasPrev := c.prevCPUFastSet
	c.prevCPUFast = cur
	c.prevCPUFastSet = true
	c.mu.Unlock()

	if hasPrev {
		if percent, ok := cpuUsagePercent(prev, cur); ok {
			return availableNumber(round(percent, 1), "%")
		}
		// No tick advanced between samples; fall through to a back-to-back read.
	}

	if err := waitForSample(ctx, cpuFastWarmup); err != nil {
		return unavailableNumber("%", "CPU sampler canceled: "+err.Error())
	}
	later, err := readSystemTimes()
	if err != nil {
		return unavailableNumber("%", err.Error())
	}
	c.mu.Lock()
	c.prevCPUFast = later
	c.prevCPUFastSet = true
	c.mu.Unlock()
	if percent, ok := cpuUsagePercent(cur, later); ok {
		return availableNumber(round(percent, 1), "%")
	}
	return unavailableNumber("%", "CPU times did not advance between samples")
}

// cpuUsagePercent converts two GetSystemTimes samples into a 0-100 utilization
// figure. kernel time includes idle, so the denominator is kernel+user and busy
// is that total minus idle.
func cpuUsagePercent(prev, cur cpuFastSample) (float64, bool) {
	idleDelta := cur.idle - prev.idle
	total := (cur.kernel - prev.kernel) + (cur.user - prev.user)
	if total == 0 {
		return 0, false
	}
	if idleDelta > total {
		idleDelta = total
	}
	percent := float64(total-idleDelta) / float64(total) * 100
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	return percent, true
}

func readSystemTimes() (cpuFastSample, error) {
	var idle, kernel, user syscall.Filetime
	r, _, errno := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	)
	if r == 0 {
		return cpuFastSample{}, fmt.Errorf("GetSystemTimes failed: %v", errno)
	}
	return cpuFastSample{
		idle:   filetimeTicks(idle),
		kernel: filetimeTicks(kernel),
		user:   filetimeTicks(user),
	}, nil
}

func filetimeTicks(ft syscall.Filetime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

// windowsMemoryFast reports physical memory usage from GlobalMemoryStatusEx,
// degrading to an unavailable capacity (never a hard error) on failure.
func windowsMemoryFast() CapacityMetric {
	var status memoryStatusEx
	status.dwLength = uint32(unsafe.Sizeof(status))
	r, _, errno := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&status)))
	if r == 0 {
		return unavailableCapacity(fmt.Sprintf("GlobalMemoryStatusEx failed: %v", errno))
	}
	return availableCapacityFromTotalFree(status.ullTotalPhys, status.ullAvailPhys, "invalid Windows memory counters")
}
