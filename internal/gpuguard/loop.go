package gpuguard

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// Interval is how often the guard samples, with or without a viewer.
const Interval = 2 * time.Second

// Source reads one sample. It never fails as a whole: a missing reading is a
// nil field plus Sample.Error.
type Source interface {
	ReadSample(ctx context.Context) Sample
}

// SourceFunc adapts a function to Source.
type SourceFunc func(ctx context.Context) Sample

func (f SourceFunc) ReadSample(ctx context.Context) Sample { return f(ctx) }

// Actor carries out the guard's commands. dryRun asks only for a description
// of what would happen, which is how warn mode logs its "would" lines.
type Actor interface {
	LimitPower(ctx context.Context, dryRun bool) (string, error)
	RestorePower(ctx context.Context, dryRun bool) (string, error)
	StopApps(ctx context.Context, dryRun bool) (string, error)
	Notify(ctx context.Context, level Level, message string) error
}

// Loop runs a Guard against a Source and an Actor. The Windows agent runs one
// in process; on Linux the root sysmon-gpu-guard helper runs it and the
// agent only reads its state file.
type Loop struct {
	source   Source
	actor    Actor
	logf     func(string, ...any)
	Interval time.Duration
	// OnStep, when set, sees every state after its commands ran (the Linux
	// helper publishes its state file from it).
	OnStep func(State)

	mu        sync.Mutex
	guard     *Guard
	state     *State
	actionErr string
}

func NewLoop(cfg Config, source Source, actor Actor, logf func(string, ...any)) *Loop {
	return &Loop{source: source, actor: actor, logf: logf, Interval: Interval, guard: New(cfg)}
}

// State returns a copy of the newest state, or nil before the first sample.
func (l *Loop) State() *State {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == nil {
		return nil
	}
	copied := *l.state
	copied.Reasons = append([]string(nil), l.state.Reasons...)
	return &copied
}

// Recoverer is an Actor that can put back a power limit an earlier run
// lowered and never restored (PowerControl.RecordPath). It returns "" when
// there was nothing to put back.
type Recoverer interface {
	RecoverPower(ctx context.Context) (string, error)
}

func (l *Loop) Run(ctx context.Context) {
	cfg := l.guard.Config()
	l.logf("gpu guard: %s mode, hotspot warn %.0f C / critical %.0f C, delta warn %.0f C", cfg.Mode, cfg.HotspotWarnC, cfg.HotspotCritC, cfg.DeltaWarnC)
	// A fresh state machine knows nothing of a limit the last run lowered,
	// so it would never restore it. Put it back now; if the card is still
	// hot, the first critical episode lowers it again within seconds.
	if recoverer, ok := l.actor.(Recoverer); ok {
		recoverCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		what, err := recoverer.RecoverPower(recoverCtx)
		cancel()
		switch {
		case err != nil:
			l.logf("gpu guard: putting back the power limit an earlier run lowered failed: %v", err)
		case what != "":
			l.logf("gpu guard: an earlier run left the power limit lowered; %s", what)
		}
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		l.Step(ctx)
		timer.Reset(l.Interval)
	}
}

// Step takes one sample and carries out whatever it calls for.
func (l *Loop) Step(ctx context.Context) State {
	sample := readSampleSafely(ctx, l.source)
	l.mu.Lock()
	state, cmds := l.guard.Observe(sample)
	copied := state
	l.state = &copied
	l.mu.Unlock()
	dryRun := state.Mode != ModeAct
	for _, cmd := range cmds {
		err := l.execute(ctx, cmd, state, dryRun)
		if cmd.Kind == CmdNotify {
			continue
		}
		l.mu.Lock()
		l.actionErr = ""
		if err != nil {
			l.actionErr = fmt.Sprintf("%s: %v", cmd.Kind, err)
		}
		l.mu.Unlock()
	}
	l.mu.Lock()
	state.ActionError = l.actionErr
	l.state.ActionError = l.actionErr
	l.mu.Unlock()
	if l.OnStep != nil {
		l.OnStep(state)
	}
	return state
}

func readSampleSafely(ctx context.Context, source Source) (sample Sample) {
	defer func() {
		if failure := recover(); failure != nil {
			sample = Sample{At: time.Now(), Error: fmt.Sprintf("GPU guard sample failed: %v", failure)}
		}
	}()
	sample = source.ReadSample(ctx)
	if sample.At.IsZero() {
		sample.At = time.Now()
	}
	return sample
}

// execute carries out one command and logs it. Only an action's error is
// returned; a failed host alarm is logged and nothing more.
func (l *Loop) execute(parent context.Context, cmd Command, state State, dryRun bool) error {
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	readings := fmt.Sprintf("hotspot %s, core %s, delta %s, power %s",
		state.Hotspot, state.Core, state.Delta, state.Power)
	if cmd.Kind == CmdNotify {
		l.logf("gpu guard %s: %s (%s)", cmd.Level, cmd.Message, readings)
		if err := l.actor.Notify(ctx, cmd.Level, cmd.Message); err != nil {
			l.logf("gpu guard: host alarm failed: %v", err)
		}
		return nil
	}
	var (
		what string
		err  error
	)
	switch cmd.Kind {
	case CmdLimitPower:
		what, err = l.actor.LimitPower(ctx, dryRun)
	case CmdRestorePower:
		what, err = l.actor.RestorePower(ctx, dryRun)
	case CmdStopApps:
		what, err = l.actor.StopApps(ctx, dryRun)
	default:
		return nil
	}
	verb := ""
	if dryRun {
		verb = "would "
	}
	if err != nil {
		l.logf("gpu guard %s: %s%s failed: %v (%s)", cmd.Kind, verb, what, err, readings)
		return err
	}
	l.logf("gpu guard %s: %s%s (%s)", cmd.Kind, verb, what, readings)
	return nil
}

// String formats a reading for the log.
func (r Reading) String() string {
	if !r.Available {
		return "n/a"
	}
	return fmt.Sprintf("%.1f %s", r.Value, r.Unit)
}

// TestSource lets a file stand in for some readings
// ({"core":60,"hotspot":96,"power":400}), so the alarm and the dry-run log
// can be checked end to end without heating the card. Fields left out keep
// the real reading; with the file absent, every reading is real.
type TestSource struct {
	Inner Source
	Path  string
}

type testReading struct {
	Core    *float64 `json:"core"`
	Hotspot *float64 `json:"hotspot"`
	Power   *float64 `json:"power"`
}

func (s TestSource) ReadSample(ctx context.Context) Sample {
	sample := s.Inner.ReadSample(ctx)
	data, err := os.ReadFile(s.Path)
	if err != nil || len(data) > 4096 {
		return sample
	}
	var fake testReading
	if json.Unmarshal(data, &fake) != nil {
		return sample
	}
	if fake.Core != nil {
		sample.Core = fake.Core
	}
	if fake.Hotspot != nil {
		sample.Hotspot = fake.Hotspot
	}
	if fake.Power != nil {
		sample.Power = fake.Power
	}
	sample.Error = "test readings from " + s.Path
	return sample
}
