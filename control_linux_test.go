//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// stubLinuxControl replaces the command runner with responses keyed by the
// joined argv and records every call. Unknown commands fail.
func stubLinuxControl(t *testing.T, responses map[string]string, failures map[string]string) *[]string {
	t.Helper()
	var calls []string
	prevRun, prevLook := runLinuxControl, lookLinuxControlPath
	runLinuxControl = func(_ context.Context, args ...string) (string, error) {
		key := strings.Join(args, " ")
		calls = append(calls, key)
		if out, ok := failures[key]; ok {
			return out, errors.New("exit status 1")
		}
		if out, ok := responses[key]; ok {
			return out, nil
		}
		return "", errors.New("unexpected command: " + key)
	}
	lookLinuxControlPath = func(file string) (string, error) { return "/usr/bin/" + file, nil }
	t.Cleanup(func() { runLinuxControl, lookLinuxControlPath = prevRun, prevLook })
	return &calls
}

const pactlSources = "57\talsa_input.usb-mic.mono-fallback\tPipeWire\ts16le 1ch 8000Hz\tSUSPENDED\n" +
	"58\talsa_output.pci.analog-stereo.monitor\tPipeWire\ts32le 2ch 48000Hz\tSUSPENDED\n" +
	"59\talsa_input.pci.analog-stereo\tPipeWire\ts32le 2ch 48000Hz\tSUSPENDED"

func TestLinuxMicMuteMutesEveryCaptureSource(t *testing.T) {
	calls := stubLinuxControl(t, map[string]string{
		"pactl get-source-mute @DEFAULT_SOURCE@":                   "Mute: no",
		"pactl list short sources":                                 pactlSources,
		"pactl set-source-mute alsa_input.usb-mic.mono-fallback 1": "",
		"pactl set-source-mute alsa_input.pci.analog-stereo 1":     "",
	}, nil)
	got := linuxController{}.Apply(context.Background(), ControlMicMute)
	if !got.Applied || got.State != "muted" || got.Error != "" {
		t.Fatalf("result = %+v, want applied muted", got)
	}
	for _, call := range *calls {
		if strings.Contains(call, ".monitor") {
			t.Fatalf("monitor source touched: %q", call)
		}
	}
}

func TestLinuxMicMuteUnmutesEverySourceWhenDefaultMuted(t *testing.T) {
	stubLinuxControl(t, map[string]string{
		"pactl get-source-mute @DEFAULT_SOURCE@":                   "Mute: yes",
		"pactl list short sources":                                 pactlSources,
		"pactl set-source-mute alsa_input.usb-mic.mono-fallback 0": "",
		"pactl set-source-mute alsa_input.pci.analog-stereo 0":     "",
	}, nil)
	got := linuxController{}.Apply(context.Background(), ControlMicMute)
	if !got.Applied || got.State != "unmuted" {
		t.Fatalf("result = %+v, want applied unmuted", got)
	}
}

func TestLinuxMicMuteReportsPartialFailure(t *testing.T) {
	stubLinuxControl(t, map[string]string{
		"pactl get-source-mute @DEFAULT_SOURCE@":               "Mute: no",
		"pactl list short sources":                             pactlSources,
		"pactl set-source-mute alsa_input.pci.analog-stereo 1": "",
	}, map[string]string{
		"pactl set-source-mute alsa_input.usb-mic.mono-fallback 1": "No such entity",
	})
	got := linuxController{}.Apply(context.Background(), ControlMicMute)
	if !got.Applied || !strings.Contains(got.Message, "No such entity") {
		t.Fatalf("result = %+v, want applied with partial-failure message", got)
	}
}

func TestLinuxMicMuteFallsBackToDefaultWithoutPulse(t *testing.T) {
	calls := stubLinuxControl(t, map[string]string{
		"wpctl set-mute @DEFAULT_AUDIO_SOURCE@ toggle": "",
	}, map[string]string{
		"pactl get-source-mute @DEFAULT_SOURCE@": "Connection failure",
	})
	got := linuxController{}.Apply(context.Background(), ControlMicMute)
	if !got.Applied || got.State != "toggled" {
		t.Fatalf("result = %+v, want default-source toggle fallback", got)
	}
	if last := (*calls)[len(*calls)-1]; last != "wpctl set-mute @DEFAULT_AUDIO_SOURCE@ toggle" {
		t.Fatalf("last call = %q", last)
	}
}

func TestParsePactlMute(t *testing.T) {
	for in, want := range map[string]bool{"Mute: yes": true, "Mute: no\n": false} {
		got, ok := parsePactlMute(in)
		if !ok || got != want {
			t.Fatalf("parsePactlMute(%q) = %v,%v", in, got, ok)
		}
	}
	if _, ok := parsePactlMute("Connection failure"); ok {
		t.Fatal("garbage parsed as a mute state")
	}
}

