//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"sysmon-agent/internal/gpuguard"
)

// ReadGPUGuardSample reads the NVIDIA GPU's core, hotspot and package power
// from one GPU-only daemon request (lhm-bridge-daemon.ps1 'gpu'), so the
// delta between core and hotspot comes from a single Update(). Without the
// daemon it falls back to nvidia-smi, which has no hotspot.
func (c *systemCollector) ReadGPUGuardSample(ctx context.Context) gpuguard.Sample {
	sample := gpuguard.Sample{At: time.Now()}
	c.lhmMu.Lock()
	var (
		bridge lhmBridgeResult
		err    = errLhmDaemonDisabled
	)
	if c.useDaemon {
		if c.daemon == nil {
			c.daemon = &lhmDaemon{}
		}
		bridge, err = c.daemon.readRequest(ctx, "gpu")
	}
	c.lhmMu.Unlock()
	if err == nil && bridge.Available {
		if device, ok := pickGuardGPU(bridge.GPU); ok {
			return guardSampleFromGPU(sample, device)
		}
		sample.Error = "no NVIDIA GPU in the sensor bridge reply"
		return sample
	}
	if err != nil && !errors.Is(err, errLhmDaemonDisabled) {
		// A transient daemon failure (cold start, restart backoff): say so
		// rather than spawn nvidia-smi every two seconds while it heals.
		sample.Error = "GPU sensor bridge: " + err.Error()
		return sample
	}
	// The daemon is off, or it runs but LibreHardwareMonitor failed inside
	// it: nvidia-smi still has the core (cached 5 s, collectNVIDIAGPU).
	why := "no hotspot without the LibreHardwareMonitor daemon"
	if err == nil {
		why = "GPU sensor bridge: " + strings.TrimSpace(bridge.Error)
	}
	gpu := collectNVIDIAGPU(ctx)
	if device, ok := pickGuardGPU(gpu.Devices); ok {
		sample = guardSampleFromGPU(sample, device)
		if sample.Error == "" {
			sample.Error = why
		}
		return sample
	}
	sample.Error = "no NVIDIA GPU: " + gpu.Error
	return sample
}

// pickGuardGPU takes the first NVIDIA device; a lone device of unknown make
// is taken as well.
func pickGuardGPU(devices []GPUMetric) (GPUMetric, bool) {
	for _, device := range devices {
		if strings.Contains(strings.ToLower(device.Name), "nvidia") {
			return device, true
		}
	}
	if len(devices) == 1 && devices[0].HotspotTemperature.Available {
		return devices[0], true
	}
	return GPUMetric{}, false
}

func guardSampleFromGPU(sample gpuguard.Sample, device GPUMetric) gpuguard.Sample {
	sample.GPU = device.Name
	value := func(m NumberMetric) *float64 {
		if !m.Available {
			return nil
		}
		v := m.Value
		return &v
	}
	sample.Core = value(device.Temperature)
	sample.Hotspot = value(device.HotspotTemperature)
	sample.Power = value(device.Power)
	if sample.Core == nil && sample.Hotspot == nil {
		sample.Error = strings.TrimSpace(device.Temperature.Error + " " + device.HotspotTemperature.Error)
	}
	return sample
}

// windowsGPUGuardActor acts as LocalSystem: nvidia-smi for the power limit,
// TerminateProcess for GPU apps, WTSSendMessageW for the alarm on the console
// session (a service in session 0 cannot show a window of its own).
type windowsGPUGuardActor struct {
	power   *gpuguard.PowerControl
	engines gpuEngineSampler // the guard's own PDH query, apart from the slow lane's
	spare   []string
	self    int
}

// windowsGPUGuardRecordPath keeps the limit to put back beside the service's
// settings (install-windows.ps1 puts them in %ProgramData%\SysmonAgent).
func windowsGPUGuardRecordPath() string {
	base := os.Getenv("ProgramData")
	if base == "" {
		return ""
	}
	return filepath.Join(base, "SysmonAgent", "gpu-guard-power-limit.txt")
}

