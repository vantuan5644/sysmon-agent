package main

import (
	"os"
	"strings"
	"testing"
)

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
// Emitting them is not merely noisy. windowsStorageTemperatureForModel takes the
// FIRST bridge temperature whose name contains the drive model, so on a drive
// whose Composite channel reads 0 (unpopulated, and dropped by
// Test-PlausibleTemperature) the next match is the warning threshold, publishing
// a static limit as that drive's live temperature. That is the same class of
// defect as picking the wrong CPU die sensor: one field name carrying a
// different physical quantity depending on the host.
//
// A latent hazard rather than an observed failure: the only drive here whose
// Composite reads 0 sits in a USB enclosure and is spared by an unrelated name
// mismatch (LHM names it by the drive inside, Windows by the enclosure), so
// nothing matches it. An internal drive that stopped reporting Composite would
// hit it.
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

// TestLhmBridgeScriptsKeepStorageOffTheSensorWalk guards the fix for the daemon
// restart loop. A storage node's Update() issues a per-drive SMART health-log
// ioctl costing 0.7-2.7 s, against single-digit milliseconds for every other
// sensor in the walk. Measured on a 4-NVMe host: the walk took 4.8-6.4 s with
// storage in it and 0.39 s without.
//
// With storage on the hot path every daemon read exceeded its deadline, so the
// agent killed and respawned the daemon on every sample and CPU power, CPU
// temperature and PSU power were never reported at all - reading drive temps
// cost the dashboard the metrics it exists to show. Both scripts must therefore
// classify storage nodes and skip them in their main sensor walk, reading them
// on a slower cadence instead.
func TestLhmBridgeScriptsKeepStorageOffTheSensorWalk(t *testing.T) {
	for _, name := range lhmBridgeScriptFiles {
		script := readLhmBridgeScript(t, name)
		if !strings.Contains(script, "function Test-StorageNode") {
			t.Errorf("%s: no Test-StorageNode helper; storage nodes cannot be kept off the per-read sensor walk", name)
			continue
		}
		if !strings.Contains(script, "'Storage'") {
			t.Errorf("%s: Test-StorageNode does not match the 'Storage' HardwareType", name)
		}
		if !strings.Contains(script, "if (Test-StorageNode $hw) { continue }") {
			t.Errorf("%s: the main sensor walk does not skip storage nodes; a per-drive SMART ioctl back on the hot path re-breaks the read deadline", name)
		}
	}
}

// TestLhmBridgeDaemonRefreshesStorageOnItsOwnCadence pins the daemon's storage
// rotation: at most one drive per read, gated on an interval, with the readings
// cached and re-emitted in between so the JSON contract is unchanged.
func TestLhmBridgeDaemonRefreshesStorageOnItsOwnCadence(t *testing.T) {
	script := readLhmBridgeScript(t, "lhm-bridge-daemon.ps1")
	for _, want := range []string{
		"$StorageRefreshIntervalMs = 15000",
		"SYSMON_LHM_STORAGE_MS",
		"function Update-StorageTemperatures",
		"$script:StorageTemps",
		"$script:StorageCursor",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("lhm-bridge-daemon.ps1: missing %q from the storage refresh cadence", want)
		}
	}
	// The cadence is worthless if the startup prime walks storage anyway: that
	// pass alone cost ~5 s of the cold path on a 4-NVMe host.
	if !strings.Contains(script, "$script:StorageNodes.Add($hw)") {
		t.Error("lhm-bridge-daemon.ps1: the startup prime does not collect storage nodes for out-of-band refresh")
	}
}
