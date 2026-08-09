//go:build windows

package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed lhm-bridge.ps1
var lhmBridgeFS embed.FS

// lhmBridgePath resolves to a temp copy of the embedded LibreHardwareMonitor
// bridge script, created once per process so every metrics refresh reuses it.
var (
	lhmBridgeOnce sync.Once
	lhmBridgeFile string
	lhmBridgeErr  error
)

func lhmBridgePath() (string, error) {
	lhmBridgeOnce.Do(func() {
		src, err := lhmBridgeFS.ReadFile("lhm-bridge.ps1")
		if err != nil {
			lhmBridgeErr = fmt.Errorf("read embedded lhm-bridge.ps1: %w", err)
			return
		}
		lhmBridgeFile = filepath.Join(os.TempDir(), "sysmon-lhm-bridge.ps1")
		if err := os.WriteFile(lhmBridgeFile, src, 0o644); err != nil {
			lhmBridgeErr = fmt.Errorf("write lhm-bridge temp copy: %w", err)
			return
		}
	})
	return lhmBridgeFile, lhmBridgeErr
}

type systemCollector struct {
	hostname          string
	platformCache     nonEmptyStringCache
	gpuFallbackMu     sync.Mutex
	gpuFallback       GPUSet
	gpuFallbackCached bool
	lhmMu             sync.Mutex
	lhmBridgeCached   bool
	lhmBridgeResult   lhmBridgeResult
	lhmBridgeErr      error
	lhmBridgeAt       time.Time
	// useDaemon selects the persistent LibreHardwareMonitor bridge daemon over
	// the one-shot bridge. It is set by EnablePersistentBridge() when the
	// sampler starts (and SYSMON_LHM_DAEMON is not explicitly disabled), so
	// one-shot modes such as -self-check - which never start the sampler - keep
	// using the per-sample bridge and spawn nothing long-lived. Guarded by
	// lhmMu alongside the cache fields below it.
	useDaemon      bool
	daemon         *lhmDaemon
	mu             sync.Mutex
	prevNet        map[string]netCounter
	prevNetAt      time.Time
	prevCPUFast    cpuFastSample
	prevCPUFastSet bool
	prevCPUCores   []cpuFastSample
	// prevProc holds the previous per-PID cumulative CPU-second counters used to
	// derive per-process CPU% as a delta. It is replaced wholesale each slow
	// pass, so dead PIDs are pruned automatically. Guarded by mu.
	prevProc map[int]procSample
	// hardwareOnce resolves the static identity strings (CPU model, RAM
	// type/speed/channel) exactly once -- they never change at runtime and the
	// lookups spawn CIM queries, so this keeps the cost off every slow pass.
	hardwareOnce sync.Once
	cpuName      string
	memoryName   string
	// uplink caches the active default-route identity (Wi-Fi SSID / wired link)
	// behind uplinkCacheTTL so the slow lane does not spawn Get-NetRoute/netsh
	// every pass. Guarded by mu.
	uplink   NetworkUplink
	uplinkAt time.Time
	// clockPeak ratchets the observed CPU boost ceiling reported as
	// cpu_clock_max, because Win32_Processor.MaxClockSpeed reports only the rated
	// base clock and Windows exposes no boost figure. Shared, untagged
	// implementation so the field means the same thing here as on Linux. Carries
	// its own lock (NOT mu).
	clockPeak cpuClockPeakTracker
}

type netCounter struct {
	rxBytes uint64
	txBytes uint64
}

// procSample holds the previous cumulative TotalProcessorTime (seconds) for one
// PID so the slow lane can derive per-process CPU% as a delta between passes.
// Disk I/O on Windows comes straight from the Get-Counter rate counter (no
// delta needed), so no disk counters are stored here.
type procSample struct {
	cpuSeconds float64
	ts         time.Time
}

func NewSystemCollector() MetricsCollector {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	return &systemCollector{
		hostname:  hostname,
		prevNet:   map[string]netCounter{},
		prevNetAt: time.Now(),
		prevProc:  map[int]procSample{},
	}
}

// Hostname exposes the resolved hostname so the sampler's warming snapshot can
// carry it before the first lane pass (see collectorHostname in sampler.go).
func (c *systemCollector) Hostname() string { return c.hostname }

// EnablePersistentBridge switches the LibreHardwareMonitor path from the
// per-sample one-shot bridge to the long-lived daemon (see lhm_bridge_windows.go).
// It is gated on SYSMON_LHM_DAEMON (default on) and is called by the sampler on
// Start(), so -self-check - which never starts the sampler - keeps the one-shot
// path and spawns no daemon.
func (c *systemCollector) EnablePersistentBridge() {
	if !envBool("SYSMON_LHM_DAEMON", true) {
		return
	}
	c.lhmMu.Lock()
	c.useDaemon = true
	c.lhmMu.Unlock()
}

// Close tears down any persistent resource owned by the collector - the LHM
// daemon on Windows. It implements io.Closer so the sampler's Stop() closes the
// inner collector on graceful shutdown, sending stdin EOF (clean pwsh exit) and
// best-effort-killing any lingering process so a service stop leaves no orphan.
func (c *systemCollector) Close() error {
	c.lhmMu.Lock()
	daemon := c.daemon
	c.lhmMu.Unlock()
	if daemon == nil {
		return nil
	}
	return daemon.Close()
}

