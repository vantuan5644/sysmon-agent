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
	"sync/atomic"
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
	Microphone     DeviceActivity     `json:"microphone"`
	Camera         DeviceActivity     `json:"camera"`
	CameraControl  CameraControlState `json:"camera_control"`
	MicrophoneMute AudioMuteState     `json:"microphone_mute"`
	SpeakerMute    AudioMuteState     `json:"speaker_mute"`
}

// AudioMuteState is the host's current mute setting for what the mic_mute and
// volume_mute toggles act on: every active microphone, and the default
// speaker. It is read on each device sample so the dashboard shows the state
// even when it changed outside Sysmon (a keyboard key, the OS mixer).
type AudioMuteState struct {
	State      string    `json:"state"` // muted, unmuted, mixed, unknown
	ObservedAt time.Time `json:"observed_at"`
	Error      string    `json:"error,omitempty"`
}

// audioMuteReader is implemented by backends that can read mute state.
type audioMuteReader interface {
	AudioMute(context.Context) (microphone, speaker AudioMuteState)
}

func unknownAudioMute(reason string) AudioMuteState {
	return AudioMuteState{State: "unknown", ObservedAt: time.Now().UTC(), Error: reason}
}

func observedAudioMute(muted bool) AudioMuteState {
	state := "unmuted"
	if muted {
		state = "muted"
	}
	return AudioMuteState{State: state, ObservedAt: time.Now().UTC()}
}

// aggregateAudioMute combines the endpoints a toggle acts on into one state.
func aggregateAudioMute(muted, unmuted, failed int, none string) AudioMuteState {
	switch {
	case failed > 0:
		return unknownAudioMute(fmt.Sprintf("mute state unreadable for %d of %d devices", failed, muted+unmuted+failed))
	case muted > 0 && unmuted > 0:
		return AudioMuteState{State: "mixed", ObservedAt: time.Now().UTC()}
	case muted > 0:
		return observedAudioMute(true)
	case unmuted > 0:
		return observedAudioMute(false)
	default:
		return unknownAudioMute(none)
	}
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
	// refresh makes the next sample bypass the 2 s reuse window, and wake
	// starts that sample now: after a toggle the dashboard waits on the stream
	// to confirm the new state, so it must not wait out the loop's sleep.
	refresh atomic.Bool
	wake    chan struct{}
}

func newDeviceManager(settingsPath string, backend deviceBackend) *deviceManager {
	d := &deviceManager{backend: backend, wake: make(chan struct{}, 1)}
	d.latest.CameraControl = CameraControlState{State: "unavailable", Error: "camera discovery is warming up"}
	d.latest.MicrophoneMute = unknownAudioMute("mute state is warming up")
	d.latest.SpeakerMute = unknownAudioMute("mute state is warming up")
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

// requestRefresh asks the device loop for a fresh sample right away. It never
// blocks: Apply calls it while holding d.mu.
func (d *deviceManager) requestRefresh() {
	d.refresh.Store(true)
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

func (d *deviceManager) sample(ctx context.Context) DeviceActivitySet {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.refresh.Swap(false) && time.Since(d.lastSample) < 2*time.Second {
		return d.snapshot()
	}
	latest := d.snapshot()
	latest.Microphone, latest.Camera = d.backend.Activity(ctx)
	if reader, ok := d.backend.(audioMuteReader); ok {
		latest.MicrophoneMute, latest.SpeakerMute = reader.AudioMute(ctx)
	} else {
		latest.MicrophoneMute = unknownAudioMute("mute state is unavailable on this platform")
		latest.SpeakerMute = latest.MicrophoneMute
	}
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
	d.requestRefresh() // Publish the toggled state and its activity at once.
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
	result := c.SystemController.Apply(ctx, action)
	if action == ControlMicMute || action == ControlVolumeMute {
		// Even a failed toggle may have changed some devices; resample either way.
		c.devices.requestRefresh()
	}
	return result
}
