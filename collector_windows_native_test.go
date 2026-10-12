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

// TestWindowsLiveParentRejectsAReusedPID: Windows never reparents, so a child
// whose parent exited still reports the old PID, and a newer process may hold
// it now. A parent younger than its child is that case and must be dropped;
// an unreadable creation time keeps the link.
func TestWindowsLiveParentRejectsAReusedPID(t *testing.T) {
	created := map[int]uint64{100: 1_000, 200: 5_000, 300: 0}
	cases := []struct {
		name string
		row  windowsNativeProcessRow
		want int
	}{
		{"older parent", windowsNativeProcessRow{pid: 400, ppid: 100, created: 2_000}, 100},
		{"parent younger than child", windowsNativeProcessRow{pid: 401, ppid: 200, created: 2_000}, 0},
		{"parent creation unreadable", windowsNativeProcessRow{pid: 402, ppid: 300, created: 2_000}, 300},
		{"child creation unreadable", windowsNativeProcessRow{pid: 403, ppid: 200, created: 0}, 200},
		{"parent not in this pass", windowsNativeProcessRow{pid: 404, ppid: 999, created: 2_000}, 0},
		{"self-parented", windowsNativeProcessRow{pid: 100, ppid: 100, created: 1_000}, 0},
	}
	for _, tc := range cases {
		if got := windowsLiveParent(tc.row, created); got != tc.want {
			t.Fatalf("%s: windowsLiveParent = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestReadWindowsProcessesReportsParents: the live Toolhelp walk must carry a
// parent for this test process, which some process necessarily started.
func TestReadWindowsProcessesReportsParents(t *testing.T) {
	rows, err := readWindowsProcesses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	self := os.Getpid()
	for _, row := range rows {
		if row.pid == self {
			if row.ppid != os.Getppid() {
				t.Fatalf("own ppid = %d, want %d", row.ppid, os.Getppid())
			}
			return
		}
	}
	t.Fatalf("own pid %d not enumerated", self)
}

// TestWindowsProcessSetReportsTheAgentsOwnCost runs the real enumeration twice
// (CPU needs a delta) and expects the test binary to find itself.
func TestWindowsProcessSetReportsTheAgentsOwnCost(t *testing.T) {
	c := NewSystemCollector().(*systemCollector)
	c.collectProcesses(context.Background(), unavailableGPUEngineSummary("not sampled in this test"))
	time.Sleep(300 * time.Millisecond)
	set := c.collectProcesses(context.Background(), unavailableGPUEngineSummary("not sampled in this test"))
	if set.Agent == nil || !set.Agent.Available {
		t.Fatalf("agent cost = %+v", set.Agent)
	}
	if set.Agent.PID != os.Getpid() || set.Agent.Processes < 1 {
		t.Fatalf("agent cost = %+v, want this test process", set.Agent)
	}
	if !set.Agent.Memory.Available || set.Agent.Memory.Value <= 0 {
		t.Fatalf("agent memory = %+v", set.Agent.Memory)
	}
}

// TestWindowsMemoryStatusFastReportsCommitCharge: the commit limit is RAM plus
// page files, so it sits near or above physical memory and is never tiny.
func TestWindowsMemoryStatusFastReportsCommitCharge(t *testing.T) {
	memory, commit := windowsMemoryStatusFast()
	if !memory.Available || !commit.Available {
		t.Fatalf("memory=%+v commit=%+v", memory, commit)
	}
	if commit.TotalBytes < memory.TotalBytes/2 || commit.UsedBytes == 0 {
		t.Fatalf("commit %+v is implausible next to RAM %+v", commit, memory)
	}
}

// TestReadWindowsDiskPerformanceReadsPhysicalDrive0: every Windows machine has
// a PhysicalDrive0, and the counters are cumulative since boot.
func TestReadWindowsDiskPerformanceReadsPhysicalDrive0(t *testing.T) {
	counter, err := readWindowsDiskPerformance("PhysicalDrive0")
	if err != nil {
		t.Fatal(err)
	}
	if counter.read == 0 && counter.write == 0 {
		t.Fatalf("PhysicalDrive0 reports no I/O since boot: %+v", counter)
	}
	if _, err := readWindowsDiskPerformance("C:"); err == nil {
		t.Fatal("a volume name was accepted as a physical drive")
	}
}

// TestGPUEngineSamplerReadsTheLiveCounters: the first collection only primes
// the rate counter; the second returns instances. A machine without GPU
// Engine counters (a VM with no GPU driver) skips.
func TestGPUEngineSamplerReadsTheLiveCounters(t *testing.T) {
	var sampler gpuEngineSampler
	defer sampler.close()
	first := sampler.summary()
	if !first.Available && strings.Contains(first.Error, "no GPU Engine performance counters") {
		t.Skip(first.Error)
	}
	if first.Available || first.Error != errGPUEnginesWarming.Error() {
		t.Fatalf("first summary = %+v, want warming up", first)
	}
	time.Sleep(300 * time.Millisecond)
	second := sampler.summary()
	if !second.Available || len(second.Instances) == 0 {
		t.Fatalf("second summary = %+v", second)
	}
}

// TestEnumerateDXGIAdaptersListsTheSoftwareRenderer: every Windows install
// has the Microsoft Basic Render Driver, flagged as a software adapter.
func TestEnumerateDXGIAdaptersListsTheSoftwareRenderer(t *testing.T) {
	adapters, err := enumerateDXGIAdapters()
	if err != nil {
		t.Fatal(err)
	}
	software := false
	for _, adapter := range adapters {
		if adapter.Name == "" || !strings.HasPrefix(adapter.LUID, "0x") {
			t.Fatalf("malformed adapter %+v", adapter)
		}
		software = software || adapter.Software
	}
	if !software {
		t.Fatalf("no software adapter among %+v", adapters)
	}
}

// TestGPUEngineWarmSummaryNeverOpensTheQuery: the direct Collect path must not
// pay the one-time PDH open.
func TestGPUEngineWarmSummaryNeverOpensTheQuery(t *testing.T) {
	var sampler gpuEngineSampler
	defer sampler.close()
	if got := sampler.warmSummary(); got.Available || got.Error != errGPUEnginesWarming.Error() {
		t.Fatalf("warm summary on a fresh sampler = %+v", got)
	}
	sampler.mu.Lock()
	opened := sampler.opened
	sampler.mu.Unlock()
	if opened {
		t.Fatal("warmSummary opened the PDH query")
	}
}
