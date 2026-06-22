//go:build windows

package main

import (
	"errors"
	"math"
	"os"
	"strings"
	"testing"
)

func TestWindowsAdapterRAMCapacityReportsTotalOnly(t *testing.T) {
	got := windowsAdapterRAMCapacity(8 * 1024 * 1024 * 1024)
	if got.Available {
		t.Fatalf("adapter RAM capacity = %+v, want unavailable live usage", got)
	}
	if got.TotalBytes != 8*1024*1024*1024 {
		t.Fatalf("total_bytes = %d, want 8 GiB", got.TotalBytes)
	}
	if !strings.Contains(got.Error, "total adapter RAM only") {
		t.Fatalf("error = %q, want total adapter RAM caveat", got.Error)
	}
}

func TestWindowsMemoryCapacity(t *testing.T) {
	got := windowsMemoryCapacity(16*1024*1024, 4*1024*1024)
	if !got.Available {
		t.Fatalf("Windows memory capacity = %+v, want available", got)
	}
	if got.TotalBytes != 16*1024*1024*1024 || got.UsedBytes != 12*1024*1024*1024 || got.Percent != 75 {
		t.Fatalf("Windows memory capacity = %+v, want 12 GiB used of 16 GiB", got)
	}
}

func TestWindowsMemoryCapacityRejectsInvalidCounters(t *testing.T) {
	for _, tc := range []struct {
		name     string
		totalKiB uint64
		freeKiB  uint64
	}{
		{name: "zero total", totalKiB: 0, freeKiB: 0},
		{name: "free exceeds total", totalKiB: 1024, freeKiB: 2048},
		{name: "total overflow", totalKiB: ^uint64(0), freeKiB: 1},
		{name: "free overflow", totalKiB: 1024, freeKiB: ^uint64(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := windowsMemoryCapacity(tc.totalKiB, tc.freeKiB)
			if got.Available {
				t.Fatalf("Windows memory capacity = %+v, want unavailable", got)
			}
			if got.Error != "invalid Windows memory counters" {
				t.Fatalf("error = %q, want invalid Windows memory counters", got.Error)
			}
		})
	}
}

func TestWindowsCPULoadMetric(t *testing.T) {
	value := 42.5
	got := windowsCPULoadMetric(&value)
	if !got.Available || got.Value != 42.5 || got.Unit != "%" {
		t.Fatalf("CPU load metric = %+v, want available 42.5%%", got)
	}
}

func TestWindowsCPULoadMetricRejectsMissingOrInvalidValues(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value *float64
	}{
		{name: "missing", value: nil},
		{name: "negative", value: floatPointer(-1)},
		{name: "over 100", value: floatPointer(101)},
		{name: "nan", value: floatPointer(math.NaN())},
		{name: "inf", value: floatPointer(math.Inf(1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := windowsCPULoadMetric(tc.value)
			if got.Available {
				t.Fatalf("CPU load metric = %+v, want unavailable", got)
			}
			if got.Unit != "%" || got.Error != "Windows CPU load percentage unavailable" {
				t.Fatalf("CPU load metric = %+v, want CPU unavailable error", got)
			}
		})
	}
}

func TestWindowsAdapterRAMCapacityToleratesInvalidValues(t *testing.T) {
	for _, value := range []int64{0, -1, -2147483648} {
		got := windowsAdapterRAMCapacity(value)
		if got.Available || got.TotalBytes != 0 {
			t.Fatalf("adapter RAM %d = %+v, want unavailable zero total", value, got)
		}
		if got.Error == "" {
			t.Fatalf("adapter RAM %d missing error", value)
		}
	}
}

