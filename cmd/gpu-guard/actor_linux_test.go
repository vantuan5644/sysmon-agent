//go:build linux

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"sysmon-agent/internal/gpuguard"
	"sysmon-agent/internal/gpuhotspot"
)

func hot(v float64) *float64 { return &v }

func TestPickHotspotMatchesThePCIAddress(t *testing.T) {
	snapshot := gpuhotspot.Snapshot{Devices: []gpuhotspot.Device{
		{PCIBusID: "0000:01:00.0", Celsius: hot(71)},
		{PCIBusID: "0000:02:00.0", Celsius: hot(99)},
	}}
	if got, why := pickHotspot(snapshot, "00000000:01:00.0"); got == nil || *got != 71 {
		t.Fatalf("got %v (%s)", got, why)
	}
	if got, _ := pickHotspot(snapshot, "00000000:03:00.0"); got != nil {
		t.Fatal("matched a GPU that is not there")
	}
	// nvidia-smi down: two devices cannot be told apart, one can.
	if got, _ := pickHotspot(snapshot, ""); got != nil {
		t.Fatal("guessed between two GPUs")
	}
	lone := gpuhotspot.Snapshot{Devices: snapshot.Devices[:1]}
	if got, _ := pickHotspot(lone, ""); got == nil || *got != 71 {
		t.Fatal("lone device not used without nvidia-smi")
	}
	failed := gpuhotspot.Snapshot{Devices: []gpuhotspot.Device{{PCIBusID: "0000:01:00.0", Error: "register read failed"}}}
	if got, why := pickHotspot(failed, "00000000:01:00.0"); got != nil || why != "register read failed" {
		t.Fatalf("got %v (%s)", got, why)
	}
}

func TestSourceCombinesTheStreamAndTheHotspotFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "readings.json")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := os.WriteFile(path, []byte(`{"timestamp":"`+now+`","devices":[{"pci_bus_id":"0000:01:00.0","celsius":84}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	reading, _ := gpuguard.ParseSMILine("NVIDIA GeForce RTX 4090, 00000000:01:00.0, 66, 410.5")
	smi := &smiStream{every: 2 * time.Second, latest: reading, at: time.Now()}
	sample := (&source{smi: smi, hotspotPath: path}).ReadSample(context.Background())
	if sample.Error != "" || *sample.Core != 66 || *sample.Hotspot != 84 || *sample.Power != 410.5 || sample.GPU != "NVIDIA GeForce RTX 4090" {
		t.Fatalf("%+v", sample)
	}
	// nvidia-smi silent: the hotspot still comes through, with the reason.
	smi.at = time.Now().Add(-time.Minute)
	sample = (&source{smi: smi, hotspotPath: path}).ReadSample(context.Background())
	if sample.Core != nil || sample.Hotspot == nil || !strings.Contains(sample.Error, "nvidia-smi silent") {
		t.Fatalf("%+v", sample)
	}
}

func TestProcStartTimeAndName(t *testing.T) {
	proc := t.TempDir()
	dir := filepath.Join(proc, "4321")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A comm with spaces and a parenthesis must not shift the fields.
	stat := "4321 (Web Content) x) S 1 4321 4321 0 -1 4194560 100 0 0 0 5 3 0 0 20 0 30 0 987654 1000 100"
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "comm"), []byte("sysmon-gpu-guar\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &actor{proc: proc}
	if start, alive := a.startTime(4321); !alive || start != 987654 {
		t.Fatalf("start %d alive %v", start, alive)
	}
	if name := a.processName(4321); name != "sysmon-gpu-guar" {
		t.Fatalf("comm fallback: %q", name)
	}
	if err := os.Symlink("/usr/local/bin/sysmon-gpu-guard", filepath.Join(dir, "exe")); err != nil {
		t.Fatal(err)
	}
	if name := a.processName(4321); name != "sysmon-gpu-guard" {
		t.Fatalf("exe name: %q", name)
	}
	zombie := strings.Replace(stat, ") S ", ") Z ", 1)
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(zombie), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, alive := a.startTime(4321); alive {
		t.Fatal("a zombie counted as alive")
	}
}

// fakeTools answers nvidia-smi's app queries with the given PIDs.
func fakeTools(compute string, pmon string) gpuguard.CommandFunc {
	return func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "pmon" {
			return []byte(pmon), nil
		}
		return []byte(compute), nil
	}
}

func TestStopAppsEndsTheGPUAppAndSparesTheRest(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("no sleep binary")
	}
	victim := exec.Command("sleep", "60")
	if err := victim.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { victim.Wait(); close(done) }()
	defer victim.Process.Kill()

	pid := strconv.Itoa(victim.Process.Pid)
	self := strconv.Itoa(os.Getpid())
	a := &actor{
		run:   fakeTools(pid+", /usr/bin/sleep\n"+self+", /tmp/test\n", "# gpu pid type sm mem enc dec command\n"),
		power: &gpuguard.PowerControl{Binary: "nvidia-smi"},
		smi:   &smiStream{},
		self:  os.Getpid(),
		proc:  "/proc",
	}
	what, err := a.StopApps(context.Background(), true)
	if err != nil || !strings.Contains(what, "stop sleep ("+pid+")") {
		t.Fatalf("dry run: %q %v", what, err)
	}
	select {
	case <-done:
		t.Fatal("a dry run ended the process")
	case <-time.After(100 * time.Millisecond):
	}
	what, err = a.StopApps(context.Background(), false)
	if err != nil || !strings.Contains(what, "stopped sleep ("+pid+")") {
		t.Fatalf("stop: %q %v", what, err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the GPU app is still running")
	}
	// Spared by name: the same process under a spared name survives.
	a.spare = []string{"sleep"}
	survivor := exec.Command("sleep", "60")
	if err := survivor.Start(); err != nil {
		t.Fatal(err)
	}
	defer survivor.Process.Kill()
	a.run = fakeTools(strconv.Itoa(survivor.Process.Pid)+", /usr/bin/sleep\n", "")
	what, err = a.StopApps(context.Background(), false)
	if err != nil || !strings.Contains(what, "stopped none") || !strings.Contains(what, "spared sleep") {
		t.Fatalf("spare: %q %v", what, err)
	}
	if _, alive := a.startTime(survivor.Process.Pid); !alive {
		t.Fatal("a spared process was ended")
	}
}

func TestNotifyCountsAlarmsForTheAgent(t *testing.T) {
	a := &actor{}
	a.Notify(context.Background(), gpuguard.LevelWarn, "warm")
	a.Notify(context.Background(), gpuguard.LevelCritical, "hot")
	if seq, level, message := a.alarm(); seq != 2 || level != gpuguard.LevelCritical || message != "hot" {
		t.Fatalf("%d %s %s", seq, level, message)
	}
}