func (c *systemCollector) Collect(ctx context.Context) (Metrics, error) {
	started := time.Now()
	metrics := baseMetrics(c.hostname)
	metrics.Platform = c.platformCache.Get(func() string {
		return windowsPlatform(ctx)
	})
	metrics.CPUName, metrics.MemoryName = c.resolveHardwareNames(ctx)

	var wg sync.WaitGroup
	var cpu NumberMetric
	var cpuPower NumberMetric
	var cpuCorePower NumberMetric
	var cpuSocPower NumberMetric
	var cpuMiscPower NumberMetric
	var cpuClocks cpuClockSet
	var psuOutputPower NumberMetric
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
		return windowsCPU(ctx)
	}, func(recovered any) NumberMetric {
		return unavailableNumber("%", fmt.Sprintf("Windows CPU collector panicked: %v", recovered))
	})
	// LibreHardwareMonitor's kernel driver does not tolerate concurrent
	// Computer.Open() calls, so the bridge runs once per sample behind a
	// collector-scoped mutex and feeds both CPU power and temperatures.
	bridgeResult, bridgeErr := c.fetchLhmBridge(ctx)
	collectMetricAsync(&wg, &cpuPower, func() NumberMetric {
		return windowsCPUPowerFromBridge(bridgeResult, bridgeErr)
	}, func(recovered any) NumberMetric {
		return unavailableNumber("W", fmt.Sprintf("Windows CPU power collector panicked: %v", recovered))
	})
	collectMetricAsync(&wg, &cpuCorePower, func() NumberMetric {
		core, soc, misc := windowsCPURailPowerFromBridge(bridgeResult, bridgeErr)
		cpuSocPower = soc
		cpuMiscPower = misc
		return core
	}, func(recovered any) NumberMetric {
		message := fmt.Sprintf("Windows CPU rail power collector panicked: %v", recovered)
		cpuSocPower = unavailableNumber("W", message)
		cpuMiscPower = unavailableNumber("W", message)
		return unavailableNumber("W", message)
	})
	collectMetricAsync(&wg, &psuOutputPower, func() NumberMetric {
		return windowsPSUOutputPowerFromBridge(bridgeResult, bridgeErr)
	}, func(recovered any) NumberMetric {
		return unavailableNumber("W", fmt.Sprintf("Windows PSU output power collector panicked: %v", recovered))
	})
	collectMetricAsync(&wg, &cpuClocks, func() cpuClockSet {
		return c.windowsCPUClockSet(ctx, bridgeResult, bridgeErr)
	}, func(recovered any) cpuClockSet {
		return degradedCPUClockSet(fmt.Sprintf("Windows CPU clock collector panicked: %v", recovered))
	})
	collectMetricAsync(&wg, &memory, func() CapacityMetric {
		return windowsMemory(ctx)
	}, func(recovered any) CapacityMetric {
		return unavailableCapacity(fmt.Sprintf("Windows memory collector panicked: %v", recovered))
	})
	collectMetricAsync(&wg, &swap, func() CapacityMetric {
		return windowsSwap(ctx)
	}, func(recovered any) CapacityMetric {
		return unavailableCapacity(fmt.Sprintf("Windows swap collector panicked: %v", recovered))
	})
	collectMetricAsync(&wg, &disks, func() []DiskMetric {
		return windowsDisks(ctx)
	}, func(recovered any) []DiskMetric {
		return unavailableDisk(fmt.Sprintf("Windows disk collector panicked: %v", recovered))
	})
	collectMetricAsync(&wg, &storage, func() StorageSet {
		return windowsStorage(ctx, bridgeResult, bridgeErr)
	}, func(recovered any) StorageSet {
		return unavailableStorage(fmt.Sprintf("Windows storage collector panicked: %v", recovered))
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
	collectMetricAsync(&wg, &processes, func() ProcessSet {
		return c.collectProcesses(ctx)
	}, func(recovered any) ProcessSet {
		return unavailableProcessSet(fmt.Sprintf("Windows process collector panicked: %v", recovered))
	})
	wg.Wait()

	metrics.CPU = cpu
	metrics.CPUCores = c.windowsCPUCores(ctx)
	metrics.CPUPower = cpuPower
	metrics.CPUCorePower = cpuCorePower
	metrics.CPUSocPower = cpuSocPower
	metrics.CPUMiscPower = cpuMiscPower
	cpuClocks.applyTo(&metrics)
	metrics.CPUTemperature, metrics.CPUTemperatureSensor = pickCPUTemperatureSensor(temperatures)
	metrics.PSUOutputPower = psuOutputPower
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

func windowsPlatform(ctx context.Context) string {
	var result struct {
		Caption string
		Version string
	}
	err := runPowerShellJSON(ctx, `Get-CimInstance Win32_OperatingSystem | Select-Object Caption,Version`, &result)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(result.Caption + " " + result.Version)
}

func windowsCPU(ctx context.Context) NumberMetric {
	var result struct {
		Average *float64
	}
	err := runPowerShellJSON(ctx, `Get-CimInstance Win32_Processor | Measure-Object -Property LoadPercentage -Average | Select-Object Average`, &result)
	if err != nil {
		return unavailableNumber("%", err.Error())
	}
	return windowsCPULoadMetric(result.Average)
}

// windowsCPUClockSet resolves the full clock set. The live clocks come from the
// LibreHardwareMonitor bridge when available, because
// Win32_Processor.CurrentClockSpeed is NOT a live frequency: Windows refreshes
// it rarely (often only at boot) and many systems just echo the rated speed, so
// it pins at a constant value and never reflects idle/boost. LHM reads the
// per-core MSRs each sample, so it tracks the real frequency, and WMI
// CurrentClockSpeed is the fallback on hosts without it.
//
// Win32_Processor.MaxClockSpeed is the rated BASE clock on modern CPUs (4500 on
// a 7950X), not the turbo ceiling, so it is reported as Base. Windows exposes no
// declared boost figure at all, hence Rated is permanently unavailable here --
// unlike Linux, which has cpufreq's (over-optimistic) cpuinfo_max_freq. Max is
// the peak-hold ceiling, ratcheted from the per-core peak.
func (c *systemCollector) windowsCPUClockSet(ctx context.Context, bridge lhmBridgeResult, bridgeErr error) cpuClockSet {
	var result struct {
		CurrentClockSpeed *float64
		MaxClockSpeed     *float64
	}
	err := runPowerShellJSON(ctx, `Get-CimInstance Win32_Processor | Select-Object -First 1 CurrentClockSpeed,MaxClockSpeed`, &result)

	var base NumberMetric
	var baseMHz float64
	var baseOK bool
	if err != nil {
		base = unavailableNumber("MHz", err.Error())
	} else if value, ok := windowsClockMHz(result.MaxClockSpeed); ok {
		baseMHz, baseOK = value, true
		base = availableNumber(value, "MHz")
	} else {
		base = unavailableNumber("MHz", "Win32_Processor did not report MaxClockSpeed")
	}

	// Resolve the live current clock: prefer the LibreHardwareMonitor core clock,
	// fall back to the (static) WMI CurrentClockSpeed.
	var current NumberMetric
	peakCore := unavailableNumber("MHz", "LibreHardwareMonitor did not report per-core CPU clocks")
	if bridgeErr == nil && bridge.Available {
		if metric := lhmClockMetric(bridge.CPUClock); metric.Available {
			current = metric
		}
		if metric := lhmClockMetric(bridge.CPUClockPeakCore); metric.Available {
			peakCore = metric
		}
	}
	if !current.Available {
		if err != nil {
			current = unavailableNumber("MHz", err.Error())
		} else if value, ok := windowsClockMHz(result.CurrentClockSpeed); ok {
			current = availableNumber(value, "MHz")
		} else {
			current = unavailableNumber("MHz", "Win32_Processor CurrentClockSpeed is not a live frequency; install LibreHardwareMonitor for live CPU clock")
		}
	}

	return cpuClockSet{
		Current:  current,
		PeakCore: peakCore,
		Max:      c.clockPeak.observe(peakCore, current, baseMHz, baseOK),
		Rated:    unavailableNumber("MHz", "Windows exposes no declared CPU boost clock (Win32_Processor.MaxClockSpeed is the base clock)"),
		Base:     base,
	}
}

func windowsClockMHz(value *float64) (float64, bool) {
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || *value <= 0 {
		return 0, false
	}
	return *value, true
}

func windowsMemory(ctx context.Context) CapacityMetric {
	var result struct {
		TotalVisibleMemorySize uint64
		FreePhysicalMemory     uint64
	}
	err := runPowerShellJSON(ctx, `Get-CimInstance Win32_OperatingSystem | Select-Object TotalVisibleMemorySize,FreePhysicalMemory`, &result)
	if err != nil {
		return unavailableCapacity(err.Error())
	}
	return windowsMemoryCapacity(result.TotalVisibleMemorySize, result.FreePhysicalMemory)
}

// windowsSwap reports pagefile (the Windows analog of swap) usage from
// Win32_PageFileUsage, summing AllocatedBaseSize/CurrentUsage (MiB) across all
// page files. A host with no page file reports unavailable so the dashboard
// shows a graceful "no swap". It runs on the slow lane because it spawns a
// PowerShell/CIM query.
func windowsSwap(ctx context.Context) CapacityMetric {
	var rows []struct {
		AllocatedBaseSize uint64
		CurrentUsage      uint64
	}
	err := runPowerShellJSONArray(ctx, `Get-CimInstance Win32_PageFileUsage | Select-Object AllocatedBaseSize,CurrentUsage`, &rows)
	if err != nil {
		return unavailableCapacity(err.Error())
	}
	var allocated, current uint64
	for _, row := range rows {
		allocated += row.AllocatedBaseSize
		current += row.CurrentUsage
	}
	if allocated == 0 {
		return unavailableCapacity("no swap configured")
	}
	if current > allocated {
		current = allocated
	}
	const mib = 1024 * 1024
	return availableCapacity(current*mib, allocated*mib)
}

func windowsDisks(ctx context.Context) []DiskMetric {
	var rows []struct {
		DeviceID   string
		VolumeName string
		FileSystem string
		Size       uint64
		FreeSpace  uint64
	}
	err := runPowerShellJSONArray(ctx, `Get-CimInstance Win32_LogicalDisk -Filter "DriveType=3" | Select-Object DeviceID,VolumeName,FileSystem,Size,FreeSpace`, &rows)
	if err != nil {
		return unavailableDisk(err.Error())
	}
	disks := make([]DiskMetric, 0, len(rows))
	for _, row := range rows {
		name := row.DeviceID
		if row.VolumeName != "" {
			name = fmt.Sprintf("%s %s", row.DeviceID, row.VolumeName)
		}
		disks = append(disks, DiskMetric{
			Name:       name,
			Mountpoint: row.DeviceID,
			FSType:     strings.ToLower(row.FileSystem),
			Capacity:   availableCapacityFromTotalFree(row.Size, row.FreeSpace, "invalid Windows disk capacity counters"),
		})
	}
	sort.Slice(disks, func(i, j int) bool { return disks[i].Mountpoint < disks[j].Mountpoint })
	return ensureDiskMetrics(disks, "no fixed disks found")
}

// windowsPhysicalDisk is the JSON shape emitted by the MSFT_PhysicalDisk +
// MSFT_Partition/MSFT_Volume join below: one entry per physical drive with its
// model, physical size, and aggregated used/total over the drive-lettered
// volumes that live on it.
type windowsPhysicalDisk struct {
	Name        string   `json:"name"`
	Model       string   `json:"model"`
	SizeBytes   uint64   `json:"size_bytes"`
	UsedBytes   uint64   `json:"used_bytes"`
	TotalBytes  uint64   `json:"total_bytes"`
	Mountpoints []string `json:"mountpoints"`
}

// windowsStorage builds the per-physical-drive storage set on Windows. Capacity
// comes from a Get-CimInstance MSFT_PhysicalDisk query joined to volumes via
// MSFT_Partition (root/Microsoft/Windows/Storage); temperature is matched from
// the LibreHardwareMonitor bridge by FriendlyName (the bridge harvest loop is
// generic over whatever IsStorageEnabled emits). Any failure degrades the whole
// set to unavailable rather than failing the response. Mirrors the Linux
// collectStorage shape so the dashboard panel is identical.
func windowsStorage(ctx context.Context, bridge lhmBridgeResult, bridgeErr error) StorageSet {
	// Join physical disks to their drive-lettered volumes and aggregate capacity.
	// -ErrorAction Stop on the CIM queries makes a missing namespace / cmdlet
	// surface as a non-zero exit, which runPowerShellJSONArray turns into a Go
	// error so the whole set degrades cleanly.
	const script = `$ns='root/Microsoft/Windows/Storage'; ` +
		`$disks=@(Get-CimInstance -Namespace $ns -ClassName MSFT_PhysicalDisk -ErrorAction Stop); ` +
		`$parts=@(Get-CimInstance -Namespace $ns -ClassName MSFT_Partition -ErrorAction Stop); ` +
		`foreach ($d in $disks) { ` +
		`$dp=@($parts | Where-Object { [uint32]$_.DiskNumber -eq [uint32]$d.DeviceId }); ` +
		`$u=[uint64]0; $t=[uint64]0; $m=@(); ` +
		`foreach ($p in $dp) { ` +
		`$l=$p.DriveLetter; ` +
		`if($l){ $v=Get-Volume -DriveLetter $l -ErrorAction SilentlyContinue; ` +
		`if($v -and $v.Size -gt 0){ $u+=[uint64]$v.Size-[uint64]$v.SizeRemaining; $t+=[uint64]$v.Size; $m+=($l.ToString()+':') } } ` +
		`} ` +
		`[ordered]@{name=('PhysicalDrive'+$d.DeviceId);model=($d.FriendlyName);size_bytes=([uint64]$d.Size);used_bytes=$u;total_bytes=$t;mountpoints=$m} ` +
		`}`
	var rows []windowsPhysicalDisk
	if err := runPowerShellJSONArray(ctx, script, &rows); err != nil {
		return unavailableStorage(err.Error())
	}
	if len(rows) == 0 {
		return unavailableStorage("no physical disks found")
	}
	devices := make([]StorageDevice, 0, len(rows))
	for _, row := range rows {
		var capacity CapacityMetric
		if row.TotalBytes > 0 {
			capacity = availableCapacity(row.UsedBytes, row.TotalBytes)
		} else {
			capacity = unavailableCapacity("no mounted filesystems")
		}
		devices = append(devices, StorageDevice{
			Name:        row.Name,
			Model:       strings.TrimSpace(row.Model),
			SizeBytes:   row.SizeBytes,
			Mountpoints: row.Mountpoints,
			Capacity:    capacity,
			Temperature: windowsStorageTemperatureForModel(row.Model, bridge, bridgeErr),
		})
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].Name < devices[j].Name })
	return StorageSet{Available: true, Devices: devices}
}

