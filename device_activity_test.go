package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeDeviceBackend struct {
	devices       []cameraDevice
	failID        string
	calls         int
	activityCalls int
	unsupported   int
}

func (f *fakeDeviceBackend) Activity(context.Context) (DeviceActivity, DeviceActivity) {
	f.activityCalls++
	return observedDeviceActivity(true, "test capture"), observedDeviceActivity(false, "test camera")
}
func (f *fakeDeviceBackend) Cameras(context.Context) ([]cameraDevice, int, error) {
	return append([]cameraDevice(nil), f.devices...), f.unsupported, nil
}
func (f *fakeDeviceBackend) SetCamera(_ context.Context, device cameraDevice, enabled bool) error {
	f.calls++
	if device.ID == f.failID {
		return errors.New("device refused change")
	}
	for i := range f.devices {
		if f.devices[i].ID == device.ID {
			f.devices[i].Enabled = enabled
			return nil
		}
	}
	return errors.New("device missing")
}
func testCamera(id string, enabled bool) cameraDevice {
	return cameraDevice{ID: id, Identity: id + "-identity", Name: id, Enabled: enabled}
}

func TestCameraRestorePersistsAndPreservesOriginallyDisabled(t *testing.T) {
	ctx := context.Background()
	backend := &fakeDeviceBackend{devices: []cameraDevice{testCamera("on", true), testCamera("off", false)}}
	path := filepath.Join(t.TempDir(), "settings.json")
	d := newDeviceManager(path, backend)
	result := d.Apply(ctx)
	if !result.Applied || backend.devices[0].Enabled || backend.devices[1].Enabled {
		t.Fatalf("disable: %+v devices=%+v", result, backend.devices)
	}
	if !result.CameraControl.RestorePending {
		t.Fatal("missing restore indication")
	}
	// A new process reconstructs recovery ownership from the journal.
	d = newDeviceManager(path, backend)
	result = d.Apply(ctx)
	if !result.Applied || !backend.devices[0].Enabled || backend.devices[1].Enabled {
		t.Fatalf("restore: %+v devices=%+v", result, backend.devices)
	}
	if result.CameraControl.RestorePending {
		t.Fatal("completed restore retained records")
	}
}

func TestCameraPartialDisableRestoresSuccessfulDevices(t *testing.T) {
	backend := &fakeDeviceBackend{devices: []cameraDevice{testCamera("a", true), testCamera("b", true)}, failID: "b", unsupported: 1}
	d := newDeviceManager(filepath.Join(t.TempDir(), "settings.json"), backend)
	r := d.Apply(context.Background())
	if r.Applied || r.State != "mixed" || r.Error == "" {
		t.Fatalf("partial disable: %+v", r)
	}
	if r.CameraControl.Unsupported != 1 || strings.Contains(r.Message+r.Error, "unsupported") {
		t.Fatalf("unsupported devices must be counted, not reported as errors: %+v", r)
	}
	backend.failID = ""
	r = d.Apply(context.Background())
	if !r.Applied || !backend.devices[0].Enabled || !backend.devices[1].Enabled {
		t.Fatalf("restore: %+v", r)
	}
}

func TestCameraUnwritableJournalPreventsMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(path, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	backend := &fakeDeviceBackend{devices: []cameraDevice{testCamera("a", true)}}
	d := newDeviceManager(filepath.Join(path, "settings.json"), backend)
	r := d.Apply(context.Background())
	if r.Applied || backend.calls != 0 || !backend.devices[0].Enabled {
		t.Fatalf("mutation without journal: %+v", r)
	}
}

func TestCameraDisconnectedOrChangedIdentityRetainsRecovery(t *testing.T) {
	backend := &fakeDeviceBackend{devices: []cameraDevice{testCamera("a", true)}}
	d := newDeviceManager(filepath.Join(t.TempDir(), "settings.json"), backend)
	if r := d.Apply(context.Background()); !r.Applied {
		t.Fatal(r)
	}
	backend.devices = nil
	if r := d.Apply(context.Background()); r.Applied || len(d.journal) != 1 {
		t.Fatalf("disconnected recovery: %+v", r)
	}
	backend.devices = []cameraDevice{testCamera("a", false)}
	backend.devices[0].Identity = "replacement"
	if r := d.Apply(context.Background()); r.Applied || backend.devices[0].Enabled {
		t.Fatalf("replacement modified: %+v", r)
	}
	backend.devices[0].Identity = "a-identity"
	if r := d.Apply(context.Background()); !r.Applied || !backend.devices[0].Enabled {
		t.Fatalf("reconnected restore: %+v", r)
	}
}

func TestCameraRequiresPersistentSettingsAndValidRecovery(t *testing.T) {
	backend := &fakeDeviceBackend{devices: []cameraDevice{testCamera("a", true)}}
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path+".cameras.json", []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, configured := range []string{"", path} {
		d := newDeviceManager(configured, backend)
		if r := d.Apply(context.Background()); r.Applied || r.Available || backend.calls != 0 {
			t.Fatalf("unsafe control: %+v", r)
		}
	}
}

