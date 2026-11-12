package gpuguard

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

func f(v float64) *float64 { return &v }

type step struct {
	at      time.Duration
	core    *float64
	hotspot *float64
	power   *float64
}

// run feeds samples every 2 s from t0 and returns the last state plus every
// command, each tagged with the second it was issued.
func run(t *testing.T, g *Guard, samples []step) (State, []string) {
	t.Helper()
	var st State
	var log []string
	for _, s := range samples {
		var cmds []Command
		st, cmds = g.Observe(Sample{At: t0.Add(s.at), GPU: "RTX 4090", Core: s.core, Hotspot: s.hotspot, Power: s.power})
		for _, c := range cmds {
			log = append(log, string(c.Kind)+":"+string(c.Level)+"@"+s.at.String())
		}
	}
	return st, log
}

// actConfig is the default config in act mode, where the state reports the
// action taken.
func actConfig() Config {
	cfg := DefaultConfig()
	cfg.Mode = ModeAct
	return cfg
}

func series(from, to time.Duration, core, hotspot, power float64) []step {
	var out []step
	for at := from; at <= to; at += 2 * time.Second {
		out = append(out, step{at: at, core: f(core), hotspot: f(hotspot), power: f(power)})
	}
	return out
}

func joined(parts ...[]step) []step {
	var out []step
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func TestQuietCardStaysOK(t *testing.T) {
	st, log := run(t, New(DefaultConfig()), series(0, 120*time.Second, 47, 57, 50))
	if st.Level != LevelOK || len(log) != 0 || st.Source != SourceHotspot || st.Action != ActionNone {
		t.Fatalf("state %+v, commands %v", st, log)
	}
	if !st.Delta.Available || st.Delta.Value != 10 {
		t.Fatalf("delta %+v", st.Delta)
	}
}

func TestOneHotSampleIsNotAnAlarm(t *testing.T) {
	samples := joined(series(0, 10*time.Second, 70, 80, 300),
		[]step{{at: 12 * time.Second, core: f(80), hotspot: f(99), power: f(400)}},
		series(14*time.Second, 30*time.Second, 70, 80, 300))
	st, log := run(t, New(DefaultConfig()), samples)
	if st.Level != LevelOK || len(log) != 0 {
		t.Fatalf("a single 99 C sample raised %v: %+v", log, st)
	}
}

func TestWarnNeedsThreeSamplesAndNotifiesOnce(t *testing.T) {
	st, log := run(t, New(DefaultConfig()), series(0, 60*time.Second, 75, 91, 400))
	if st.Level != LevelWarn || len(st.Reasons) != 1 || st.Reasons[0] != ReasonHotspotWarn {
		t.Fatalf("state %+v", st)
	}
	if len(log) != 1 || log[0] != "notify:warn@4s" {
		t.Fatalf("commands %v", log)
	}
}

func TestCriticalStagesPowerLimitThenStopsApps(t *testing.T) {
	st, log := run(t, New(actConfig()), series(0, 30*time.Second, 80, 97, 440))
	want := []string{"notify:critical@4s", "limit_power:critical@4s", "stop_apps:critical@14s", "stop_apps:critical@24s"}
	if strings.Join(log, " ") != strings.Join(want, " ") {
		t.Fatalf("commands %v, want %v", log, want)
	}
	if st.Level != LevelCritical || st.Action != ActionAppsStopped || st.CriticalSince == nil || !st.CriticalSince.Equal(t0.Add(4*time.Second)) {
		t.Fatalf("state %+v", st)
	}
	if !strings.Contains(st.Message, "97°C") || !strings.Contains(st.Message, "critical at 95°C") {
		t.Fatalf("message %q", st.Message)
	}
}

func TestCoolingAfterPowerLimitClearsWithoutStoppingApps(t *testing.T) {
	g := New(actConfig())
	samples := joined(series(0, 6*time.Second, 80, 96, 440), series(8*time.Second, 400*time.Second, 60, 70, 150))
	st, log := run(t, g, samples)
	// Critical at 4 s, the limit worked: 70 C from 8 s, so no apps are stopped.
	// Below 92 (critical-3) and below 87 (warn-3) from 8 s: ok after 30 s.
	// Below 80 (warn-10) from 8 s: restored after 5 min, at 308 s.
	want := []string{"notify:critical@4s", "limit_power:critical@4s", "restore_power:ok@5m8s"}
	if strings.Join(log, " ") != strings.Join(want, " ") {
		t.Fatalf("commands %v, want %v", log, want)
	}
	if st.Level != LevelOK || st.Action != ActionNone || st.CriticalSince != nil || st.LastAction != string(CmdRestorePower) {
		t.Fatalf("state %+v", st)
	}
}

func TestMessageSaysTheCardIsCoolingWhileTheLevelHolds(t *testing.T) {
	g := New(actConfig())
	st, _ := run(t, g, joined(series(0, 6*time.Second, 80, 96, 440), series(8*time.Second, 20*time.Second, 50, 60, 100)))
	if st.Level != LevelCritical || st.Message != "GPU hotspot 60°C, cooling after reaching the critical 95°C" {
		t.Fatalf("level %s, message %q", st.Level, st.Message)
	}
	st, _ = run(t, New(DefaultConfig()), joined(series(0, 6*time.Second, 75, 91, 300), series(8*time.Second, 14*time.Second, 70, 80, 300)))
	if st.Level != LevelWarn || st.Message != "GPU hotspot 80°C, cooling after reaching the warning 90°C" {
		t.Fatalf("level %s, message %q", st.Level, st.Message)
	}
}

func TestCriticalFallsBackToWarnBeforeOK(t *testing.T) {
	samples := joined(series(0, 6*time.Second, 80, 96, 440), series(8*time.Second, 50*time.Second, 75, 89, 300))
	st, log := run(t, New(actConfig()), samples)
	// 89 is under 92 for 30 s but not under 87, so the level steps down to warn,
	// without a second notification, and the limit stays lowered.
	if st.Level != LevelWarn || st.Action != ActionPowerLimited {
		t.Fatalf("state %+v", st)
	}
	if strings.Join(log, " ") != "notify:critical@4s limit_power:critical@4s" {
		t.Fatalf("commands %v", log)
	}
}

func TestHysteresisKeepsAlarmNearTheThreshold(t *testing.T) {
	samples := joined(series(0, 6*time.Second, 80, 96, 440), series(8*time.Second, 120*time.Second, 80, 93, 440))
	st, _ := run(t, New(DefaultConfig()), samples)
	if st.Level != LevelCritical {
		t.Fatalf("93 C sits within the 3 C clear margin of 95 C, yet the level is %s", st.Level)
	}
}

func TestCoreFallbackUsesCoreThresholds(t *testing.T) {
	var samples []step
	for at := time.Duration(0); at <= 20*time.Second; at += 2 * time.Second {
		samples = append(samples, step{at: at, core: f(89), power: f(400)})
	}
	st, log := run(t, New(DefaultConfig()), samples)
	if st.Source != SourceCore || st.Level != LevelCritical || st.Reasons[0] != ReasonCoreCritical {
		t.Fatalf("state %+v", st)
	}
	if st.Delta.Available || st.Thresholds.CritC != DefaultCoreCritC {
		t.Fatalf("delta %+v thresholds %+v", st.Delta, st.Thresholds)
	}
	if len(log) < 2 || log[1] != "limit_power:critical@4s" {
		t.Fatalf("commands %v", log)
	}
	if !strings.Contains(st.Message, "no hotspot sensor") {
		t.Fatalf("message %q", st.Message)
	}
}

func TestLostSensorKeepsLimitButStopsNothing(t *testing.T) {
	samples := series(0, 6*time.Second, 80, 96, 440)
	for at := 8 * time.Second; at <= 60*time.Second; at += 2 * time.Second {
		samples = append(samples, step{at: at})
	}
	st, log := run(t, New(actConfig()), samples)
	if strings.Join(log, " ") != "notify:critical@4s limit_power:critical@4s" {
		t.Fatalf("commands %v", log)
	}
	if st.Level != LevelCritical || st.Source != SourceNone || st.Action != ActionPowerLimited || !strings.Contains(st.Message, "power limit kept") {
		t.Fatalf("state %+v", st)
	}
}

func TestWarnModeReportsNoActionTaken(t *testing.T) {
	st, log := run(t, New(DefaultConfig()), series(0, 30*time.Second, 80, 97, 440))
	// The commands still come (the loop logs them as "would ..."), but the
	// state must not tell the dashboard that anything was done.
	if !strings.Contains(strings.Join(log, " "), "stop_apps:critical@14s") {
		t.Fatalf("commands %v", log)
	}
	if st.Level != LevelCritical || st.Action != ActionNone || st.LastAction != "would_stop_apps" {
		t.Fatalf("state %+v", st)
	}
}

func TestNoReadingIsUnknown(t *testing.T) {
	st, log := run(t, New(DefaultConfig()), []step{{at: 0}, {at: 2 * time.Second}})
	if st.Level != LevelUnknown || len(log) != 0 || st.Message == "" {
		t.Fatalf("state %+v commands %v", st, log)
	}
}

func TestDeltaWarnsOnlyUnderLoadAndAfterAMinute(t *testing.T) {
	// 30 C apart at idle power: never judged.
	st, log := run(t, New(DefaultConfig()), series(0, 120*time.Second, 50, 80, 60))
	if st.Level != LevelOK || len(log) != 0 {
		t.Fatalf("idle delta raised %v: %+v", log, st)
	}
	// 30 C apart under load: warns once the minute is up.
	st, log = run(t, New(DefaultConfig()), series(0, 70*time.Second, 55, 85, 350))
	if st.Level != LevelWarn || len(st.Reasons) != 1 || st.Reasons[0] != ReasonDeltaWarn {
		t.Fatalf("state %+v", st)
	}
	if len(log) != 1 || log[0] != "notify:warn@1m0s" || !strings.Contains(st.Message, "thermal contact") {
		t.Fatalf("commands %v message %q", log, st.Message)
	}
}

func TestDeltaDefaultSitsAboveTheMeasuredNormal(t *testing.T) {
	// The 4090's measured gap in a game, about 10 C: never a warning.
	st, log := run(t, New(DefaultConfig()), series(0, 180*time.Second, 64, 74, 380))
	if st.Level != LevelOK || len(log) != 0 {
		t.Fatalf("a normal 10 C gap raised %v: %+v", log, st)
	}
	// Just under the line, held for minutes: still nothing.
	st, log = run(t, New(DefaultConfig()), series(0, 180*time.Second, 64, 83.5, 380))
	if st.Level != LevelOK || len(log) != 0 {
		t.Fatalf("a 19.5 C gap raised %v: %+v", log, st)
	}
	// 21 C, double the normal: warns.
	st, _ = run(t, New(DefaultConfig()), series(0, 70*time.Second, 64, 85, 380))
	if st.Level != LevelWarn || len(st.Reasons) != 1 || st.Reasons[0] != ReasonDeltaWarn {
		t.Fatalf("a 21 C gap: %+v", st)
	}
}

func TestDeltaWarningClearsWhenTheLoadStops(t *testing.T) {
	samples := joined(series(0, 70*time.Second, 55, 85, 350), series(72*time.Second, 110*time.Second, 45, 55, 60))
	st, _ := run(t, New(DefaultConfig()), samples)
	if st.Level != LevelOK || len(st.Reasons) != 0 {
		t.Fatalf("state %+v", st)
	}
}

func TestDeltaAndTemperatureReasonsCombine(t *testing.T) {
	st, _ := run(t, New(DefaultConfig()), series(0, 70*time.Second, 64, 91, 400))
	if st.Level != LevelWarn || strings.Join(st.Reasons, ",") != ReasonHotspotWarn+","+ReasonDeltaWarn {
		t.Fatalf("state %+v", st)
	}
}

func TestAlarmRepeatsWhileCritical(t *testing.T) {
	cfg := DefaultConfig()
	cfg.KillEvery = time.Hour // keep the log short
	cfg.KillAfter = time.Hour
	_, log := run(t, New(cfg), series(0, 11*time.Minute, 80, 97, 440))
	notifies := 0
	for _, l := range log {
		if strings.HasPrefix(l, "notify:critical") {
			notifies++
		}
	}
	if notifies != 3 { // 4 s, 5m4s, 10m4s
		t.Fatalf("critical notifications %d in %v", notifies, log)
	}
}

// Found in the 2026-11-12 end-to-end run: the 5-minute repeat landed while the
// card was already cooling through the clear hold, and raised a fresh
// system-modal box that said so.
func TestAlarmDoesNotRepeatWhileCooling(t *testing.T) {
	cfg := DefaultConfig()
	cfg.KillEvery = time.Hour
	cfg.KillAfter = time.Hour
	// Critical from 4 s; cools to 60 C at 4m58s, inside the 30 s clear hold
	// when the repeat comes due at 5m4s.
	samples := joined(series(0, 4*time.Minute+56*time.Second, 80, 97, 440), series(4*time.Minute+58*time.Second, 6*time.Minute, 50, 60, 100))
	st, log := run(t, New(cfg), samples)
	// The restore is still minutes away (5 min below 80 C from 4m58s).
	if strings.Join(log, " ") != "notify:critical@4s limit_power:critical@4s" {
		t.Fatalf("commands %v", log)
	}
	if st.Level != LevelOK {
		t.Fatalf("state %+v", st)
	}
	// Blind is not cooling: with the sensor gone the alarm still repeats.
	blind := series(0, 6*time.Second, 80, 97, 440)
	for at := 8 * time.Second; at <= 6*time.Minute; at += 2 * time.Second {
		blind = append(blind, step{at: at})
	}
	_, log = run(t, New(cfg), blind)
	if strings.Count(strings.Join(log, " "), "notify:critical") != 2 {
		t.Fatalf("blind commands %v", log)
	}
}

func TestParseModeAndValidate(t *testing.T) {
	for raw, want := range map[string]Mode{"": ModeWarn, "warn": ModeWarn, "ACT": ModeAct, " off ": ModeOff} {
		if got, err := ParseMode(raw); err != nil || got != want {
			t.Fatalf("ParseMode(%q) = %q, %v", raw, got, err)
		}
	}
	if _, err := ParseMode("kill"); err == nil {
		t.Fatal("ParseMode accepted kill")
	}
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatal(err)
	}
	bad := DefaultConfig()
	bad.HotspotCritC = 90
	if err := bad.Validate(); err == nil {
		t.Fatal("critical equal to warn validated")
	}
	bad = DefaultConfig()
	bad.DeltaWarnC = 5
	if err := bad.Validate(); err == nil {
		t.Fatal("delta 5 validated")
	}
}
