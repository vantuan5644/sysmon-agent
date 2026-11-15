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
	VRAMWarnC      float64 // memory junction; judged only when reported
	VRAMCritC      float64
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

// VRAM (GDDR6X memory junction) lines, chosen 2026-11-14 for the RTX 3090,
// whose back-side memory is the part that runs hottest under long compute
// loads. The card slows its memory by itself at about 110 C; warn 100 /
// critical 106 acts before that throttling starts. Only the Linux helper
// reads the junction so far, so on Windows these lines are idle.
const (
	DefaultVRAMWarnC = 100
	DefaultVRAMCritC = 106

	MinVRAMWarnC, MaxVRAMWarnC = 80, 110
	MinVRAMCritC, MaxVRAMCritC = 90, 115
)

func DefaultConfig() Config {
	return Config{
		Mode:           ModeWarn,
		HotspotWarnC:   DefaultHotspotWarnC,
		HotspotCritC:   DefaultHotspotCritC,
		CoreWarnC:      DefaultCoreWarnC,
		CoreCritC:      DefaultCoreCritC,
		DeltaWarnC:     DefaultDeltaWarnC,
		VRAMWarnC:      DefaultVRAMWarnC,
		VRAMCritC:      DefaultVRAMCritC,
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
	if c.VRAMWarnC < MinVRAMWarnC || c.VRAMWarnC > MaxVRAMWarnC {
		return fmt.Errorf("gpu vram warn %g C: want %d-%d", c.VRAMWarnC, MinVRAMWarnC, MaxVRAMWarnC)
	}
	if c.VRAMCritC < MinVRAMCritC || c.VRAMCritC > MaxVRAMCritC {
		return fmt.Errorf("gpu vram critical %g C: want %d-%d", c.VRAMCritC, MinVRAMCritC, MaxVRAMCritC)
	}
	if c.VRAMCritC <= c.VRAMWarnC {
		return fmt.Errorf("gpu vram critical %g C must be above warn %g C", c.VRAMCritC, c.VRAMWarnC)
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
	VRAM    *float64 // memory junction
	Power   *float64
	Error   string
	// VRAMError is a junction register that a helper tried and failed to
	// read. A helper with no junction support sends none, and that is no fault.
	VRAMError string
}

// Reading mirrors the agent's NumberMetric JSON shape.
type Reading struct {
	Available bool    `json:"available"`
	Value     float64 `json:"value"`
	Unit      string  `json:"unit,omitempty"`
}

type Thresholds struct {
	WarnC     float64 `json:"warn_c"`
	CritC     float64 `json:"critical_c"`
	DeltaC    float64 `json:"delta_warn_c"`
	VRAMWarnC float64 `json:"vram_warn_c"`
	VRAMCritC float64 `json:"vram_critical_c"`
}

const (
	ReasonHotspotWarn     = "hotspot_warn"
	ReasonHotspotCritical = "hotspot_critical"
	ReasonCoreWarn        = "core_warn"
	ReasonCoreCritical    = "core_critical"
	ReasonDeltaWarn       = "delta_warn"
	ReasonVRAMWarn        = "vram_warn"
	ReasonVRAMCritical    = "vram_critical"

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
	VRAM       Reading    `json:"vram_celsius"`
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
	VRAMError     string     `json:"vram_error,omitempty"`
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

	tempLevel  Level // ok|warn|critical, from the die temperature alone
	source     string
	overWarn   int
	overCrit   int
	belowSince time.Time // reading continuously below the current level's clear point

	// The memory junction is a second sensor with its own lines and the same
	// hold and hysteresis. An episode runs while either one is critical.
	vramLevel        Level
	vramOverWarn     int
	vramOverCrit     int
	vramBelowSince   time.Time
	vramWarnNotified bool
	vramSeen         bool      // this card has reported a junction reading
	vramLostSince    time.Time // the junction reading has been missing since

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
	return &Guard{cfg: cfg, tempLevel: LevelOK, vramLevel: LevelOK, source: SourceNone}
}

// level is the worse of the die's and the memory junction's levels.
func (g *Guard) level() Level {
	if levelRank(g.vramLevel) > levelRank(g.tempLevel) {
		return g.vramLevel
	}
	return g.tempLevel
}

// vramLeads says the memory junction, not the die, sets the level, so the
// messages name it.
func (g *Guard) vramLeads() bool { return levelRank(g.vramLevel) > levelRank(g.tempLevel) }

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
	vram := reading(s.VRAM, "C")
	if vram.Available {
		g.vramSeen, g.vramLostSince = true, time.Time{}
		g.stepVRAM(now, *s.VRAM)
	} else {
		// Losing the junction reading is no evidence that the memory cooled
		// down. Its level holds, so a reading that drops out for a sample
		// neither ends a warning nor warns again when it comes back. Only a
		// warning lost for longer than the clear hold ends, or it could
		// never end; a critical level holds until a reading clears it.
		g.vramOverWarn, g.vramOverCrit, g.vramBelowSince = 0, 0, time.Time{}
		if g.vramLostSince.IsZero() {
			g.vramLostSince = now
		}
		if g.vramLevel == LevelWarn && now.Sub(g.vramLostSince) >= g.cfg.ClearHold {
			g.vramLevel = LevelOK
		}
	}
	// Any reading at all, die or memory, can end or start an episode.
	measured := source != SourceNone || vram.Available
	vramTemp := 0.0
	if vram.Available {
		vramTemp = *s.VRAM
	}
	lead := func() string {
		if g.vramLeads() {
			return g.vramMessage(vramTemp, vram.Available)
		}
		return g.message(source, temp, warn, crit)
	}
	// overCritNow: a sensor that is itself critical reads over its critical
	// line in this very sample. One unheld sample from the other sensor
	// does not count.
	overCritNow := (source != SourceNone && g.tempLevel == LevelCritical && g.overCrit > 0) ||
		(vram.Available && g.vramLevel == LevelCritical && g.vramOverCrit > 0)
	// unreadCritical: a critical sensor that has no reading to say it cooled.
	unreadCritical := (g.tempLevel == LevelCritical && source == SourceNone) ||
		(g.vramLevel == LevelCritical && !vram.Available)
	delta := Reading{Unit: "C"}
	power := reading(s.Power, "W")
	if hot.Available && core.Available {
		delta = Reading{Available: true, Value: math.Round((*s.Hotspot-*s.Core)*10) / 10, Unit: "C"}
		g.stepDelta(now, *s.Hotspot-*s.Core, power)
	} else {
		g.deltaSince, g.deltaBelow = time.Time{}, time.Time{}
	}

	// Commands follow the level just computed.
	if g.level() == LevelCritical {
		if g.criticalSince.IsZero() {
			g.criticalSince = now
			g.lastAlarm = now
			cmds = append(cmds, Command{Kind: CmdNotify, Level: LevelCritical, Message: lead()})
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
			if overCritNow && !now.Before(g.nextKill) {
				g.appsStopped = true
				g.nextKill = now.Add(g.cfg.KillEvery)
				cmds = append(cmds, Command{Kind: CmdStopApps, Level: LevelCritical})
			}
			// The host alarm repeats while the reading is still over the line,
			// or while there is none to say otherwise. A reading that shows the
			// card cooling through the clear hold gets no fresh system-modal box.
			stillHot := overCritNow || unreadCritical
			if stillHot && g.cfg.AlarmRepeat > 0 && now.Sub(g.lastAlarm) >= g.cfg.AlarmRepeat {
				g.lastAlarm = now
				cmds = append(cmds, Command{Kind: CmdNotify, Level: LevelCritical, Message: lead()})
			}
		}
	} else if measured {
		g.criticalSince = time.Time{}
	}

	// Each sensor warns once per episode of its own; during a critical
	// episode the critical alarm speaks for both.
	if g.tempLevel == LevelWarn && !g.warnNotified && g.vramLevel != LevelCritical {
		g.warnNotified = true
		cmds = append(cmds, Command{Kind: CmdNotify, Level: LevelWarn, Message: g.message(source, temp, warn, crit)})
	}
	if g.tempLevel == LevelOK {
		g.warnNotified = false
	}
	if g.vramLevel == LevelWarn && !g.vramWarnNotified && g.tempLevel != LevelCritical {
		g.vramWarnNotified = true
		cmds = append(cmds, Command{Kind: CmdNotify, Level: LevelWarn, Message: g.vramMessage(vramTemp, vram.Available)})
	}
	if g.vramLevel == LevelOK {
		g.vramWarnNotified = false
	}
	if g.deltaOn && !g.deltaNotify {
		g.deltaNotify = true
		cmds = append(cmds, Command{Kind: CmdNotify, Level: LevelWarn, Message: g.deltaMessage(delta.Value)})
	}
	if !g.deltaOn {
		g.deltaNotify = false
	}

	// Restore the power limit once the card has been cool for a while: the
	// die, and the memory junction on a card that reports one. A junction
	// seen before and missing now is not cool; it keeps the limit lowered.
	if g.powerLimited && source != SourceNone {
		vramCool := !g.vramSeen || (vram.Available && vramTemp < g.cfg.VRAMWarnC-g.cfg.RestoreMarginC)
		if temp < warn-g.cfg.RestoreMarginC && vramCool {
			if g.coolSince.IsZero() {
				g.coolSince = now
			}
			if g.level() != LevelCritical && now.Sub(g.coolSince) >= g.cfg.RestoreAfter {
				g.powerLimited, g.appsStopped, g.coolSince = false, false, time.Time{}
				cmds = append(cmds, Command{Kind: CmdRestorePower, Level: g.level()})
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
	return g.state(now, s, source, temp, warn, crit, hot, core, delta, vram, power), cmds
}

func (g *Guard) stepTemperature(now time.Time, temp, warn, crit float64) {
	if g.stepSensor(now, temp, warn, crit, &g.tempLevel, &g.overWarn, &g.overCrit, &g.belowSince) {
		g.warnNotified = true // the critical alarm already said it
	}
}

func (g *Guard) stepVRAM(now time.Time, temp float64) {
	if g.stepSensor(now, temp, g.cfg.VRAMWarnC, g.cfg.VRAMCritC, &g.vramLevel, &g.vramOverWarn, &g.vramOverCrit, &g.vramBelowSince) {
		g.vramWarnNotified = true
	}
}

// stepSensor moves one sensor's level: up after HoldSamples readings over a
// line, down after ClearHold below it by ClearMarginC. It reports a fall
// from critical to warn, which needs no fresh warning.
func (g *Guard) stepSensor(now time.Time, temp, warn, crit float64, level *Level, overWarn, overCrit *int, belowSince *time.Time) bool {
	if temp >= crit {
		*overCrit++
	} else {
		*overCrit = 0
	}
	if temp >= warn {
		*overWarn++
	} else {
		*overWarn = 0
	}
	hold := g.cfg.HoldSamples
	switch {
	case *overCrit >= hold && *level != LevelCritical:
		*level, *belowSince = LevelCritical, time.Time{}
		return false
	case *overWarn >= hold && *level == LevelOK:
		*level, *belowSince = LevelWarn, time.Time{}
		return false
	}
	clearAt := 0.0
	switch *level {
	case LevelCritical:
		clearAt = crit - g.cfg.ClearMarginC
	case LevelWarn:
		clearAt = warn - g.cfg.ClearMarginC
	default:
		return false
	}
	if temp >= clearAt {
		*belowSince = time.Time{}
		return false
	}
	if belowSince.IsZero() {
		*belowSince = now
	}
	if now.Sub(*belowSince) < g.cfg.ClearHold {
		return false
	}
	*belowSince = time.Time{}
	if *level == LevelCritical && temp >= warn-g.cfg.ClearMarginC {
		*level = LevelWarn
		return true
	}
	*level = LevelOK
	return false
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

// vramMessage follows message's wording for the memory junction.
func (g *Guard) vramMessage(temp float64, available bool) string {
	warn, crit := g.cfg.VRAMWarnC, g.cfg.VRAMCritC
	switch {
	case !available:
		return fmt.Sprintf("GPU memory junction reading lost during a %s episode", g.vramLevel)
	case g.vramLevel == LevelCritical && temp < crit:
		return fmt.Sprintf("GPU memory (VRAM) %.0f°C, cooling after reaching the critical %.0f°C", temp, crit)
	case g.vramLevel == LevelCritical:
		return fmt.Sprintf("GPU memory (VRAM) %.0f°C, critical at %.0f°C", temp, crit)
	case temp < warn:
		return fmt.Sprintf("GPU memory (VRAM) %.0f°C, cooling after reaching the warning %.0f°C", temp, warn)
	}
	return fmt.Sprintf("GPU memory (VRAM) %.0f°C, warning at %.0f°C", temp, warn)
}

func (g *Guard) state(now time.Time, s Sample, source string, temp, warn, crit float64, hot, core, delta, vram, power Reading) State {
	st := State{
		Mode:    g.cfg.Mode,
		Source:  source,
		GPU:     s.GPU,
		Hotspot: hot,
		Core:    core,
		Delta:   delta,
		VRAM:    vram,
		Power:   power,
		Thresholds: Thresholds{
			WarnC: warn, CritC: crit, DeltaC: g.cfg.DeltaWarnC,
			VRAMWarnC: g.cfg.VRAMWarnC, VRAMCritC: g.cfg.VRAMCritC,
		},
		Action:    ActionNone,
		Error:     s.Error,
		VRAMError: s.VRAMError,
		UpdatedAt: now.UTC(),
	}
	level := g.level()
	if g.deltaOn && levelRank(level) < levelRank(LevelWarn) {
		level = LevelWarn
	}
	if source == SourceNone && !vram.Available && level == LevelOK {
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
	switch g.vramLevel {
	case LevelCritical:
		st.Reasons = append(st.Reasons, ReasonVRAMCritical)
	case LevelWarn:
		st.Reasons = append(st.Reasons, ReasonVRAMWarn)
	}
	if g.deltaOn {
		st.Reasons = append(st.Reasons, ReasonDeltaWarn)
	}
	switch {
	case g.vramLeads():
		st.Message = g.vramMessage(vram.Value, vram.Available)
	case source == SourceNone && levelRank(g.tempLevel) == 0 && vram.Available:
		// The memory junction alone is fine and says so by its silence.
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