// A system-service agent has no session of its own: bare lock-session fails and
// lock-sessions is polkit-gated, so the user's own graphical session must be
// locked by ID before falling through to lock-sessions.
func TestLinuxLockScreenLocksOwnGraphicalSessionByID(t *testing.T) {
	uid := strconv.Itoa(os.Getuid())
	other := strconv.Itoa(os.Getuid() + 1)
	stubLinuxControl(t, map[string]string{
		"loginctl list-sessions --no-legend": "  1 " + uid + " u - 1998 manager - no -\n" +
			"  2 " + uid + " u seat0 2052 user tty2 yes 2 days ago\n" +
			"  3 " + uid + " u - 12112 user - no -\n" +
			"  9 " + other + " v seat0 99 user tty3 no -",
		"loginctl show-session 1 -p Type": "Type=unspecified",
		"loginctl show-session 2 -p Type": "Type=wayland",
		"loginctl show-session 3 -p Type": "Type=tty",
		"loginctl lock-session 2":         "",
	}, map[string]string{
		"omarchy-system-lock":   "not found",
		"loginctl lock-session": "Failed to issue method call: Unknown object '/org/freedesktop/login1/session/auto'.",
	})
	got := linuxController{}.Apply(context.Background(), ControlLockScreen)
	if !got.Applied {
		t.Fatalf("result = %+v, want applied", got)
	}
}

func TestWithOwnGraphicalSessionLockKeepsOrderWithoutSessions(t *testing.T) {
	stubLinuxControl(t, map[string]string{"loginctl list-sessions --no-legend": ""}, nil)
	cmds := linuxControlCommands(ControlLockScreen)
	if got := withOwnGraphicalSessionLock(context.Background(), cmds); !reflect.DeepEqual(got, cmds) {
		t.Fatalf("commands changed with no graphical session: %v", got)
	}
}

// stubLinuxReads replaces the read-only query runner the same way
// stubLinuxControl replaces the command runner, and puts every tool on PATH.
func stubLinuxReads(t *testing.T, responses map[string]string) {
	t.Helper()
	prevRead, prevLook := readLinuxControl, lookLinuxControlPath
	readLinuxControl = func(_ context.Context, args ...string) (string, error) {
		if out, ok := responses[strings.Join(args, " ")]; ok {
			return out, nil
		}
		return "", errors.New("exit status 1")
	}
	lookLinuxControlPath = func(file string) (string, error) { return "/usr/bin/" + file, nil }
	t.Cleanup(func() { readLinuxControl, lookLinuxControlPath = prevRead, prevLook })
}

// The mute reading covers what the toggle changes: every non-monitor source.
func TestLinuxMicrophoneMuteCoversEveryCaptureSource(t *testing.T) {
	const list = "pactl --format=json list sources"
	for _, test := range []struct{ json, want string }{
		{`[{"name":"alsa_input.usb","mute":true},{"name":"alsa_output.pci.monitor","mute":false},{"name":"alsa_input.pci","mute":true}]`, "muted"},
		{`[{"name":"alsa_input.usb","mute":false},{"name":"alsa_output.pci.monitor","mute":true},{"name":"alsa_input.pci","mute":false}]`, "unmuted"},
		{`[{"name":"alsa_input.usb","mute":true},{"name":"alsa_input.pci","mute":false}]`, "mixed"},
	} {
		stubLinuxReads(t, map[string]string{list: test.json})
		if got := linuxMicrophoneMute(context.Background()); got.State != test.want {
			t.Fatalf("sources %s: state %+v, want %s", test.json, got, test.want)
		}
	}
}

// Without a pactl source list the toggle changes only the default source,
// and the reading follows it.
func TestLinuxMuteReadsFallBackToTheDefaultNodes(t *testing.T) {
	stubLinuxReads(t, map[string]string{
		"wpctl get-volume @DEFAULT_AUDIO_SOURCE@": "Volume: 1.00 [MUTED]",
		"pactl get-sink-mute @DEFAULT_SINK@":      "Mute: no",
	})
	if got := linuxMicrophoneMute(context.Background()); got.State != "muted" {
		t.Fatalf("microphone = %+v, want muted from wpctl", got)
	}
	if got := linuxDefaultMute(context.Background(), false); got.State != "unmuted" {
		t.Fatalf("speaker = %+v, want unmuted from the pactl fallback", got)
	}
	stubLinuxReads(t, nil)
	if got := linuxDefaultMute(context.Background(), false); got.State != "unknown" || !strings.Contains(got.Error, "pactl") {
		t.Fatalf("speaker with no answering tool = %+v, want unknown naming the last tool", got)
	}
}
