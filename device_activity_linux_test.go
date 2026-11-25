//go:build linux

package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestLinuxCameraInterfacesRestoreTogetherAndPreserveAudio(t *testing.T) {
	root := filepath.Join(t.TempDir(), "devices")
	write := func(path, value string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"1-1:1.0", "1-1:1.1"} {
		write(filepath.Join(root, id, "bInterfaceClass"), "0e")
		write(filepath.Join(root, id, "authorized"), "1")
	}
	write(filepath.Join(root, "1-1:1.2", "bInterfaceClass"), "01")
	write(filepath.Join(root, "1-1:1.2", "authorized"), "1")
	write(filepath.Join(root, "1-1", "idVendor"), "abcd")
	write(filepath.Join(root, "1-1", "idProduct"), "1234")
	write(filepath.Join(root, "1-1", "serial"), "fixture")
	write(filepath.Join(root, "1-1", "product"), "Fixture webcam")
	write(filepath.Join(root, "../drivers_probe"), "")
	backend := linuxDeviceBackend{usbRoot: root, canControl: func() bool { return true }}
	devices, _, err := backend.Cameras(context.Background())
	if err != nil || len(devices) != 2 {
		t.Fatalf("camera interface discovery: devices=%+v err=%v", devices, err)
	}
	d := newDeviceManager(filepath.Join(t.TempDir(), "settings.json"), backend)
	if r := d.Apply(context.Background()); !r.Applied {
		t.Fatal(r)
	}
	for _, device := range devices {
		if readDeviceText(filepath.Join(root, device.ID, "authorized")) != "0" {
			t.Fatal("video interface remained enabled")
		}
	}
	if readDeviceText(filepath.Join(root, "1-1:1.2", "authorized")) != "1" {
		t.Fatal("webcam microphone was disabled")
	}
	if r := d.Apply(context.Background()); !r.Applied {
		t.Fatal(r)
	}
	for _, device := range devices {
		if readDeviceText(filepath.Join(root, device.ID, "authorized")) != "1" {
			t.Fatal("video interface did not restore")
		}
	}
	if readDeviceText(filepath.Join(root, "../drivers_probe")) == "" {
		t.Fatal("driver rebind was not requested")
	}
}

func TestLinuxCameraControlFollowsProbeWriteAccess(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	root := filepath.Join(t.TempDir(), "devices")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(root, "../drivers_probe")
	if err := os.WriteFile(probe, nil, 0600); err != nil {
		t.Fatal(err)
	}
	backend := linuxDeviceBackend{usbRoot: root}
	if _, _, err := backend.Cameras(context.Background()); err != nil {
		t.Fatalf("writable drivers_probe should grant control: %v", err)
	}
	if err := os.Chmod(probe, 0400); err != nil {
		t.Fatal(err)
	}
	if _, _, err := backend.Cameras(context.Background()); err == nil {
		t.Fatal("read-only drivers_probe granted control")
	}
}

func TestProcOwnedByMatchesOnlyTheGivenUser(t *testing.T) {
	self := filepath.Join("/proc", strconv.Itoa(os.Getpid()), "fd")
	uid := uint32(os.Geteuid())
	if !procOwnedBy(self, uid) {
		t.Fatal("own process not recognised as owned")
	}
	if procOwnedBy(self, uid+1) {
		t.Fatal("own process attributed to another user")
	}
	if !procOwnedBy(filepath.Join(t.TempDir(), "gone"), uid+1) {
		t.Fatal("unstat-able process must count against completeness")
	}
}

func TestScanV4L2HoldersNamesCameraHoldersFromAFixtureProc(t *testing.T) {
	root := t.TempDir()
	link := func(pid, fd, target string) {
		t.Helper()
		dir := filepath.Join(root, pid, "fd")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, fd)); err != nil {
			t.Fatal(err)
		}
	}
	writeFixture(t, filepath.Join(root, "200/comm"), "obs\n")
	link("200", "0", "/dev/null")
	link("200", "31", "/dev/video0")
	link("200", "32", "/dev/video1")
	writeFixture(t, filepath.Join(root, "201/comm"), "pipewire\n") // the session server keeps cameras open idle
	link("201", "40", "/dev/video0")
	writeFixture(t, filepath.Join(root, "202/comm"), "bash\n")
	link("202", "1", "/dev/pts/0")
	writeFixture(t, filepath.Join(root, "self/comm"), "ignored\n") // not a PID
	holders, complete := scanV4L2Holders(context.Background(), root, uint32(os.Geteuid()))
	if !complete {
		t.Fatal("readable fixture reported incomplete")
	}
	if len(holders) != 1 || holders[0] != (DeviceApp{PID: 200, Name: "obs"}) {
		t.Fatalf("holders = %+v", holders)
	}
	if got := withDeviceApps(observedDeviceActivity(true, "x"), holders); len(got.Apps) != 1 || got.Apps[0].Name != "obs" {
		t.Fatalf("activity apps = %+v", got.Apps)
	}
}
