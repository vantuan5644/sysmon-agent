package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"sysmon-agent/internal/gpuguard"
)

// The GPU guard watches the first NVIDIA GPU's hotspot every two seconds
// whether or not a dashboard is open, raises an alarm on the host and in every
// metrics frame (gpu_guard), and in act mode lowers the power limit and then
// stops GPU apps (docs/plans/gpu-guard.md). The decisions are the state
// machine in internal/gpuguard, and gpuguard.Loop runs it.
//
// On Windows the agent runs the loop itself: the service is LocalSystem, so
// it can set the power limit and end any process (gpu_guard_windows.go). On
// Linux the agent is unprivileged (NoNewPrivileges), so the root
// sysmon-gpu-guard service runs the loop and the agent publishes its state
// file and notifies the desktop (gpu_guard_linux.go).

// gpuGuardService is what main.go starts and the sampler reads.
type gpuGuardService interface {
	Run(ctx context.Context)
	State() *gpuguard.State
}

type gpuGuardOptions struct {
	Config   gpuguard.Config
	Spare    []string
	TestFile string
}

// parseGPUGuardOptions builds the guard's config from its flags. A zero
// threshold keeps the default.
func parseGPUGuardOptions(mode string, warnC, critC, deltaC int, spare, testFile string) (gpuGuardOptions, error) {
	cfg := gpuguard.DefaultConfig()
	m, err := gpuguard.ParseMode(mode)
	if err != nil {
		return gpuGuardOptions{}, err
	}
	cfg.Mode = m
	if warnC != 0 {
		cfg.HotspotWarnC = float64(warnC)
	}
	if critC != 0 {
		cfg.HotspotCritC = float64(critC)
	}
	if deltaC != 0 {
		cfg.DeltaWarnC = float64(deltaC)
	}
	if err := cfg.Validate(); err != nil {
		return gpuGuardOptions{}, err
	}
	return gpuGuardOptions{Config: cfg, Spare: gpuguard.ParseSpareList(spare), TestFile: strings.TrimSpace(testFile)}, nil
}

// attachGPUGuard puts the guard's state on a snapshot and its faults in the
// snapshot's issue lists, so /api/metrics and the issues panel agree about
// them. A guard that can read nothing, or whose action failed, is an issue.
// One that judges the core because the hotspot is missing is a note: the
// guard still works, with a lower critical, and on a Linux host without the
// hotspot helper that never clears by itself.
func attachGPUGuard(m *Metrics, guard gpuGuardService) {
	if guard == nil {
		return
	}
	st := guard.State()
	m.GPUGuard = st
	if st == nil {
		return
	}
	var issues, notes []string
	switch {
	case st.Source == gpuguard.SourceNone:
		issues = append(issues, "gpu guard: "+firstNonEmpty(st.Error, "no GPU temperature"))
	case st.Source == gpuguard.SourceCore && st.Error != "":
		notes = append(notes, "gpu guard judges the core temperature: "+st.Error)
	}
	if st.ActionError != "" {
		issues = append(issues, "gpu guard action failed: "+st.ActionError)
	}
	// Copied rather than appended in place: the slices can share a backing
	// array with the sampler's working snapshot.
	if len(issues) > 0 {
		m.CollectionErrors = append(append([]string(nil), m.CollectionErrors...), issues...)
	}
	if len(notes) > 0 {
		m.CollectionNotes = append(append([]string(nil), m.CollectionNotes...), notes...)
	}
}

// gpuGuardCommand runs nvidia-smi with the child's CPU charged to the agent
// (agent_cost.go) and its stderr in the error.
func gpuGuardCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	hideChildWindow(cmd)
	out, err := trackedOutput(cmd)
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && len(exitErr.Stderr) > 0 {
			return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		if text := strings.TrimSpace(string(out)); text != "" {
			return out, fmt.Errorf("%w: %s", err, text)
		}
	}
	return out, err
}