// windowsStorageTemperatureForModel matches one physical disk's temperature
// from the LibreHardwareMonitor bridge by FriendlyName. LHM names storage
// sensors "<FriendlyName> Temperature", so a case-insensitive substring match
// on the model is specific enough (no CPU/GPU/board temp carries a drive model).
// Returns unavailable (never an error) when the bridge is down or no match.
func windowsStorageTemperatureForModel(model string, bridge lhmBridgeResult, bridgeErr error) NumberMetric {
	if bridgeErr != nil || !bridge.Available {
		return unavailableNumber("C", "storage temperature unavailable")
	}
	needle := strings.ToLower(strings.TrimSpace(model))
	if needle == "" {
		return unavailableNumber("C", "no storage temperature sensor reported")
	}
	for _, temp := range bridge.Temperatures {
		name := strings.ToLower(temp.Name)
		if !strings.Contains(name, needle) {
			continue
		}
		if !isFinite(temp.Value) || temp.Value < -50 || temp.Value > 150 {
			continue
		}
		return availableNumber(temp.Value, "C")
	}
	return unavailableNumber("C", "no storage temperature sensor reported")
}

// windowsNetwork builds the per-interface throughput set and attaches the
// active-network identity (Wi-Fi SSID / wired link) for the NET card. It mirrors
// the Linux collectNetwork wrapper.
func (c *systemCollector) windowsNetwork(ctx context.Context) NetworkSet {
	set := c.windowsNetworkRates(ctx)
	set.Uplink = c.collectNetworkUplink(ctx)
	return set
}

