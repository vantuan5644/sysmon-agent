//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type linuxController struct{}

// NewSystemController returns the Linux host controller. It drives standard
// desktop tooling (WirePlumber/PulseAudio, playerctl, systemd-logind) on a
// best-effort basis. When the agent runs as a system service outside the user's
// session bus these may degrade; each action reports the underlying error.
func NewSystemController() SystemController { return linuxController{} }

// linuxControlCommands lists candidate command pipelines per action, tried in
// order until one is found on PATH and exits zero.
func linuxControlCommands(action ControlAction) [][]string {
	switch action {
	case ControlMicMute:
		return [][]string{
			{"wpctl", "set-mute", "@DEFAULT_AUDIO_SOURCE@", "toggle"},
			{"pactl", "set-source-mute", "@DEFAULT_SOURCE@", "toggle"},
		}
	case ControlVolumeMute:
		return [][]string{
			{"wpctl", "set-mute", "@DEFAULT_AUDIO_SINK@", "toggle"},
			{"pactl", "set-sink-mute", "@DEFAULT_SINK@", "toggle"},
		}
	case ControlMediaToggle:
		return [][]string{
			{"playerctl", "play-pause"},
		}
	case ControlLockScreen:
		// Order matters here in a way it does not for the other actions.
		// `loginctl lock-session` only emits logind's Lock signal; it exits 0
		// whenever a session exists, whether or not anything is listening to
		// actually put a locker on screen. Since Apply stops at the first
		// command that exits 0, loginctl placed first would silently swallow
		// every candidate after it -- which is exactly what happened on the
		// omarchy host, where no hypridle/hyprlock listens for that signal and
		// the button reported applied:true with the screen still unlocked.
		// So session-specific lockers that lock directly go first, and the
		// signal-only generic path stays last (it is still the right answer on
		// KDE/GNOME, which do listen).
		//
		// Only non-blocking lockers belong on this list. hyprlock/swaylock run
		// in the foreground until the user unlocks, and Apply uses
		// exec.CommandContext -- the locker would be killed the moment the HTTP
		// request's context is cancelled. Adding one means detaching it first.
		return [][]string{
			{"omarchy-system-lock"},
			{"loginctl", "lock-session"},
			{"loginctl", "lock-sessions"},
		}
	default:
		return nil
	}
}

func linuxControlAvailable(action ControlAction) bool {
	for _, cmd := range linuxControlCommands(action) {
		if _, err := exec.LookPath(cmd[0]); err == nil {
			return true
		}
	}
	return false
}

func (linuxController) Capabilities() []ControlCapability {
	return capabilitiesFor(linuxControlAvailable)
}

