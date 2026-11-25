//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type linuxDeviceBackend struct {
	usbRoot    string
	procRoot   string // "" means /proc
	canControl func() bool
}

func newDeviceBackend() deviceBackend { return linuxDeviceBackend{usbRoot: "/sys/bus/usb/devices"} }

// probeWritable reports whether an unprivileged agent was granted the sysfs
// access camera control needs. Root-only defaults fail this check; the udev
// rule and tmpfiles entry in scripts/ grant it to the sysmon-camera group. A
// camera whose authorized file was missed still fails as a device error.
func (b linuxDeviceBackend) probeWritable() bool {
	const wOK = 2
	return syscall.Access(filepath.Join(b.usbRoot, "../drivers_probe"), wOK) == nil
}

// procOwnedBy reports whether a /proc entry belongs to uid. An entry that
// vanished or cannot be stat'ed is treated as owned, so it still counts
// against completeness rather than being silently excluded.
func procOwnedBy(path string, uid uint32) bool {
	info, err := os.Stat(path)
	if err != nil {
		return true
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return !ok || st.Uid == uid
}

func readDeviceText(path string) string {
	b, _ := os.ReadFile(path)
	return strings.TrimSpace(string(b))
}

func (b linuxDeviceBackend) Cameras(ctx context.Context) ([]cameraDevice, int, error) {
	privileged := os.Geteuid() == 0 || b.probeWritable()
	if b.canControl != nil {
		privileged = b.canControl()
	}
	if !privileged {
		return nil, 0, errors.New("USB camera control requires root or the sysmon-camera group (scripts/udev/70-sysmon-camera.rules)")
	}
	paths, err := filepath.Glob(filepath.Join(b.usbRoot, "*:*"))
	if err != nil {
		return nil, 0, err
	}
	var devices []cameraDevice
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		if readDeviceText(filepath.Join(path, "bInterfaceClass")) != "0e" {
			continue
		}
		id := filepath.Base(path)
		parent := filepath.Join(b.usbRoot, strings.Split(id, ":")[0])
		identity := strings.Join([]string{id, readDeviceText(filepath.Join(parent, "idVendor")), readDeviceText(filepath.Join(parent, "idProduct")), readDeviceText(filepath.Join(parent, "serial"))}, "|")
		name := readDeviceText(filepath.Join(parent, "product"))
		if name == "" {
			name = id
		}
		authorized := readDeviceText(filepath.Join(path, "authorized"))
		if authorized != "0" && authorized != "1" {
			return nil, 0, fmt.Errorf("%s: interface authorization unavailable", name)
		}
		devices = append(devices, cameraDevice{ID: id, Identity: identity, Name: name, Enabled: authorized == "1"})
	}
	// Count video devices outside USB, including virtual cameras, without changing them.
	unsupported := 0
	videoPaths, _ := filepath.Glob("/sys/class/video4linux/video*")
	for _, path := range videoPaths {
		real, _ := filepath.EvalSymlinks(filepath.Join(path, "device"))
		if !strings.Contains(real, "/usb") {
			unsupported++
		}
	}
	return devices, unsupported, nil
}

func (b linuxDeviceBackend) SetCamera(ctx context.Context, device cameraDevice, enabled bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Never accept arbitrary persisted paths. Re-enumerate and match identity.
	devices, _, err := b.Cameras(ctx)
	if err != nil {
		return err
	}
	valid := false
	for _, current := range devices {
		if current.ID == device.ID && current.Identity == device.Identity {
			valid = true
			break
		}
	}
	if !valid {
		return errors.New("camera identity changed or device disconnected")
	}
	value := "0"
	if enabled {
		value = "1"
	}
	path := filepath.Join(b.usbRoot, device.ID, "authorized")
	if err := os.WriteFile(path, []byte(value), 0200); err != nil {
		return err
	}
	if enabled {
		if err := os.WriteFile(filepath.Join(b.usbRoot, "../drivers_probe"), []byte(device.ID), 0200); err != nil {
			return fmt.Errorf("authorize succeeded, driver restore failed: %w", err)
		}
	}
	if readDeviceText(path) != value {
		return errors.New("camera authorization did not reach requested state")
	}
	return nil
}

