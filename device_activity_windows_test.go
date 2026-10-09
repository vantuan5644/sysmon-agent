//go:build windows

package main

import (
	"context"
	"os"
	"runtime"
	"syscall"
	"testing"
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