func (c *systemCollector) windowsNetworkRates(ctx context.Context) NetworkSet {
	nowCounters, err := windowsNetCounters(ctx)
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
		return c.sampleWindowsNetworkAfterDelay(ctx, nowCounters, now, 250*time.Millisecond)
	}
	return buildWindowsNetworkSet(prevCounters, nowCounters, elapsed)
}

func (c *systemCollector) sampleWindowsNetworkAfterDelay(ctx context.Context, previous map[string]netCounter, previousAt time.Time, delay time.Duration) NetworkSet {
	if err := waitForSample(ctx, delay); err != nil {
		return NetworkSet{Available: false, Error: "network sampler canceled: " + err.Error()}
	}
	later, err := windowsNetCounters(ctx)
	if err != nil {
		return NetworkSet{Available: false, Error: err.Error()}
	}
	laterAt := time.Now()

	c.mu.Lock()
	c.prevNet = later
	c.prevNetAt = laterAt
	c.mu.Unlock()

	return buildWindowsNetworkSet(previous, later, networkSampleElapsedSeconds(previousAt, laterAt, delay))
}

func buildWindowsNetworkSet(prevCounters, nowCounters map[string]netCounter, elapsed float64) NetworkSet {
	names := make([]string, 0, len(nowCounters))
	for name := range nowCounters {
		if shouldIncludeWindowsNetworkInterface(name) {
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
		return NetworkSet{Available: false, Error: "no non-virtual network adapter statistics found"}
	}
	sortNetworkInterfacesByActivity(interfaces)
	return NetworkSet{Available: true, Interfaces: interfaces}
}

func windowsNetCounters(ctx context.Context) (map[string]netCounter, error) {
	var rows []struct {
		Name          string
		ReceivedBytes uint64
		SentBytes     uint64
	}
	err := runPowerShellJSONArray(ctx, `Get-NetAdapterStatistics | Select-Object Name,ReceivedBytes,SentBytes`, &rows)
	if err != nil {
		return nil, err
	}
	counters := map[string]netCounter{}
	for _, row := range rows {
		if row.Name == "" {
			continue
		}
		counters[row.Name] = netCounter{rxBytes: row.ReceivedBytes, txBytes: row.SentBytes}
	}
	return counters, nil
}

// collectProcesses builds the per-process set from a single Get-Process call
// (Id, ProcessName, WorkingSet64, and a computed CpuSeconds) plus a Get-Counter
// call for per-process disk I/O rates, and joins NVIDIA per-process GPU memory
// by PID. Per-process CPU% is derived as a delta of cumulative processor time
// against the previous slow pass, whole-host normalized by the core count so
// values are comparable to the aggregated CPU gauge. Each field degrades
// independently: a row whose disk instance could not be resolved reports disk
// unavailable rather than failing the response. Dead PIDs are pruned
// automatically because prevProc is rebuilt each pass.
func (c *systemCollector) collectProcesses(ctx context.Context) ProcessSet {
	numCPU := runtime.NumCPU()
	if numCPU < 1 {
		numCPU = 1
	}
	now := time.Now()
	gpuMem := nvidiaProcessGPUMemory(ctx)

	var rows []windowsProcessRow
	if err := runPowerShellJSONArray(ctx, `Get-Process | Select-Object Id,ProcessName,WorkingSet64,@{Name='CpuSeconds';Expression={[double]$_.TotalProcessorTime.TotalSeconds}}`, &rows); err != nil {
		return unavailableProcessSet("Get-Process failed: " + err.Error())
	}
	diskReadByPID, diskWriteByPID := windowsProcessDiskRates(ctx)

	current := make(map[int]procSample, len(rows))
	raw := make([]ProcessMetric, 0, len(rows))
	for _, row := range rows {
		if row.ID <= 0 {
			continue
		}
		pm := ProcessMetric{PID: row.ID, Name: windowsProcessName(row)}
		pm.Memory = availableNumber(float64(row.WorkingSet64), "B")
		cpuSeconds := windowsProcessorTimeSeconds(row.CpuSeconds)

		c.mu.Lock()
		prev, hadPrev := c.prevProc[row.ID]
		c.mu.Unlock()
		if hadPrev {
			elapsed := now.Sub(prev.ts).Seconds()
			pm.CPU = processCPUPercent(cpuSeconds, prev.cpuSeconds, elapsed, numCPU)
		}
		if !pm.CPU.Available {
			pm.CPU = unavailableNumber("%", "process sampler is warming up")
		}

		pm.DiskRead = windowsProcessRate(diskReadByPID, row.ID)
		pm.DiskWrite = windowsProcessRate(diskWriteByPID, row.ID)

		if bytes, onGPU := gpuMem[row.ID]; onGPU {
			pm.GPUMemory = availableNumber(float64(bytes), "B")
		} else {
			pm.GPUMemory = unavailableNumber("B", "not a CUDA/compute process")
		}

		current[row.ID] = procSample{cpuSeconds: cpuSeconds, ts: now}
		raw = append(raw, pm)
	}

	c.mu.Lock()
	c.prevProc = current
	c.mu.Unlock()
	return buildProcessSet(raw, len(raw))
}

// windowsProcessRow models one Get-Process result. CpuSeconds is computed in
// PowerShell ([double]$_.TotalProcessorTime.TotalSeconds) so the JSON carries a
// plain number rather than a TimeSpan object/string, which ConvertTo-Json does
// not serialize predictably. WorkingSet64 is the resident set in bytes.
type windowsProcessRow struct {
	ID           int     `json:"Id"`
	ProcessName  string  `json:"ProcessName"`
	WorkingSet64 uint64  `json:"WorkingSet64"`
	CpuSeconds   float64 `json:"CpuSeconds"`
}

func windowsProcessName(row windowsProcessRow) string {
	name := strings.TrimSpace(row.ProcessName)
	if name == "" {
		return fmt.Sprintf("pid %d", row.ID)
	}
	return name
}

// windowsProcessorTimeSeconds returns the cumulative processor time for a PID
// as seconds, guarded against non-finite/negative garbage. The value is
// computed in PowerShell from TotalProcessorTime.TotalSeconds.
func windowsProcessorTimeSeconds(seconds float64) float64 {
	if !isFinite(seconds) || seconds < 0 {
		return 0
	}
	return seconds
}

// windowsProcessDiskRates queries the per-process IO Read/Write Bytes/sec rate
// counters and resolves each instance back to a PID via the ID Process counter
// (Get-Counter names instances like "chrome#1"; the ID Process counter is the
// authoritative PID mapping). Returns pid -> bytes/sec maps; PIDs without a
// resolved counter are absent and degrade to unavailable per row. The counter
// is combined disk+file+device I/O (an approximation of "disk"), since a
// pure-disk per-process number is ETW-only.
func windowsProcessDiskRates(ctx context.Context) (readByPID, writeByPID map[int]float64) {
	readByPID = map[int]float64{}
	writeByPID = map[int]float64{}
	var rows []windowsProcessIOCounter
	script := `Get-Counter '\Process(*)\IO Read Bytes/sec','\Process(*)\IO Write Bytes/sec','\Process(*)\ID Process' -ErrorAction SilentlyContinue | Select-Object -ExpandProperty CounterSamples | Select-Object Path,CookedValue`
	if err := runPowerShellJSONArray(ctx, script, &rows); err != nil {
		return readByPID, writeByPID
	}
	pidsByInstance := map[string]int{}
	readByInstance := map[string]float64{}
	writeByInstance := map[string]float64{}
	for _, row := range rows {
		value, ok := windowsProcessIOValue(row.CookedValue)
		if !ok {
			continue
		}
		switch windowsProcessIOKind(row.Path) {
		case "id":
			pidsByInstance[windowsProcessIOInstance(row.Path)] = int(value)
		case "read":
			readByInstance[windowsProcessIOInstance(row.Path)] = value
		case "write":
			writeByInstance[windowsProcessIOInstance(row.Path)] = value
		}
	}
	for instance, pid := range pidsByInstance {
		if pid <= 0 {
			continue
		}
		if v, ok := readByInstance[instance]; ok {
			readByPID[pid] = v
		}
		if v, ok := writeByInstance[instance]; ok {
			writeByPID[pid] = v
		}
	}
	return readByPID, writeByPID
}

type windowsProcessIOCounter struct {
	Path        string   `json:"Path"`
	CookedValue *float64 `json:"CookedValue"`
}

func windowsProcessIOValue(value *float64) (float64, bool) {
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 {
		return 0, false
	}
	return *value, true
}

// windowsProcessIOKind classifies a Get-Counter sample path as the read, write,
// or ID Process counter, so the three queries can be resolved back to per-PID
// rates. Counter paths look like "\\\\computer\\process(...)\\io read bytes/sec".
func windowsProcessIOKind(path string) string {
	p := strings.ToLower(path)
	switch {
	case strings.Contains(p, "id process"):
		return "id"
	case strings.Contains(p, "io read bytes"):
		return "read"
	case strings.Contains(p, "io write bytes"):
		return "write"
	}
	return ""
}

// windowsProcessIOInstance extracts the bare instance name (the parenthesised
// token) from a Get-Counter sample path, lowercased so read/write/id samples
// for the same instance match. The instance is everything between the last
// "process(" and the following ")".
func windowsProcessIOInstance(path string) string {
	p := strings.ToLower(path)
	start := strings.LastIndex(p, "process(")
	if start < 0 {
		return ""
	}
	rest := p[start+len("process("):]
	end := strings.IndexByte(rest, ')')
	if end < 0 {
		return rest
	}
	return rest[:end]
}

// windowsProcessRate turns a per-PID bytes/sec map entry into a NumberMetric,
// degrading to unavailable when the PID had no resolved counter (the _Total
// instance and idle processes have none).
func windowsProcessRate(rates map[int]float64, pid int) NumberMetric {
	value, ok := rates[pid]
	if !ok || !isFinite(value) {
		return unavailableNumber("B/s", "no disk I/O counter for this process")
	}
	return availableNumber(value, "B/s")
}

// windowsCPUPower reads CPU package power from the LibreHardwareMonitor WMI
// provider (root/LibreHardwareMonitor) when it is installed. Without that
// vendor bridge, Windows exposes no reliable CPU power counter to CIM, so the
// metric degrades to an explicit unavailable field instead of failing the
// whole metrics response. It prefers a single "Package" sensor to avoid
// double-counting the per-core sensors that are a subset of package power.
// lhmBridgeResult models the JSON object emitted by lhm-bridge.ps1. It is the
// single source for CPU package power and board/CPU/GPU temperatures on Windows
// hosts with LibreHardwareMonitor installed, because modern LHM builds no longer
// publish a root/LibreHardwareMonitor WMI namespace.
type lhmBridgeResult struct {
	Available bool            `json:"available"`
	Error     string          `json:"error,omitempty"`
	Power     *lhmBridgeValue `json:"power,omitempty"`
	// AMD SMU per-rail breakdown of Power. Only Zen parts whose SMU PM-table
	// version LibreHardwareMonitor has a sensor map for report these, so each is
	// absent (nil) on Intel and on unmapped AMD parts.
	CPUCorePower *lhmBridgeValue `json:"cpu_core_power,omitempty"`
	CPUSocPower  *lhmBridgeValue `json:"cpu_soc_power,omitempty"`
	CPUMiscPower *lhmBridgeValue `json:"cpu_misc_power,omitempty"`
	// CPUClock is the mean across cores; CPUClockPeakCore is the single fastest
	// core in the same sample. The mean on a many-core part sits ~1 GHz below the
	// boosting core, so only the peak can answer "does this chip reach its rated
	// boost?" -- and only the peak is a sane thing to ratchet the ceiling with.
	// Absent (nil) when the bridge is older than the agent, which degrades
	// cpu_clock_peak_core rather than the whole clock set.
	CPUClock         *lhmBridgeValue `json:"cpu_clock,omitempty"`
	CPUClockPeakCore *lhmBridgeValue `json:"cpu_clock_peak_core,omitempty"`
	PSUOutputPower   *lhmBridgeValue `json:"psu_output_power,omitempty"`
	Temperatures     []lhmBridgeTemp `json:"temperatures,omitempty"`
}

type lhmBridgeValue struct {
	Available bool    `json:"available"`
	Value     float64 `json:"value"`
}

type lhmBridgeTemp struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
}

