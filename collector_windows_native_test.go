//go:build windows

package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestReadWindowsProcessesIncludesCurrentProcess(t *testing.T) {
	rows, err := readWindowsProcesses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.pid != os.Getpid() {
			continue
		}
		if row.name == "" || !row.cpuOK || !row.memoryOK || !row.diskOK {
			t.Fatalf("current process counters incomplete: %+v", row)
		}
		if row.created == 0 {
			t.Fatal("current process has no creation timestamp")
		}
		return
	}
	t.Fatalf("current PID %d missing from native process snapshot", os.Getpid())
}

func TestWindowsProcessIORatesUseCumulativeDeltas(t *testing.T) {
	now := time.Unix(100, 0)
	prev := procSample{ts: now.Add(-2 * time.Second), diskRead: 100, diskWrite: 200, diskAvail: true}
	row := windowsNativeProcessRow{diskRead: 500, diskWrite: 800, diskOK: true}
	read, write := windowsProcessIORates(row, prev, true, now)
	if !read.Available || read.Value != 200 {
		t.Fatalf("read rate = %+v, want 200 B/s", read)
	}
	if !write.Available || write.Value != 300 {
		t.Fatalf("write rate = %+v, want 300 B/s", write)
	}
}

func TestWindowsProcessIORatesWarmOnPIDReuse(t *testing.T) {
	now := time.Unix(100, 0)
	prev := procSample{ts: now.Add(-time.Second), diskRead: 100, diskWrite: 200, diskAvail: true}
	row := windowsNativeProcessRow{diskRead: 500, diskWrite: 800, diskOK: true}
	read, write := windowsProcessIORates(row, prev, false, now)
	if read.Available || write.Available {
		t.Fatalf("reused PID must warm up I/O counters: read=%+v write=%+v", read, write)
	}
}

func TestSameWindowsProcessRejectsReusedPID(t *testing.T) {
	prev := procSample{created: 100}
	if sameWindowsProcess(prev, true, windowsNativeProcessRow{created: 200, cpuOK: true}) {
		t.Fatal("different creation timestamps must be treated as PID reuse")
	}
	if !sameWindowsProcess(prev, true, windowsNativeProcessRow{created: 100, cpuOK: true}) {
		t.Fatal("matching PID creation timestamps should continue the sample")
	}
}

