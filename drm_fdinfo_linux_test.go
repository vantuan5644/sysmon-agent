//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeProc builds a /proc tree of DRM clients for the fdinfo sampler.
type fakeProc struct {
	t    *testing.T
	root string
}

func newFakeProc(t *testing.T) *fakeProc {
	return &fakeProc{t: t, root: t.TempDir()}
}

// fd opens (or rewrites) fd n of pid, linked to target, with an fdinfo body.
func (p *fakeProc) fd(pid, n int, target, fdinfo string) {
	p.t.Helper()
	pidDir := filepath.Join(p.root, strconv.Itoa(pid))
	link := filepath.Join(pidDir, "fd", strconv.Itoa(n))
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		p.t.Fatal(err)
	}
	_ = os.Remove(link)
	if err := os.Symlink(target, link); err != nil {
		p.t.Fatal(err)
	}
	if err := writeTestFile(filepath.Join(pidDir, "fdinfo", strconv.Itoa(n)), fdinfo); err != nil {
		p.t.Fatal(err)
	}
}

func (p *fakeProc) close(pid, n int) {
	pidDir := filepath.Join(p.root, strconv.Itoa(pid))
	_ = os.Remove(filepath.Join(pidDir, "fd", strconv.Itoa(n)))
	_ = os.Remove(filepath.Join(pidDir, "fdinfo", strconv.Itoa(n)))
}

func (p *fakeProc) process(pid int) {
	p.t.Helper()
	if err := os.MkdirAll(filepath.Join(p.root, strconv.Itoa(pid), "fd"), 0o755); err != nil {
		p.t.Fatal(err)
	}
}

func fdinfoText(driver, pdev, client string, lines ...string) string {
	var b strings.Builder
	b.WriteString("pos:\t0\nflags:\t02100002\nmnt_id:\t26\n")
	b.WriteString("drm-driver:\t" + driver + "\n")
	b.WriteString("drm-pdev:\t" + pdev + "\n")
	b.WriteString("drm-client-id:\t" + client + "\n")
	for _, line := range lines {
		b.WriteString(line + "\n")
	}
	return b.String()
}

// testSampler is a sampler over a fake /proc with a hand-driven clock.
func testSampler(proc *fakeProc, clock *time.Time) *drmFdinfoSampler {
	s := newDRMFdinfoSampler(proc.root)
	s.now = func() time.Time { return *clock }
	return s
}

const (
	intelPdev = "0000:00:02.0"
	amdPdev   = "0000:03:00.0"
)

func TestParseDRMFdinfo(t *testing.T) {
	record, ok := parseDRMFdinfo(strings.NewReader(fdinfoText("i915", "0000:00:02.0", "7",
		"drm-engine-render:\t25662044495 ns",
		"drm-engine-copy:\t0 ns",
		"drm-engine-video:\t1000 ns",
		"drm-engine-capacity-video:\t2",
		"drm-engine-bogus:\t17 us",
		"drm-memory-system:\t1024 KiB",
	)))
	if !ok || record.driver != "i915" || record.clientID != "7" || record.pdev != "0000:00:02.0" {
		t.Fatalf("record = %+v ok=%v", record, ok)
	}
	if len(record.engines) != 3 {
		t.Fatalf("engines = %+v, want render/copy/video (not the us-unit line)", record.engines)
	}
	if video := record.engines["video"]; video.busy != 1000 || video.capacity != 2 || video.cycles {
		t.Fatalf("video = %+v", video)
	}

	record, ok = parseDRMFdinfo(strings.NewReader(fdinfoText("xe", "0000:00:02.0", "3",
		"drm-cycles-rcs:\t300",
		"drm-total-cycles-rcs:\t1000",
		"drm-cycles-bcs:\t5",
	)))
	if !ok || len(record.engines) != 1 || !record.engines["rcs"].cycles || record.engines["rcs"].total != 1000 {
		t.Fatalf("xe record = %+v, want rcs only (bcs has no total)", record.engines)
	}

	if _, ok := parseDRMFdinfo(strings.NewReader("pos:\t0\nflags:\t0100002\n")); ok {
		t.Fatal("an fdinfo without drm-client-id is not a DRM client")
	}
}