func TestDeviceSamplesAreThrottledAndCameraCallsSerialize(t *testing.T) {
	backend := &fakeDeviceBackend{devices: []cameraDevice{testCamera("a", true)}}
	d := newDeviceManager(filepath.Join(t.TempDir(), "settings.json"), backend)
	first := d.sample(context.Background())
	d.sample(context.Background())
	if backend.activityCalls != 1 || first.Microphone.State != "active" || first.Camera.State != "idle" {
		t.Fatalf("unexpected sampling: %+v", first)
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); d.Apply(context.Background()) }()
	}
	wg.Wait()
	if !backend.devices[0].Enabled || len(d.journal) != 0 {
		t.Fatalf("serialized disable and restore failed: %+v", backend.devices)
	}
}

func TestCameraAlreadyDisabledDoesNotEnableUnownedDevices(t *testing.T) {
	backend := &fakeDeviceBackend{devices: []cameraDevice{testCamera("a", false)}}
	d := newDeviceManager(filepath.Join(t.TempDir(), "settings.json"), backend)
	if activity := d.sample(context.Background()); activity.CameraControl.Available {
		t.Fatal("advertised unowned restore")
	}
	d.Apply(context.Background())
	if backend.calls != 0 || backend.devices[0].Enabled {
		t.Fatal("enabled unowned camera")
	}
}

// muteBackend also reports mute state, which tests change the way a toggle or
// a keyboard mute key would.
type muteBackend struct {
	fakeDeviceBackend
	mu         sync.Mutex
	microphone string
	speaker    string
}

func (m *muteBackend) AudioMute(context.Context) (AudioMuteState, AudioMuteState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return AudioMuteState{State: m.microphone, ObservedAt: time.Now().UTC()}, AudioMuteState{State: m.speaker, ObservedAt: time.Now().UTC()}
}

func (m *muteBackend) setMicrophone(state string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.microphone = state
}

// muteToggleController mutes the backend's microphone like the real mic_mute.
type muteToggleController struct{ backend *muteBackend }

func (muteToggleController) Capabilities() []ControlCapability {
	return capabilitiesFor(func(ControlAction) bool { return true })
}

func (c muteToggleController) Apply(_ context.Context, action ControlAction) ControlResult {
	if action == ControlMicMute {
		c.backend.setMicrophone("muted")
	}
	return ControlResult{Action: action, Available: true, Applied: true, State: "muted"}
}

func woke(d *deviceManager) bool {
	select {
	case <-d.wake:
		return true
	default:
		return false
	}
}

func TestDeviceSampleReportsMuteStateAndResamplesAfterAToggle(t *testing.T) {
	ctx := context.Background()
	backend := &muteBackend{microphone: "unmuted", speaker: "muted"}
	d := newDeviceManager(filepath.Join(t.TempDir(), "settings.json"), backend)
	first := d.sample(ctx)
	if first.MicrophoneMute.State != "unmuted" || first.SpeakerMute.State != "muted" {
		t.Fatalf("mute state not sampled: mic=%+v speaker=%+v", first.MicrophoneMute, first.SpeakerMute)
	}
	controller := deviceController{SystemController: muteToggleController{backend: backend}, devices: d}
	controller.Apply(ctx, ControlMediaToggle)
	if woke(d) {
		t.Fatal("a control that changes no reported state woke the device loop")
	}
	controller.Apply(ctx, ControlMicMute)
	if !woke(d) {
		t.Fatal("a mute toggle did not wake the device loop")
	}
	// Inside the 2 s reuse window, only the requested refresh makes this read
	// the host again.
	if after := d.sample(ctx); after.MicrophoneMute.State != "muted" {
		t.Fatalf("the sample after a toggle reused the pre-toggle reading: %+v", after.MicrophoneMute)
	}
	backend.setMicrophone("unmuted")
	if cached := d.sample(ctx); cached.MicrophoneMute.State != "muted" {
		t.Fatalf("an unrequested sample skipped the reuse window: %+v", cached.MicrophoneMute)
	}
}

func TestCameraToggleRequestsAResample(t *testing.T) {
	d := newDeviceManager(filepath.Join(t.TempDir(), "settings.json"), &fakeDeviceBackend{devices: []cameraDevice{testCamera("a", true)}})
	d.sample(context.Background())
	if r := d.Apply(context.Background()); !r.Applied {
		t.Fatalf("disable: %+v", r)
	}
	if !woke(d) || !d.refresh.Load() {
		t.Fatal("a camera toggle did not request a fresh sample")
	}
}

func TestMuteStateIsUnknownWithoutAReader(t *testing.T) {
	d := newDeviceManager(filepath.Join(t.TempDir(), "settings.json"), &fakeDeviceBackend{})
	if warming := d.snapshot(); warming.MicrophoneMute.State != "unknown" || warming.SpeakerMute.Error == "" {
		t.Fatalf("warming snapshot claims a mute state: %+v %+v", warming.MicrophoneMute, warming.SpeakerMute)
	}
	sampled := d.sample(context.Background())
	for _, state := range []AudioMuteState{sampled.MicrophoneMute, sampled.SpeakerMute} {
		if state.State != "unknown" || state.Error == "" || state.ObservedAt.IsZero() {
			t.Fatalf("backend without a mute reader reported %+v", state)
		}
	}
}
