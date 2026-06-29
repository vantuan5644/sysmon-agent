//go:build linux

package main

import "testing"

func TestParsePerCoreCPUTimes(t *testing.T) {
	data := "cpu  100 0 100 800 0 0 0 0 0 0\n" +
		"cpu0 50 0 50 100 0 0 0 0 0 0\n" +
		"cpu1 10 0 10 380 0 0 0 0 0 0\n" +
		"intr 12345\n" +
		"cpufoo 1 2 3 4\n"
	cores, err := parsePerCoreCPUTimes(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(cores) != 2 {
		t.Fatalf("len(cores) = %d, want 2 (aggregate and non-core lines must be skipped)", len(cores))
	}
	if cores[0].total != 200 || cores[0].idle != 100 {
		t.Fatalf("cpu0 = %+v, want total 200 idle 100", cores[0])
	}
	if cores[1].total != 400 || cores[1].idle != 380 {
		t.Fatalf("cpu1 = %+v, want total 400 idle 380", cores[1])
	}
}

func TestParsePerCoreCPUTimesRequiresCoreLines(t *testing.T) {
	if _, err := parsePerCoreCPUTimes("cpu  100 0 100 800\nintr 1\n"); err == nil {
		t.Fatal("parsePerCoreCPUTimes accepted /proc/stat with no per-core lines")
	}
}

func TestPerCoreUsage(t *testing.T) {
	prev := []cpuTimes{{idle: 100, total: 200}, {idle: 380, total: 400}}
	now := []cpuTimes{{idle: 100, total: 300}, {idle: 480, total: 500}}
	set := perCoreUsage(prev, now)
	if !set.Available {
		t.Fatalf("perCoreUsage set unavailable: %s", set.Error)
	}
	if set.Count != 2 {
		t.Fatalf("count = %d, want 2", set.Count)
	}
	if set.Cores[0] != 100 {
		t.Fatalf("core0 = %v, want 100 (all non-idle ticks)", set.Cores[0])
	}
	if set.Cores[1] != 0 {
		t.Fatalf("core1 = %v, want 0 (all idle ticks)", set.Cores[1])
	}
	if set.Busy != 1 {
		t.Fatalf("busy = %d, want 1 core at/above %.0f%%", set.Busy, cpuCoreBusyPercent)
	}
}

func TestAvailableCPUCoresBusyCount(t *testing.T) {
	set := availableCPUCores([]float64{95, 80, 79.9, 0})
	if set.Busy != 2 {
		t.Fatalf("busy = %d, want 2 (>= %.0f%%)", set.Busy, cpuCoreBusyPercent)
	}
	if set.BusyThreshold != cpuCoreBusyPercent {
		t.Fatalf("busy_threshold = %v, want %v", set.BusyThreshold, cpuCoreBusyPercent)
	}
	if empty := availableCPUCores(nil); empty.Available {
		t.Fatal("availableCPUCores(nil) reported available; want degraded")
	}
}