func TestDRMFdinfoSamplerI915Nanoseconds(t *testing.T) {
	proc := newFakeProc(t)
	clock := time.Unix(1000, 0)
	s := testSampler(proc, &clock)
	s.walkEvery = 0
	wanted := map[string]bool{intelPdev: true}

	proc.fd(100, 3, "/dev/null", "pos:\t0\nflags:\t0100002\n")
	proc.fd(100, 4, "/dev/dri/renderD128", fdinfoText("i915", intelPdev, "7",
		"drm-engine-render:\t1000000000 ns", "drm-engine-video:\t0 ns", "drm-engine-capacity-video:\t2"))
	proc.process(200) // a process with no DRM fds

	first := s.Sample(wanted)
	if first.Devices[intelPdev].Load.Available {
		t.Fatalf("first sample = %+v, want warming up", first.Devices[intelPdev])
	}

	clock = clock.Add(time.Second)
	proc.fd(100, 4, "/dev/dri/renderD128", fdinfoText("i915", intelPdev, "7",
		"drm-engine-render:\t1500000000 ns", "drm-engine-video:\t1000000000 ns", "drm-engine-capacity-video:\t2"))
	second := s.Sample(wanted)
	device := second.Devices[intelPdev]
	if !device.Load.Available || device.Load.Value != 50 {
		t.Fatalf("load = %+v, want 50", device.Load)
	}
	if len(device.Engines) != 2 || device.Engines[0].Usage.Value != 50 || device.Engines[1].Usage.Value != 50 {
		t.Fatalf("engines = %+v, want 3D 50 and Video 50 (1 s over capacity 2)", device.Engines)
	}
	if usage := second.Processes[100]; usage.Percent != 50 || usage.Engine != "3D" {
		t.Fatalf("process 100 = %+v", usage)
	}
	if value, _ := second.processUsage(200); !value.Available || value.Value != 0 {
		t.Fatalf("process 200 = %+v, want a scanned 0", value)
	}
	if value, _ := second.processUsage(300); value.Available {
		t.Fatalf("process 300 (never scanned) = %+v, want unavailable", value)
	}
}

func TestDRMFdinfoSamplerAMDGPU(t *testing.T) {
	proc := newFakeProc(t)
	clock := time.Unix(1000, 0)
	s := testSampler(proc, &clock)
	wanted := map[string]bool{amdPdev: true}

	// gnome-shell style: one client in several dup'd fds, gfx only so far.
	for _, fd := range []int{12, 13, 14} {
		proc.fd(100, fd, "/dev/dri/card0", fdinfoText("amdgpu", amdPdev, "22", "drm-engine-gfx:\t1000000000 ns"))
	}
	// A client that has never run anything prints no engine lines.
	proc.fd(150, 5, "/dev/dri/renderD128", fdinfoText("amdgpu", amdPdev, "30"))
	s.Sample(wanted)

	clock = clock.Add(2 * time.Second)
	for _, fd := range []int{12, 13, 14} {
		proc.fd(100, fd, "/dev/dri/card0", fdinfoText("amdgpu", amdPdev, "22",
			"drm-engine-gfx:\t1500000000 ns", "drm-engine-compute:\t200000000 ns"))
	}
	got := s.Sample(wanted) // between walks: only the known paths are read again
	device := got.Devices[amdPdev]
	if !device.Load.Available || device.Load.Value != 25 {
		t.Fatalf("load = %+v, want 25 (500 ms of gfx in 2 s, dup'd fds counted once)", device.Load)
	}
	if len(device.Engines) != 2 || device.Engines[0].Name != "3D" || device.Engines[1].Name != "Compute" || device.Engines[1].Usage.Value != 10 {
		t.Fatalf("engines = %+v, want 3D 25 and Compute 10 (a new class starts from 0)", device.Engines)
	}
	if usage, ok := got.Processes[150]; !ok || usage.Percent != 0 || usage.Engine != "" {
		t.Fatalf("idle client = %+v (%v), want a measured 0", usage, ok)
	}
}

