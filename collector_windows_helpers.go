package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func windowsMemoryCapacity(totalKiB, freeKiB uint64) CapacityMetric {
	total, totalOK := kibToBytes(totalKiB)
	free, freeOK := kibToBytes(freeKiB)
	if !totalOK || !freeOK || total == 0 || free > total {
		return unavailableCapacity("invalid Windows memory counters")
	}
	return availableCapacity(total-free, total)
}

func windowsCPULoadMetric(average *float64) NumberMetric {
	if average == nil || math.IsNaN(*average) || math.IsInf(*average, 0) || *average < 0 || *average > 100 {
		return unavailableNumber("%", "Windows CPU load percentage unavailable")
	}
	return availableNumber(*average, "%")
}

func windowsAdapterRAMCapacity(adapterRAM int64) CapacityMetric {
	if adapterRAM <= 0 {
		return unavailableCapacity("GPU memory usage is not exposed by Win32_VideoController")
	}
	return CapacityMetric{
		Available:  false,
		TotalBytes: uint64(adapterRAM),
		Error:      "GPU memory usage unavailable; total adapter RAM only",
	}
}

type windowsGPUEngineCounter struct {
	InstanceName string   `json:"InstanceName"`
	CookedValue  *float64 `json:"CookedValue"`
}

func windowsGPUEngineUsageMetrics(rows []windowsGPUEngineCounter) (map[int]NumberMetric, NumberMetric) {
	sums := map[int]float64{}
	aggregate := 0.0
	hasAggregate := false
	for _, row := range rows {
		value, ok := windowsGPUEngineCounterValue(row.CookedValue)
		if !ok {
			continue
		}
		aggregate += value
		hasAggregate = true
		if index, ok := windowsGPUPhysicalAdapterIndex(row.InstanceName); ok {
			sums[index] += value
		}
	}

	usageByAdapter := map[int]NumberMetric{}
	for index, value := range sums {
		usageByAdapter[index] = availableNumber(clampFloat(value, 0, 100), "%")
	}
	if !hasAggregate {
		return usageByAdapter, unavailableNumber("%", "Windows GPU Engine counters did not report usage")
	}
	return usageByAdapter, availableNumber(clampFloat(aggregate, 0, 100), "%")
}

func windowsGPUEngineCounterValue(value *float64) (float64, bool) {
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 {
		return 0, false
	}
	return *value, true
}

func windowsGPUPhysicalAdapterIndex(instanceName string) (int, bool) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(instanceName)), "_")
	for i := 0; i < len(parts)-1; i++ {
		if parts[i] != "phys" {
			continue
		}
		index, err := strconv.Atoi(parts[i+1])
		if err != nil || index < 0 {
			return 0, false
		}
		return index, true
	}
	return 0, false
}

func windowsGPUEngineUsageMetricForAdapter(adapterIndex, adapterCount int, usageByAdapter map[int]NumberMetric, aggregate NumberMetric, engineErr error) NumberMetric {
	if metric, ok := usageByAdapter[adapterIndex]; ok && metric.Available {
		return metric
	}
	if adapterCount == 1 && aggregate.Available {
		return aggregate
	}
	if engineErr != nil {
		return unavailableNumber("%", fmt.Sprintf("GPU usage requires vendor tools such as nvidia-smi; Windows GPU Engine counters unavailable: %v", engineErr))
	}
	return unavailableNumber("%", "Windows GPU Engine counters did not expose adapter usage")
}

func clampFloat(value, minValue, maxValue float64) float64 {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func shouldIncludeWindowsNetworkInterface(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return false
	}
	for _, prefix := range skippedWindowsNetworkInterfacePrefixes {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	for _, fragment := range skippedWindowsNetworkInterfaceFragments {
		if strings.Contains(name, fragment) {
			return false
		}
	}
	return true
}

var skippedWindowsNetworkInterfacePrefixes = []string{
	"docker",
	"hyper-v",
	"npcap loopback",
	"vethernet",
	"virtualbox host-only",
	"vmware network adapter",
}

var skippedWindowsNetworkInterfaceFragments = []string{
	"default switch",
	"docker",
	"hyper-v",
	"loopback",
	"microsoft wi-fi direct virtual adapter",
	"nat network",
	"nat switch",
	"npcap",
	"virtualbox host-only",
	"vmware network adapter",
	"wi-fi direct virtual adapter",
	"wsl",
}

func cacheableWindowsGPUFallback(nvidiaError string) bool {
	return strings.Contains(strings.ToLower(nvidiaError), "nvidia-smi not found")
}

func windowsPowerShellCommand(lookPath func(string) (string, error)) string {
	for _, name := range []string{"powershell.exe", "pwsh.exe", "pwsh", "powershell"} {
		path, err := lookPath(name)
		if err == nil && strings.TrimSpace(path) != "" {
			return path
		}
	}
	return "powershell.exe"
}

// windowsPowerShellCoreCommand prefers PowerShell 7+ (pwsh) for callers that need
// to load .NET assemblies such as LibreHardwareMonitorLib.dll, which Windows
// PowerShell 5.1 cannot host. It falls back to powershell.exe so the caller
// still gets a sensible command on hosts without pwsh installed.
//
// A plain PATH lookup is not enough: when the agent runs as the LocalSystem
// service account, its PATH does not include the PowerShell 7 install directory,
// so LookPath("pwsh") fails and the bridge would silently fall back to Windows
// PowerShell 5.1 - which loads the DLL but cannot resolve its types ("Cannot
// find type [LibreHardwareMonitor.Hardware.Computer]"), leaving CPU power and
// board temperatures unavailable under the service. So when PATH misses pwsh,
// probe the standard install locations (mirrors Find-PwshForService in
// install-windows.ps1) before degrading to powershell.exe.
func windowsPowerShellCoreCommand(lookPath func(string) (string, error)) string {
	for _, name := range []string{"pwsh.exe", "pwsh"} {
		path, err := lookPath(name)
		if err == nil && strings.TrimSpace(path) != "" {
			return path
		}
	}
	if path := lookupInstalledPwsh(); path != "" {
		return path
	}
	for _, name := range []string{"powershell.exe", "powershell"} {
		path, err := lookPath(name)
		if err == nil && strings.TrimSpace(path) != "" {
			return path
		}
	}
	return "pwsh"
}

// lookupInstalledPwsh resolves pwsh.exe from the standard PowerShell install
// directories when it is not on PATH. It is a package variable so tests can
// stub the filesystem probe and stay deterministic across platforms.
var lookupInstalledPwsh = installedPwshPath

// installedPwshPath searches the well-known PowerShell 7 install directories for
// pwsh.exe and returns the newest stable build found, or "" if none exists.
func installedPwshPath() string {
	var found []string
	for _, env := range []string{"ProgramFiles", "ProgramW6432", "ProgramFiles(x86)"} {
		base := os.Getenv(env)
		if strings.TrimSpace(base) == "" {
			continue
		}
		matches, err := filepath.Glob(filepath.Join(base, "PowerShell", "*", "pwsh.exe"))
		if err != nil {
			continue
		}
		found = append(found, matches...)
	}
	// Highest version directory first; "7" sorts ahead of "7-preview" and "6"
	// under a descending string sort, so a stable release wins over a preview.
	sort.Sort(sort.Reverse(sort.StringSlice(found)))
	for _, path := range found {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	return ""
}

func windowsPowerShellArgs(script string) []string {
	return []string{
		"-NoLogo",
		"-NoProfile",
		"-NonInteractive",
		"-ExecutionPolicy",
		"Bypass",
		"-Command",
		script,
	}
}