func TestWindowsGPUEngineUsageMetricsGroupsPhysicalAdapters(t *testing.T) {
	value20 := 20.5
	value5 := 5.0
	value120 := 120.0
	valueNegative := -1.0
	valueNaN := math.NaN()
	byAdapter, aggregate := windowsGPUEngineUsageMetrics([]windowsGPUEngineCounter{
		{InstanceName: "pid_100_phys_0_eng_0_engtype_3D", CookedValue: &value20},
		{InstanceName: "pid_200_phys_0_eng_1_engtype_Copy", CookedValue: &value5},
		{InstanceName: "pid_300_phys_1_eng_0_engtype_3D", CookedValue: &value120},
		{InstanceName: "pid_400_phys_2_eng_0_engtype_3D", CookedValue: &valueNegative},
		{InstanceName: "pid_500_phys_3_eng_0_engtype_3D", CookedValue: &valueNaN},
		{InstanceName: "pid_600_phys_4_eng_0_engtype_3D", CookedValue: nil},
	})

	if got := byAdapter[0]; !got.Available || got.Value != 25.5 || got.Unit != "%" {
		t.Fatalf("adapter 0 usage = %+v, want 25.5%%", got)
	}
	if got := byAdapter[1]; !got.Available || got.Value != 100 || got.Unit != "%" {
		t.Fatalf("adapter 1 usage = %+v, want clamped 100%%", got)
	}
	if _, ok := byAdapter[2]; ok {
		t.Fatalf("adapter 2 usage should ignore negative counter: %+v", byAdapter[2])
	}
	if !aggregate.Available || aggregate.Value != 100 || aggregate.Unit != "%" {
		t.Fatalf("aggregate usage = %+v, want clamped 100%%", aggregate)
	}
}

