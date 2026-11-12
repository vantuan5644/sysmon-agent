// Package gpuguard decides when a GPU is too hot and what to do about it.
//
// It is a pure state machine: Observe takes one temperature sample and returns
// the published state plus the commands the caller should carry out (notify,
// lower the power limit, stop GPU apps, restore the limit). It performs no I/O,
// so the Windows agent and the Linux root helper share it and its tests.
//
// Levels follow the hotspot when it is reported and fall back to the core
// temperature, with lower thresholds, when it is not. A level rises only after
// HoldSamples consecutive samples over its threshold and falls only after the
// reading stays ClearMarginC below it for ClearHold, so one noisy sample never
// starts or ends an alarm. A large hotspot-minus-core delta under load raises a
// separate warning: the two read close together on a card whose cooler
// touches the die evenly, and drift apart when the paste has pumped out.
package gpuguard

import (
	"fmt"
	"math"
	"strings"
	"time"
)

type Mode string

const (
	ModeOff  Mode = "off"
	ModeWarn Mode = "warn" // alarms only; actions are logged as "would ..."
	ModeAct  Mode = "act"  // alarms plus the staged action
)

func ParseMode(raw string) (Mode, error) {
	switch Mode(strings.ToLower(strings.TrimSpace(raw))) {
	case "", ModeWarn:
		return ModeWarn, nil
	case ModeOff:
		return ModeOff, nil
	case ModeAct:
		return ModeAct, nil
	}
	return "", fmt.Errorf("gpu guard mode %q: want off, warn or act", raw)
}

type Level string

const (
	LevelOK       Level = "ok"
	LevelWarn     Level = "warn"
	LevelCritical Level = "critical"
	// LevelUnknown means no temperature could be read. An episode in progress
	// keeps its action state (a lowered power limit stays lowered), because
	// losing the sensor is no evidence that the card cooled down.
	LevelUnknown Level = "unknown"
)

func levelRank(l Level) int {
	switch l {
	case LevelWarn:
		return 1
	case LevelCritical:
		return 2
	}
	return 0
}

type Config struct {
	Mode           Mode
	HotspotWarnC   float64
	HotspotCritC   float64
	CoreWarnC      float64 // used only when no hotspot is reported
	CoreCritC      float64
	DeltaWarnC     float64
	DeltaMinPowerW float64 // the delta is judged only while the GPU draws this much
	HoldSamples    int
	DeltaHold      time.Duration
	ClearHold      time.Duration
	ClearMarginC   float64
	KillAfter      time.Duration // critical this long after lowering the limit: stop apps
	KillEvery      time.Duration // and again this often while it stays critical
	RestoreAfter   time.Duration // below warn-RestoreMarginC this long: restore the limit
	RestoreMarginC float64
	AlarmRepeat    time.Duration // re-send the host alarm this often while critical
}

// Thresholds limits, chosen 2026-11-12 for the RTX 4090: NVIDIA publishes no
// hotspot limit, the core's documented maximum is 90 C, and Ada cards throttle
// on their own at a hotspot of about 100-105 C. Warn 90 / critical 95 leaves
// 5-10 C before that. The delta line is 10 C above the card's measured normal:
// BBLWIN's 4090 runs its hotspot about 10 C above the core both at idle and in
// a game (2026-11-12), so 20 flags pump-out well before it gets bad.
const (
	DefaultHotspotWarnC = 90
	DefaultHotspotCritC = 95
	DefaultCoreWarnC    = 83
	DefaultCoreCritC    = 88
	DefaultDeltaWarnC   = 20

	MinHotspotWarnC, MaxHotspotWarnC = 70, 105
	MinHotspotCritC, MaxHotspotCritC = 80, 110
	MinDeltaWarnC, MaxDeltaWarnC     = 10, 40
)