// runLhmBridge invokes the embedded LibreHardwareMonitor bridge script. The
// bridge requires PowerShell 7+ because modern LibreHardwareMonitorLib.dll is
// built for .NET and cannot be loaded by Windows PowerShell 5.1, so the bridge
// host prefers pwsh and falls back to powershell.exe only as a last resort.
//
// It is a package var so tests can stub the one-shot fallback exercised when
// the persistent daemon disables itself.
var runLhmBridge = runLhmBridgeOnce

func runLhmBridgeOnce(ctx context.Context) (lhmBridgeResult, error) {
	scriptPath, err := lhmBridgePath()
	if err != nil {
		return lhmBridgeResult{}, err
	}

	queryCtx, cancel := context.WithTimeout(ctx, lhmBridgeTimeout)
	defer cancel()

	cmd := exec.CommandContext(
		queryCtx,
		windowsPowerShellCoreCommand(exec.LookPath),
		"-NoLogo",
		"-NoProfile",
		"-NonInteractive",
		"-ExecutionPolicy",
		"Bypass",
		"-Command",
		fmt.Sprintf("& '%s'", scriptPath),
	)
	// An empty, immediately-EOF stdin pipe keeps pwsh's -Command host from
	// trying to bind the null device when the agent runs without a console
	// (which otherwise makes pwsh exit with no output).
	cmd.Stdin = strings.NewReader("")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" && len(stdout) > 0 {
			detail = strings.TrimSpace(string(stdout))
		}
		if detail == "" {
			detail = err.Error()
		}
		return lhmBridgeResult{}, fmt.Errorf("LibreHardwareMonitor bridge failed: %s", detail)
	}

	var result lhmBridgeResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout), &result); err != nil {
		return lhmBridgeResult{}, fmt.Errorf("parse LibreHardwareMonitor bridge output: %w", err)
	}
	return result, nil
}

