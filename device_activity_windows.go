//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

var (
	deviceSetup          = syscall.NewLazyDLL("setupapi.dll")
	deviceConfig         = syscall.NewLazyDLL("cfgmgr32.dll")
	deviceGetClassDevs   = deviceSetup.NewProc("SetupDiGetClassDevsW")
	deviceEnumInfo       = deviceSetup.NewProc("SetupDiEnumDeviceInfo")
	deviceGetID          = deviceSetup.NewProc("SetupDiGetDeviceInstanceIdW")
	deviceGetProperty    = deviceSetup.NewProc("SetupDiGetDeviceRegistryPropertyW")
	deviceDestroyList    = deviceSetup.NewProc("SetupDiDestroyDeviceInfoList")
	deviceGetStatus      = deviceConfig.NewProc("CM_Get_DevNode_Status")
	deviceDisable        = deviceConfig.NewProc("CM_Disable_DevNode")
	deviceEnable         = deviceConfig.NewProc("CM_Enable_DevNode")
	deviceIsAdmin        = syscall.NewLazyDLL("shell32.dll").NewProc("IsUserAnAdmin")
	deviceOle            = syscall.NewLazyDLL("ole32.dll")
	deviceCoInitialize   = deviceOle.NewProc("CoInitializeEx")
	deviceCoUninitialize = deviceOle.NewProc("CoUninitialize")
	deviceCoCreate       = deviceOle.NewProc("CoCreateInstance")
)

type windowsDeviceBackend struct{}

func newDeviceBackend() deviceBackend { return windowsDeviceBackend{} }

type cameraDevInfo struct {
	Size     uint32
	Class    windowsGUID
	DevInst  uint32
	Reserved uintptr
}

var cameraClassGUID = windowsGUID{Data1: 0xca3e7ab9, Data2: 0xb4c3, Data3: 0x4ae6, Data4: [8]byte{0x82, 0x51, 0x57, 0x9e, 0xf9, 0x33, 0x89, 0x0f}}
var cameraImageGUID = windowsGUID{Data1: 0x6bdd1fc6, Data2: 0x810f, Data3: 0x11d0, Data4: [8]byte{0xbe, 0xc7, 0x08, 0x00, 0x2b, 0xe2, 0x09, 0x2f}}

func cameraProperty(handle uintptr, info *cameraDevInfo, property uint32) string {
	var buffer [2048]uint16
	ok, _, _ := deviceGetProperty.Call(handle, uintptr(unsafe.Pointer(info)), uintptr(property), 0, uintptr(unsafe.Pointer(&buffer[0])), uintptr(unsafe.Sizeof(buffer)), 0)
	if ok == 0 {
		return ""
	}
	return syscall.UTF16ToString(buffer[:])
}