func TestWindowsSlowCachesReturnFreshValuesWithoutCollection(t *testing.T) {
	now := time.Now()
	c := &systemCollector{
		swapCached:         availableCapacity(1, 2),
		swapCachedAt:       now,
		storageRows:        []windowsPhysicalDisk{{Name: "PhysicalDrive0", Model: "test", SizeBytes: 10}},
		storageRowsAt:      now,
		storageAttemptAt:   now,
		tailscaleCached:    TailscaleStatus{Available: true, Online: true},
		tailscaleAt:        now,
		gpuCached:          GPUSet{Available: true},
		gpuCachedAt:        now,
		processGPUMemory:   map[int]int64{42: 1024},
		processGPUMemoryAt: now,
		acpiSensors:        []TemperatureMetric{{Name: "cached"}},
		acpiSensorsAt:      now,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := c.cachedWindowsSwap(ctx); !got.Available || got.TotalBytes != 2 {
		t.Fatalf("cached swap = %+v", got)
	}
	rows, err := c.cachedWindowsStorageRows(ctx)
	if err != nil || len(rows) != 1 || rows[0].Name != "PhysicalDrive0" {
		t.Fatalf("cached storage rows = %+v, %v", rows, err)
	}
	if got := c.cachedTailscaleStatus(ctx); !got.Available || !got.Online {
		t.Fatalf("cached Tailscale status = %+v", got)
	}
	if got := c.cachedWindowsGPU(ctx); !got.Available {
		t.Fatalf("cached GPU status = %+v", got)
	}
	if got := c.cachedNVIDIAProcessGPUMemory(ctx); got[42] != 1024 {
		t.Fatalf("cached process GPU memory = %+v", got)
	}
	if got := c.cachedWindowsACPITemperatures(ctx); len(got) != 1 || got[0].Name != "cached" {
		t.Fatalf("cached ACPI temperatures = %+v", got)
	}
}

func TestWindowsNetCountersUseNativeInterfaceTable(t *testing.T) {
	counters, err := windowsNetCounters(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(counters) == 0 {
		t.Fatal("native interface table returned no rows")
	}
	for name := range counters {
		if name == "" {
			t.Fatal("native interface table returned an empty name")
		}
	}
}

// TestRunPowerShellNamesADeadlineKill pins the message a cut-short query
// produces. Go kills a timed-out child with TerminateProcess(handle, 1), so the
// raw failure is a bare "exit status 1" with both streams empty - the dashboard
// reported that verbatim as "storage: exit status 1" while the storage script
// itself ran in 2.0 s and exited 0, which sent the diagnosis after a PowerShell
// bug that did not exist.
func TestRunPowerShellNamesADeadlineKill(t *testing.T) {
	_, err := runPowerShellWithTimeout(context.Background(), "Start-Sleep -Seconds 30", time.Second)
	if err == nil {
		t.Fatal("a query killed at its deadline must report an error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("deadline kill reported %q, want a timeout", err)
	}
	if strings.Contains(err.Error(), "exit status") {
		t.Fatalf("deadline kill leaked the kill exit status: %q", err)
	}
}

// TestWindowsStorageRowsSurviveAFailedRefresh covers the other half of that
// defect: one failed discovery pass used to blank the storage panel (and raise
// a collection error) until the 5-minute TTL expired, even though the previous
// rows were still correct.
func TestWindowsStorageRowsSurviveAFailedRefresh(t *testing.T) {
	now := time.Now()
	rows := []windowsPhysicalDisk{{Name: "PhysicalDrive0", Model: "test", SizeBytes: 10}}
	failure := errors.New("powershell query timed out after 8s")

	got, err := windowsStorageRowsOrError(rows, failure, now.Add(-time.Minute), now)
	if err != nil || len(got) != 1 {
		t.Fatalf("recent rows = %+v, %v; want them served through the failure", got, err)
	}

	got, err = windowsStorageRowsOrError(rows, failure, now.Add(-windowsStorageStaleGrace-time.Minute), now)
	if err == nil || got != nil {
		t.Fatalf("rows past the grace window = %+v, %v; want the failure surfaced", got, err)
	}

	got, err = windowsStorageRowsOrError(nil, failure, time.Time{}, now)
	if err == nil || got != nil {
		t.Fatalf("failure with no prior success = %+v, %v; want the failure surfaced", got, err)
	}
}

// TestWindowsStorageRetriesFailuresBeforeTheSuccessTTL pins the shorter retry
// window. Caching a failure for the full 5 minutes meant a momentary miss held
// the panel empty long after the condition had passed.
func TestWindowsStorageRetriesFailuresBeforeTheSuccessTTL(t *testing.T) {
	if windowsStorageRetryAfter >= windowsStorageCacheTTL {
		t.Fatalf("retry window %s must be shorter than the success TTL %s", windowsStorageRetryAfter, windowsStorageCacheTTL)
	}
	sentinel := errors.New("cached sentinel failure")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Inside the retry window the cached failure is reused untouched.
	held := &systemCollector{
		storageRows:      []windowsPhysicalDisk{{Name: "PhysicalDrive0"}},
		storageRowsAt:    time.Now().Add(-windowsStorageStaleGrace - time.Minute),
		storageRowsErr:   sentinel,
		storageAttemptAt: time.Now().Add(-windowsStorageRetryAfter / 2),
	}
	if _, err := held.cachedWindowsStorageRows(ctx); !errors.Is(err, sentinel) {
		t.Fatalf("inside the retry window the cached failure was replaced: %v", err)
	}

	// Past it, a fresh pass runs - here against a canceled context, so the
	// error is the new attempt's rather than the sentinel.
	retried := &systemCollector{
		storageRows:      []windowsPhysicalDisk{{Name: "PhysicalDrive0"}},
		storageRowsAt:    time.Now().Add(-windowsStorageStaleGrace - time.Minute),
		storageRowsErr:   sentinel,
		storageAttemptAt: time.Now().Add(-windowsStorageRetryAfter - time.Second),
	}
	_, err := retried.cachedWindowsStorageRows(ctx)
	if err == nil || errors.Is(err, sentinel) {
		t.Fatalf("past the retry window no fresh pass ran: %v", err)
	}
}
