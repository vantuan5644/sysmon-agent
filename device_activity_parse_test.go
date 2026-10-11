package main

import (
	"fmt"
	"testing"
)

func TestPulseActivityExcludesMonitorSources(t *testing.T) {
	sources := []byte(`[{"index":1,"monitor_of_sink":null},{"index":2,"monitor_of_sink":0}]`)
	for _, test := range []struct {
		data   string
		active bool
	}{
		{`[]`, false},
		{`[{"source":1,"corked":false}]`, true},
		{`[{"source":1,"corked":true}]`, false},
		{`[{"source":2,"corked":false}]`, false},
	} {
		active, err := parsePulseActivity([]byte(test.data), sources)
		if err != nil || active != test.active {
			t.Fatalf("output=%s active=%v err=%v", test.data, active, err)
		}
	}
	if _, err := parsePulseActivity([]byte(`[{"source":99}]`), sources); err == nil {
		t.Fatal("unknown recording source treated as idle")
	}
}

func TestPipewireCaptureActivityExcludesPlaybackAndMonitor(t *testing.T) {
	for _, test := range []struct {
		name, data  string
		mic, camera bool
	}{
		{"idle", `[]`, false, false},
		{"playback", `[{"type":"PipeWire:Interface:Node","info":{"state":"running","props":{"media.class":"Stream/Output/Audio"}}}]`, false, false},
		{"monitor", `[{"type":"PipeWire:Interface:Node","info":{"state":"running","props":{"media.class":"Stream/Input/Audio","stream.capture.sink":true}}}]`, false, false},
		{"sink monitor link", `[{"id":10,"type":"PipeWire:Interface:Node","info":{"state":"running","props":{"media.class":"Stream/Input/Audio"}}},{"id":20,"type":"PipeWire:Interface:Node","info":{"state":"running","props":{"media.class":"Audio/Sink"}}},{"type":"PipeWire:Interface:Link","info":{"input-node-id":10,"output-node-id":20}}]`, false, false},
		{"microphone", `[{"type":"PipeWire:Interface:Node","info":{"state":"running","props":{"media.class":"Stream/Input/Audio"}}}]`, true, false},
		{"camera", `[{"type":"PipeWire:Interface:Node","info":{"state":"running","props":{"media.class":"Stream/Input/Video"}}}]`, false, true},
		{"paused camera", `[{"type":"PipeWire:Interface:Node","info":{"state":"idle","props":{"media.class":"Stream/Input/Video"}}}]`, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, c, err := parsePipewireActivity([]byte(test.data))
			if err != nil || m != test.mic || c != test.camera {
				t.Fatalf("mic=%v camera=%v err=%v", m, c, err)
			}
		})
	}
	if _, _, err := parsePipewireActivity([]byte("invalid")); err == nil {
		t.Fatal("malformed observation treated as idle")
	}
}

func TestParseWpctlMute(t *testing.T) {
	for _, test := range []struct {
		out       string
		muted, ok bool
	}{
		{"Volume: 0.40\n", false, true},
		{"Volume: 1.00 [MUTED]\n", true, true},
		{"Translate ID error: '@DEFAULT_AUDIO_SOURCE@' is not a valid ID\n", false, false},
		{"", false, false},
	} {
		muted, ok := parseWpctlMute(test.out)
		if ok != test.ok || muted != test.muted {
			t.Fatalf("parseWpctlMute(%q) = %v,%v, want %v,%v", test.out, muted, ok, test.muted, test.ok)
		}
	}
}

// Shape measured on minipc (PipeWire, Ubuntu 26.04): the monitor carries
// monitor_of_sink null and is known only by its name.
func TestParsePactlSourceMutesSkipsMonitors(t *testing.T) {
	sources := func(usb, monitor, pci bool) []byte {
		return []byte(fmt.Sprintf(`[
			{"name":"alsa_input.usb-USB_Composite_Device-01.mono-fallback","mute":%v,"monitor_of_sink":null,"properties":{"device.class":"sound"}},
			{"name":"alsa_output.pci-0000_e5_00.6.analog-stereo.monitor","mute":%v,"monitor_of_sink":null,"properties":{"device.class":"monitor"}},
			{"name":"alsa_input.pci-0000_e5_00.6.analog-stereo","mute":%v,"monitor_of_sink":null,"properties":{"device.class":"sound"}}
		]`, usb, monitor, pci))
	}
	for _, test := range []struct {
		usb, monitor, pci      bool
		wantMuted, wantUnmuted int
	}{
		{true, false, true, 2, 0},
		{false, true, false, 0, 2},
		{true, false, false, 1, 1},
	} {
		muted, unmuted, err := parsePactlSourceMutes(sources(test.usb, test.monitor, test.pci))
		if err != nil || muted != test.wantMuted || unmuted != test.wantUnmuted {
			t.Fatalf("%+v: muted=%d unmuted=%d err=%v", test, muted, unmuted, err)
		}
	}
	if _, _, err := parsePactlSourceMutes([]byte(`[{"name":"alsa_input.x"}]`)); err == nil {
		t.Fatal("a source without a mute field was counted")
	}
	if _, _, err := parsePactlSourceMutes([]byte(`not json`)); err == nil {
		t.Fatal("unparseable pactl output was accepted")
	}
}

func TestAggregateAudioMute(t *testing.T) {
	for _, test := range []struct {
		muted, unmuted, failed int
		want                   string
	}{
		{2, 0, 0, "muted"},
		{0, 3, 0, "unmuted"},
		{1, 1, 0, "mixed"},
		{0, 0, 0, "unknown"},
		{2, 0, 1, "unknown"},
	} {
		got := aggregateAudioMute(test.muted, test.unmuted, test.failed, "no active microphones")
		if got.State != test.want || got.ObservedAt.IsZero() {
			t.Fatalf("aggregate(%d muted, %d unmuted, %d failed) = %+v, want %s", test.muted, test.unmuted, test.failed, got, test.want)
		}
		if test.want == "unknown" && got.Error == "" {
			t.Fatalf("unknown aggregate carries no reason: %+v", got)
		}
	}
}
