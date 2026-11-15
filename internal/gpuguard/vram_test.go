package gpuguard

import (
	"strings"
	"testing"
	"time"
)

type vstep struct {
	at                 time.Duration
	core, hotspot, mem *float64
	power              *float64
}

// vseries is a steady die (core 70, hotspot 80, 300 W) with the memory
// junction at mem, one sample every 2 s.
func vseries(from, to time.Duration, mem float64) []vstep {
	var out []vstep
	for at := from; at <= to; at += 2 * time.Second {
		out = append(out, vstep{at: at, core: f(70), hotspot: f(80), mem: f(mem), power: f(300)})
	}
	return out
}

func vrun(t *testing.T, g *Guard, parts ...[]vstep) (State, []string, []Command) {
	t.Helper()
	var st State
	var log []string
	var all []Command
	for _, part := range parts {
		for _, s := range part {
			var cmds []Command
			st, cmds = g.Observe(Sample{At: t0.Add(s.at), GPU: "RTX 3090", Core: s.core, Hotspot: s.hotspot, VRAM: s.mem, Power: s.power})
			for _, c := range cmds {
				log = append(log, string(c.Kind)+":"+string(c.Level)+"@"+s.at.String())
			}
			all = append(all, cmds...)
		}
	}
	return st, log, all
}

func TestVRAMIsPublishedAndQuietBelowItsLines(t *testing.T) {
	st, log, _ := vrun(t, New(DefaultConfig()), vseries(0, 60*time.Second, 84))
	if st.Level != LevelOK || len(log) != 0 {
		t.Fatalf("state %+v, commands %v", st, log)
	}
	if !st.VRAM.Available || st.VRAM.Value != 84 || st.Thresholds.VRAMWarnC != DefaultVRAMWarnC || st.Thresholds.VRAMCritC != DefaultVRAMCritC {
		t.Fatalf("vram %+v thresholds %+v", st.VRAM, st.Thresholds)
	}
}

func TestVRAMWarnsOnceWhileTheDieIsFine(t *testing.T) {
	st, log, cmds := vrun(t, New(DefaultConfig()), vseries(0, 30*time.Second, 101))
	if st.Level != LevelWarn || len(log) != 1 || log[0] != "notify:warn@4s" {
		t.Fatalf("state %+v, commands %v", st, log)
	}
	if !strings.Contains(cmds[0].Message, "memory (VRAM) 101°C") || !strings.Contains(st.Message, "VRAM") {
		t.Fatalf("message %q / %q", cmds[0].Message, st.Message)
	}
	if len(st.Reasons) != 1 || st.Reasons[0] != ReasonVRAMWarn {
		t.Fatalf("reasons %v", st.Reasons)
	}
}

func TestVRAMCriticalRunsTheStagedAction(t *testing.T) {
	st, log, cmds := vrun(t, New(actConfig()), vseries(0, 20*time.Second, 108))
	want := []string{"notify:critical@4s", "limit_power:critical@4s", "stop_apps:critical@14s"}
	if strings.Join(log, " ") != strings.Join(want, " ") {
		t.Fatalf("commands %v, want %v", log, want)
	}
	if st.Level != LevelCritical || st.Action != ActionAppsStopped || st.Reasons[0] != ReasonVRAMCritical {
		t.Fatalf("state %+v", st)
	}
	if !strings.Contains(cmds[0].Message, "memory (VRAM) 108°C, critical at 106°C") {
		t.Fatalf("alarm %q", cmds[0].Message)
	}
}

func TestVRAMHoldsTheLimitUntilTheMemoryCools(t *testing.T) {
	g := New(actConfig())
	// Critical on memory, then the die stays cool while the memory sits
	// just under the warning: the limit must stay lowered.
	_, log, _ := vrun(t, g, vseries(0, 6*time.Second, 108), vseries(8*time.Second, 400*time.Second, 98))
	for _, line := range log {
		if strings.HasPrefix(line, "restore_power") {
			t.Fatalf("restored while the memory was still warm: %v", log)
		}
	}
	// Once both are 10 below their warnings for five minutes, the limit
	// returns.
	var cool []vstep
	for at := 402 * time.Second; at <= 760*time.Second; at += 2 * time.Second {
		cool = append(cool, vstep{at: at, core: f(60), hotspot: f(70), mem: f(85), power: f(150)})
	}
	_, log, _ = vrun(t, g, cool)
	restored := false
	for _, line := range log {
		restored = restored || strings.HasPrefix(line, "restore_power")
	}
	if !restored {
		t.Fatalf("never restored: %v", log)
	}
}

func TestVRAMWarningDoesNotStopAppsDuringAHotspotEpisode(t *testing.T) {
	// The hotspot goes critical with the memory merely warm, then the
	// hotspot cools below its line: apps are ended only while some reading
	// is over a critical line.
	var hot []vstep
	for at := time.Duration(0); at <= 8*time.Second; at += 2 * time.Second {
		hot = append(hot, vstep{at: at, core: f(80), hotspot: f(97), mem: f(102), power: f(350)})
	}
	var cooling []vstep
	for at := 10 * time.Second; at <= 40*time.Second; at += 2 * time.Second {
		cooling = append(cooling, vstep{at: at, core: f(75), hotspot: f(93), mem: f(102), power: f(150)})
	}
	_, log, _ := vrun(t, New(actConfig()), hot, cooling)
	for _, line := range log {
		if strings.HasPrefix(line, "stop_apps") {
			t.Fatalf("apps ended with nothing over a critical line: %v", log)
		}
	}
	for _, line := range log {
		if line == "notify:warn@"+(10*time.Second).String() {
			t.Fatalf("a VRAM warning was raised during the critical episode: %v", log)
		}
	}
}

