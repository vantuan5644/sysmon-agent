//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"sysmon-agent/internal/gpuguard"
	"sysmon-agent/internal/gpuhotspot"
)

// source reads core temperature and power from the nvidia-smi stream and
// the hotspot from the sysmon-gpu-hotspot helper's file, matched by PCI
// address.
type source struct {
	smi         *smiStream
	hotspotPath string
}

func (s *source) ReadSample(_ context.Context) gpuguard.Sample {
	now := time.Now()
	sample := gpuguard.Sample{At: now}
	var problems []string
	reading, err := s.smi.Latest(now)
	if err != nil {
		problems = append(problems, err.Error())
	} else {
		sample.GPU, sample.Core, sample.Power = reading.Name, reading.Core, reading.Power
	}
	snapshot, err := gpuhotspot.ReadFile(s.hotspotPath, now)
	if err != nil {
		problems = append(problems, err.Error())
	} else if hot, why := pickHotspot(snapshot, reading.PCIBusID); hot != nil {
		sample.Hotspot = hot
	} else {
		problems = append(problems, why)
	}
	sample.Error = strings.Join(problems, "; ")
	return sample
}

// pickHotspot takes the device with the nvidia-smi PCI address; without one
// (nvidia-smi down), a lone device is taken, since the hotspot alone is the
// reading that matters most.
func pickHotspot(snapshot gpuhotspot.Snapshot, busID string) (*float64, string) {
	want, wantErr := gpuhotspot.NormalizePCIBusID(busID)
	var match []gpuhotspot.Device
	for _, device := range snapshot.Devices {
		id, err := gpuhotspot.NormalizePCIBusID(device.PCIBusID)
		if err == nil && wantErr == nil && id == want {
			match = append(match, device)
		}
	}
	if wantErr != nil && len(snapshot.Devices) == 1 {
		match = snapshot.Devices
	}
	if len(match) != 1 {
		return nil, fmt.Sprintf("no hotspot reading for GPU %s", busID)
	}
	if match[0].Error != "" {
		return nil, match[0].Error
	}
	if match[0].Celsius == nil {
		return nil, "hotspot helper reported no value"
	}
	value := *match[0].Celsius
	return &value, ""
}

// actor runs as root: nvidia-smi for the power limit, signals for GPU apps.
// The desktop alarm is the unprivileged agent's job; Notify only counts it
// into the state file, which the agent watches.
type actor struct {
	power *gpuguard.PowerControl
	run   gpuguard.CommandFunc
	spare []string
	self  int
	smi   *smiStream
	proc  string // /proc, a parameter for tests

	mu           sync.Mutex
	alarmSeq     uint64
	alarmLevel   gpuguard.Level
	alarmMessage string
}

func (a *actor) LimitPower(ctx context.Context, dryRun bool) (string, error) {
	if dryRun {
		return a.power.Plan(ctx)
	}
	return a.power.Limit(ctx)
}

func (a *actor) RestorePower(ctx context.Context, dryRun bool) (string, error) {
	if dryRun {
		return "restore the power limit (nothing was changed)", nil
	}
	return a.power.Restore(ctx)
}

func (a *actor) RecoverPower(ctx context.Context) (string, error) {
	if !a.power.Pending() {
		return "", nil
	}
	return a.power.Restore(ctx)
}

func (a *actor) Notify(_ context.Context, level gpuguard.Level, message string) error {
	a.mu.Lock()
	a.alarmSeq++
	a.alarmLevel, a.alarmMessage = level, message
	a.mu.Unlock()
	return nil
}

func (a *actor) alarm() (uint64, gpuguard.Level, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.alarmSeq, a.alarmLevel, a.alarmMessage
}

// gpuApps lists compute clients plus graphics clients that are drawing
// (pmon SM use of 1% or more), each named by its executable.
func (a *actor) gpuApps(ctx context.Context) ([]gpuguard.App, error) {
	var lists []gpuguard.App
	var problems []string
	if out, err := a.run(ctx, a.power.Binary, "--query-compute-apps=pid,process_name", "--format=csv,noheader"); err == nil {
		lists = append(lists, gpuguard.ParseComputeApps(out)...)
	} else {
		problems = append(problems, err.Error())
	}
	if out, err := a.run(ctx, a.power.Binary, "pmon", "-c", "1"); err == nil {
		lists = append(lists, gpuguard.ParsePmon(out, 1)...)
	} else {
		problems = append(problems, err.Error())
	}
	if len(lists) == 0 && len(problems) == 2 {
		return nil, fmt.Errorf("list GPU apps: %s", strings.Join(problems, "; "))
	}
	apps := lists[:0]
	for _, app := range lists {
		name := a.processName(app.PID)
		if name == "" {
			continue // already gone
		}
		apps = append(apps, gpuguard.App{PID: app.PID, Name: name})
	}
	return apps, nil
}