func TestDRMFdinfoSamplerXeCycles(t *testing.T) {
	proc := newFakeProc(t)
	clock := time.Unix(1000, 0)
	s := testSampler(proc, &clock)
	wanted := map[string]bool{intelPdev: true}

	proc.fd(100, 4, "/dev/dri/renderD128", fdinfoText("xe", intelPdev, "3",
		"drm-cycles-rcs:\t100", "drm-total-cycles-rcs:\t1000",
		"drm-cycles-vcs:\t0", "drm-total-cycles-vcs:\t1000"))
	s.Sample(wanted)

	clock = clock.Add(1500 * time.Millisecond)
	proc.fd(100, 4, "/dev/dri/renderD128", fdinfoText("xe", intelPdev, "3",
		"drm-cycles-rcs:\t400", "drm-total-cycles-rcs:\t2000",
		"drm-cycles-vcs:\t100", "drm-total-cycles-vcs:\t2000"))
	got := s.Sample(wanted)
	device := got.Devices[intelPdev]
	if !device.Load.Available || device.Load.Value != 30 {
		t.Fatalf("load = %+v, want 30 (300 of 1000 cycles)", device.Load)
	}
	if device.Engines[0].Name != "3D" || device.Engines[1].Name != "Video" || device.Engines[1].Usage.Value != 10 {
		t.Fatalf("engines = %+v", device.Engines)
	}
}

func TestDRMFdinfoSamplerClientChurnAndGaps(t *testing.T) {
	proc := newFakeProc(t)
	clock := time.Unix(1000, 0)
	s := testSampler(proc, &clock)
	s.walkEvery = 0
	wanted := map[string]bool{intelPdev: true}

	proc.fd(100, 4, "/dev/dri/renderD128", fdinfoText("i915", intelPdev, "1", "drm-engine-render:\t0 ns"))
	s.Sample(wanted)

	// Client 1 closes; client 2 opens with a large lifetime counter.
	clock = clock.Add(time.Second)
	proc.close(100, 4)
	proc.fd(101, 4, "/dev/dri/renderD128", fdinfoText("i915", intelPdev, "2", "drm-engine-render:\t900000000000 ns"))
	got := s.Sample(wanted)
	if load := got.Devices[intelPdev].Load; load.Available {
		t.Fatalf("load = %+v, want warming up: a new client only takes a baseline", load)
	}
	if _, ok := got.Processes[101]; ok {
		t.Fatal("a new client must not be reported before its second reading")
	}

	clock = clock.Add(time.Second)
	proc.fd(101, 4, "/dev/dri/renderD128", fdinfoText("i915", intelPdev, "2", "drm-engine-render:\t900100000000 ns"))
	got = s.Sample(wanted)
	if load := got.Devices[intelPdev].Load; !load.Available || load.Value != 10 {
		t.Fatalf("load = %+v, want 10", load)
	}

	// A long gap (the slow lane parked) rebaselines instead of averaging.
	clock = clock.Add(5 * time.Minute)
	proc.fd(101, 4, "/dev/dri/renderD128", fdinfoText("i915", intelPdev, "2", "drm-engine-render:\t999000000000 ns"))
	got = s.Sample(wanted)
	if load := got.Devices[intelPdev].Load; load.Available {
		t.Fatalf("load after a park = %+v, want warming up", load)
	}

	// A counter that goes backwards (client id reuse) is not a negative share.
	clock = clock.Add(time.Second)
	proc.fd(101, 4, "/dev/dri/renderD128", fdinfoText("i915", intelPdev, "2", "drm-engine-render:\t5 ns"))
	got = s.Sample(wanted)
	if load := got.Devices[intelPdev].Load; load.Available && load.Value != 0 {
		t.Fatalf("load = %+v after a counter reset", load)
	}

	// Every process readable and none holds the GPU: a true 0.
	clock = clock.Add(time.Second)
	proc.close(101, 4)
	got = s.Sample(wanted)
	if load := got.Devices[intelPdev].Load; !load.Available || load.Value != 0 {
		t.Fatalf("load with no clients = %+v, want 0", load)
	}
}

