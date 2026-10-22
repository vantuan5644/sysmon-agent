//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// Opt-in read-only hardware smoke. Never disables a device or starts capture.
func TestWindowsDeviceActivityReadOnly(t *testing.T) {
	if os.Getenv("SYSMON_DEVICE_READ_ONLY_SMOKE") != "1" {
		t.Skip("requires opt-in hardware inspection")
	}
	core, privacy := windowsMicActivity(), windowsPrivacyActivity("microphone")
	t.Logf("microphone sources: Core Audio=%s privacy=%s", core.State, privacy.State)
	mic, camera := newDeviceBackend().Activity(context.Background())
	for _, activity := range []DeviceActivity{mic, camera} {
		if activity.State != "active" && activity.State != "idle" && activity.State != "unknown" {
			t.Fatalf("invalid activity: %+v", activity)
		}
		if activity.ObservedAt.IsZero() {
			t.Fatal("missing observation timestamp")
		}
	}
	devices, _, err := windowsCameras(context.Background(), nil)
	t.Logf("microphone=%s camera=%s local camera devices=%d discovery error=%v", mic.State, camera.State, len(devices), err)
}

// This test binary must come back with its creation time and a definite "no
// package"; otherwise every NonPackaged privacy record would be kept forever.
func TestReadPrivacyProcessesSeesThisProcess(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	processes, err := readPrivacyProcesses()
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(began)
	found := false
	var unreadable []string
	for _, p := range processes {
		if !p.familyOK {
			unreadable = append(unreadable, p.exe)
		}
		if strings.EqualFold(p.exe, filepath.Base(exe)) && p.created != 0 && p.familyOK && p.family == "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("%s missing, or without creation time and package state, among %d processes", filepath.Base(exe), len(processes))
	}
	// One unread package keeps every packaged record, so name them. Unelevated
	// runs cannot open other users' processes; the LocalSystem service can.
	if len(unreadable) > 0 {
		t.Logf("package unreadable for %q", unreadable)
	}
	t.Logf("%d processes in %v", len(processes), elapsed)
}

// The registry half against a real tree: open records come back with their key
// path, and one whose owner is not running stops counting. The tree is built
// under a scratch HKCU key, never the privacy store itself.
func TestPrivacyRecordsFromRegistry(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	scratch := fmt.Sprintf(`Software\HomelabSysmonAgentTest-%d`, os.Getpid())
	root := scratch + `\microphone`
	t.Cleanup(func() { exec.Command("reg", "delete", `HKCU\`+scratch, "/f").Run() })
	add := func(key string, start, stop uint64) {
		for name, value := range map[string]uint64{"LastUsedTimeStart": start, "LastUsedTimeStop": stop} {
			if out, err := exec.Command("reg", "add", `HKCU\`+root+`\`+key, "/v", name, "/t", "REG_QWORD", "/d", strconv.FormatUint(value, 10), "/f").CombinedOutput(); err != nil {
				t.Fatalf("reg add %s: %v: %s", key, err, out)
			}
		}
	}
	now := filetimeTicks(syscall.NsecToFiletime(time.Now().UnixNano()))
	self := strings.ReplaceAll(exe, `\`, "#")
	add(`NonPackaged\`+self, now, 0)
	add(`NonPackaged\C:#Gone#sysmon-ghost.exe`, now, 0)
	add(`NonPackaged\C:#Done#finished.exe`, now-100, now)

	path, _ := syscall.UTF16PtrFromString(root)
	var key syscall.Handle
	if err := syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, path, 0, syscall.KEY_READ, &key); err != nil {
		t.Fatal(err)
	}
	defer syscall.RegCloseKey(key)
	var records []privacyRecord
	if !registryOpenCaptures(key, nil, &records) {
		t.Fatal("scratch tree read as incomplete")
	}
	var ghost []privacyRecord
	for _, record := range records {
		if len(record.key) != 2 || record.key[0] != "NonPackaged" || record.start != now {
			t.Fatalf("unexpected record %+v", record)
		}
		if strings.HasSuffix(record.key[1], "sysmon-ghost.exe") {
			ghost = append(ghost, record)
		}
	}
	if len(records) != 2 || len(ghost) != 1 {
		t.Fatalf("records=%+v; want this test and the ghost, not the finished one", records)
	}
	if !privacyCaptureRunning(records) {
		t.Fatal("capture by this running test was dropped")
	}
	if privacyCaptureRunning(ghost) {
		t.Fatal("capture whose process is gone still counts")
	}
}

//go:noinline
func growDeviceCallbackStack(depth int) uint64 {
	var buffer [512]uint64
	buffer[0] = uint64(depth)
	if depth == 0 {
		runtime.GC()
		return buffer[0]
	}
	result := growDeviceCallbackStack(depth - 1)
	runtime.KeepAlive(&buffer)
	return result + buffer[0]
}

func TestDeviceCOMCallKeepsOutputAliveAcrossNativeCallback(t *testing.T) {
	callback := syscall.NewCallback(func(object, output uintptr) uintptr {
		growDeviceCallbackStack(64)
		*(*uint64)(unsafe.Pointer(output)) = 0x12345678
		return 0
	})
	table := [1]uintptr{callback}
	object := [1]uintptr{uintptr(unsafe.Pointer(&table[0]))}
	var pinned runtime.Pinner
	pinned.Pin(&table)
	pinned.Pin(&object)
	defer pinned.Unpin()
	for i := 0; i < 100; i++ {
		var value uint64
		deviceCOMCall(uintptr(unsafe.Pointer(&object[0])), 0, uintptr(unsafe.Pointer(&value)))
		if value != 0x12345678 {
			t.Fatalf("native output lost after stack growth: %#x", value)
		}
	}
	runtime.KeepAlive(&object)
	runtime.KeepAlive(&table)
}

func TestWindowsMicActivityPrivacyFallback(t *testing.T) {
	for _, tc := range []struct{ name, core, privacy, want string }{
		{"desktop capture missed by service", "idle", "active", "active"},
		{"privacy capture when COM unavailable", "unknown", "active", "active"},
		{"capture absent from privacy reporting", "active", "idle", "active"},
		{"capture despite unreadable privacy", "active", "unknown", "active"},
		{"both idle", "idle", "idle", "idle"},
		{"unreadable user profile", "idle", "unknown", "unknown"},
		{"unreadable audio sessions", "unknown", "idle", "unknown"},
		{"both unavailable", "unknown", "unknown", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			makeReading := func(state, coverage string) DeviceActivity {
				if state == "unknown" {
					return unknownDeviceActivity(coverage + " unavailable")
				}
				return observedDeviceActivity(state == "active", coverage)
			}
			got := mergeWindowsMicActivity(makeReading(tc.core, "Core Audio"), makeReading(tc.privacy, "Privacy"))
			if got.State != tc.want {
				t.Fatalf("state=%s; want=%s", got.State, tc.want)
			}
			if got.ObservedAt.IsZero() {
				t.Fatal("missing observation time")
			}
			if got.State == "unknown" && got.Error == "" {
				t.Fatal("missing partial coverage error")
			}
		})
	}
}