// windowsBootTime is now minus GetTickCount64, which keeps counting through
// sleep, so it is when this boot started.
func windowsBootTime() time.Time {
	ms, _, _ := procGetTickCount64.Call()
	if ms == 0 {
		return time.Time{}
	}
	return time.Now().Add(-time.Duration(ms) * time.Millisecond)
}

func newWindowsGPUGuardActor(spare []string) *windowsGPUGuardActor {
	return &windowsGPUGuardActor{
		power: &gpuguard.PowerControl{Run: gpuGuardCommand, RecordPath: windowsGPUGuardRecordPath(), BootTime: windowsBootTime},
		spare: spare,
		self:  os.Getpid(),
	}
}

func (a *windowsGPUGuardActor) LimitPower(ctx context.Context, dryRun bool) (string, error) {
	// Start the per-process GPU counters now, so the app list ten seconds
	// later covers those ten seconds.
	_ = a.engines.summary()
	if dryRun {
		return a.power.Plan(ctx)
	}
	return a.power.Limit(ctx)
}

func (a *windowsGPUGuardActor) RecoverPower(ctx context.Context) (string, error) {
	if !a.power.Pending() {
		return "", nil
	}
	return a.power.Restore(ctx)
}

func (a *windowsGPUGuardActor) RestorePower(ctx context.Context, dryRun bool) (string, error) {
	if dryRun {
		return "restore the power limit (nothing was changed)", nil
	}
	return a.power.Restore(ctx)
}

// gpuGuardMinAppPercent leaves out processes that hold a GPU context but are
// not using it: most of the desktop has one.
const gpuGuardMinAppPercent = 1.0

func (a *windowsGPUGuardActor) gpuApps(ctx context.Context) ([]gpuguard.App, map[int]string, error) {
	summary := a.engines.summary()
	if !summary.Available && summary.Error == errGPUEnginesWarming.Error() {
		if sleepCanceled(ctx, time.Second) {
			return nil, nil, ctx.Err()
		}
		summary = a.engines.summary()
	}
	rows, err := readWindowsProcesses(ctx)
	if err != nil {
		return nil, nil, err
	}
	names := make(map[int]string, len(rows))
	for _, row := range rows {
		names[row.pid] = row.name
	}
	// Only the engine counters decide. Under WDDM, nvidia-smi
	// --query-compute-apps lists every process with a GPU context, busy or
	// not (explorer, LockApp, SearchHost, every browser and Electron app:
	// 35 rows on BBLWIN at idle, 2026-11-12, all with used_memory [N/A]),
	// so merging it would end the whole session. CUDA work shows up on the
	// counters' compute engines, so nothing is lost without it.
	if !summary.Available {
		return nil, names, fmt.Errorf("no per-process GPU readings: %s", summary.Error)
	}
	pids := make([]int, 0, len(summary.Processes))
	for pid, usage := range summary.Processes {
		if usage.Percent >= gpuGuardMinAppPercent {
			pids = append(pids, pid)
		}
	}
	sort.Ints(pids)
	var apps []gpuguard.App
	for _, pid := range pids {
		if name, ok := names[pid]; ok {
			apps = append(apps, gpuguard.App{PID: pid, Name: name})
		}
	}
	return apps, names, nil
}

func (a *windowsGPUGuardActor) StopApps(ctx context.Context, dryRun bool) (string, error) {
	apps, _, err := a.gpuApps(ctx)
	if err != nil {
		return "list GPU apps", err
	}
	stop, spared := gpuguard.Targets([][]gpuguard.App{apps}, a.spare, a.self)
	if dryRun {
		return fmt.Sprintf("stop %s; spare %s", gpuguard.AppList(stop), gpuguard.AppList(spared)), nil
	}
	// Names come from a snapshot a moment old; check each PID still belongs
	// to the same program before ending it, since Windows reuses PIDs.
	rows, err := readWindowsProcesses(ctx)
	if err != nil {
		return "recheck GPU apps", err
	}
	current := make(map[int]string, len(rows))
	for _, row := range rows {
		current[row.pid] = row.name
	}
	var stopped, failed []string
	for _, app := range stop {
		if !strings.EqualFold(current[app.PID], app.Name) {
			continue
		}
		if err := terminateWindowsProcess(app.PID); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", app, err))
			continue
		}
		stopped = append(stopped, app.String())
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

func terminateWindowsProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	defer process.Release()
	return process.Kill()
}

