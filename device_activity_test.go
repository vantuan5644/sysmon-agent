package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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