func DefaultConfig() Config {
	return Config{
		Mode:           ModeWarn,
		HotspotWarnC:   DefaultHotspotWarnC,
		HotspotCritC:   DefaultHotspotCritC,
		CoreWarnC:      DefaultCoreWarnC,
		CoreCritC:      DefaultCoreCritC,
		DeltaWarnC:     DefaultDeltaWarnC,
		DeltaMinPowerW: 150,
		HoldSamples:    3,
		DeltaHold:      60 * time.Second,
		ClearHold:      30 * time.Second,
		ClearMarginC:   3,
		KillAfter:      10 * time.Second,
		KillEvery:      10 * time.Second,
		RestoreAfter:   5 * time.Minute,
		RestoreMarginC: 10,
		AlarmRepeat:    5 * time.Minute,
	}
}

// Validate checks the user-settable thresholds. Zero values were replaced by
// defaults before this runs.
func (c Config) Validate() error {
	if c.HotspotWarnC < MinHotspotWarnC || c.HotspotWarnC > MaxHotspotWarnC {
		return fmt.Errorf("gpu hotspot warn %g C: want %d-%d", c.HotspotWarnC, MinHotspotWarnC, MaxHotspotWarnC)
	}
	if c.HotspotCritC < MinHotspotCritC || c.HotspotCritC > MaxHotspotCritC {
		return fmt.Errorf("gpu hotspot critical %g C: want %d-%d", c.HotspotCritC, MinHotspotCritC, MaxHotspotCritC)
	}
	if c.HotspotCritC <= c.HotspotWarnC {
		return fmt.Errorf("gpu hotspot critical %g C must be above warn %g C", c.HotspotCritC, c.HotspotWarnC)
	}
	if c.DeltaWarnC < MinDeltaWarnC || c.DeltaWarnC > MaxDeltaWarnC {
		return fmt.Errorf("gpu hotspot-core delta warn %g C: want %d-%d", c.DeltaWarnC, MinDeltaWarnC, MaxDeltaWarnC)
	}
	if c.HoldSamples < 1 {
		return fmt.Errorf("gpu guard hold samples must be at least 1")
	}
	return nil
}

// Sample is one reading of the guarded GPU. Nil means not reported.
type Sample struct {
	At      time.Time
	GPU     string
	Core    *float64
	Hotspot *float64
	Power   *float64
	Error   string
}

// Reading mirrors the agent's NumberMetric JSON shape.
type Reading struct {
	Available bool    `json:"available"`
	Value     float64 `json:"value"`
	Unit      string  `json:"unit,omitempty"`
}

type Thresholds struct {
	WarnC  float64 `json:"warn_c"`
	CritC  float64 `json:"critical_c"`
	DeltaC float64 `json:"delta_warn_c"`
}

const (
	ReasonHotspotWarn     = "hotspot_warn"
	ReasonHotspotCritical = "hotspot_critical"
	ReasonCoreWarn        = "core_warn"
	ReasonCoreCritical    = "core_critical"
	ReasonDeltaWarn       = "delta_warn"

	SourceHotspot = "hotspot"
	SourceCore    = "core"
	SourceNone    = "none"

	ActionNone         = "none"
	ActionPowerLimited = "power_limited"
	ActionAppsStopped  = "apps_stopped"
)

