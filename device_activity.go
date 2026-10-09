package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Activity is independent of mute and device enablement: muted streams can run.
type DeviceActivity struct {
	State      string    `json:"state"` // active, idle, unknown
	ObservedAt time.Time `json:"observed_at"`
	Coverage   string    `json:"coverage,omitempty"`
	Error      string    `json:"error,omitempty"`
}

type DeviceActivitySet struct {
	Microphone    DeviceActivity     `json:"microphone"`
	Camera        DeviceActivity     `json:"camera"`
	CameraControl CameraControlState `json:"camera_control"`
}

type CameraControlState struct {
	RestorePending bool   `json:"restore_pending"`
	Available      bool   `json:"available"`
	State          string `json:"state"` // enabled, disabled, mixed, unavailable
	Supported      int    `json:"supported"`
	Unsupported    int    `json:"unsupported"`
	Error          string `json:"error,omitempty"`
}

type cameraDevice struct {
	ID       string `json:"id"`
	Identity string `json:"identity"`
	Name     string `json:"name"`
	Enabled  bool   `json:"enabled"`
}

type deviceBackend interface {
	Activity(context.Context) (DeviceActivity, DeviceActivity)
	Cameras(context.Context) ([]cameraDevice, int, error)
	SetCamera(context.Context, cameraDevice, bool) error
}

// deviceManager owns camera mutations, their durable undo records, and cached
// observations. Collection runs only in the sampler; capability reads are cheap.
type deviceManager struct {
	mu          sync.Mutex
	cacheMu     sync.RWMutex
	backend     deviceBackend
	journalPath string
	journal     []cameraDevice
	journalErr  error
	latest      DeviceActivitySet
	lastSample  time.Time
}

