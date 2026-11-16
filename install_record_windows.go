//go:build windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

// installUninstallKey is the release installer's entry (dist/installer.nsi,
// UNINSTKEY). The installer is 32-bit, so it lands in the 32-bit view.
const installUninstallKey = `Software\Microsoft\Windows\CurrentVersion\Uninstall\Sysmon`

var procRegSetValueExW = advapi32.NewProc("RegSetValueExW")

const (
	keyWow6432 = 0x0200 // KEY_WOW64_32KEY
	keyWow6464 = 0x0100 // KEY_WOW64_64KEY
)

// syncInstalledVersionRecord brings the uninstall entry's DisplayVersion to
// this binary's version. Opening the key for writing needs an administrator
// (the service is LocalSystem); a console run without one skips it quietly.
func syncInstalledVersionRecord(logf func(string, ...any)) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	keyName, err := syscall.UTF16PtrFromString(installUninstallKey)
	if err != nil {
		return
	}
	for _, view := range []uint32{keyWow6432, keyWow6464} {
		var key syscall.Handle
		if syscall.RegOpenKeyEx(syscall.HKEY_LOCAL_MACHINE, keyName, 0, syscall.KEY_QUERY_VALUE|syscall.KEY_SET_VALUE|view, &key) != nil {
			continue
		}
		location := readRegString(key, "InstallLocation")
		current := readRegString(key, "DisplayVersion")
		next, ok := installRecordUpdate(version, filepath.Dir(exe), location, current)
		if ok {
			if err := writeRegString(key, "DisplayVersion", next); err != nil {
				logf("installed apps: could not record version %s: %v", next, err)
			} else {
				logf("installed apps: version record %s -> %s", current, next)
			}
		}
		syscall.RegCloseKey(key)
		if location != "" {
			return
		}
	}
}

func readRegString(key syscall.Handle, name string) string {
	namePtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return ""
	}
	var valueType, size uint32
	if syscall.RegQueryValueEx(key, namePtr, nil, &valueType, nil, &size) != nil || size == 0 || size > 64<<10 {
		return ""
	}
	if valueType != syscall.REG_SZ && valueType != syscall.REG_EXPAND_SZ {
		return ""
	}
	buf := make([]uint16, (size+1)/2+1)
	size = uint32(len(buf) * 2)
	if syscall.RegQueryValueEx(key, namePtr, nil, &valueType, (*byte)(unsafe.Pointer(&buf[0])), &size) != nil {
		return ""
	}
	return syscall.UTF16ToString(buf)
}

func writeRegString(key syscall.Handle, name, value string) error {
	namePtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	data, err := syscall.UTF16FromString(value)
	if err != nil {
		return err
	}
	ret, _, _ := procRegSetValueExW.Call(uintptr(key), uintptr(unsafe.Pointer(namePtr)), 0, syscall.REG_SZ,
		uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)*2))
	if ret != 0 {
		return syscall.Errno(ret)
	}
	return nil
}
