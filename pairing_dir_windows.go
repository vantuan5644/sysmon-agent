//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

// privateDirSDDL is a protected DACL that ignores the parent's entries:
// SYSTEM, Administrators and the folder's owner get full control, nobody
// else gets anything. %ProgramData%\SysmonAgent itself is readable by Users,
// which is fine for settings.json but not for token hashes or the PIN hash.
const privateDirSDDL = "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;OW)"

var (
	procConvertStringSecurityDescriptorToSecurityDescriptorW = advapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
	procLocalFree                                            = kernel32.NewProc("LocalFree")
)

// ensurePrivateDir creates dir with privateDirSDDL. A folder that already
// exists is left as it is, so an administrator's own ACL survives.
func ensurePrivateDir(dir string) error {
	if info, err := os.Stat(dir); err == nil {
		if !info.IsDir() {
			return errors.New("exists and is not a folder")
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	sddl, err := syscall.UTF16PtrFromString(privateDirSDDL)
	if err != nil {
		return err
	}
	var sd uintptr
	ok, _, callErr := procConvertStringSecurityDescriptorToSecurityDescriptorW.Call(
		uintptr(unsafe.Pointer(sddl)), 1 /* SDDL_REVISION_1 */, uintptr(unsafe.Pointer(&sd)), 0)
	if ok == 0 {
		return callErr
	}
	defer procLocalFree.Call(sd)
	path, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	sa := syscall.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(sa))
	if err := syscall.CreateDirectory(path, &sa); err != nil && !errors.Is(err, syscall.ERROR_ALREADY_EXISTS) {
		return err
	}
	return nil
}

// matchDirOwner is a no-op on Windows; the folder's inherited ACL applies.
func matchDirOwner(*os.File, string) {}
