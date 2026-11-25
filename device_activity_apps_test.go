package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeFixture(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMergeDeviceAppsDedupesSortsCapsAndHidesTheSoundServer(t *testing.T) {
	got := mergeDeviceApps(
		[]DeviceApp{{PID: 900, Name: "zoom"}, {PID: 12, Name: "pipewire"}},
		[]DeviceApp{{PID: 300, Name: "Zoom"}, {PID: 0, Name: "firefox"}, {Name: "  "}},
		[]DeviceApp{{PID: 0, Name: "Discord"}},
	)
	want := []DeviceApp{{Name: "Discord"}, {Name: "firefox"}, {PID: 300, Name: "zoom"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge = %+v, want %+v", got, want)
	}

	// The server is the only holder known: say so instead of guessing.
	got = mergeDeviceApps([]DeviceApp{{PID: 12, Name: "pipewire"}, {PID: 13, Name: "pipewire"}})
	if len(got) != 1 || got[0].Name != "pipewire" || got[0].PID != 12 || !got[0].SoundServer {
		t.Fatalf("server-only holder = %+v", got)
	}

	var many []DeviceApp
	for _, name := range []string{"g", "f", "e", "d", "c", "b", "a"} {
		many = append(many, DeviceApp{Name: name})
	}
	got = mergeDeviceApps(many)
	if len(got) != deviceAppLimit || got[0].Name != "a" || got[deviceAppLimit-1].Name != "e" {
		t.Fatalf("cap = %+v", got)
	}
	if mergeDeviceApps(nil, []DeviceApp{{Name: ""}}) != nil {
		t.Fatal("no names must give no list")
	}
}

func TestWithDeviceAppsOnlyNamesActiveUse(t *testing.T) {
	apps := []DeviceApp{{PID: 1, Name: "obs"}}
	if got := withDeviceApps(observedDeviceActivity(true, "x"), apps); len(got.Apps) != 1 {
		t.Fatalf("active reading lost its holder: %+v", got)
	}
	for _, reading := range []DeviceActivity{observedDeviceActivity(false, "x"), unknownDeviceActivity("x")} {
		if got := withDeviceApps(reading, apps); got.Apps != nil {
			t.Fatalf("%s reading named a holder: %+v", reading.State, got)
		}
	}
	data, _ := json.Marshal(observedDeviceActivity(false, "x"))
	if strings.Contains(string(data), "apps") {
		t.Fatalf("idle reading serialized apps: %s", data)
	}
}

func TestReadALSACaptureNamesRunningOwners(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "asound/card0/pcm0c/sub0/status"),
		"state: RUNNING\nowner_pid   : 4242\ntrigger_time: 5.1\ntstamp      : 5.2\n")
	writeFixture(t, filepath.Join(root, "asound/card1/pcm0c/sub0/status"), "closed\n")
	writeFixture(t, filepath.Join(root, "asound/card1/pcm0p/sub0/status"), "state: RUNNING\nowner_pid   : 7\n") // playback
	got := readALSACapture(root)
	if !got.found || !got.complete || !got.running || !reflect.DeepEqual(got.owners, []int{4242}) {
		t.Fatalf("alsa = %+v", got)
	}

	idle := t.TempDir()
	writeFixture(t, filepath.Join(idle, "asound/card0/pcm0c/sub0/status"), "closed\n")
	if got := readALSACapture(idle); !got.found || !got.complete || got.running || got.owners != nil {
		t.Fatalf("idle alsa = %+v", got)
	}
	if got := readALSACapture(t.TempDir()); got.found || got.complete {
		t.Fatalf("missing ALSA must not read as complete: %+v", got)
	}
}

func TestProcAppNamePrefersFullExecutableOverTruncatedComm(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "10/comm"), "obs\n")
	writeFixture(t, filepath.Join(root, "11/comm"), "chromium-browse\n") // 15 chars: truncated by the kernel
	if err := os.Symlink("/usr/lib/chromium/chromium-browser", filepath.Join(root, "11/exe")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	writeFixture(t, filepath.Join(root, "12/comm"), "")
	if err := os.Symlink("/opt/zoom/zoom (deleted)", filepath.Join(root, "12/exe")); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(root, "13/comm"), "Isolated Web Co\n") // exe unreadable (another user)
	writeFixture(t, filepath.Join(root, "14/comm"), "Isolated Web Co\n")
	if err := os.Symlink("/usr/lib/firefox/firefox", filepath.Join(root, "14/exe")); err != nil {
		t.Fatal(err)
	}
	for pid, want := range map[int]string{10: "obs", 11: "chromium-browser", 12: "zoom", 13: "Isolated Web Co", 14: "Isolated Web Co", 99: ""} {
		if got := procAppName(root, pid); got != want {
			t.Errorf("pid %d: name %q, want %q", pid, got, want)
		}
	}
}