func TestDRMFdinfoSamplerSharedClientAndOtherDevices(t *testing.T) {
	proc := newFakeProc(t)
	clock := time.Unix(1000, 0)
	s := testSampler(proc, &clock)
	s.walkEvery = 0
	wanted := map[string]bool{intelPdev: true}

	// A forked child holds the parent's DRM file: one client, two pids.
	for _, pid := range []int{40, 41} {
		proc.fd(pid, 5, "/dev/dri/renderD128", fdinfoText("i915", intelPdev, "9", "drm-engine-render:\t0 ns"))
	}
	// A device the sampler was not asked about is ignored.
	proc.fd(50, 5, "/dev/dri/renderD129", fdinfoText("amdgpu", amdPdev, "1", "drm-engine-gfx:\t0 ns"))
	s.Sample(wanted)
	clock = clock.Add(time.Second)
	for _, pid := range []int{40, 41} {
		proc.fd(pid, 5, "/dev/dri/renderD128", fdinfoText("i915", intelPdev, "9", "drm-engine-render:\t200000000 ns"))
	}
	proc.fd(50, 5, "/dev/dri/renderD129", fdinfoText("amdgpu", amdPdev, "1", "drm-engine-gfx:\t1000000000 ns"))
	got := s.Sample(wanted)
	if load := got.Devices[intelPdev].Load; load.Value != 20 {
		t.Fatalf("load = %+v, want 20 (the shared client counted once)", load)
	}
	if got.Processes[40].Percent != 20 {
		t.Fatalf("pid 40 = %+v, want the shared client", got.Processes[40])
	}
	if usage, ok := got.Processes[41]; ok {
		t.Fatalf("pid 41 = %+v, want the client attributed to the lowest pid only", usage)
	}
	if _, ok := got.Devices[amdPdev]; ok {
		t.Fatal("an unwanted device must not be reported")
	}
}

func TestDRMFdinfoSamplerCaps(t *testing.T) {
	proc := newFakeProc(t)
	clock := time.Unix(1000, 0)
	s := testSampler(proc, &clock)
	s.walkEvery = 0
	s.maxReads = 1
	wanted := map[string]bool{intelPdev: true}

	proc.fd(10, 3, "/dev/dri/renderD128", fdinfoText("i915", intelPdev, "1", "drm-engine-render:\t0 ns"))
	proc.fd(20, 3, "/dev/dri/renderD128", fdinfoText("i915", intelPdev, "2", "drm-engine-render:\t0 ns"))
	got := s.Sample(wanted)
	if !got.Capped || !got.Scanned[10] || got.Scanned[20] {
		t.Fatalf("capped=%v scanned=%v, want the walk stopped after pid 10", got.Capped, got.Scanned)
	}
	if value, _ := got.processUsage(20); value.Available || !strings.Contains(value.Error, "cap") {
		t.Fatalf("pid 20 = %+v, want unavailable at the cap", value)
	}

	s.maxReads = drmFdinfoMaxReads
	s.maxFds = 3
	for fd := 4; fd < 8; fd++ {
		proc.fd(20, fd, "/dev/null", "pos:\t0\n")
	}
	got = s.Sample(wanted)
	if !got.Capped || !got.Scanned[10] || got.Scanned[20] {
		t.Fatalf("fd cap: capped=%v scanned=%v", got.Capped, got.Scanned)
	}
}

func TestDRMFdinfoSamplerUnreadableProcess(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every fd table")
	}
	proc := newFakeProc(t)
	clock := time.Unix(1000, 0)
	s := testSampler(proc, &clock)
	wanted := map[string]bool{intelPdev: true}

	proc.fd(10, 3, "/dev/dri/renderD128", fdinfoText("i915", intelPdev, "1", "drm-engine-render:\t0 ns"))
	proc.process(20)
	fdDir := filepath.Join(proc.root, "20", "fd")
	if err := os.Chmod(fdDir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(fdDir, 0o755) })

	got := s.Sample(wanted)
	if !got.Unreadable[20] || got.Scanned[20] {
		t.Fatalf("unreadable=%v scanned=%v", got.Unreadable, got.Scanned)
	}
	if value, _ := got.processUsage(20); value.Available {
		t.Fatalf("pid 20 = %+v, want unavailable, not a false 0", value)
	}
	// With an unreadable process, a device without visible clients is unknown.
	proc.close(10, 3)
	clock = clock.Add(31 * time.Second)
	got = s.Sample(wanted)
	if load := got.Devices[intelPdev].Load; load.Available {
		t.Fatalf("load = %+v, want unavailable while some processes are unreadable", load)
	}
}

