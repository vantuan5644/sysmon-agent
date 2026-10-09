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

func (b linuxDeviceBackend) Activity(ctx context.Context) (DeviceActivity, DeviceActivity) {
	mic := unknownDeviceActivity("capture session is inaccessible")
	camera := unknownDeviceActivity("capture session is inaccessible")
	graphCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if data, err := exec.CommandContext(graphCtx, "pw-dump").Output(); err == nil {
		m, c, err := parsePipewireActivity(data)
		if err == nil {
			mic = observedDeviceActivity(m, "PipeWire recording streams in the agent's session")
			camera = observedDeviceActivity(c, "PipeWire capture streams in the agent's session")
			if os.Geteuid() != 0 {
				camera.Coverage += " and V4L2 handles readable by the agent's user"
			}
		}
	}
	if mic.State == "unknown" && graphCtx.Err() == nil {
		if data, err := exec.CommandContext(graphCtx, "pactl", "--format=json", "list", "source-outputs").Output(); err == nil {
			if sources, err := exec.CommandContext(graphCtx, "pactl", "--format=json", "list", "sources").Output(); err == nil {
				if active, err := parsePulseActivity(data, sources); err == nil {
					mic = observedDeviceActivity(active, "PulseAudio microphone recording streams in the agent's session")
				}
			}
		}
	}
	// Direct ALSA capture bypasses session servers. Running capture is positive
	// evidence; an unreadable status is never interpreted as idle.
	statuses, _ := filepath.Glob("/proc/asound/card*/pcm*c/sub*/status")
	alsaComplete := len(statuses) > 0
	for _, path := range statuses {
		data, err := os.ReadFile(path)
		if err != nil {
			alsaComplete = false
		}
		if err == nil && strings.Contains(string(data), "RUNNING") {
			mic = observedDeviceActivity(true, "ALSA running capture")
		}
	}
	if mic.State == "unknown" && alsaComplete {
		mic = observedDeviceActivity(false, "ALSA hardware capture status; desktop session unavailable")
	}
	// Open V4L2 handles indicate use, not necessarily frame streaming. Ignore
	// session-server handles which can remain open while applications are idle.
	// An unprivileged agent cannot read fd tables the kernel gives to another
	// user: other users' processes, and its own non-dumpable ones (setcap or
	// PR_SET_DUMPABLE=0, e.g. apollo, sd-pam), whose fd dir becomes root's.
	// Those are outside its coverage, like PipeWire streams outside its
	// session; only an unreadable table it owns leaves the observation incomplete.
	procs, procErr := os.ReadDir("/proc")
	processesComplete := procErr == nil
	uid := uint32(os.Geteuid())
	for _, proc := range procs {
		if ctx.Err() != nil {
			break
		}
		if _, err := strconv.Atoi(proc.Name()); err != nil {
			continue
		}
		comm := readDeviceText(filepath.Join("/proc", proc.Name(), "comm"))
		if comm == "pipewire" || comm == "wireplumber" {
			continue
		}
		fds, err := os.ReadDir(filepath.Join("/proc", proc.Name(), "fd"))
		if err != nil {
			if errors.Is(err, os.ErrPermission) && (uid == 0 || procOwnedBy(filepath.Join("/proc", proc.Name(), "fd"), uid)) {
				processesComplete = false
			}
			continue
		}
		for _, fd := range fds {
			target, _ := os.Readlink(filepath.Join("/proc", proc.Name(), "fd", fd.Name()))
			if strings.HasPrefix(target, "/dev/video") {
				camera = observedDeviceActivity(true, "application holds a V4L2 camera device")
			}
		}
	}
	if camera.State == "idle" && !processesComplete {
		camera = unknownDeviceActivity("some process camera handles are inaccessible")
	}
	if ctx.Err() != nil {
		a := unknownDeviceActivity(ctx.Err().Error())
		return a, a
	}
	return mic, camera
}
