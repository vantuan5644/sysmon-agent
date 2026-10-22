//go:build windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
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
	var calls atomic.Int32
	discover := func() ([]windowsPhysicalDisk, error) {
		calls.Add(1)
		return []windowsPhysicalDisk{{Name: "PhysicalDrive1"}}, nil
	}
	ctx := context.Background()

	// Inside the retry window the cached failure is reused untouched.
	held := &systemCollector{
		storageRows:      []windowsPhysicalDisk{{Name: "PhysicalDrive0"}},
		storageRowsAt:    time.Now().Add(-windowsStorageStaleGrace - time.Minute),
		storageRowsErr:   sentinel,
		storageAttemptAt: time.Now().Add(-windowsStorageRetryAfter / 2),
		storageDiscover:  discover,
	}
	if _, err := held.cachedWindowsStorageRows(ctx); !errors.Is(err, sentinel) {
		t.Fatalf("inside the retry window the cached failure was replaced: %v", err)
	}
	waitWindowsStorageRefresh(t, held)
	if calls.Load() != 0 {
		t.Fatal("a discovery ran inside the retry window")
	}

	// Past it, a fresh pass runs and its rows replace the failure.
	retried := &systemCollector{
		storageRows:      []windowsPhysicalDisk{{Name: "PhysicalDrive0"}},
		storageRowsAt:    time.Now().Add(-windowsStorageStaleGrace - time.Minute),
		storageRowsErr:   sentinel,
		storageAttemptAt: time.Now().Add(-windowsStorageRetryAfter - time.Second),
		storageDiscover:  discover,
	}
	retried.cachedWindowsStorageRows(ctx)
	waitWindowsStorageRefresh(t, retried)
	rows, err := retried.cachedWindowsStorageRows(ctx)
	if err != nil || len(rows) != 1 || rows[0].Name != "PhysicalDrive1" {
		t.Fatalf("past the retry window = %+v, %v; want the fresh discovery", rows, err)
	}
}

// waitWindowsStorageRefresh waits for a background storage discovery, if one
// is running, to finish.
func waitWindowsStorageRefresh(t *testing.T, c *systemCollector) {
	t.Helper()
	c.mu.Lock()
	pending := c.storageRefresh
	c.mu.Unlock()
	if pending == nil {
		return
	}
	select {
	case <-pending:
	case <-time.After(5 * time.Second):
		t.Fatal("storage discovery did not finish")
	}
}

// TestWindowsStorageRefreshNeverBlocksTheSlowLane pins the second half of the
// ZEPHYR recorder defect: discovery used to run inside the slow-lane pass, so a
// stalled one held power, temperatures, GPU and network back with it. A due
// refresh now runs in the background, the caller keeps the last good rows,
// and a second pass while it runs starts no other.
func TestWindowsStorageRefreshNeverBlocksTheSlowLane(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	stale := time.Now().Add(-windowsStorageCacheTTL - time.Minute)
	c := &systemCollector{
		storageRows:      []windowsPhysicalDisk{{Name: "PhysicalDrive0"}},
		storageRowsAt:    stale,
		storageAttemptAt: stale,
		storageDiscover: func() ([]windowsPhysicalDisk, error) {
			calls.Add(1)
			<-release
			return []windowsPhysicalDisk{{Name: "PhysicalDrive9"}}, nil
		},
	}
	ctx := context.Background()

	started := time.Now()
	rows, err := c.cachedWindowsStorageRows(ctx)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("a due refresh held the caller for %s", elapsed)
	}
	if err != nil || len(rows) != 1 || rows[0].Name != "PhysicalDrive0" {
		t.Fatalf("during the refresh = %+v, %v; want the last good rows", rows, err)
	}
	if _, err := c.cachedWindowsStorageRows(ctx); err != nil {
		t.Fatalf("second pass during the refresh: %v", err)
	}

	close(release)
	waitWindowsStorageRefresh(t, c)
	rows, err = c.cachedWindowsStorageRows(ctx)
	if err != nil || len(rows) != 1 || rows[0].Name != "PhysicalDrive9" {
		t.Fatalf("after the refresh = %+v, %v; want its rows", rows, err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("%d discoveries ran, want 1", n)
	}
}