func TestStreamClientAppPrefersTheProcessBinary(t *testing.T) {
	for _, test := range []struct {
		props map[string]any
		want  DeviceApp
		ok    bool
	}{
		{map[string]any{"application.name": "WEBRTC VoiceEngine", "application.process.binary": "Discord", "application.process.id": "4100"}, DeviceApp{PID: 4100, Name: "Discord"}, true},
		{map[string]any{"application.name": "OBS Studio", "application.process.id": float64(77)}, DeviceApp{PID: 77, Name: "OBS Studio"}, true},
		{map[string]any{"media.class": "Stream/Input/Audio"}, DeviceApp{}, false},
	} {
		got, ok := streamClientApp(test.props)
		if ok != test.ok || got != test.want {
			t.Errorf("props %v: %+v %v, want %+v %v", test.props, got, ok, test.want, test.ok)
		}
	}
}

func TestPipewireCaptureNamesClientsOfCountedStreamsOnly(t *testing.T) {
	data := `[
	  {"id":10,"type":"PipeWire:Interface:Node","info":{"state":"running","props":{"media.class":"Stream/Input/Audio","application.process.binary":"zoom","application.process.id":"501"}}},
	  {"id":11,"type":"PipeWire:Interface:Node","info":{"state":"running","props":{"media.class":"Stream/Input/Audio","stream.capture.sink":true,"application.process.binary":"obs"}}},
	  {"id":12,"type":"PipeWire:Interface:Node","info":{"state":"idle","props":{"media.class":"Stream/Input/Audio","application.process.binary":"idle-recorder"}}},
	  {"id":13,"type":"PipeWire:Interface:Node","info":{"state":"running","props":{"media.class":"Stream/Input/Video","application.name":"Firefox","application.process.id":"700"}}}
	]`
	got, err := parsePipewireCapture([]byte(data))
	if err != nil || !got.mic || !got.camera {
		t.Fatalf("capture = %+v err=%v", got, err)
	}
	if !reflect.DeepEqual(got.micApps, []DeviceApp{{PID: 501, Name: "zoom"}}) {
		t.Fatalf("mic apps = %+v", got.micApps)
	}
	if !reflect.DeepEqual(got.cameraApps, []DeviceApp{{PID: 700, Name: "Firefox"}}) {
		t.Fatalf("camera apps = %+v", got.cameraApps)
	}
}

func TestPulseCaptureNamesRecordingClients(t *testing.T) {
	sources := []byte(`[{"index":1,"monitor_of_sink":null},{"index":2,"monitor_of_sink":0}]`)
	outputs := []byte(`[
	  {"source":2,"corked":false,"properties":{"application.process.binary":"pavucontrol"}},
	  {"source":1,"corked":true,"properties":{"application.process.binary":"paused"}},
	  {"source":1,"corked":false,"properties":{"application.name":"WEBRTC VoiceEngine","application.process.binary":"chrome","application.process.id":"321"}},
	  {"source":99,"corked":false}
	]`)
	active, apps, err := parsePulseCapture(outputs, sources)
	if err != nil || !active || !reflect.DeepEqual(apps, []DeviceApp{{PID: 321, Name: "chrome"}}) {
		t.Fatalf("active=%v apps=%+v err=%v", active, apps, err)
	}
}

func TestPrivacyRecordAndImageNamesCarryNoPath(t *testing.T) {
	for _, test := range []struct {
		key  []string
		want string
		ok   bool
	}{
		{[]string{"NonPackaged", `C:#Users#someone#AppData#Roaming#Zoom#bin#Zoom.exe`}, "Zoom", true},
		{[]string{"NonPackaged", `C:#Program Files#OBS#obs64.EXE`}, "obs64", true},
		{[]string{"Microsoft.WindowsCamera_8wekyb3d8bbwe"}, "WindowsCamera", true},
		{[]string{"5319275A.WhatsAppDesktop_cv1g1gvanyjgm"}, "WhatsAppDesktop", true},
		{[]string{"NonPackaged"}, "", false},
		{[]string{"a", "b", "c"}, "", false},
	} {
		got, ok := privacyRecordApp(test.key)
		if ok != test.ok || got.Name != test.want {
			t.Errorf("key %v: %+v %v, want %q %v", test.key, got, ok, test.want, test.ok)
		}
	}
	for path, want := range map[string]string{
		`C:\Program Files\Discord\app-1.0\Discord.exe`: "Discord",
		`C:\Windows\System32\svchost.EXE`:              "svchost",
		`firefox.exe`:                                  "firefox",
	} {
		if got := windowsImageAppName(path); got != want {
			t.Errorf("%s: %q, want %q", path, got, want)
		}
	}
}