// runLinuxControl runs one control command and returns its trimmed combined
// output. It is a variable so tests can stub the host tooling.
var runLinuxControl = func(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = linuxControlEnv()
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// readLinuxControl runs a read-only query with a 1 s budget and returns its
// trimmed stdout. Stderr stays out because the output is parsed, some of it
// as JSON. A variable so tests can stub it.
var readLinuxControl = func(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = linuxControlEnv()
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// linuxControlEnv pins the C locale: pactl translates the "Mute: yes" lines
// that parsePactlMute reads.
func linuxControlEnv() []string {
	return append(os.Environ(), "LC_ALL=C")
}

var lookLinuxControlPath = exec.LookPath

func (linuxController) Apply(ctx context.Context, action ControlAction) ControlResult {
	cmds := linuxControlCommands(action)
	if len(cmds) == 0 {
		return unavailableControlResult(action, "unknown control action")
	}
	switch action {
	case ControlMicMute:
		if result, ok := applyLinuxCaptureMute(ctx); ok {
			return result
		}
	case ControlLockScreen:
		cmds = withOwnGraphicalSessionLock(ctx, cmds)
	}
	attempted := false
	var lastErr string
	for _, args := range cmds {
		if _, err := lookLinuxControlPath(args[0]); err != nil {
			continue
		}
		attempted = true
		detail, err := runLinuxControl(ctx, args...)
		if err == nil {
			return ControlResult{
				Action:    action,
				Available: true,
				Applied:   true,
				State:     "toggled",
				Message:   detail,
			}
		}
		if detail == "" {
			detail = err.Error()
		}
		lastErr = fmt.Sprintf("%s: %s", args[0], detail)
	}
	if !attempted {
		return unavailableControlResult(action, "no supported control tool found on PATH (install wireplumber/pulseaudio, playerctl, or systemd-logind)")
	}
	return ControlResult{Action: action, Available: true, Applied: false, Error: lastErr}
}

// applyLinuxCaptureMute honours the mic_mute contract (every capture endpoint,
// not just the default) the same way the Windows bridge does: the target state
// is the inverse of the default source's mute, applied to every non-monitor
// source so they end up consistent. wpctl cannot enumerate sources in a
// script-friendly form, so this needs pactl (pipewire-pulse or PulseAudio).
// ok is false when pactl is missing or cannot read the default source, and the
// caller falls back to toggling the default source alone.
func applyLinuxCaptureMute(ctx context.Context) (ControlResult, bool) {
	if _, err := lookLinuxControlPath("pactl"); err != nil {
		return ControlResult{}, false
	}
	out, err := runLinuxControl(ctx, "pactl", "get-source-mute", "@DEFAULT_SOURCE@")
	if err != nil {
		return ControlResult{}, false
	}
	current, ok := parsePactlMute(out)
	if !ok {
		return ControlResult{}, false
	}
	listing, err := runLinuxControl(ctx, "pactl", "list", "short", "sources")
	if err != nil {
		return ControlResult{}, false
	}
	sources := parsePactlCaptureSources(listing)
	if len(sources) == 0 {
		sources = []string{"@DEFAULT_SOURCE@"}
	}
	target, state := "1", "muted"
	if current {
		target, state = "0", "unmuted"
	}
	var failures []string
	for _, name := range sources {
		if detail, err := runLinuxControl(ctx, "pactl", "set-source-mute", name, target); err != nil {
			if detail == "" {
				detail = err.Error()
			}
			failures = append(failures, name+": "+detail)
		}
	}
	if len(failures) == len(sources) {
		return ControlResult{Action: ControlMicMute, Available: true, Applied: false, Error: "pactl: " + strings.Join(failures, "; ")}, true
	}
	result := ControlResult{Action: ControlMicMute, Available: true, Applied: true, State: state}
	if len(failures) > 0 {
		result.Message = "some microphones did not change: " + strings.Join(failures, "; ")
	}
	return result, true
}

// parsePactlMute reads `pactl get-source-mute` output ("Mute: yes").
func parsePactlMute(out string) (muted bool, ok bool) {
	value, found := strings.CutPrefix(strings.TrimSpace(out), "Mute:")
	if !found {
		return false, false
	}
	switch strings.TrimSpace(value) {
	case "yes":
		return true, true
	case "no":
		return false, true
	}
	return false, false
}

// parsePactlCaptureSources returns the source names from `pactl list short
// sources`, skipping the ".monitor" loopbacks of playback sinks: muting those
// would not silence a microphone, and would break recording what is playing.
func parsePactlCaptureSources(out string) []string {
	var names []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || pulseMonitorSource(fields[1]) {
			continue
		}
		names = append(names, fields[1])
	}
	return names
}

// linuxMicrophoneMute reads what mic_mute changes. With pactl that is every
// non-monitor source, read in one `pactl --format=json list sources`
// (8 ms on minipc). Without it the toggle falls back to the default source,
// and so does this reading.
func linuxMicrophoneMute(ctx context.Context) AudioMuteState {
	if _, err := lookLinuxControlPath("pactl"); err == nil {
		if out, err := readLinuxControl(ctx, "pactl", "--format=json", "list", "sources"); err == nil {
			if muted, unmuted, err := parsePactlSourceMutes([]byte(out)); err == nil {
				return aggregateAudioMute(muted, unmuted, 0, "no microphones")
			}
		}
	}
	return linuxDefaultMute(ctx, true)
}

// linuxDefaultMute reads the default source or sink, trying the same tools in
// the same order as the toggle commands, and reports unknown with the reason
// when none answers.
func linuxDefaultMute(ctx context.Context, source bool) AudioMuteState {
	wpctlTarget, pactlArgs := "@DEFAULT_AUDIO_SINK@", []string{"get-sink-mute", "@DEFAULT_SINK@"}
	if source {
		wpctlTarget, pactlArgs = "@DEFAULT_AUDIO_SOURCE@", []string{"get-source-mute", "@DEFAULT_SOURCE@"}
	}
	candidates := []struct {
		args  []string
		parse func(string) (bool, bool)
	}{
		{[]string{"wpctl", "get-volume", wpctlTarget}, parseWpctlMute},
		{append([]string{"pactl"}, pactlArgs...), parsePactlMute},
	}
	reason := "no wpctl or pactl on PATH"
	for _, candidate := range candidates {
		if _, err := lookLinuxControlPath(candidate.args[0]); err != nil {
			continue
		}
		out, err := readLinuxControl(ctx, candidate.args...)
		if err != nil {
			reason = fmt.Sprintf("%s: %v", candidate.args[0], err)
			continue
		}
		if muted, ok := candidate.parse(out); ok {
			return observedAudioMute(muted)
		}
		reason = fmt.Sprintf("%s: unexpected output %q", candidate.args[0], out)
	}
	return unknownAudioMute(reason)
}

// withOwnGraphicalSessionLock inserts a `loginctl lock-session <id>...` for the
// agent user's own graphical sessions just before the final
// `loginctl lock-sessions`. A system-service agent has no session of its own,
// so bare `lock-session` fails ("Unknown object .../session/auto") and
// `lock-sessions` needs polkit authentication. Locking a session the caller
// owns by ID needs neither.
func withOwnGraphicalSessionLock(ctx context.Context, cmds [][]string) [][]string {
	ids := ownGraphicalSessionIDs(ctx)
	if len(ids) == 0 || len(cmds) == 0 {
		return cmds
	}
	lock := append([]string{"loginctl", "lock-session"}, ids...)
	out := append([][]string{}, cmds[:len(cmds)-1]...)
	out = append(out, lock)
	return append(out, cmds[len(cmds)-1])
}

func ownGraphicalSessionIDs(ctx context.Context) []string {
	if _, err := lookLinuxControlPath("loginctl"); err != nil {
		return nil
	}
	listing, err := runLinuxControl(ctx, "loginctl", "list-sessions", "--no-legend")
	if err != nil {
		return nil
	}
	uid := strconv.Itoa(os.Getuid())
	var ids []string
	for _, line := range strings.Split(listing, "\n") {
		// The first two columns (SESSION UID) are stable across systemd versions.
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[1] != uid {
			continue
		}
		props, err := runLinuxControl(ctx, "loginctl", "show-session", fields[0], "-p", "Type")
		if err != nil {
			continue
		}
		switch strings.TrimPrefix(strings.TrimSpace(props), "Type=") {
		case "x11", "wayland", "mir":
			ids = append(ids, fields[0])
		}
	}
	return ids
}
