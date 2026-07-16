package main

import (
	"math"
	"testing"
)

func availableProc(value float64, unit string) NumberMetric { return availableNumber(value, unit) }

func TestAppKeyStripsExeAndLowercases(t *testing.T) {
	cases := map[string]string{
		"Chrome.exe":     "chrome",
		"chrome":         "chrome",
		"CHROME.EXE":     "chrome",
		"  Firefox.exe ": "firefox",
		"/usr/bin/go":    "/usr/bin/go",
		"":               "unknown",
		"   ":            "unknown",
	}
	for input, want := range cases {
		if got := appKey(input); got != want {
			t.Errorf("appKey(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestStripExe(t *testing.T) {
	if got := stripExe("chrome.exe"); got != "chrome" {
		t.Errorf("stripExe(chrome.exe) = %q, want chrome", got)
	}
	if got := stripExe("CHROME.EXE"); got != "CHROME" {
		t.Errorf("stripExe(CHROME.EXE) = %q, want CHROME", got)
	}
	if got := stripExe("bash"); got != "bash" {
		t.Errorf("stripExe(bash) = %q, want bash", got)
	}
}

func TestAggregateByAppGroupsAndCounts(t *testing.T) {
	raw := []ProcessMetric{
		{PID: 1, Name: "chrome.exe", CPU: availableProc(10, "%"), Memory: availableProc(100, "B")},
		{PID: 2, Name: "Chrome.EXE", CPU: availableProc(20, "%"), Memory: availableProc(200, "B")},
		{PID: 3, Name: "firefox", CPU: availableProc(5, "%"), Memory: availableProc(50, "B")},
	}
	apps := aggregateByApp(raw)
	if len(apps) != 2 {
		t.Fatalf("aggregateByApp produced %d apps, want 2: %+v", len(apps), apps)
	}
	chrome := apps[0]
	if chrome.Name != "chrome" {
		t.Errorf("chrome app name = %q, want chrome", chrome.Name)
	}
	if chrome.Count != 2 {
		t.Errorf("chrome app count = %d, want 2", chrome.Count)
	}
	if !chrome.CPU.Available || chrome.CPU.Value != 30 {
		t.Errorf("chrome summed CPU = %+v, want 30", chrome.CPU)
	}
	if !chrome.Memory.Available || chrome.Memory.Value != 300 {
		t.Errorf("chrome summed memory = %+v, want 300", chrome.Memory)
	}
}

func TestAggregateByAppAllUnavailableDegrades(t *testing.T) {
	raw := []ProcessMetric{
		{PID: 1, Name: "x", CPU: unavailableNumber("%", "warm"), Memory: unavailableNumber("B", "warm")},
		{PID: 2, Name: "x", CPU: unavailableNumber("%", "warm"), Memory: unavailableNumber("B", "warm")},
	}
	apps := aggregateByApp(raw)
	if len(apps) != 1 {
		t.Fatalf("got %d apps, want 1", len(apps))
	}
	if apps[0].CPU.Available {
		t.Errorf("all-unavailable CPU should degrade, got %+v", apps[0].CPU)
	}
	if apps[0].Memory.Available {
		t.Errorf("all-unavailable memory should degrade, got %+v", apps[0].Memory)
	}
}

func TestAggregateByAppMixedAvailabilitySumsAvailableOnly(t *testing.T) {
	raw := []ProcessMetric{
		{PID: 1, Name: "x", GPUMemory: availableProc(512, "B")},
		{PID: 2, Name: "x", GPUMemory: unavailableNumber("B", "not cuda")},
		{PID: 3, Name: "x", GPUMemory: availableProc(2048, "B")},
	}
	apps := aggregateByApp(raw)
	if len(apps) != 1 {
		t.Fatalf("got %d apps, want 1", len(apps))
	}
	if !apps[0].GPUMemory.Available || apps[0].GPUMemory.Value != 2560 {
		t.Errorf("mixed GPU memory sum = %+v, want 2560 available", apps[0].GPUMemory)
	}
}

func TestBuildProcessSetEmptyReportsAvailable(t *testing.T) {
	got := buildProcessSet(nil, 42)
	if !got.Available {
		t.Errorf("empty process set should be available, got %+v", got)
	}
	if got.Total != 42 {
		t.Errorf("total = %d, want 42", got.Total)
	}
	if len(got.Apps) != 0 || len(got.Processes) != 0 {
		t.Errorf("empty process set should have no rows, got apps=%d processes=%d", len(got.Apps), len(got.Processes))
	}
}

func TestBuildProcessSetPopulatesBothViews(t *testing.T) {
	raw := []ProcessMetric{
		{PID: 1, Name: "a", CPU: availableProc(10, "%")},
		{PID: 2, Name: "b", CPU: availableProc(5, "%")},
	}
	got := buildProcessSet(raw, 5)
	if !got.Available {
		t.Fatalf("process set not available: %+v", got)
	}
	if got.Total != 5 {
		t.Errorf("total = %d, want 5", got.Total)
	}
	if len(got.Processes) != 2 {
		t.Errorf("processes len = %d, want 2", len(got.Processes))
	}
	if len(got.Apps) != 2 {
		t.Errorf("apps len = %d, want 2", len(got.Apps))
	}
}

func TestSelectTopProcessesReturnsAllWhenUnderN(t *testing.T) {
	raw := []ProcessMetric{
		{PID: 1, CPU: availableProc(1, "%")},
		{PID: 2, CPU: availableProc(3, "%")},
		{PID: 3, CPU: availableProc(2, "%")},
	}
	got := selectTopProcesses(raw, 15)
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3", len(got))
	}
	// sorted by CPU desc
	if got[0].PID != 2 || got[1].PID != 3 || got[2].PID != 1 {
		t.Errorf("rows not sorted by CPU desc: %+v", got)
	}
}

func TestSelectTopProcessesUnionsColumnLeaders(t *testing.T) {
	// 20 processes: PIDs 1..10 are CPU leaders, 11..20 are memory leaders.
	raw := make([]ProcessMetric, 20)
	for i := 0; i < 10; i++ {
		raw[i] = ProcessMetric{PID: i + 1, Name: "cpuheavy", CPU: availableProc(float64(100-i), "%")}
	}
	for i := 10; i < 20; i++ {
		raw[i] = ProcessMetric{PID: i + 1, Name: "ramheavy", Memory: availableProc(float64(1000000*(20-i)), "B")}
	}
	got := selectTopProcesses(raw, 5)
	seen := map[int]bool{}
	for _, p := range got {
		seen[p.PID] = true
	}
	// The union should surface at least one RAM leader (PID 11) that is NOT a
	// CPU leader, proving the union of per-column top-N works.
	if !seen[11] {
		t.Errorf("union did not surface RAM leader PID 11; got PIDs: %v", mapKeys(seen))
	}
	if len(got) > processTopHardCap {
		t.Errorf("union exceeded hard cap: got %d, max %d", len(got), processTopHardCap)
	}
}

func TestSelectTopProcessesCapsAtHardLimit(t *testing.T) {
	// 60 distinct processes with diverging CPU and memory leaders so the union
	// can grow large; it must stay within processTopHardCap.
	raw := make([]ProcessMetric, 60)
	for i := 0; i < 60; i++ {
		raw[i] = ProcessMetric{
			PID:    i + 1,
			CPU:    availableProc(float64(60-i), "%"),
			Memory: availableProc(float64(1000000*(60-i)), "B"),
		}
	}
	got := selectTopProcesses(raw, 5)
	if len(got) > processTopHardCap {
		t.Errorf("union exceeded hard cap: got %d, max %d", len(got), processTopHardCap)
	}
}

func TestSelectTopAppsMirrorsProcesses(t *testing.T) {
	apps := []AppMetric{
		{Name: "a", CPU: availableProc(1, "%")},
		{Name: "b", CPU: availableProc(3, "%")},
	}
	got := selectTopApps(apps, 15)
	if len(got) != 2 || got[0].Name != "b" || got[1].Name != "a" {
		t.Errorf("selectTopApps = %+v, want b then a", got)
	}
}

func TestUnavailableProcessSet(t *testing.T) {
	got := unavailableProcessSet("boom")
	if got.Available {
		t.Errorf("unavailableProcessSet should be unavailable: %+v", got)
	}
	if got.Error != "boom" {
		t.Errorf("error = %q, want boom", got.Error)
	}
}

func TestMetricValueOrZero(t *testing.T) {
	if v := metricValueOrZero(availableProc(5, "%")); v != 5 {
		t.Errorf("available value = %v, want 5", v)
	}
	if v := metricValueOrZero(unavailableNumber("%", "x")); v != 0 {
		t.Errorf("unavailable value = %v, want 0", v)
	}
	if v := metricValueOrZero(NumberMetric{Available: true, Value: math.NaN()}); v != 0 {
		t.Errorf("NaN value = %v, want 0", v)
	}
}

func TestProcessCPUPercent(t *testing.T) {
	// 1 CPU-second of delta over 1 second on an 8-core host = 100% of one core
	// = 12.5% whole-host.
	got := processCPUPercent(3, 2, 1.0, 8)
	if !got.Available || got.Value != 12.5 {
		t.Errorf("processCPUPercent = %+v, want 12.5", got)
	}
	// 8 CPU-seconds over 1 second on 8 cores fully saturates the host = 100%.
	full := processCPUPercent(10, 2, 1.0, 8)
	if !full.Available || full.Value != 100 {
		t.Errorf("processCPUPercent = %+v, want 100", full)
	}
	if reset := processCPUPercent(1, 5, 1.0, 8); reset.Available {
		t.Errorf("counter reset should degrade, got %+v", reset)
	}
	if bad := processCPUPercent(5, 1, 0, 8); bad.Available {
		t.Errorf("zero elapsed should degrade, got %+v", bad)
	}
}

func TestAggregateByAppClampsCPUAtHundred(t *testing.T) {
	// Two PIDs in one app whose whole-host CPU% sums past 100 (a sampling
	// artifact under full load). The aggregated app CPU must clamp to 100 so the
	// strict >100 readiness shape check never trips on a live snapshot.
	raw := []ProcessMetric{
		{PID: 1, Name: "chrome", CPU: availableProc(70, "%")},
		{PID: 2, Name: "chrome", CPU: availableProc(45, "%")},
	}
	apps := aggregateByApp(raw)
	if len(apps) != 1 {
		t.Fatalf("got %d apps, want 1", len(apps))
	}
	if !apps[0].CPU.Available || apps[0].CPU.Value != 100 {
		t.Errorf("app CPU = %+v, want clamped to 100", apps[0].CPU)
	}
}

func TestClampPercentLeavesNormalValues(t *testing.T) {
	if got := clampPercent(availableProc(42, "%")); !got.Available || got.Value != 42 {
		t.Errorf("clampPercent(42) = %+v, want 42 unchanged", got)
	}
	if got := clampPercent(unavailableNumber("%", "x")); got.Available {
		t.Errorf("clampPercent(unavailable) should stay unavailable, got %+v", got)
	}
}

func TestSumProcessFieldIgnoresInvalidAvailableValues(t *testing.T) {
	raw := []ProcessMetric{
		{CPU: availableProc(10, "%")},
		{CPU: NumberMetric{Available: true, Value: math.NaN(), Unit: "%"}},
		{CPU: availableProc(5, "%")},
	}
	got := sumProcessField(raw, func(p ProcessMetric) NumberMetric { return p.CPU }, "%")
	if !got.Available || got.Value != 15 {
		t.Errorf("sum = %+v, want 15 (NaN ignored)", got)
	}
}

func mapKeys(m map[int]bool) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
