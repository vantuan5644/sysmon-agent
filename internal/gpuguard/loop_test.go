package gpuguard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeSource struct {
	now     time.Time
	hotspot float64
	core    float64
	power   float64
}

func (s *fakeSource) ReadSample(context.Context) Sample {
	s.now = s.now.Add(2 * time.Second)
	return Sample{At: s.now, GPU: "RTX 4090", Core: f(s.core), Hotspot: f(s.hotspot), Power: f(s.power)}
}

type fakeActor struct {
	calls    []string
	limitErr error
}

func (a *fakeActor) LimitPower(_ context.Context, dryRun bool) (string, error) {
	a.calls = append(a.calls, fmt.Sprintf("limit dry=%v", dryRun))
	return "power limit 450 W -> 150 W", a.limitErr
}

func (a *fakeActor) RestorePower(_ context.Context, dryRun bool) (string, error) {
	a.calls = append(a.calls, fmt.Sprintf("restore dry=%v", dryRun))
	return "power limit restored to 450 W", nil
}

func (a *fakeActor) StopApps(_ context.Context, dryRun bool) (string, error) {
	a.calls = append(a.calls, fmt.Sprintf("stop dry=%v", dryRun))
	return "stop Game.exe (11)", nil
}

func (a *fakeActor) Notify(_ context.Context, level Level, message string) error {
	a.calls = append(a.calls, "notify "+string(level))
	return errors.New("no console session")
}

func TestLoopWarnModeOnlyDescribesItsActions(t *testing.T) {
	source := &fakeSource{now: t0, hotspot: 97, core: 80, power: 440}
	actor := &fakeActor{}
	var logs []string
	loop := NewLoop(DefaultConfig(), source, actor, func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) })
	for i := 0; i < 8; i++ {
		loop.Step(context.Background())
	}
	if got := strings.Join(actor.calls, ", "); got != "notify critical, limit dry=true, stop dry=true" {
		t.Fatalf("calls %s", got)
	}
	all := strings.Join(logs, "\n")
	for _, want := range []string{"would power limit 450 W -> 150 W", "would stop Game.exe (11)", "host alarm failed: no console session", "hotspot 97.0 C, core 80.0 C, delta 17.0 C"} {
		if !strings.Contains(all, want) {
			t.Fatalf("log lacks %q:\n%s", want, all)
		}
	}
	if st := loop.State(); st == nil || st.Level != LevelCritical || st.Action != ActionNone || st.ActionError != "" {
		t.Fatalf("state %+v", st)
	}
}

func TestLoopReportsAFailedActionUntilOneSucceeds(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Mode = ModeAct
	source := &fakeSource{now: t0, hotspot: 97, core: 80, power: 440}
	actor := &fakeActor{limitErr: errors.New("Insufficient Permissions")}
	var published []State
	loop := NewLoop(cfg, source, actor, func(string, ...any) {})
	loop.OnStep = func(st State) { published = append(published, st) }
	for i := 0; i < 3; i++ {
		loop.Step(context.Background())
	}
	if st := loop.State(); st.ActionError != "limit_power: Insufficient Permissions" || published[2].ActionError != st.ActionError {
		t.Fatalf("state %+v", st)
	}
	for i := 0; i < 5; i++ {
		loop.Step(context.Background())
	}
	if st := loop.State(); st.ActionError != "" || st.Action != ActionAppsStopped {
		t.Fatalf("a stop that worked left %+v", st)
	}
}

type panickingSource struct{}

func (panickingSource) ReadSample(context.Context) Sample { panic("sensor bridge exploded") }

func TestLoopSurvivesAPanickingSource(t *testing.T) {
	loop := NewLoop(DefaultConfig(), panickingSource{}, &fakeActor{}, func(string, ...any) {})
	st := loop.Step(context.Background())
	if st.Level != LevelUnknown || !strings.Contains(st.Error, "sensor bridge exploded") {
		t.Fatalf("state %+v", st)
	}
}

func TestTestSourceOverridesOnlyTheGivenFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "readings.json")
	inner := &fakeSource{now: t0, hotspot: 60, core: 50, power: 100}
	source := TestSource{Inner: inner, Path: path}
	if s := source.ReadSample(context.Background()); *s.Hotspot != 60 || s.Error != "" {
		t.Fatalf("no file: %+v", s)
	}
	if err := os.WriteFile(path, []byte(`{"hotspot":96}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := source.ReadSample(context.Background())
	if *s.Hotspot != 96 || *s.Core != 50 || *s.Power != 100 || !strings.Contains(s.Error, "test readings") {
		t.Fatalf("override: %+v", s)
	}
}