func TestWindowsGPUPhysicalAdapterIndex(t *testing.T) {
	for _, tc := range []struct {
		name string
		want int
		ok   bool
	}{
		{name: "pid_100_phys_0_eng_0_engtype_3D", want: 0, ok: true},
		{name: "PID_100_PHYS_12_ENG_0_ENGTYPE_3D", want: 12, ok: true},
		{name: "pid_100_luid_0x00000000_0x00012345_eng_0", ok: false},
		{name: "pid_100_phys_bad_eng_0", ok: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := windowsGPUPhysicalAdapterIndex(tc.name)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("index = %d/%v, want %d/%v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestWindowsGPUEngineUsageMetricForAdapter(t *testing.T) {
	byAdapter := map[int]NumberMetric{1: availableNumber(42, "%")}
	aggregate := availableNumber(18, "%")
	if got := windowsGPUEngineUsageMetricForAdapter(1, 2, byAdapter, aggregate, nil); !got.Available || got.Value != 42 {
		t.Fatalf("mapped adapter usage = %+v, want 42%%", got)
	}
	if got := windowsGPUEngineUsageMetricForAdapter(0, 1, nil, aggregate, nil); !got.Available || got.Value != 18 {
		t.Fatalf("single adapter aggregate usage = %+v, want 18%%", got)
	}
	if got := windowsGPUEngineUsageMetricForAdapter(0, 2, nil, aggregate, nil); got.Available || !strings.Contains(got.Error, "did not expose adapter usage") {
		t.Fatalf("multi adapter aggregate usage = %+v, want unavailable mapping caveat", got)
	}
	if got := windowsGPUEngineUsageMetricForAdapter(0, 1, nil, NumberMetric{}, errors.New("counter set missing")); got.Available || !strings.Contains(got.Error, "GPU Engine counters unavailable") {
		t.Fatalf("counter error usage = %+v, want unavailable counter error", got)
	}
}

func floatPointer(value float64) *float64 {
	return &value
}

func TestShouldIncludeWindowsNetworkInterface(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"", false},
		{"   ", false},
		{"Loopback Pseudo-Interface 1", false},
		{"vEthernet (Default Switch)", false},
		{"vEthernet (WSL (Hyper-V firewall))", false},
		{"DockerNAT", false},
		{"Hyper-V Virtual Ethernet Adapter", false},
		{"VirtualBox Host-Only Network", false},
		{"VMware Network Adapter VMnet8", false},
		{"Npcap Loopback Adapter", false},
		{"Local Area Connection* 1 Microsoft Wi-Fi Direct Virtual Adapter", false},
		{"NAT Network", false},
		{"Ethernet", true},
		{"Wi-Fi", true},
		{"Tailscale", true},
		{"WireGuard Tunnel", true},
		{"Bluetooth Network Connection", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldIncludeWindowsNetworkInterface(tc.name); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWindowsCollectorFiltersVirtualNetworkAdapters(t *testing.T) {
	data, err := os.ReadFile("collector_windows.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	for _, needle := range []string{
		`if shouldIncludeWindowsNetworkInterface(name) {`,
		`no non-virtual network adapter statistics found`,
	} {
		if !strings.Contains(source, needle) {
			t.Fatalf("collector_windows.go missing Windows network filtering behavior %q", needle)
		}
	}
}

func TestWindowsCollectorRunsMetricQueriesConcurrently(t *testing.T) {
	data, err := os.ReadFile("collector_windows.go")
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
	} {
		if !strings.Contains(source, needle) {
			t.Fatalf("collector_windows.go missing concurrent Windows collection behavior %q", needle)
		}
	}
}

func TestWindowsCollectorDegradesPanickedMetricGroups(t *testing.T) {
	data, err := os.ReadFile("collector_windows.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	for _, needle := range []string{
		`Windows CPU collector panicked`,
		`Windows memory collector panicked`,
		`Windows disk collector panicked`,
		`Windows network collector panicked`,
		`Windows temperature collector panicked`,
		`Windows GPU collector panicked`,
	} {
		if !strings.Contains(source, needle) {
			t.Fatalf("collector_windows.go missing Windows degraded panic behavior %q", needle)
		}
	}
}

func TestCacheableWindowsGPUFallback(t *testing.T) {
	if !cacheableWindowsGPUFallback("nvidia-smi not found") {
		t.Fatal("nvidia-smi not found should allow caching static Win32 GPU fallback")
	}
	if cacheableWindowsGPUFallback("nvidia-smi failed: exit status 1") {
		t.Fatal("transient nvidia-smi failures should not cache static Win32 GPU fallback")
	}
}

func TestWindowsCollectorCachesStaticGPUFallback(t *testing.T) {
	data, err := os.ReadFile("collector_windows.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	for _, needle := range []string{
		`gpuFallbackCached bool`,
		`return c.windowsGPU(ctx)`,
		`func (c *systemCollector) windowsGPU(ctx context.Context) GPUSet`,
		`windowsGPUEngineUsage(ctx)`,
		`Get-Counter '\GPU Engine(*)\Utilization Percentage'`,
		`Usage:       windowsGPUEngineUsageMetricForAdapter`,
		`cacheableWindowsGPUFallback(nvidia.Error)`,
		`c.gpuFallbackCached`,
		`windowsVideoControllerGPU(ctx, nvidia.Error)`,
	} {
		if !strings.Contains(source, needle) {
			t.Fatalf("collector_windows.go missing GPU fallback cache behavior %q", needle)
		}
	}
}

func TestWindowsPowerShellCommandPrefersWindowsPowerShell(t *testing.T) {
	got := windowsPowerShellCommand(func(name string) (string, error) {
		switch name {
		case "powershell.exe":
			return `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, nil
		case "pwsh.exe":
			return `C:\Program Files\PowerShell\7\pwsh.exe`, nil
		default:
			return "", errors.New("not found")
		}
	})
	if !strings.HasSuffix(got, `powershell.exe`) {
		t.Fatalf("command = %q, want powershell.exe preference", got)
	}
}

func TestWindowsPowerShellCommandFallsBackToPowerShellCore(t *testing.T) {
	got := windowsPowerShellCommand(func(name string) (string, error) {
		if name == "pwsh.exe" {
			return `C:\Program Files\PowerShell\7\pwsh.exe`, nil
		}
		return "", errors.New("not found")
	})
	if !strings.HasSuffix(got, `pwsh.exe`) {
		t.Fatalf("command = %q, want pwsh.exe fallback", got)
	}
}

func TestWindowsPowerShellCommandUsesStableFallbackWhenLookupFails(t *testing.T) {
	got := windowsPowerShellCommand(func(name string) (string, error) {
		return "", errors.New("not found")
	})
	if got != "powershell.exe" {
		t.Fatalf("command = %q, want stable powershell.exe fallback", got)
	}
}

func TestWindowsPowerShellCoreCommandPrefersPwshFromPath(t *testing.T) {
	got := windowsPowerShellCoreCommand(func(name string) (string, error) {
		switch name {
		case "pwsh.exe":
			return `C:\Program Files\PowerShell\7\pwsh.exe`, nil
		case "powershell.exe":
			return `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, nil
		default:
			return "", errors.New("not found")
		}
	})
	if !strings.HasSuffix(got, `pwsh.exe`) {
		t.Fatalf("command = %q, want pwsh.exe preference", got)
	}
}

// When pwsh is not on PATH (as for the LocalSystem service account) the command
// must still resolve pwsh from the standard install directory rather than fall
// back to Windows PowerShell 5.1, which cannot host LibreHardwareMonitorLib.dll.
func TestWindowsPowerShellCoreCommandProbesInstallDirWhenNotOnPath(t *testing.T) {
	const installed = `C:\Program Files\PowerShell\7\pwsh.exe`
	original := lookupInstalledPwsh
	lookupInstalledPwsh = func() string { return installed }
	defer func() { lookupInstalledPwsh = original }()

	got := windowsPowerShellCoreCommand(func(name string) (string, error) {
		if name == "powershell.exe" {
			return `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, nil
		}
		return "", errors.New("not found")
	})
	if got != installed {
		t.Fatalf("command = %q, want probed install path %q", got, installed)
	}
}

// With neither pwsh on PATH nor a discoverable install, degrade to powershell.exe
// so the caller still gets a usable shell on hosts without PowerShell 7.
func TestWindowsPowerShellCoreCommandFallsBackToWindowsPowerShell(t *testing.T) {
	original := lookupInstalledPwsh
	lookupInstalledPwsh = func() string { return "" }
	defer func() { lookupInstalledPwsh = original }()

	got := windowsPowerShellCoreCommand(func(name string) (string, error) {
		if name == "powershell.exe" {
			return `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, nil
		}
		return "", errors.New("not found")
	})
	if !strings.HasSuffix(got, `powershell.exe`) {
		t.Fatalf("command = %q, want powershell.exe fallback", got)
	}
}

func TestWindowsPowerShellArgsDisableProfilesAndBypassPolicy(t *testing.T) {
	got := strings.Join(windowsPowerShellArgs("Get-Date"), "\x00")
	for _, needle := range []string{
		"-NoLogo",
		"-NoProfile",
		"-NonInteractive",
		"-ExecutionPolicy\x00Bypass",
		"-Command\x00Get-Date",
	} {
		if !strings.Contains(got, needle) {
			t.Fatalf("PowerShell args %q missing %q", got, needle)
		}
	}
}

func TestWindowsCPUPowerFromRows(t *testing.T) {
	packagePower := 65.0
	corePower := 40.0
	gpuPower := 200.0
	cpuPower := 45.0
	negativePower := -1.0
	for _, tc := range []struct {
		name      string
		rows      []lhmSensorRow
		available bool
		value     float64
	}{
		{
			name:      "prefers package over cores",
			rows:      []lhmSensorRow{{Name: "CPU Cores", Value: &corePower}, {Name: "CPU Package", Value: &packagePower}},
			available: true,
			value:     65,
		},
		{
			name:      "falls back to first cpu sensor",
			rows:      []lhmSensorRow{{Name: "GPU Power", Value: &gpuPower}, {Name: "CPU", Value: &cpuPower}},
			available: true,
			value:     45,
		},
		{
			name:      "ignores gpu only",
			rows:      []lhmSensorRow{{Name: "GPU", Value: &gpuPower}},
			available: false,
		},
		{name: "empty", rows: nil, available: false},
		{name: "rejects negative", rows: []lhmSensorRow{{Name: "CPU Package", Value: &negativePower}}, available: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := windowsCPUPowerFromRows(tc.rows)
			if got.Available != tc.available {
				t.Fatalf("available = %v, want %v (%+v)", got.Available, tc.available, got)
			}
			if tc.available && got.Value != tc.value {
				t.Fatalf("value = %v, want %v", got.Value, tc.value)
			}
			if tc.available && got.Unit != "W" {
				t.Fatalf("unit = %q, want W", got.Unit)
			}
		})
	}
}

func TestLhmPowerMetric(t *testing.T) {
	for _, tc := range []struct {
		name      string
		power     *lhmBridgeValue
		available bool
		value     float64
	}{
		{name: "missing", power: nil, available: false},
		{name: "unavailable", power: &lhmBridgeValue{Available: false}, available: false},
		{name: "negative", power: &lhmBridgeValue{Available: true, Value: -1}, available: false},
		{name: "zero is treated as warm-up contention", power: &lhmBridgeValue{Available: true, Value: 0}, available: false},
		{name: "real reading", power: &lhmBridgeValue{Available: true, Value: 88.08}, available: true, value: 88.08},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := lhmPowerMetric(tc.power)
			if got.Available != tc.available {
				t.Fatalf("available = %v, want %v (%+v)", got.Available, tc.available, got)
			}
			if tc.available && got.Value != tc.value {
				t.Fatalf("value = %v, want %v", got.Value, tc.value)
			}
			if tc.available && got.Unit != "W" {
				t.Fatalf("unit = %q, want W", got.Unit)
			}
			if !tc.available && got.Unit != "W" {
				t.Fatalf("unit = %q, want W even when unavailable", got.Unit)
			}
		})
	}
}

