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
		return [][]string{
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