func windowsCameras(ctx context.Context, visit func(cameraDevice, *cameraDevInfo) error) ([]cameraDevice, int, error) {
	var devices []cameraDevice
	unsupported := 0
	for _, guid := range []windowsGUID{cameraClassGUID, cameraImageGUID} {
		handle, _, err := deviceGetClassDevs.Call(uintptr(unsafe.Pointer(&guid)), 0, 0, 2) // present devices, including disabled
		if handle == ^uintptr(0) {
			return nil, 0, err
		}
		err = nil
		func() {
			defer deviceDestroyList.Call(handle)
			for index := uintptr(0); ; index++ {
				if ctx.Err() != nil {
					err = ctx.Err()
					return
				}
				info := cameraDevInfo{Size: uint32(unsafe.Sizeof(cameraDevInfo{}))}
				ok, _, callErr := deviceEnumInfo.Call(handle, index, uintptr(unsafe.Pointer(&info)))
				if ok == 0 {
					if callErr != syscall.Errno(259) {
						err = callErr
					}
					return
				}
				if guid == cameraImageGUID && !strings.EqualFold(cameraProperty(handle, &info, 4), "usbvideo") {
					continue
				}
				var id [2048]uint16
				ok, _, callErr = deviceGetID.Call(handle, uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&id[0])), uintptr(len(id)), 0)
				if ok == 0 {
					err = callErr
					return
				}
				var status, problem uint32
				result, _, _ := deviceGetStatus.Call(uintptr(unsafe.Pointer(&status)), uintptr(unsafe.Pointer(&problem)), uintptr(info.DevInst), 0)
				if result != 0 {
					err = fmt.Errorf("camera device status: configuration error %d", result)
					return
				}
				if problem != 0 && problem != 22 {
					unsupported++
					continue
				}
				instance := syscall.UTF16ToString(id[:])
				name := cameraProperty(handle, &info, 12)
				if name == "" {
					name = cameraProperty(handle, &info, 0)
				}
				device := cameraDevice{ID: instance, Identity: instance + "|" + cameraProperty(handle, &info, 1), Name: name, Enabled: problem != 22}
				devices = append(devices, device)
				if visit != nil {
					if e := visit(device, &info); e != nil {
						err = e
						return
					}
				}
			}
		}()
		// Proc.Call can return a nonzero last-error even on success. Only errors
		// explicitly assigned during enumeration are meaningful.
		if err != nil && err != syscall.Errno(0) {
			return nil, unsupported, err
		}
	}
	return devices, unsupported, nil
}

func (windowsDeviceBackend) Cameras(ctx context.Context) ([]cameraDevice, int, error) {
	admin, _, _ := deviceIsAdmin.Call()
	if admin == 0 {
		return nil, 0, errors.New("camera device control requires Administrator or LocalSystem privileges")
	}
	return windowsCameras(ctx, nil)
}

