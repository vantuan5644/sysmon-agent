package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestLhmNvidiaHotspotSelection(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell 7 is needed for bridge fixture execution")
	}
	for _, name := range lhmBridgeScriptFiles {
		t.Run(name, func(t *testing.T) {
			source := readLhmBridgeScript(t, name)
			functions := sliceFunction(t, source, "Test-PlausibleTemperature") + "\n}\n" + sliceFunction(t, source, "Get-NvidiaHotspot") + "\n}\n"
			script := `$ErrorActionPreference = 'Stop'
` + functions + `
$core = [pscustomobject]@{SensorType='Temperature'; Name='GPU Core'; Value=48}
$memory = [pscustomobject]@{SensorType='Temperature'; Name='GPU Memory Junction'; Value=96}
$spot = [pscustomobject]@{SensorType='Temperature'; Name='GPU Hot Spot'; Value=82.5}
$hw = [pscustomobject]@{HardwareType='GpuNvidia'; Name='NVIDIA GeForce RTX 4090'; Identifier='/gpu-nvidia/0'; Sensors=@($memory,$core,$spot)}
$r = @(Get-NvidiaHotspot $hw)
if ($r.Count -ne 1 -or $r[0].value -ne 82.5 -or $r[0].name -ne $hw.Name) { throw 'wrong sensor or identity' }
$spot.Value = $null
if (@(Get-NvidiaHotspot $hw).Count -ne 0) { throw 'null sensor accepted' }
$spot.Value = 0
if (@(Get-NvidiaHotspot $hw).Count -ne 0) { throw 'zero sensor accepted' }
$spot.Value = [double]::NaN
if (@(Get-NvidiaHotspot $hw).Count -ne 0) { throw 'nonfinite sensor accepted' }
$spot.Value = 82.5
$spot.Name = 'GPU Hot Spot Limit'
if (@(Get-NvidiaHotspot $hw).Count -ne 0) { throw 'threshold accepted' }
$spot.Name = 'GPU Hotspot'
$hw.HardwareType = 'GpuAmd'
if (@(Get-NvidiaHotspot $hw).Count -ne 0) { throw 'non-NVIDIA accepted' }
`
			// The budget is for pwsh's cold start, not the fixture, which runs in
			// well under a second. A fresh CI runner took longer than the 10 s
			// this used to allow (release repo PR #8, 2026-10-22) and the test
			// failed on a slow start rather than a wrong sensor.
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, pwsh, "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
			if err != nil {
				t.Fatalf("PowerShell fixtures: %v: %s", err, out)
			}
		})
	}
}

// Direct self-check collection and the resident dashboard sampler have separate
// Windows entrypoints. Both must enrich from the same already-fetched bridge.
func TestWindowsGPUHotspotBothCollectionPaths(t *testing.T) {
	for _, name := range []string{"collector_windows.go", "collector_windows_fast.go"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "mergeWindowsGPUHotspots(gpu, bridgeResult.GPUHotspots, bridgeResult.Available && bridgeErr == nil)") {
			t.Fatalf("%s does not attach hotspot readings to GPU metrics", name)
		}
	}
}

// The two LibreHardwareMonitor bridge scripts are independent implementations of
// one JSON contract: lhm-bridge.ps1 is the one-shot (used by -self-check and as
// the fallback when the daemon cannot start) and lhm-bridge-daemon.ps1 is the
// long-lived one. They deliberately duplicate their sensor-selection helpers
// rather than share a module, because each is //go:embed-ed and written to a
// standalone temp file. That makes silent divergence the standing risk, so the
// rules below are asserted against both copies.
//
// These tests read the scripts from disk rather than the embed FS so they run on
// Linux too: the embeds live behind //go:build windows, but the divergence they
// guard against is just as easy to introduce from the Linux side of the suite.
var lhmBridgeScriptFiles = []string{"lhm-bridge.ps1", "lhm-bridge-daemon.ps1"}

func readLhmBridgeScript(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// sliceFunction returns the source of the named PowerShell function, from its
// declaration to the first line that closes it at column 0.
func sliceFunction(t *testing.T, script, name string) string {
	t.Helper()
	start := strings.Index(script, "function "+name)
	if start < 0 {
		t.Fatalf("function %s not found", name)
	}
	body := script[start:]
	if end := strings.Index(body, "\n}"); end >= 0 {
		return body[:end]
	}
	return body
}

// TestLhmBridgeScriptsRejectStaticSmartThresholds pins the temperature filter in
// both scripts. Every NVMe drive reports its SMART composite warning and
// critical thresholds as Temperature sensors alongside the live Composite
// reading - measured 79-88 C against real drive temps of 29-50 C. They are
// constants, not readings.
//
// Drive sensors no longer come through LHM (TestLhmBridgeScriptsNeverEnableStorage),
// but the filter stays: publishing a configured limit as a live reading is the
// same class of defect as picking the wrong CPU die sensor, and other hardware
// can name a limit the same way.
func TestLhmBridgeScriptsRejectStaticSmartThresholds(t *testing.T) {
	// Each fragment is matched as a substring by the script, so they must be
	// unmistakable - the trap documented for cpuDieTemperatureRank. None of
	// these appears inside a live sensor name on any host we have measured.
	fragments := []string{"limit", "resolution", "warning", "critical", "threshold"}
	for _, name := range lhmBridgeScriptFiles {
		script := readLhmBridgeScript(t, name)
		fn := sliceFunction(t, script, "Test-LiveTemperature")
		for _, fragment := range fragments {
			want := "$n -match '" + fragment + "'"
			if !strings.Contains(fn, want) {
				t.Errorf("%s: Test-LiveTemperature does not reject %q (missing %s); a static SMART threshold would be published as a live temperature", name, fragment, want)
			}
		}
	}
}

// TestLhmBridgeScriptsNeverEnableStorage pins the decision to take drive
// temperatures from Windows instead of LibreHardwareMonitor. Enabling LHM's
// storage group runs DiskInfoToolkit's detection, which sends vendor
// pass-through commands to every USB disk it cannot identify, at Open() and on
// every hot-plug, with no way to exclude a drive. It USB-reset an SD card
// reader mid-write (2026-10-09) and, against an in-development recorder that
// let each command time out, kept Computer.Open() running past 5 minutes so
// the daemon never answered (2026-10-22). NVMe and SATA temperatures come from
// readWindowsDriveTemperature now.
func TestLhmBridgeScriptsNeverEnableStorage(t *testing.T) {
	for _, name := range lhmBridgeScriptFiles {
		if script := readLhmBridgeScript(t, name); strings.Contains(script, "IsStorageEnabled = $true") {
			t.Errorf("%s enables LHM's storage group, which sends vendor pass-through commands to USB disks", name)
		}
	}
}