func TestLhmPSUOutputPowerMetric(t *testing.T) {
	for _, tc := range []struct {
		name      string
		power     *lhmBridgeValue
		available bool
		value     float64
	}{
		{name: "missing sensor", power: nil, available: false},
		{name: "unavailable", power: &lhmBridgeValue{Available: false}, available: false},
		{name: "negative", power: &lhmBridgeValue{Available: true, Value: -1}, available: false},
		{name: "zero is treated as warm-up contention", power: &lhmBridgeValue{Available: true, Value: 0}, available: false},
		{name: "real output reading", power: &lhmBridgeValue{Available: true, Value: 312.4}, available: true, value: 312.4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := lhmPSUOutputPowerMetric(tc.power)
			if got.Available != tc.available {
				t.Fatalf("available = %v, want %v (%+v)", got.Available, tc.available, got)
			}
			if tc.available && got.Value != tc.value {
				t.Fatalf("value = %v, want %v", got.Value, tc.value)
			}
			if got.Unit != "W" {
				t.Fatalf("unit = %q, want W", got.Unit)
			}
		})
	}
}

func TestWindowsPSUOutputPowerFromBridge(t *testing.T) {
	for _, tc := range []struct {
		name      string
		result    lhmBridgeResult
		err       error
		available bool
		value     float64
	}{
		{
			name:      "bridge error degrades to unavailable",
			result:    lhmBridgeResult{},
			err:       errors.New("bridge failed"),
			available: false,
		},
		{
			name:      "librehardwaremonitor unavailable",
			result:    lhmBridgeResult{Available: false, Error: "LibreHardwareMonitor not installed"},
			available: false,
		},
		{
			name:      "no psu sensor reported",
			result:    lhmBridgeResult{Available: true},
			available: false,
		},
		{
			name:      "psu output power reading",
			result:    lhmBridgeResult{Available: true, PSUOutputPower: &lhmBridgeValue{Available: true, Value: 274.5}},
			available: true,
			value:     274.5,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := windowsPSUOutputPowerFromBridge(tc.result, tc.err)
			if got.Available != tc.available {
				t.Fatalf("available = %v, want %v (%+v)", got.Available, tc.available, got)
			}
			if tc.available {
				if got.Value != tc.value {
					t.Fatalf("value = %v, want %v", got.Value, tc.value)
				}
				if got.Unit != "W" {
					t.Fatalf("unit = %q, want W", got.Unit)
				}
			} else if got.Unit != "W" {
				t.Fatalf("unit = %q, want W even when unavailable", got.Unit)
			}
		})
	}
}

func TestWindowsClockMHz(t *testing.T) {
	value := 4501.0
	got, ok := windowsClockMHz(&value)
	if !ok || got != 4501 {
		t.Fatalf("windowsClockMHz(4501) = %v/%v, want 4501/true", got, ok)
	}
	for _, bad := range []*float64{nil, floatPointer(0), floatPointer(-5), floatPointer(math.NaN())} {
		if got, ok := windowsClockMHz(bad); ok {
			t.Fatalf("windowsClockMHz(%v) = %v/%v, want unavailable", bad, got, ok)
		}
	}
}
