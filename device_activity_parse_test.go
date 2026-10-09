package main

import "testing"

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
