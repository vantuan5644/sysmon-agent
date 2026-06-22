//go:build windows

package main

import (
	"context"
	"fmt"
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
	procGetSystemTimes       = kernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
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
			memory := unavailableCapacity(fmt.Sprintf("Windows memory collector panicked: %v", r))
			patch = func(m *Metrics) {
				m.CPU = cpu
				m.Memory = memory
			}
		}
	}()

	cpu := c.windowsCPUFast(ctx)
	memory := windowsMemoryFast()
	return func(m *Metrics) {
		m.CPU = cpu
		m.Memory = memory
	}
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
	var disks []DiskMetric
	var network NetworkSet
	var temperatures TemperatureSet
	var gpu GPUSet

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
	wg.Wait()

	cpuTemperature := pickCPUTemperature(temperatures)
	return func(m *Metrics) {
		m.Platform = platform
		m.CPUPower = cpuPower
		m.CPUClock = cpuClock
		m.CPUClockMax = cpuClockMax
		m.CPUTemperature = cpuTemperature
		m.PSUOutputPower = psuOutputPower
		m.Disks = disks
		m.Network = network
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
		m.Disks = unavailableDisk(message)
		m.Network = NetworkSet{Available: false, Error: message}
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