// lhmPowerMetric converts the bridge's CPU package power value into a metric,
// applying the same finite/non-negative guards as the other collectors.
func lhmPowerMetric(power *lhmBridgeValue) NumberMetric {
	if power == nil || !power.Available {
		return unavailableNumber("W", "LibreHardwareMonitor did not report CPU package power")
	}
	if !isFinite(power.Value) || power.Value < 0 {
		return unavailableNumber("W", "invalid LibreHardwareMonitor power reading")
	}
	// A powered CPU never draws exactly 0 W package power. The kernel driver
	// returns 0 while it is warming up or under contention with another LHM
	// host process; surface that as unavailable instead of a misleading "0 W".
	if power.Value == 0 {
		return unavailableNumber("W", "LibreHardwareMonitor reported 0 W package power (sensor warming up or driver contention)")
	}
	return availableNumber(power.Value, "W")
}

// lhmClockMetric converts the bridge's live CPU clock (the average per-core
// clock, in MHz) into a metric. It is the authoritative current-clock source on
// Windows when LibreHardwareMonitor is available, because
// Win32_Processor.CurrentClockSpeed is static rather than a live reading.
func lhmClockMetric(clock *lhmBridgeValue) NumberMetric {
	if clock == nil || !clock.Available {
		return unavailableNumber("MHz", "LibreHardwareMonitor did not report a CPU clock sensor")
	}
	if !isFinite(clock.Value) || clock.Value <= 0 {
		return unavailableNumber("MHz", "invalid LibreHardwareMonitor CPU clock reading")
	}
	return availableNumber(clock.Value, "MHz")
}

// lhmPSUOutputPowerMetric converts the bridge's PSU output power value into a
// metric. Hosts without a USB-linked smart PSU report no sensor at all, so a nil
// value degrades to unavailable. A 0 W reading is treated as sensor warm-up /
// driver contention for consistency with CPU package power, since even an idle
// system draws nonzero rail power.
func lhmPSUOutputPowerMetric(power *lhmBridgeValue) NumberMetric {
	if power == nil || !power.Available {
		return unavailableNumber("W", "LibreHardwareMonitor did not report a PSU output power sensor")
	}
	if !isFinite(power.Value) || power.Value < 0 {
		return unavailableNumber("W", "invalid LibreHardwareMonitor PSU output power reading")
	}
	if power.Value == 0 {
		return unavailableNumber("W", "LibreHardwareMonitor reported 0 W PSU output power (sensor warming up or driver contention)")
	}
	return availableNumber(power.Value, "W")
}

// lhmRailPowerMetric converts one AMD SMU power rail into a metric. Unlike
// package power these rails are genuinely absent on Intel and on AMD parts whose
// SMU PM-table version LibreHardwareMonitor has no sensor map for, so a missing
// rail is a normal state and carries an explanatory message rather than an error
// implying a fault. A rail may also legitimately read 0 W (LibreHardwareMonitor
// only activates a rail sensor once it reports non-zero, so a zero here means a
// stale sample), which is reported unavailable for the same reason package power
// is.
func lhmRailPowerMetric(power *lhmBridgeValue, rail string) NumberMetric {
	if power == nil || !power.Available {
		return unavailableNumber("W", "LibreHardwareMonitor did not report "+rail+" (AMD SMU telemetry only; unavailable on Intel and on AMD parts with an unmapped SMU table)")
	}
	if !isFinite(power.Value) || power.Value < 0 {
		return unavailableNumber("W", "invalid LibreHardwareMonitor "+rail+" reading")
	}
	if power.Value == 0 {
		return unavailableNumber("W", "LibreHardwareMonitor reported 0 W "+rail+" (sensor warming up or driver contention)")
	}
	return availableNumber(power.Value, "W")
}

// windowsCPURailPowerFromBridge extracts the three SMU rails from one bridge
// result. They are returned together because they are only meaningful as a set:
// core + SoC + misc is the breakdown of the package figure.
func windowsCPURailPowerFromBridge(result lhmBridgeResult, err error) (core, soc, misc NumberMetric) {
	if err != nil || !result.Available {
		message := "LibreHardwareMonitor unavailable"
		if err != nil {
			message = "LibreHardwareMonitor bridge failed: " + err.Error()
		} else if strings.TrimSpace(result.Error) != "" {
			message = strings.TrimSpace(result.Error)
		}
		return unavailableNumber("W", message), unavailableNumber("W", message), unavailableNumber("W", message)
	}
	return lhmRailPowerMetric(result.CPUCorePower, "CPU core power"),
		lhmRailPowerMetric(result.CPUSocPower, "CPU SoC power"),
		lhmRailPowerMetric(result.CPUMiscPower, "CPU misc power")
}

// lhmTemperatureMetrics converts the bridge's temperature entries to sensor
// metrics, deduping by normalized name against any existing sensors.
func lhmTemperatureMetrics(temps []lhmBridgeTemp, existing []TemperatureMetric) []TemperatureMetric {
	seen := make(map[string]struct{}, len(existing)+len(temps))
	for _, sensor := range existing {
		seen[normalizedWindowsSensorName(sensor.Name)] = struct{}{}
	}
	out := append([]TemperatureMetric(nil), existing...)
	for _, temp := range temps {
		name := strings.TrimSpace(temp.Name)
		if name == "" {
			continue
		}
		if !isFinite(temp.Value) || temp.Value < -50 || temp.Value > 150 {
			continue
		}
		key := normalizedWindowsSensorName(name)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, TemperatureMetric{Name: name, Celsius: availableNumber(temp.Value, "C")})
	}
	return out
}

// lhmBridgeCacheTTL bounds how often the agent re-reads the LibreHardwareMonitor
// bridge within a burst of requests. With the persistent daemon (default on at
// sampler Start) reads are sub-second, so this window is kept short: it merely
// coalesces a cold direct-Collect() racing a slow-lane pass into a single read.
// The slow lane (default 1.5 s) therefore refreshes CPU power / clocks / PSU /
// temperatures every pass instead of serving a stale payload. The serialization
// mutex (c.lhmMu) plus the daemon's own lock still guarantee the LHM kernel
// driver is never opened twice at once.
const lhmBridgeCacheTTL = 500 * time.Millisecond