func (windowsDeviceBackend) SetCamera(ctx context.Context, device cameraDevice, enabled bool) error {
	found := false
	_, _, err := windowsCameras(ctx, func(current cameraDevice, info *cameraDevInfo) error {
		if current.ID != device.ID || current.Identity != device.Identity {
			return nil
		}
		found = true
		if current.Enabled == enabled {
			return nil
		}
		var result uintptr
		if enabled {
			result, _, _ = deviceEnable.Call(uintptr(info.DevInst), 0)
		} else {
			result, _, _ = deviceDisable.Call(uintptr(info.DevInst), 0x8|0x4)
		} // persistent disable
		if result != 0 {
			return fmt.Errorf("device operation failed (configuration error %d)", result)
		}
		var status, problem uint32
		result, _, _ = deviceGetStatus.Call(uintptr(unsafe.Pointer(&status)), uintptr(unsafe.Pointer(&problem)), uintptr(info.DevInst), 0)
		if result != 0 || (problem == 22) == enabled || (enabled && problem != 0) {
			return errors.New("camera state could not be verified; a device restart may be required")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !found {
		return errors.New("camera disconnected or identity changed")
	}
	return nil
}

// COM calls stay on one OS thread and release every acquired interface. No
// interpreter is launched while a dashboard is connected.
// COM arguments include addresses of Go output variables. Preserve those
// objects across stack growth and GC just as syscall.SyscallN does; otherwise
// native COM can write through an address from an obsolete Go stack.
//
//go:uintptrescapes
func deviceCOMCall(object uintptr, slot int, args ...uintptr) uintptr {
	vtable := *(*uintptr)(unsafe.Pointer(object))
	fn := *(*uintptr)(unsafe.Pointer(vtable + uintptr(slot)*unsafe.Sizeof(uintptr(0))))
	arguments := append([]uintptr{object}, args...)
	result, _, _ := syscall.SyscallN(fn, arguments...)
	return result
}
func deviceCOMFailed(result uintptr) bool { return int32(result) < 0 }

func windowsMicActivity() DeviceActivity {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	hr, _, _ := deviceCoInitialize.Call(0, 0)
	if deviceCOMFailed(hr) {
		return unknownDeviceActivity("Core Audio COM initialization failed")
	}
	defer deviceCoUninitialize.Call()
	clsid := windowsGUID{Data1: 0xbcde0395, Data2: 0xe52f, Data3: 0x467c, Data4: [8]byte{0x8e, 0x3d, 0xc4, 0x57, 0x92, 0x91, 0x69, 0x2e}}
	iid := windowsGUID{Data1: 0xa95664d2, Data2: 0x9614, Data3: 0x4f35, Data4: [8]byte{0xa7, 0x46, 0xde, 0x8d, 0xb6, 0x36, 0x17, 0xe6}}
	managerIID := windowsGUID{Data1: 0x77aa99a0, Data2: 0x1bd6, Data3: 0x484f, Data4: [8]byte{0x8b, 0xc7, 0x2c, 0x65, 0x4c, 0x9a, 0x9b, 0x6f}}
	var enumerator uintptr
	hr, _, _ = deviceCoCreate.Call(uintptr(unsafe.Pointer(&clsid)), 0, 23, uintptr(unsafe.Pointer(&iid)), uintptr(unsafe.Pointer(&enumerator)))
	if deviceCOMFailed(hr) {
		return unknownDeviceActivity("Core Audio device enumeration unavailable")
	}
	defer deviceCOMCall(enumerator, 2)
	var collection uintptr
	if deviceCOMFailed(deviceCOMCall(enumerator, 3, 1, 1, uintptr(unsafe.Pointer(&collection)))) {
		return unknownDeviceActivity("capture endpoints unavailable")
	}
	defer deviceCOMCall(collection, 2)
	var count uint32
	if deviceCOMFailed(deviceCOMCall(collection, 3, uintptr(unsafe.Pointer(&count)))) {
		return unknownDeviceActivity("capture endpoint count unavailable")
	}
	active, failed := false, false
	for i := uint32(0); i < count; i++ {
		func() {
			var endpoint, manager, sessions uintptr
			if deviceCOMFailed(deviceCOMCall(collection, 4, uintptr(i), uintptr(unsafe.Pointer(&endpoint)))) {
				failed = true
				return
			}
			defer deviceCOMCall(endpoint, 2)
			if deviceCOMFailed(deviceCOMCall(endpoint, 3, uintptr(unsafe.Pointer(&managerIID)), 23, 0, uintptr(unsafe.Pointer(&manager)))) {
				failed = true
				return
			}
			defer deviceCOMCall(manager, 2)
			if deviceCOMFailed(deviceCOMCall(manager, 5, uintptr(unsafe.Pointer(&sessions)))) {
				failed = true
				return
			}
			defer deviceCOMCall(sessions, 2)
			var n uint32
			if deviceCOMFailed(deviceCOMCall(sessions, 3, uintptr(unsafe.Pointer(&n)))) {
				failed = true
				return
			}
			for j := uint32(0); j < n; j++ {
				var session uintptr
				if deviceCOMFailed(deviceCOMCall(sessions, 4, uintptr(j), uintptr(unsafe.Pointer(&session)))) {
					failed = true
					continue
				}
				var state uint32
				if deviceCOMFailed(deviceCOMCall(session, 3, uintptr(unsafe.Pointer(&state)))) {
					failed = true
				} else if state == 1 {
					active = true
				}
				deviceCOMCall(session, 2)
			}
		}()
	}
	if !active && failed {
		return unknownDeviceActivity("some Core Audio capture sessions could not be inspected")
	}
	return observedDeviceActivity(active, "Windows Core Audio capture sessions")
}

func registryDeviceChildren(key syscall.Handle) ([]string, error) {
	var names []string
	for i := uint32(0); ; i++ {
		var name [512]uint16
		size := uint32(len(name))
		err := syscall.RegEnumKeyEx(key, i, &name[0], &size, nil, nil, nil, nil)
		if err == syscall.Errno(259) {
			return names, nil
		}
		if err != nil {
			return nil, err
		}
		names = append(names, syscall.UTF16ToString(name[:size]))
	}
}

func registryCaptureUsage(key syscall.Handle, depth int) (bool, bool) {
	if depth > 4 {
		return false, false
	}
	read := func(name string) (uint64, bool) {
		ptr, _ := syscall.UTF16PtrFromString(name)
		var value uint64
		size := uint32(8)
		var kind uint32
		err := syscall.RegQueryValueEx(key, ptr, nil, &kind, (*byte)(unsafe.Pointer(&value)), &size)
		return value, err == nil && kind == 11 && size == 8
	}
	start, hasStart := read("LastUsedTimeStart")
	stop, hasStop := read("LastUsedTimeStop")
	if hasStart && hasStop && start > 0 && stop == 0 {
		return true, true
	}
	names, err := registryDeviceChildren(key)
	complete := err == nil
	for _, name := range names {
		ptr, _ := syscall.UTF16PtrFromString(name)
		var child syscall.Handle
		if syscall.RegOpenKeyEx(key, ptr, 0, syscall.KEY_READ, &child) != nil {
			complete = false
			continue
		}
		active, ok := registryCaptureUsage(child, depth+1)
		syscall.RegCloseKey(child)
		if active {
			return true, true
		}
		complete = complete && ok
	}
	return false, complete
}

func windowsPrivacyActivity(capability string) DeviceActivity {
	names, err := registryDeviceChildren(syscall.HKEY_USERS)
	if err != nil {
		return unknownDeviceActivity("Windows " + capability + " privacy activity is unreadable")
	}
	checked, complete := false, true
	for _, name := range names {
		if !strings.HasPrefix(name, "S-1-5-21-") && !strings.HasPrefix(name, "S-1-12-1-") {
			continue
		}
		if strings.HasSuffix(name, "_Classes") {
			continue
		}
		path, _ := syscall.UTF16PtrFromString(name + `\Software\Microsoft\Windows\CurrentVersion\CapabilityAccessManager\ConsentStore\` + capability)
		var key syscall.Handle
		if e := syscall.RegOpenKeyEx(syscall.HKEY_USERS, path, 0, syscall.KEY_READ, &key); e != nil {
			complete = false
			continue
		}
		checked = true
		active, ok := registryCaptureUsage(key, 0)
		syscall.RegCloseKey(key)
		if active {
			return observedDeviceActivity(true, "Windows privacy activity (best-effort; loaded user profiles)")
		}
		complete = complete && ok
	}
	if !checked || !complete {
		return unknownDeviceActivity(capability + " privacy activity unavailable for some loaded users")
	}
	return observedDeviceActivity(false, "Windows privacy activity (best-effort; loaded user profiles)")
}

func (windowsDeviceBackend) Activity(ctx context.Context) (DeviceActivity, DeviceActivity) {
	if err := ctx.Err(); err != nil {
		a := unknownDeviceActivity(err.Error())
		return a, a
	}
	return mergeWindowsMicActivity(windowsMicActivity(), windowsPrivacyActivity("microphone")), windowsPrivacyActivity("webcam")
}

// Core Audio enumeration alone can miss capture in an interactive user session
// when the agent runs as a service. Privacy records provide independent evidence
// across loaded profiles. An active reading always wins over idle or unknown.
func mergeWindowsMicActivity(core, privacy DeviceActivity) DeviceActivity {
	if core.State == "active" {
		return core
	}
	if privacy.State == "active" {
		return privacy
	}
	coverage := "Windows Core Audio capture sessions and privacy activity (best-effort; loaded user profiles)"
	if core.State == "idle" && privacy.State == "idle" {
		return observedDeviceActivity(false, coverage)
	}
	result := unknownDeviceActivity(strings.Join(nonemptyDeviceErrors(core.Error, privacy.Error), "; "))
	result.Coverage = coverage
	return result
}

func nonemptyDeviceErrors(values ...string) []string {
	var result []string
	for _, value := range values {
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}