func TestDRMFdinfoSamplerRereadsBetweenWalks(t *testing.T) {
	proc := newFakeProc(t)
	clock := time.Unix(1000, 0)
	s := testSampler(proc, &clock)
	wanted := map[string]bool{intelPdev: true}

	proc.fd(10, 3, "/dev/dri/renderD128", fdinfoText("i915", intelPdev, "1", "drm-engine-render:\t0 ns"))
	s.Sample(wanted)

	// A client that appears between walks is found by the next walk.
	clock = clock.Add(time.Second)
	proc.fd(30, 3, "/dev/dri/renderD128", fdinfoText("i915", intelPdev, "2", "drm-engine-render:\t0 ns"))
	got := s.Sample(wanted)
	if value, _ := got.processUsage(30); value.Available {
		t.Fatalf("pid 30 between walks = %+v, want not scanned yet", value)
	}
	if len(s.known) != 1 {
		t.Fatalf("known = %+v, want only the walked client", s.known)
	}

	clock = clock.Add(drmFdinfoWalkEvery)
	s.Sample(wanted)
	if len(s.known) != 2 {
		t.Fatalf("known after the next walk = %+v", s.known)
	}

	// A closed fd is dropped on the next reread.
	clock = clock.Add(time.Second)
	proc.close(30, 3)
	s.Sample(wanted)
	if len(s.known) != 1 {
		t.Fatalf("known after close = %+v", s.known)
	}
}

func TestApplyDRMProcessGPU(t *testing.T) {
	done := make(chan struct{})
	close(done)
	summary := drmUsageSummary{
		Available: true,
		Processes: map[int]processGPUUsage{10: {Percent: 42, Engine: "3D"}},
		Scanned:   map[int]bool{10: true, 11: true},
	}
	rows := []ProcessMetric{{PID: 10}, {PID: 11}, {PID: 12}}
	applyDRMProcessGPU(rows, &drmUsageFuture{done: done, summary: summary})
	if rows[0].GPU.Value != 42 || rows[0].GPUEngine != "3D" || !rows[1].GPU.Available || rows[1].GPU.Value != 0 || rows[2].GPU.Available {
		t.Fatalf("rows = %+v", rows)
	}

	rows = []ProcessMetric{{PID: 10}}
	applyDRMProcessGPU(rows, &drmUsageFuture{done: done, summary: summary, nvidia: true})
	if rows[0].GPU.Available || rows[0].GPU.Error != linuxNVIDIAProcessGPUMessage {
		t.Fatalf("NVIDIA host row = %+v, want the note", rows[0])
	}
}

func TestScanLinuxDRMCards(t *testing.T) {
	sys := t.TempDir()
	proc := t.TempDir()
	writeTestFiles(t, map[string]string{
		filepath.Join(sys, "class", "drm", "card0", "device", "vendor"):                  "0x1002\n",
		filepath.Join(sys, "class", "drm", "card0", "device", "uevent"):                  "PCI_SLOT_NAME=0000:E6:00.0\n",
		filepath.Join(sys, "class", "drm", "card1", "device", "vendor"):                  "0x8086\n",
		filepath.Join(sys, "class", "drm", "card1", "device", "uevent"):                  "PCI_SLOT_NAME=0000:00:02.0\n",
		filepath.Join(sys, "class", "drm", "card1", "device", "power", "runtime_status"): "suspended\n",
		filepath.Join(sys, "class", "drm", "card0-DP-1", "device", "vendor"):             "0x1002\n",
	})
	scan := scanLinuxDRMCards(sys, proc)
	if len(scan.pdevs) != 1 || !scan.pdevs["0000:e6:00.0"] || scan.nvidia {
		t.Fatalf("scan = %+v", scan)
	}
	writeTestFiles(t, map[string]string{filepath.Join(proc, "driver", "nvidia", "gpus", "0000:01:00.0", "information"): "x\n"})
	if scan := scanLinuxDRMCards(sys, proc); !scan.nvidia {
		t.Fatalf("scan = %+v, want the NVIDIA driver noticed", scan)
	}
}