// lhmBridgeTimeout bounds the one-shot bridge invocation (runLhmBridge) and the
// daemon's cold first read after a (re)start - both still pay Computer.Open()
// up front, which loads the ring0 driver and enumerates the SuperIO/SMBus/PSU/
// CPU/GPU/memory sensors. One one-shot run is intrinsically slow: spawn pwsh,
// load LibreHardwareMonitorLib.dll, Open(), then a 400 ms sensor-prime settle.
// Measured warm cost is ~4-5 s and the very first call after boot (cold driver
// load) is slower still, which is why the rest of the agent already budgets 8 s
// for a cold sample (metricsCollectTimeout). The previous 5 s ceiling sat right
// on top of the warm cost, so under the LocalSystem service it was routinely
// exceeded; Go's CommandContext kills a timed-out child with
// TerminateProcess(handle, 1), so the kill surfaced as a bare "exit status 1"
// with no stdout/stderr and took CPU power, CPU/board temperatures and PSU
// output power down together. 12 s gives the warm (~5 s) and cold (~8 s) cases
// comfortable headroom. Steady-state daemon reads use the much shorter
// lhmDaemonWarmReadTimeout instead; this constant only bounds the cold first
// read / one-shot fallback path.
const lhmBridgeTimeout = 12 * time.Second

// fetchLhmBridge returns the shared LibreHardwareMonitor payload for a metrics
// refresh, at most once per cache window. When the persistent daemon is enabled
// (EnablePersistentBridge, default on at sampler Start) it reads from the
// long-lived process; otherwise it falls back to the one-shot bridge. The
// serialization also protects the underlying LibreHardwareMonitorLib driver,
// which errors out if two host processes open it simultaneously.
func (c *systemCollector) fetchLhmBridge(ctx context.Context) (lhmBridgeResult, error) {
	c.lhmMu.Lock()
	defer c.lhmMu.Unlock()
	now := time.Now()
	if c.lhmBridgeCached && now.Sub(c.lhmBridgeAt) < lhmBridgeCacheTTL {
		return c.lhmBridgeResult, c.lhmBridgeErr
	}
	var result lhmBridgeResult
	var err error
	if c.useDaemon {
		result, err = c.fetchDaemonLhmBridgeLocked(ctx)
	} else {
		result, err = runLhmBridge(ctx)
	}
	c.lhmBridgeResult = result
	c.lhmBridgeErr = err
	c.lhmBridgeAt = now
	c.lhmBridgeCached = true
	return result, err
}

// fetchDaemonLhmBridgeLocked reads one payload from the persistent LHM daemon,
// lazily creating it on first use. On a permanent daemon failure (pwsh / DLL
// missing -> errLhmDaemonDisabled) it falls back to the one-shot bridge, which
// itself degrades to available:false with the install hint, so a broken daemon
// is never worse than today's behavior. On a transient transport failure
// (timeout / crash / EOF / backoff) it degrades just this sample to unavailable
// rather than spawn a second pwsh (the one-shot would race the same driver);
// the daemon has already scheduled a restart and the next slow pass heals.
func (c *systemCollector) fetchDaemonLhmBridgeLocked(ctx context.Context) (lhmBridgeResult, error) {
	if c.daemon == nil {
		c.daemon = &lhmDaemon{}
	}
	result, derr := c.daemon.read(ctx)
	if derr == nil {
		return result, nil
	}
	if errors.Is(derr, errLhmDaemonDisabled) {
		return runLhmBridge(ctx)
	}
	return lhmBridgeResult{Available: false, Error: "LibreHardwareMonitor daemon read failed: " + derr.Error()}, nil
}

func windowsCPUPowerFromBridge(result lhmBridgeResult, err error) NumberMetric {
	if err != nil {
		return unavailableNumber("W", "Windows CPU power requires LibreHardwareMonitor: "+err.Error())
	}
	if !result.Available {
		message := strings.TrimSpace(result.Error)
		if message == "" {
			message = "LibreHardwareMonitor unavailable"
		}
		return unavailableNumber("W", message)
	}
	return lhmPowerMetric(result.Power)
}

// windowsPSUOutputPowerFromBridge reports the total output power of a connected
// smart PSU from the LibreHardwareMonitor bridge. Most consumer boards have no
// USB-linked PSU, so the metric degrades to unavailable rather than failing the
// metrics response.
func windowsPSUOutputPowerFromBridge(result lhmBridgeResult, err error) NumberMetric {
	if err != nil {
		return unavailableNumber("W", "Windows PSU output power requires LibreHardwareMonitor: "+err.Error())
	}
	if !result.Available {
		message := strings.TrimSpace(result.Error)
		if message == "" {
			message = "LibreHardwareMonitor unavailable"
		}
		return unavailableNumber("W", message)
	}
	return lhmPSUOutputPowerMetric(result.PSUOutputPower)
}

func windowsTemperaturesFromBridge(ctx context.Context, result lhmBridgeResult, err error) TemperatureSet {
	sensors := windowsACPITemperatureSensors(ctx)
	if err == nil && result.Available {
		sensors = lhmTemperatureMetrics(result.Temperatures, sensors)
	}
	if len(sensors) == 0 {
		return TemperatureSet{Available: false, Error: "Windows did not report temperature sensors (ACPI thermal zones unsupported on this board; run LibreHardwareMonitor for CPU/GPU temperatures)"}
	}
	return TemperatureSet{Available: true, Sensors: sensors}
}

func windowsCPUPower(ctx context.Context) NumberMetric {
	result, err := runLhmBridge(ctx)
	if err != nil {
		return unavailableNumber("W", "Windows CPU power requires LibreHardwareMonitor: "+err.Error())
	}
	if !result.Available {
		message := strings.TrimSpace(result.Error)
		if message == "" {
			message = "LibreHardwareMonitor unavailable"
		}
		return unavailableNumber("W", message)
	}
	return lhmPowerMetric(result.Power)
}

// windowsCPUPowerFromRows is kept for back-compat with existing unit tests that
// exercise the legacy WMI row selection logic. It prefers a single "Package"
// sensor to avoid double-counting the per-core sensors that are a subset of
// package power.
func windowsCPUPowerFromRows(rows []lhmSensorRow) NumberMetric {
	var fallback *float64
	for _, row := range rows {
		value, ok := lhmSensorValue(row.Value)
		if !ok {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(row.Name))
		if strings.Contains(name, "package") {
			return availableNumber(value, "W")
		}
		if strings.Contains(name, "cpu") && fallback == nil {
			fallback = &value
		}
	}
	if fallback != nil {
		return availableNumber(*fallback, "W")
	}
	return unavailableNumber("W", "Windows CPU power requires LibreHardwareMonitor (root/LibreHardwareMonitor not found)")
}

// lhmSensorRow models one LibreHardwareMonitor WMI Sensor row. Retained for the
// legacy WMI-based unit tests; the bridge path uses lhmBridgeResult instead.
type lhmSensorRow struct {
	Name  string   `json:"Name"`
	Value *float64 `json:"Value"`
}

func lhmSensorValue(value *float64) (float64, bool) {
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 {
		return 0, false
	}
	return *value, true
}

func windowsTemperatures(ctx context.Context) TemperatureSet {
	sensors := windowsACPITemperatureSensors(ctx)
	if bridge, err := runLhmBridge(ctx); err == nil && bridge.Available {
		sensors = lhmTemperatureMetrics(bridge.Temperatures, sensors)
	}
	if len(sensors) == 0 {
		return TemperatureSet{Available: false, Error: "Windows did not report temperature sensors (ACPI thermal zones unsupported on this board; run LibreHardwareMonitor for CPU/GPU temperatures)"}
	}
	return TemperatureSet{Available: true, Sensors: sensors}
}