func newDeviceManager(settingsPath string, backend deviceBackend) *deviceManager {
	d := &deviceManager{backend: backend}
	d.latest.CameraControl = CameraControlState{State: "unavailable", Error: "camera discovery is warming up"}
	if settingsPath == "" {
		d.journalErr = errors.New("camera control requires a persistent -settings path")
		return d
	}
	d.journalPath = settingsPath + ".cameras.json"
	data, err := os.ReadFile(d.journalPath)
	if err == nil {
		d.journalErr = json.Unmarshal(data, &d.journal)
		for _, device := range d.journal {
			if device.ID == "" || device.Identity == "" || !device.Enabled {
				d.journalErr = errors.New("invalid camera recovery journal")
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		d.journalErr = err
	}
	return d
}

func unknownDeviceActivity(reason string) DeviceActivity {
	return DeviceActivity{State: "unknown", ObservedAt: time.Now().UTC(), Error: reason}
}

func observedDeviceActivity(active bool, coverage string) DeviceActivity {
	state := "idle"
	if active {
		state = "active"
	}
	return DeviceActivity{State: state, Coverage: coverage, ObservedAt: time.Now().UTC()}
}

func (d *deviceManager) sample(ctx context.Context) DeviceActivitySet {
	d.mu.Lock()
	defer d.mu.Unlock()
	if time.Since(d.lastSample) < 2*time.Second {
		return d.snapshot()
	}
	latest := d.snapshot()
	latest.Microphone, latest.Camera = d.backend.Activity(ctx)
	devices, unsupported, err := d.backend.Cameras(ctx)
	latest.CameraControl = d.cameraState(devices, unsupported, err)
	d.cacheMu.Lock()
	d.latest = latest
	d.cacheMu.Unlock()
	d.lastSample = time.Now()
	return latest
}

func (d *deviceManager) cameraState(devices []cameraDevice, unsupported int, err error) CameraControlState {
	s := CameraControlState{State: "unavailable", Supported: len(devices), Unsupported: unsupported}
	s.RestorePending = len(d.journal) > 0
	if err != nil {
		s.Error = err.Error()
		return s
	}
	if d.journalErr != nil {
		s.Error = d.journalErr.Error()
		return s
	}
	if len(devices) == 0 && len(d.journal) == 0 {
		s.Error = "no supported local cameras found"
		return s
	}
	s.Available = true
	enabled := 0
	for _, device := range devices {
		if device.Enabled {
			enabled++
		}
	}
	s.Available = enabled > 0 || s.RestorePending
	if !s.Available {
		s.Error = "cameras were already disabled outside Sysmon; no saved state to restore"
	}
	s.State = "enabled"
	if len(devices) == 0 || enabled == 0 {
		s.State = "disabled"
	} else if enabled != len(devices) {
		s.State = "mixed"
	}
	if s.RestorePending && enabled > 0 {
		s.State = "mixed"
	}
	missing := 0
	for _, saved := range d.journal {
		present := false
		for _, device := range devices {
			if device.ID == saved.ID && device.Identity == saved.Identity {
				present = true
				break
			}
		}
		if !present {
			missing++
		}
	}
	if missing > 0 {
		s.State = "mixed"
		s.Error = fmt.Sprintf("%d recorded camera interfaces are disconnected; restore retained", missing)
	}
	// Unsupported devices (virtual, network) are reported by count only. They
	// are outside coverage, not a failure, so they never become an error that
	// the dashboard would surface as a warning on every toggle.
	return s
}

func (d *deviceManager) available() bool {
	return d.snapshot().CameraControl.Available
}

func (d *deviceManager) snapshot() DeviceActivitySet {
	d.cacheMu.RLock()
	defer d.cacheMu.RUnlock()
	return d.latest
}

func (d *deviceManager) saveJournal(next []cameraDevice) error {
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	// Persist before changing a device. An unwritable location cannot leave a
	// camera disabled without a recovery record.
	if err := os.MkdirAll(filepath.Dir(d.journalPath), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(d.journalPath), ".sysmon-camera-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = replaceFile(name, d.journalPath)
	}
	if err == nil {
		d.journal = next
	}
	return err
}

func (d *deviceManager) Apply(ctx context.Context) ControlResult {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := ControlResult{Action: ControlCameraToggle, Available: true}
	if d.journalErr != nil {
		return unavailableControlResult(r.Action, d.journalErr.Error())
	}
	devices, unsupported, err := d.backend.Cameras(ctx)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	if len(devices) == 0 && len(d.journal) == 0 {
		return unavailableControlResult(r.Action, "no supported local cameras found")
	}
	var failures []string
	// Outstanding records always select restore, including after partial disable.
	if len(d.journal) > 0 {
		if preparer, ok := d.backend.(interface {
			PrepareRestore(context.Context, []cameraDevice) error
		}); ok {
			if err := preparer.PrepareRestore(ctx, d.journal); err != nil {
				failures = append(failures, err.Error())
			}
		}
		for _, saved := range append([]cameraDevice(nil), d.journal...) {
			var found *cameraDevice
			for i := range devices {
				if devices[i].ID == saved.ID && devices[i].Identity == saved.Identity {
					found = &devices[i]
					break
				}
			}
			if found == nil {
				failures = append(failures, saved.Name+": disconnected; restore retained")
				continue
			}
			if err := d.backend.SetCamera(ctx, *found, true); err != nil {
				failures = append(failures, saved.Name+": "+err.Error())
				continue
			}
			next := make([]cameraDevice, 0, len(d.journal))
			for _, entry := range d.journal {
				if entry.ID != saved.ID {
					next = append(next, entry)
				}
			}
			if err := d.saveJournal(next); err != nil {
				failures = append(failures, err.Error())
				break
			}
		}
	} else {
		for _, device := range devices {
			if !device.Enabled {
				continue
			}
			next := append(append([]cameraDevice(nil), d.journal...), device)
			if err := d.saveJournal(next); err != nil {
				failures = append(failures, err.Error())
				break
			}
			if err := d.backend.SetCamera(ctx, device, false); err != nil {
				failures = append(failures, device.Name+": "+err.Error())
			}
		}
	}
	devices, unsupported, err = d.backend.Cameras(ctx)
	control := d.cameraState(devices, unsupported, err)
	d.cacheMu.Lock()
	d.latest.CameraControl = control
	d.cacheMu.Unlock()
	d.lastSample = time.Time{} // Next slow pass refreshes activity after a toggle.
	r.State = control.State
	r.CameraControl = &control
	if err != nil {
		failures = append(failures, err.Error())
	}
	r.Applied = len(failures) == 0
	r.Error = strings.Join(failures, "; ")
	r.Message = control.Error
	if len(failures) > 0 {
		r.State = "mixed"
		control.State = "mixed"
		control.Error = r.Error
		d.cacheMu.Lock()
		d.latest.CameraControl = control
		d.cacheMu.Unlock()
	}
	return r
}

type deviceController struct {
	SystemController
	devices *deviceManager
}

func (c deviceController) Capabilities() []ControlCapability {
	caps := c.SystemController.Capabilities()
	for i := range caps {
		if caps[i].Action == ControlCameraToggle {
			caps[i].Available = c.devices.available()
		}
	}
	return caps
}

func (c deviceController) Apply(ctx context.Context, action ControlAction) ControlResult {
	if action == ControlCameraToggle {
		return c.devices.Apply(ctx)
	}
	return c.SystemController.Apply(ctx, action)
}