// A UVC camera often has separate video control and streaming interfaces. All
// recorded interfaces must be authorized before rebinding the control driver.
// Originally disabled interfaces remain absent from the recovery records.
func (b linuxDeviceBackend) PrepareRestore(ctx context.Context, saved []cameraDevice) error {
	devices, _, err := b.Cameras(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, entry := range saved {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, current := range devices {
			if current.ID == entry.ID && current.Identity == entry.Identity {
				if err := os.WriteFile(filepath.Join(b.usbRoot, current.ID, "authorized"), []byte("1"), 0200); err != nil {
					failures = append(failures, fmt.Errorf("%s: %w", current.Name, err))
				}
				break
			}
		}
	}
	return errors.Join(failures...)
}

func (linuxDeviceBackend) AudioMute(ctx context.Context) (AudioMuteState, AudioMuteState) {
	return linuxMicrophoneMute(ctx), linuxDefaultMute(ctx, false)
}

func (b linuxDeviceBackend) Activity(ctx context.Context) (DeviceActivity, DeviceActivity) {
	mic := unknownDeviceActivity("capture session is inaccessible")
	camera := unknownDeviceActivity("capture session is inaccessible")
	var micApps, cameraApps []DeviceApp
	graphCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if data, err := trackedOutput(exec.CommandContext(graphCtx, "pw-dump")); err == nil {
		capture, err := parsePipewireCapture(data)
		if err == nil {
			mic = observedDeviceActivity(capture.mic, "PipeWire recording streams in the agent's session")
			camera = observedDeviceActivity(capture.camera, "PipeWire capture streams in the agent's session")
			micApps, cameraApps = capture.micApps, capture.cameraApps
			if os.Geteuid() != 0 {
				camera.Coverage += " and V4L2 handles readable by the agent's user"
			}
		}
	}
	if mic.State == "unknown" && graphCtx.Err() == nil {
		if data, err := trackedOutput(exec.CommandContext(graphCtx, "pactl", "--format=json", "list", "source-outputs")); err == nil {
			if sources, err := trackedOutput(exec.CommandContext(graphCtx, "pactl", "--format=json", "list", "sources")); err == nil {
				if active, apps, err := parsePulseCapture(data, sources); err == nil {
					mic = observedDeviceActivity(active, "PulseAudio microphone recording streams in the agent's session")
					micApps = apps
				}
			}
		}
	}
	procRoot := b.proc()
	// Direct ALSA capture bypasses session servers. Running capture is positive
	// evidence; an unreadable status is never interpreted as idle. The owner of
	// a running substream is named; on a desktop that is usually the sound
	// server, which mergeDeviceApps keeps only when no client is known.
	alsa := readALSACapture(procRoot)
	if alsa.running {
		mic = observedDeviceActivity(true, "ALSA running capture")
		for _, pid := range alsa.owners {
			if name := procAppName(procRoot, pid); name != "" {
				micApps = append(micApps, DeviceApp{PID: pid, Name: name})
			}
		}
	}
	if mic.State == "unknown" && alsa.found && alsa.complete {
		mic = observedDeviceActivity(false, "ALSA hardware capture status; desktop session unavailable")
	}
	// Open V4L2 handles indicate use, not necessarily frame streaming. Ignore
	// session-server handles which can remain open while applications are idle.
	// An unprivileged agent cannot read fd tables the kernel gives to another
	// user: other users' processes, and its own non-dumpable ones (setcap or
	// PR_SET_DUMPABLE=0, e.g. apollo, sd-pam), whose fd dir becomes root's.
	// Those are outside its coverage, like PipeWire streams outside its
	// session; only an unreadable table it owns leaves the observation incomplete.
	holders, processesComplete := scanV4L2Holders(ctx, procRoot, uint32(os.Geteuid()))
	if len(holders) > 0 {
		camera = observedDeviceActivity(true, "application holds a V4L2 camera device")
		cameraApps = append(cameraApps, holders...)
	}
	if camera.State == "idle" && !processesComplete {
		camera = unknownDeviceActivity("some process camera handles are inaccessible")
	}
	if ctx.Err() != nil {
		a := unknownDeviceActivity(ctx.Err().Error())
		return a, a
	}
	return withDeviceApps(mic, micApps), withDeviceApps(camera, cameraApps)
}

func (b linuxDeviceBackend) proc() string {
	if b.procRoot != "" {
		return b.procRoot
	}
	return "/proc"
}

// scanV4L2Holders walks every process's fd table under procRoot and names the
// processes holding a /dev/video* handle, skipping the session servers.
// complete is false when a table the agent should be able to read was not.
func scanV4L2Holders(ctx context.Context, procRoot string, uid uint32) (holders []DeviceApp, complete bool) {
	procs, procErr := os.ReadDir(procRoot)
	complete = procErr == nil
	for _, proc := range procs {
		if ctx.Err() != nil {
			break
		}
		pid, err := strconv.Atoi(proc.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join(procRoot, proc.Name())
		comm := readDeviceText(filepath.Join(dir, "comm"))
		if comm == "pipewire" || comm == "wireplumber" {
			continue
		}
		fds, err := os.ReadDir(filepath.Join(dir, "fd"))
		if err != nil {
			if errors.Is(err, os.ErrPermission) && (uid == 0 || procOwnedBy(filepath.Join(dir, "fd"), uid)) {
				complete = false
			}
			continue
		}
		for _, fd := range fds {
			target, _ := os.Readlink(filepath.Join(dir, "fd", fd.Name()))
			if strings.HasPrefix(target, "/dev/video") {
				name := procAppName(procRoot, pid)
				if name == "" {
					name = comm
				}
				holders = append(holders, DeviceApp{PID: pid, Name: name})
				break
			}
		}
	}
	return holders, complete
}