// windowsACPITemperatureSensors reads native ACPI thermal zones. On most
// consumer boards this returns nothing ("Not supported"), which is why
// LibreHardwareMonitor is consulted as well.
func windowsACPITemperatureSensors(ctx context.Context) []TemperatureMetric {
	var rows []struct {
		InstanceName       string
		CurrentTemperature uint64
	}
	err := runPowerShellJSONArray(ctx, `Get-CimInstance -Namespace root/wmi -ClassName MSAcpi_ThermalZoneTemperature | Select-Object InstanceName,CurrentTemperature`, &rows)
	if err != nil {
		return nil
	}
	sensors := make([]TemperatureMetric, 0, len(rows))
	for _, row := range rows {
		if row.CurrentTemperature == 0 {
			continue
		}
		celsius := (float64(row.CurrentTemperature) / 10) - 273.15
		if celsius < -50 || celsius > 150 {
			continue
		}
		name := row.InstanceName
		if name == "" {
			name = "ACPI thermal zone"
		}
		sensors = append(sensors, TemperatureMetric{Name: name, Celsius: availableNumber(celsius, "C")})
	}
	return sensors
}

// appendWindowsLibreHardwareTemperatures is retained only for the legacy WMI
// unit tests; the production path now uses the embedded lhm-bridge.ps1 script
// (see runLhmBridge / lhmTemperatureMetrics).
func appendWindowsLibreHardwareTemperatures(ctx context.Context, sensors []TemperatureMetric) []TemperatureMetric {
	var rows []lhmSensorRow
	err := runPowerShellJSONArray(ctx, `Get-CimInstance -Namespace root/LibreHardwareMonitor -ClassName Sensor -Filter "SensorType='Temperature'" -ErrorAction SilentlyContinue | Select-Object Name,Value`, &rows)
	if err != nil {
		return sensors
	}
	seen := make(map[string]struct{}, len(sensors)+len(rows))
	for _, sensor := range sensors {
		seen[normalizedWindowsSensorName(sensor.Name)] = struct{}{}
	}
	for _, row := range rows {
		value, ok := lhmSensorValue(row.Value)
		if !ok {
			continue
		}
		name := strings.TrimSpace(row.Name)
		if name == "" {
			continue
		}
		key := normalizedWindowsSensorName(name)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		sensors = append(sensors, TemperatureMetric{Name: name, Celsius: availableNumber(value, "C")})
	}
	return sensors
}

func normalizedWindowsSensorName(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(name), " "))
}

func (c *systemCollector) windowsGPU(ctx context.Context) GPUSet {
	nvidia := collectNVIDIAGPU(ctx)
	if nvidia.Available {
		return nvidia
	}
	if cacheableWindowsGPUFallback(nvidia.Error) {
		c.gpuFallbackMu.Lock()
		if c.gpuFallbackCached {
			cached := c.gpuFallback
			c.gpuFallbackMu.Unlock()
			return cached
		}
		c.gpuFallbackMu.Unlock()
	}

	fallback := windowsVideoControllerGPU(ctx, nvidia.Error)
	if cacheableWindowsGPUFallback(nvidia.Error) {
		c.gpuFallbackMu.Lock()
		c.gpuFallback = fallback
		c.gpuFallbackCached = true
		c.gpuFallbackMu.Unlock()
	}
	return fallback
}

func windowsVideoControllerGPU(ctx context.Context, nvidiaError string) GPUSet {
	var rows []struct {
		Name       string
		AdapterRAM int64
	}
	err := runPowerShellJSONArray(ctx, `Get-CimInstance Win32_VideoController | Select-Object Name,AdapterRAM`, &rows)
	if err != nil {
		return GPUSet{Available: false, Error: nvidiaError + "; Win32_VideoController failed: " + err.Error()}
	}
	usageByAdapter, aggregateUsage, engineErr := windowsGPUEngineUsage(ctx)
	validRows := make([]struct {
		Name       string
		AdapterRAM int64
	}, 0, len(rows))
	for _, row := range rows {
		if row.Name == "" {
			continue
		}
		validRows = append(validRows, row)
	}

	devices := make([]GPUMetric, 0, len(validRows))
	for index, row := range validRows {
		devices = append(devices, GPUMetric{
			Name:        row.Name,
			Usage:       windowsGPUEngineUsageMetricForAdapter(index, len(validRows), usageByAdapter, aggregateUsage, engineErr),
			Memory:      windowsAdapterRAMCapacity(row.AdapterRAM),
			Temperature: unavailableNumber("C", "GPU temperature requires vendor tools such as nvidia-smi"),
		})
	}
	if len(devices) == 0 {
		return GPUSet{Available: false, Error: nvidiaError + "; no Win32_VideoController devices found"}
	}
	return GPUSet{Available: true, Devices: devices, Error: nvidiaError}
}

func windowsGPUEngineUsage(ctx context.Context) (map[int]NumberMetric, NumberMetric, error) {
	var rows []windowsGPUEngineCounter
	err := runPowerShellJSONArray(ctx, `Get-Counter '\GPU Engine(*)\Utilization Percentage' -ErrorAction Stop | Select-Object -ExpandProperty CounterSamples | Select-Object InstanceName,CookedValue`, &rows)
	if err != nil {
		return nil, NumberMetric{}, err
	}
	usageByAdapter, aggregateUsage := windowsGPUEngineUsageMetrics(rows)
	return usageByAdapter, aggregateUsage, nil
}

func runPowerShellJSON(ctx context.Context, script string, target any) error {
	out, err := runPowerShell(ctx, script+" | ConvertTo-Json -Compress")
	if err != nil {
		return err
	}
	return json.Unmarshal(out, target)
}

func runPowerShellJSONArray(ctx context.Context, script string, target any) error {
	out, err := runPowerShell(ctx, "$items = @("+script+"); ConvertTo-Json -Compress -InputObject $items")
	if err != nil {
		return err
	}
	return json.Unmarshal(out, target)
}

func runPowerShell(ctx context.Context, script string) ([]byte, error) {
	queryCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	cmd := exec.CommandContext(
		queryCtx,
		windowsPowerShellCommand(exec.LookPath),
		windowsPowerShellArgs(script)...,
	)
	// Capture stdout (the JSON payload) separately from stderr (PowerShell
	// non-terminating errors / warnings). CIM cmdlets such as
	// Get-CimInstance -Namespace root/wmi -ClassName MSAcpi_ThermalZoneTemperature
	// frequently emit non-terminating errors to stderr while still exiting 0 and
	// printing valid JSON on stdout. Merging the streams (CombinedOutput) let
	// those error lines prefix the JSON and broke json.Unmarshal with misleading
	// "invalid character 'G'" messages. Stdout drives parsing; stderr is only
	// surfaced when the command actually fails or produces no JSON.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(string(out))
		}
		if detail != "" {
			return nil, fmt.Errorf("%v: %s", err, detail)
		}
		return nil, err
	}
	if len(bytes.TrimSpace(out)) == 0 && strings.TrimSpace(stderr.String()) != "" {
		return nil, fmt.Errorf("powershell produced no output: %s", strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