// processName is the executable's base name, which unlike comm is not cut
// at 15 characters (sysmon-gpu-guard would otherwise not match its own
// spare entry), falling back to comm when exe is unreadable.
func (a *actor) processName(pid int) string {
	dir := filepath.Join(a.proc, strconv.Itoa(pid))
	if target, err := os.Readlink(filepath.Join(dir, "exe")); err == nil {
		return filepath.Base(strings.TrimSuffix(target, " (deleted)"))
	}
	comm, err := os.ReadFile(filepath.Join(dir, "comm"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(comm))
}

// startTime is field 22 of /proc/<pid>/stat, which with the PID names one
// process for the life of the boot. A zombie counts as gone.
func (a *actor) startTime(pid int) (uint64, bool) {
	data, err := os.ReadFile(filepath.Join(a.proc, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, false
	}
	end := bytes.LastIndexByte(data, ')')
	if end < 0 {
		return 0, false
	}
	fields := strings.Fields(string(data[end+1:]))
	// fields[0] is the state (field 3); starttime is field 22.
	if len(fields) < 20 || fields[0] == "Z" || fields[0] == "X" {
		return 0, false
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	return start, err == nil
}

const stopGrace = 3 * time.Second

func (a *actor) StopApps(ctx context.Context, dryRun bool) (string, error) {
	apps, err := a.gpuApps(ctx)
	if err != nil {
		return "list GPU apps", err
	}
	stop, spared := gpuguard.Targets([][]gpuguard.App{apps}, a.spare, a.self, a.smi.ChildPID())
	if dryRun {
		return fmt.Sprintf("stop %s; spare %s", gpuguard.AppList(stop), gpuguard.AppList(spared)), nil
	}
	type target struct {
		app   gpuguard.App
		start uint64
	}
	var (
		signalled []target
		stopped   []string
		failed    []string
	)
	for _, app := range stop {
		start, alive := a.startTime(app.PID)
		if !alive {
			continue
		}
		if err := syscall.Kill(app.PID, syscall.SIGTERM); err != nil {
			if !errors.Is(err, syscall.ESRCH) {
				failed = append(failed, fmt.Sprintf("%s: %v", app, err))
			}
			continue
		}
		signalled = append(signalled, target{app, start})
	}
	deadline := time.Now().Add(stopGrace)
	for len(signalled) > 0 {
		var left []target
		for _, t := range signalled {
			if start, alive := a.startTime(t.app.PID); alive && start == t.start {
				left = append(left, t)
			} else {
				stopped = append(stopped, t.app.String())
			}
		}
		signalled = left
		if len(signalled) == 0 || time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	for _, t := range signalled {
		// Still the same process (same start time) after the grace period.
		if start, alive := a.startTime(t.app.PID); !alive || start != t.start {
			stopped = append(stopped, t.app.String())
			continue
		}
		if err := syscall.Kill(t.app.PID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			failed = append(failed, fmt.Sprintf("%s: %v", t.app, err))
			continue
		}
		stopped = append(stopped, t.app.String()+" (killed)")
	}
	what := fmt.Sprintf("stopped %s; spared %s", joinOrNone(stopped), gpuguard.AppList(spared))
	if len(failed) > 0 {
		return what, fmt.Errorf("could not stop %s", strings.Join(failed, ", "))
	}
	return what, nil
}

func joinOrNone(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	return strings.Join(items, ", ")
}

// command runs nvidia-smi with its stderr in the error.
func command(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if text := strings.TrimSpace(stderr.String() + " " + string(out)); text != "" {
			return out, fmt.Errorf("%w: %s", err, text)
		}
	}
	return out, err
}

// bootTime is when this boot started, from /proc/stat btime.
func bootTime() time.Time {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "btime "); ok {
			if seconds, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64); err == nil {
				return time.Unix(seconds, 0)
			}
		}
	}
	return time.Time{}
}