// TestWindowsStorageReportsAnUnfinishedDiscovery: a discovery stuck on a hung
// device must not leave the panel on rows that are silently going stale. The
// last good rows carry it through the grace window, then the stuck refresh is
// named.
func TestWindowsStorageReportsAnUnfinishedDiscovery(t *testing.T) {
	stuck := make(chan struct{})
	ctx := context.Background()

	recent := &systemCollector{
		storageRows:      []windowsPhysicalDisk{{Name: "PhysicalDrive0"}},
		storageRowsAt:    time.Now().Add(-windowsStorageCacheTTL - time.Minute),
		storageAttemptAt: time.Now().Add(-windowsStorageCacheTTL - time.Minute),
		storageRefresh:   stuck,
		storageRefreshAt: time.Now().Add(-time.Minute),
	}
	if rows, err := recent.cachedWindowsStorageRows(ctx); err != nil || len(rows) != 1 {
		t.Fatalf("inside the grace window = %+v, %v; want the last good rows", rows, err)
	}

	expired := time.Now().Add(-windowsStorageStaleGrace - time.Minute)
	gone := &systemCollector{
		storageRows:      []windowsPhysicalDisk{{Name: "PhysicalDrive0"}},
		storageRowsAt:    expired,
		storageAttemptAt: expired,
		storageRefresh:   stuck,
		storageRefreshAt: expired,
	}
	if _, err := gone.cachedWindowsStorageRows(ctx); err == nil || !strings.Contains(err.Error(), "has not finished") {
		t.Fatalf("past the grace window = %v; want the unfinished discovery named", err)
	}
}