var (
	wtsapi32                         = syscall.NewLazyDLL("wtsapi32.dll")
	procWTSSendMessageW              = wtsapi32.NewProc("WTSSendMessageW")
	procWTSGetActiveConsoleSessionId = kernel32.NewProc("WTSGetActiveConsoleSessionId")
	procGetTickCount64               = kernel32.NewProc("GetTickCount64")
)

const (
	mbIconError     = 0x00000010
	mbIconWarning   = 0x00000030
	mbSystemModal   = 0x00001000
	mbSetForeground = 0x00010000
	mbTopmost       = 0x00040000
	noConsoleUser   = 0xFFFFFFFF
)

// Notify shows a message box on the console session without waiting for it.
// A critical box is system-modal with no timeout; a warning closes itself
// after a minute so a long session cannot pile them up.
func (a *windowsGPUGuardActor) Notify(_ context.Context, level gpuguard.Level, message string) error {
	session, _, _ := procWTSGetActiveConsoleSessionId.Call()
	if uint32(session) == noConsoleUser {
		return errors.New("no console session")
	}
	title := "Sysmon GPU guard: warning"
	style := uintptr(mbIconWarning | mbSetForeground | mbTopmost)
	timeout := uintptr(60)
	if level == gpuguard.LevelCritical {
		title = "Sysmon GPU guard: GPU OVERHEATING"
		style = mbIconError | mbSystemModal | mbSetForeground | mbTopmost
		timeout = 0
	}
	body := message
	if level == gpuguard.LevelCritical {
		body += "\r\n\r\nSave your work. If it stays this hot, the GPU power limit is lowered and GPU apps are ended."
	}
	titlePtr, err := syscall.UTF16FromString(title)
	if err != nil {
		return err
	}
	bodyPtr, err := syscall.UTF16FromString(body)
	if err != nil {
		return err
	}
	var response uint32
	ok, _, callErr := procWTSSendMessageW.Call(0, session,
		uintptr(unsafe.Pointer(&titlePtr[0])), uintptr((len(titlePtr)-1)*2),
		uintptr(unsafe.Pointer(&bodyPtr[0])), uintptr((len(bodyPtr)-1)*2),
		style, timeout, uintptr(unsafe.Pointer(&response)), 0)
	if ok == 0 {
		return fmt.Errorf("WTSSendMessageW: %v", callErr)
	}
	return nil
}

// startGPUGuard builds the Windows guard. The service is LocalSystem, so it
// acts by itself; a console run under an ordinary account can warn but its
// power-limit and stop actions fail and are logged.
func startGPUGuard(opts gpuGuardOptions, collector MetricsCollector, logf func(string, ...any)) gpuGuardService {
	if opts.Config.Mode == gpuguard.ModeOff {
		return nil
	}
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		logf("gpu guard: off, nvidia-smi not found")
		return nil
	}
	reader, ok := collector.(interface {
		ReadGPUGuardSample(context.Context) gpuguard.Sample
	})
	if !ok {
		logf("gpu guard: off, this collector reads no GPU temperatures")
		return nil
	}
	var source gpuguard.Source = gpuguard.SourceFunc(reader.ReadGPUGuardSample)
	if opts.TestFile != "" {
		source = gpuguard.TestSource{Inner: source, Path: filepath.Clean(opts.TestFile)}
		logf("gpu guard: readings may be overridden by %s", opts.TestFile)
	}
	return gpuguard.NewLoop(opts.Config, source, newWindowsGPUGuardActor(opts.Spare), logf)
}