// State is what the dashboard and the logs see.
type State struct {
	Mode       Mode       `json:"mode"`
	Level      Level      `json:"level"`
	Source     string     `json:"source"`
	Reasons    []string   `json:"reasons,omitempty"`
	Message    string     `json:"message,omitempty"`
	GPU        string     `json:"gpu,omitempty"`
	Hotspot    Reading    `json:"hotspot_celsius"`
	Core       Reading    `json:"core_celsius"`
	Delta      Reading    `json:"delta_celsius"`
	Power      Reading    `json:"power_watts"`
	Thresholds Thresholds `json:"thresholds"`
	Action     string     `json:"action"`
	// ActionError is the newest failed action's error, set by Loop and
	// cleared by the next action that succeeds.
	ActionError   string     `json:"action_error,omitempty"`
	LastAction    string     `json:"last_action,omitempty"`
	LastActionAt  *time.Time `json:"last_action_at,omitempty"`
	CriticalSince *time.Time `json:"critical_since,omitempty"`
	Error         string     `json:"error,omitempty"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

type CommandKind string

const (
	CmdNotify       CommandKind = "notify"
	CmdLimitPower   CommandKind = "limit_power"
	CmdStopApps     CommandKind = "stop_apps"
	CmdRestorePower CommandKind = "restore_power"
)

type Command struct {
	Kind    CommandKind
	Level   Level
	Message string
}

type Guard struct {
	cfg Config

	tempLevel  Level // ok|warn|critical, from the temperature alone
	source     string
	overWarn   int
	overCrit   int
	belowSince time.Time // reading continuously below the current level's clear point

	deltaOn     bool
	deltaSince  time.Time // over the delta threshold continuously since
	deltaBelow  time.Time // under the delta clear point continuously since
	deltaNotify bool

	warnNotified  bool
	criticalSince time.Time
	lastAlarm     time.Time
	powerLimited  bool
	appsStopped   bool
	nextKill      time.Time
	coolSince     time.Time

	lastAction   string
	lastActionAt time.Time
}

func New(cfg Config) *Guard {
	return &Guard{cfg: cfg, tempLevel: LevelOK, source: SourceNone}
}

func (g *Guard) Config() Config { return g.cfg }

func reading(v *float64, unit string) Reading {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return Reading{Unit: unit}
	}
	return Reading{Available: true, Value: math.Round(*v*10) / 10, Unit: unit}
}

func (g *Guard) thresholds(source string) (warn, crit float64) {
	if source == SourceCore {
		return g.cfg.CoreWarnC, g.cfg.CoreCritC
	}
	return g.cfg.HotspotWarnC, g.cfg.HotspotCritC
}

// Observe folds one sample into the guard and returns the state to publish and
// the commands to carry out now.
func (g *Guard) Observe(s Sample) (State, []Command) {
	now := s.At
	if now.IsZero() {
		now = time.Now()
	}
	var cmds []Command

	source, temp := SourceNone, 0.0
	hot, core := reading(s.Hotspot, "C"), reading(s.Core, "C")
	switch {
	case hot.Available:
		source, temp = SourceHotspot, *s.Hotspot
	case core.Available:
		source, temp = SourceCore, *s.Core
	}
	if source != g.source {
		// Different sensor, different thresholds: start counting again. The
		// level and any action in progress stay as they are.
		g.overWarn, g.overCrit, g.belowSince = 0, 0, time.Time{}
		g.source = source
	}
	warn, crit := g.thresholds(source)

	if source != SourceNone {
		g.stepTemperature(now, temp, warn, crit)
	}
	delta := Reading{Unit: "C"}
	power := reading(s.Power, "W")
	if hot.Available && core.Available {
		delta = Reading{Available: true, Value: math.Round((*s.Hotspot-*s.Core)*10) / 10, Unit: "C"}
		g.stepDelta(now, *s.Hotspot-*s.Core, power)
	} else {
		g.deltaSince, g.deltaBelow = time.Time{}, time.Time{}
	}

	// Commands follow the level just computed.
	if g.tempLevel == LevelCritical {
		if g.criticalSince.IsZero() {
			g.criticalSince = now
			g.lastAlarm = now
			cmds = append(cmds, Command{Kind: CmdNotify, Level: LevelCritical, Message: g.message(source, temp, warn, crit)})
			if !g.powerLimited {
				g.powerLimited = true
				cmds = append(cmds, Command{Kind: CmdLimitPower, Level: LevelCritical})
			}
			g.nextKill = now.Add(g.cfg.KillAfter)
		} else {
			// With the sensor gone the limit stays lowered, but nothing new is
			// ended on a reading nobody took.
			// Apps are stopped only while the reading itself is still over
			// the critical line: a card the lower limit already cooled sits
			// out the clear hold at the critical level, and needs nothing
			// ended for it.
			if source != SourceNone && g.overCrit > 0 && !now.Before(g.nextKill) {
				g.appsStopped = true
				g.nextKill = now.Add(g.cfg.KillEvery)
				cmds = append(cmds, Command{Kind: CmdStopApps, Level: LevelCritical})
			}
			// The host alarm repeats while the reading is still over the line,
			// or while there is none to say otherwise. A reading that shows the
			// card cooling through the clear hold gets no fresh system-modal box.
			stillHot := source == SourceNone || g.overCrit > 0
			if stillHot && g.cfg.AlarmRepeat > 0 && now.Sub(g.lastAlarm) >= g.cfg.AlarmRepeat {
				g.lastAlarm = now
				cmds = append(cmds, Command{Kind: CmdNotify, Level: LevelCritical, Message: g.message(source, temp, warn, crit)})
			}
		}
	} else if source != SourceNone {
		g.criticalSince = time.Time{}
	}

	if g.tempLevel == LevelWarn && !g.warnNotified {
		g.warnNotified = true
		cmds = append(cmds, Command{Kind: CmdNotify, Level: LevelWarn, Message: g.message(source, temp, warn, crit)})
	}
	if g.tempLevel == LevelOK {
		g.warnNotified = false
	}
	if g.deltaOn && !g.deltaNotify {
		g.deltaNotify = true
		cmds = append(cmds, Command{Kind: CmdNotify, Level: LevelWarn, Message: g.deltaMessage(delta.Value)})
	}
	if !g.deltaOn {
		g.deltaNotify = false
	}

	// Restore the power limit once the card has been cool for a while.
	if g.powerLimited && source != SourceNone {
		if temp < warn-g.cfg.RestoreMarginC {
			if g.coolSince.IsZero() {
				g.coolSince = now
			}
			if g.tempLevel != LevelCritical && now.Sub(g.coolSince) >= g.cfg.RestoreAfter {
				g.powerLimited, g.appsStopped, g.coolSince = false, false, time.Time{}
				cmds = append(cmds, Command{Kind: CmdRestorePower, Level: g.tempLevel})
			}
		} else {
			g.coolSince = time.Time{}
		}
	} else {
		g.coolSince = time.Time{}
	}

	for _, c := range cmds {
		if c.Kind != CmdNotify {
			g.lastAction, g.lastActionAt = string(c.Kind), now
			if g.cfg.Mode != ModeAct {
				g.lastAction = "would_" + g.lastAction
			}
		}
	}
	return g.state(now, s, source, temp, warn, crit, hot, core, delta, power), cmds
}

func (g *Guard) stepTemperature(now time.Time, temp, warn, crit float64) {
	if temp >= crit {
		g.overCrit++
	} else {
		g.overCrit = 0
	}
	if temp >= warn {
		g.overWarn++
	} else {
		g.overWarn = 0
	}
	hold := g.cfg.HoldSamples
	switch {
	case g.overCrit >= hold && g.tempLevel != LevelCritical:
		g.tempLevel, g.belowSince = LevelCritical, time.Time{}
		return
	case g.overWarn >= hold && g.tempLevel == LevelOK:
		g.tempLevel, g.belowSince = LevelWarn, time.Time{}
		return
	}
	clearAt := 0.0
	switch g.tempLevel {
	case LevelCritical:
		clearAt = crit - g.cfg.ClearMarginC
	case LevelWarn:
		clearAt = warn - g.cfg.ClearMarginC
	default:
		return
	}
	if temp >= clearAt {
		g.belowSince = time.Time{}
		return
	}
	if g.belowSince.IsZero() {
		g.belowSince = now
	}
	if now.Sub(g.belowSince) < g.cfg.ClearHold {
		return
	}
	g.belowSince = time.Time{}
	if g.tempLevel == LevelCritical && temp >= warn-g.cfg.ClearMarginC {
		g.tempLevel = LevelWarn
		g.warnNotified = true // the critical alarm already said it
		return
	}
	g.tempLevel = LevelOK
}

func (g *Guard) stepDelta(now time.Time, delta float64, power Reading) {
	loaded := !power.Available || power.Value >= g.cfg.DeltaMinPowerW
	if loaded && delta >= g.cfg.DeltaWarnC {
		g.deltaBelow = time.Time{}
		if g.deltaSince.IsZero() {
			g.deltaSince = now
		}
		if now.Sub(g.deltaSince) >= g.cfg.DeltaHold {
			g.deltaOn = true
		}
		return
	}
	g.deltaSince = time.Time{}
	if !g.deltaOn {
		return
	}
	if !loaded || delta < g.cfg.DeltaWarnC-g.cfg.ClearMarginC {
		if g.deltaBelow.IsZero() {
			g.deltaBelow = now
		}
		if now.Sub(g.deltaBelow) >= g.cfg.ClearHold {
			g.deltaOn, g.deltaBelow = false, time.Time{}
		}
		return
	}
	g.deltaBelow = time.Time{}
}

func (g *Guard) message(source string, temp, warn, crit float64) string {
	name := "hotspot"
	if source == SourceCore {
		name = "core (no hotspot sensor)"
	}
	// A level holds for ClearHold after the reading falls, so the message
	// must say the card is cooling, or "60°C, critical at 95°C" reads as a
	// contradiction.
	switch {
	case g.tempLevel == LevelCritical && temp < crit:
		return fmt.Sprintf("GPU %s %.0f°C, cooling after reaching the critical %.0f°C", name, temp, crit)
	case g.tempLevel == LevelCritical:
		return fmt.Sprintf("GPU %s %.0f°C, critical at %.0f°C", name, temp, crit)
	case temp < warn:
		return fmt.Sprintf("GPU %s %.0f°C, cooling after reaching the warning %.0f°C", name, temp, warn)
	}
	return fmt.Sprintf("GPU %s %.0f°C, warning at %.0f°C", name, temp, warn)
}

func (g *Guard) deltaMessage(delta float64) string {
	return fmt.Sprintf("GPU hotspot is %.0f°C above the core under load (warning at %.0f°C): poor thermal contact?", delta, g.cfg.DeltaWarnC)
}

func (g *Guard) state(now time.Time, s Sample, source string, temp, warn, crit float64, hot, core, delta, power Reading) State {
	st := State{
		Mode:       g.cfg.Mode,
		Source:     source,
		GPU:        s.GPU,
		Hotspot:    hot,
		Core:       core,
		Delta:      delta,
		Power:      power,
		Thresholds: Thresholds{WarnC: warn, CritC: crit, DeltaC: g.cfg.DeltaWarnC},
		Action:     ActionNone,
		Error:      s.Error,
		UpdatedAt:  now.UTC(),
	}
	level := g.tempLevel
	if g.deltaOn && levelRank(level) < levelRank(LevelWarn) {
		level = LevelWarn
	}
	if source == SourceNone && level == LevelOK {
		level = LevelUnknown
	}
	st.Level = level
	switch {
	case g.tempLevel == LevelCritical && source == SourceCore:
		st.Reasons = append(st.Reasons, ReasonCoreCritical)
	case g.tempLevel == LevelCritical:
		st.Reasons = append(st.Reasons, ReasonHotspotCritical)
	case g.tempLevel == LevelWarn && source == SourceCore:
		st.Reasons = append(st.Reasons, ReasonCoreWarn)
	case g.tempLevel == LevelWarn:
		st.Reasons = append(st.Reasons, ReasonHotspotWarn)
	}
	if g.deltaOn {
		st.Reasons = append(st.Reasons, ReasonDeltaWarn)
	}
	switch {
	case source == SourceNone && levelRank(g.tempLevel) == 0:
		st.Message = "no GPU temperature reported"
	case source == SourceNone && g.powerLimited && g.cfg.Mode == ModeAct:
		st.Message = fmt.Sprintf("GPU temperature lost during a %s episode; power limit kept", g.tempLevel)
	case source == SourceNone:
		st.Message = fmt.Sprintf("GPU temperature lost during a %s episode", g.tempLevel)
	case levelRank(g.tempLevel) > 0 && source != SourceNone:
		st.Message = g.message(source, temp, warn, crit)
	case g.deltaOn:
		st.Message = g.deltaMessage(delta.Value)
	}
	// Warn mode only logs what it would do, so it never reports an action.
	switch {
	case g.cfg.Mode != ModeAct:
	case g.appsStopped:
		st.Action = ActionAppsStopped
	case g.powerLimited:
		st.Action = ActionPowerLimited
	}
	if !g.criticalSince.IsZero() {
		t := g.criticalSince.UTC()
		st.CriticalSince = &t
	}
	if g.lastAction != "" {
		t := g.lastActionAt.UTC()
		st.LastAction, st.LastActionAt = g.lastAction, &t
	}
	return st
}