// TestWindowsStorageFirstSampleWaitsForTheFirstDiscovery: with nothing cached
// the first pass (and -self-check, which takes only one) waits briefly so it
// reports drives instead of a placeholder, but never past its context.
func TestWindowsStorageFirstSampleWaitsForTheFirstDiscovery(t *testing.T) {
	if windowsStorageDiscoveryWait <= windowsDeviceReadBudget {
		t.Fatalf("first-sample wait %s must cover the per-device budget %s, or a slow drive at startup leaves the first sample without rows",
			windowsStorageDiscoveryWait, windowsDeviceReadBudget)
	}
	quick := &systemCollector{storageDiscover: func() ([]windowsPhysicalDisk, error) {
		time.Sleep(50 * time.Millisecond)
		return []windowsPhysicalDisk{{Name: "PhysicalDrive0"}}, nil
	}}
	if rows, err := quick.cachedWindowsStorageRows(context.Background()); err != nil || len(rows) != 1 {
		t.Fatalf("first pass = %+v, %v; want the first discovery's rows", rows, err)
	}

	release := make(chan struct{})
	defer close(release)
	hung := &systemCollector{storageDiscover: func() ([]windowsPhysicalDisk, error) {
		<-release
		return nil, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := hung.cachedWindowsStorageRows(ctx); err == nil || !strings.Contains(err.Error(), "has not finished") {
		t.Fatalf("first pass with a hung discovery = %v; want the unfinished discovery named", err)
	}

	panics := &systemCollector{storageDiscover: func() ([]windowsPhysicalDisk, error) {
		panic("boom")
	}}
	if _, err := panics.cachedWindowsStorageRows(context.Background()); err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("panicking discovery = %v; want it recovered into an error", err)
	}
}

// TestDiscoverWindowsPhysicalDisksMatchesTheStorageProvider holds the native
// discovery to what it replaced. Names, sizes and bus types come from
// MSFT_PhysicalDisk; the model must equal its FriendlyName exactly, because
// the LibreHardwareMonitor temperature match looks for that name. Drive
// letters are checked against Win32_LogicalDiskToPartition, which goes through
// a different provider (and stays fast while a slow drive is attached).
//
// Discovery runs first: the MSFT_PhysicalDisk query sends a slow drive the
// very commands that stall it, and a discovery right behind it would wait in
// that drive's queue. A drive that misses windowsDeviceReadBudget anyway is
// listed with no size by design, so only answered sizes are compared.
func TestDiscoverWindowsPhysicalDisksMatchesTheStorageProvider(t *testing.T) {
	rows, err := discoverWindowsPhysicalDisks(nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	var want []struct {
		DeviceID     string `json:"DeviceId"`
		FriendlyName string
		Size         uint64
		BusType      uint16
	}
	if err := runPowerShellJSONArrayFor(ctx, `Get-CimInstance -Namespace root/Microsoft/Windows/Storage -ClassName MSFT_PhysicalDisk -ErrorAction Stop | `+
		`ForEach-Object { [ordered]@{DeviceId=[string]$_.DeviceId; FriendlyName=[string]$_.FriendlyName; Size=[uint64]$_.Size; `+
		`BusType=[uint16]$_.CimInstanceProperties['BusType'].Value} }`, &want); err != nil {
		t.Skipf("MSFT_PhysicalDisk unavailable on this host: %v", err)
	}
	var letters []struct {
		Letter string
		Disk   int
	}
	if err := runPowerShellJSONArrayFor(ctx, `Get-CimInstance Win32_LogicalDiskToPartition -ErrorAction Stop | `+
		`ForEach-Object { [ordered]@{Letter=[string]$_.Dependent.DeviceID; `+
		`Disk=[int]([regex]::Match([string]$_.Antecedent.DeviceID, 'Disk #(\d+)').Groups[1].Value)} }`, &letters); err != nil {
		t.Skipf("Win32_LogicalDiskToPartition unavailable on this host: %v", err)
	}

	got := make(map[string]windowsPhysicalDisk, len(rows))
	for _, row := range rows {
		got[row.Name] = row
	}
	if len(rows) != len(want) {
		t.Fatalf("discovered %d drives %+v, the storage provider reports %d %+v", len(rows), rows, len(want), want)
	}
	sized := 0
	for _, disk := range want {
		row, ok := got["PhysicalDrive"+disk.DeviceID]
		if !ok {
			t.Fatalf("PhysicalDrive%s (%s) not discovered: %+v", disk.DeviceID, disk.FriendlyName, rows)
		}
		if row.SizeBytes == 0 {
			t.Logf("%s (%s) did not report its size in time", row.Name, row.Model)
		} else {
			sized++
		}
		if row.Model != strings.TrimSpace(disk.FriendlyName) || (row.SizeBytes != 0 && row.SizeBytes != disk.Size) || row.BusType != disk.BusType {
			t.Fatalf("%s = model %q size %d bus %d; storage provider says %q %d %d",
				row.Name, row.Model, row.SizeBytes, row.BusType, disk.FriendlyName, disk.Size, disk.BusType)
		}
	}
	// The system drive answers from cached geometry, so a discovery that sized
	// nothing has lost the size read rather than met a slow drive.
	if sized == 0 {
		t.Fatalf("no drive reported a size: %+v", rows)
	}
	for _, letter := range letters {
		row := got["PhysicalDrive"+strconv.Itoa(letter.Disk)]
		if !slices.Contains(row.Mountpoints, letter.Letter) || row.TotalBytes == 0 {
			t.Fatalf("%s lives on disk %d, but discovery has that drive as %+v", letter.Letter, letter.Disk, row)
		}
	}
}

// TestDiscoverWindowsPhysicalDisksLeavesOutExcludedDrives excludes one real
// drive by its model and expects exactly that drive gone, its drive letters
// with it, and every other drive still listed.
func TestDiscoverWindowsPhysicalDisksLeavesOutExcludedDrives(t *testing.T) {
	all, err := discoverWindowsPhysicalDisks(nil)
	if err != nil {
		t.Fatal(err)
	}
	var target windowsPhysicalDisk
	for _, row := range all {
		if row.Model != "" && !slices.ContainsFunc(all, func(other windowsPhysicalDisk) bool {
			return other.Name != row.Name && parseStorageExclusions(row.Model).excludes(other.Model)
		}) {
			target = row
			break
		}
	}
	if target.Name == "" {
		t.Skipf("no drive with a model unique enough to exclude alone: %+v", all)
	}

	rest, err := discoverWindowsPhysicalDisks(parseStorageExclusions(target.Model))
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != len(all)-1 {
		t.Fatalf("excluding %q left %+v, want %d drives", target.Model, rest, len(all)-1)
	}
	for _, row := range rest {
		if row.Name == target.Name {
			t.Fatalf("excluded drive %s (%s) still listed", row.Name, row.Model)
		}
		for _, letter := range target.Mountpoints {
			if slices.Contains(row.Mountpoints, letter) {
				t.Fatalf("%s from the excluded drive turned up on %s", letter, row.Name)
			}
		}
	}
}

// TestReadWindowsDriveTemperatureReadsTheLiveDrives reads the real thing. On
// BBLWIN it measured 1-6 ms per NVMe drive and matched LibreHardwareMonitor.
// A SATA drive may have no temperature to give, which must then fail as
// unsupported rather than some other way.
func TestReadWindowsDriveTemperatureReadsTheLiveDrives(t *testing.T) {
	rows, err := discoverWindowsPhysicalDisks(nil)
	if err != nil {
		t.Fatal(err)
	}
	read := 0
	for _, row := range rows {
		if !driveTemperatureReadable(row.BusType) {
			continue
		}
		celsius, err := readWindowsDriveTemperature(row.Name)
		if err != nil && row.BusType != storageBusTypeNVMe && windowsDriveTemperatureUnsupported(err) {
			t.Logf("%s (%s) does not report a temperature: %v", row.Name, row.Model, err)
			continue
		}
		if err != nil {
			t.Fatalf("%s (%s): %v", row.Name, row.Model, err)
		}
		if celsius < 5 || celsius > 100 {
			t.Fatalf("%s (%s) reads an implausible %v C", row.Name, row.Model, celsius)
		}
		read++
	}
	if read == 0 {
		t.Skip("no drive on this host reports a temperature")
	}
}

// runPowerShellJSONArrayFor is runPowerShellJSONArray with a budget a busy
// storage provider fits in: the ground-truth queries above must not flake on
// the host this suite runs on.
func runPowerShellJSONArrayFor(ctx context.Context, script string, target any) error {
	out, err := runPowerShellWithTimeout(ctx, "$items = @("+script+"); ConvertTo-Json -Compress -InputObject $items", 30*time.Second)
	if err != nil {
		return err
	}
	return json.Unmarshal(out, target)
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