// noVRAM is a steady, cool die with the junction reading missing.
func noVRAM(from, to time.Duration, hotspot float64) []vstep {
	var out []vstep
	for at := from; at <= to; at += 2 * time.Second {
		out = append(out, vstep{at: at, core: f(hotspot - 10), hotspot: f(hotspot), power: f(300)})
	}
	return out
}

func TestALostVRAMWarningHoldsThroughAShortGapAndEndsAfterTheClearHold(t *testing.T) {
	g := New(DefaultConfig())
	st, log, _ := vrun(t, g, vseries(0, 10*time.Second, 101))
	if st.Level != LevelWarn || len(log) != 1 {
		t.Fatalf("not warning once: %+v %v", st, log)
	}
	// A flapping reading neither ends the warning nor warns again.
	for i := 0; i < 4; i++ {
		base := time.Duration(12+i*8) * time.Second
		st, log, _ = vrun(t, g, noVRAM(base, base, 70), vseries(base+2*time.Second, base+6*time.Second, 101))
		if st.Level != LevelWarn || len(log) != 0 {
			t.Fatalf("flap %d: %+v %v", i, st, log)
		}
	}
	// Lost for longer than the clear hold, the warning ends.
	st, _, _ = vrun(t, g, noVRAM(50*time.Second, 90*time.Second, 70))
	if st.Level != LevelOK {
		t.Fatalf("a long-lost warning stuck: %+v", st)
	}
}

func TestALostVRAMCriticalKeepsTheLimitAndKeepsAlarming(t *testing.T) {
	g := New(actConfig())
	vrun(t, g, vseries(0, 6*time.Second, 108))
	st, log, _ := vrun(t, g, noVRAM(8*time.Second, 2*time.Hour, 60))
	if st.Level != LevelCritical || st.Action == ActionNone || !strings.Contains(st.Message, "lost") {
		t.Fatalf("a lost critical cleared: %+v", st)
	}
	alarms, restores := 0, 0
	for _, line := range log {
		if strings.HasPrefix(line, "notify:critical") {
			alarms++
		}
		if strings.HasPrefix(line, "restore_power") || strings.HasPrefix(line, "stop_apps") {
			restores++
		}
	}
	// Two hours at one alarm every five minutes, and nothing ended or
	// restored on a reading nobody took.
	if alarms < 20 || restores != 0 {
		t.Fatalf("%d repeat alarms, %d actions: %v", alarms, restores, log)
	}
}

func TestTheLimitStaysLoweredWhileAJunctionSeenHotIsMissing(t *testing.T) {
	g := New(actConfig())
	vrun(t, g, vseries(0, 6*time.Second, 107), vseries(8*time.Second, 48*time.Second, 100))
	_, log, _ := vrun(t, g, noVRAM(50*time.Second, 20*time.Minute, 70))
	for _, line := range log {
		if strings.HasPrefix(line, "restore_power") {
			t.Fatalf("restored with the junction last seen at 100 C: %v", log)
		}
	}
}

func TestOneUnheldSampleFromTheOtherSensorEndsNothing(t *testing.T) {
	var steps []vstep
	for at := time.Duration(0); at <= 16*time.Second; at += 2 * time.Second {
		steps = append(steps, vstep{at: at, core: f(80), hotspot: f(96), mem: f(90), power: f(350)})
	}
	for at := 18 * time.Second; at <= 22*time.Second; at += 2 * time.Second {
		steps = append(steps, vstep{at: at, core: f(75), hotspot: f(85), mem: f(90), power: f(150)})
	}
	g := New(actConfig())
	vrun(t, g, steps)
	_, log, _ := vrun(t, g, []vstep{{at: 30 * time.Second, core: f(75), hotspot: f(85), mem: f(106.5), power: f(150)}})
	for _, line := range log {
		if strings.HasPrefix(line, "stop_apps") {
			t.Fatalf("one junction sample over the line ended apps: %v", log)
		}
	}
}

func TestADieEpisodeKeepsAlarmingWithOnlyTheJunctionRead(t *testing.T) {
	g := New(DefaultConfig())
	var hot, lost []vstep
	for at := time.Duration(0); at <= 4*time.Second; at += 2 * time.Second {
		hot = append(hot, vstep{at: at, core: f(80), hotspot: f(96), mem: f(90), power: f(350)})
	}
	for at := 6 * time.Second; at <= 12*time.Minute; at += 2 * time.Second {
		lost = append(lost, vstep{at: at, mem: f(90)})
	}
	_, log, _ := vrun(t, g, hot, lost)
	alarms := 0
	for _, line := range log {
		if strings.HasPrefix(line, "notify:critical") {
			alarms++
		}
	}
	if alarms < 3 {
		t.Fatalf("a cool junction silenced a die episode with no die reading: %v", log)
	}
}

func TestNoVRAMReadingLeavesTheGuardAsBefore(t *testing.T) {
	st, log := run(t, New(DefaultConfig()), series(0, 30*time.Second, 47, 57, 50))
	if st.Level != LevelOK || len(log) != 0 || st.VRAM.Available {
		t.Fatalf("state %+v, commands %v", st, log)
	}
}

func TestVRAMLinesValidate(t *testing.T) {
	for _, mut := range []func(*Config){
		func(c *Config) { c.VRAMWarnC = 79 },
		func(c *Config) { c.VRAMCritC = 116 },
		func(c *Config) { c.VRAMWarnC, c.VRAMCritC = 105, 104 },
	} {
		cfg := DefaultConfig()
		mut(&cfg)
		if cfg.Validate() == nil {
			t.Fatalf("accepted %+v", cfg)
		}
	}
}
