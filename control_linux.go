//go:build linux

package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
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

func (linuxController) Apply(ctx context.Context, action ControlAction) ControlResult {
	cmds := linuxControlCommands(action)
	if len(cmds) == 0 {
		return unavailableControlResult(action, "unknown control action")
	}
	attempted := false
	var lastErr string
	for _, args := range cmds {
		if _, err := exec.LookPath(args[0]); err != nil {
			continue
		}
		attempted = true
		out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
		if err == nil {
			return ControlResult{
				Action:    action,
				Available: true,
				Applied:   true,
				State:     "toggled",
				Message:   strings.TrimSpace(string(out)),
			}
		}
		detail := strings.TrimSpace(string(out))
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
