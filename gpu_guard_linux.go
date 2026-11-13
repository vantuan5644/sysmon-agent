//go:build linux

package main

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"time"

	"sysmon-agent/internal/gpuguard"
)

// linuxGPUGuardReader publishes the root sysmon-gpu-guard helper's state and
// raises its alarms on the desktop. The agent's own threshold flags do not
// apply here: the helper has its own (scripts/install-gpu-guard.sh), and
// -gpu-guard off only stops this reader.
type linuxGPUGuardReader struct {
	path   string
	logf   func(string, ...any)
	notify func(ctx context.Context, level gpuguard.Level, message string) error

	mu    sync.Mutex
	state *gpuguard.State

	// only Run's goroutine touches these
	instance  string
	seq       uint64
	seen      bool
	lastIssue string
}

func startGPUGuard(opts gpuGuardOptions, _ MetricsCollector, logf func(string, ...any)) gpuGuardService {
	if opts.Config.Mode == gpuguard.ModeOff {
		return nil
	}
	return &linuxGPUGuardReader{path: gpuguard.StatePath, logf: logf, notify: notifyDesktop}
}

func (r *linuxGPUGuardReader) State() *gpuguard.State {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == nil {
		return nil
	}
	copied := *r.state
	copied.Reasons = append([]string(nil), r.state.Reasons...)
	return &copied
}

func (r *linuxGPUGuardReader) Run(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		r.poll(ctx, time.Now())
		timer.Reset(gpuguard.Interval)
	}
}

func (r *linuxGPUGuardReader) poll(ctx context.Context, now time.Time) {
	file, err := gpuguard.ReadStateFile(r.path, now)
	switch {
	case os.IsNotExist(err):
		// No helper on this host: publish nothing, as on a host with no GPU.
		r.setState(nil)
		r.issue("gpu guard: no helper state at " + r.path + "; scripts/install-gpu-guard.sh installs sysmon-gpu-guard")
		return
	case err != nil:
		// A helper that stopped writing protects nothing; say so rather than
		// keep showing its last level.
		r.setState(&gpuguard.State{Mode: file.State.Mode, Level: gpuguard.LevelUnknown, Source: gpuguard.SourceNone, Error: err.Error(), UpdatedAt: now.UTC()})
		r.issue("gpu guard: " + err.Error())
		return
	}
	state := file.State
	r.setState(&state)
	r.issue("")

	if file.Instance != r.instance {
		first := !r.seen
		r.instance, r.seen = file.Instance, true
		if first {
			// Alarms from before this agent started are old news, except an
			// episode still going on.
			r.seq = file.AlarmSeq
			if state.Level == gpuguard.LevelCritical {
				r.raise(ctx, gpuguard.LevelCritical, state.Message)
			}
			return
		}
		r.seq = 0 // the helper restarted and counts from zero again
	}
	if file.AlarmSeq > r.seq {
		r.seq = file.AlarmSeq
		r.raise(ctx, file.AlarmLevel, file.AlarmMessage)
	}
}

func (r *linuxGPUGuardReader) setState(state *gpuguard.State) {
	r.mu.Lock()
	r.state = state
	r.mu.Unlock()
}

// issue logs a condition when it changes, and its end once.
func (r *linuxGPUGuardReader) issue(text string) {
	if text == r.lastIssue {
		return
	}
	if text == "" {
		r.logf("gpu guard: reading the helper's state")
	} else {
		r.logf("%s", text)
	}
	r.lastIssue = text
}

func (r *linuxGPUGuardReader) raise(ctx context.Context, level gpuguard.Level, message string) {
	if message == "" {
		return
	}
	notifyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := r.notify(notifyCtx, level, message); err != nil {
		r.logf("gpu guard: desktop notification failed: %v", err)
	}
}

// notifyDesktop works from the desktop's systemctl --user agent, which has
// the session bus. A system-unit agent (minipc) has none, and the failure is
// only logged.
func notifyDesktop(ctx context.Context, level gpuguard.Level, message string) error {
	args := []string{"-a", "Sysmon GPU guard"}
	if level == gpuguard.LevelCritical {
		args = append(args, "-u", "critical", "-i", "dialog-error", "GPU OVERHEATING",
			message+"\nSave your work. If it stays this hot, the GPU power limit is lowered and GPU apps are ended.")
	} else {
		args = append(args, "-u", "normal", "-t", "60000", "-i", "dialog-warning", "GPU temperature warning", message)
	}
	cmd := exec.CommandContext(ctx, "notify-send", args...)
	_, err := trackedCombinedOutput(cmd)
	return err
}
