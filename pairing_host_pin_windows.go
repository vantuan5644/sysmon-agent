//go:build windows

package main

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"unsafe"
)

var (
	procWTSQuerySessionInformationW = wtsapi32.NewProc("WTSQuerySessionInformationW")
	procWTSFreeMemory               = wtsapi32.NewProc("WTSFreeMemory")
)

const (
	wtsUserName       = 5
	mbIconInformation = 0x00000040
	// hostPINBoxSeconds closes the box when the pairing window closes.
	hostPINBoxSeconds = 600
)

// windowsHostPINDisplay is a message box on the console session, sent from
// the session-0 service the same way the GPU guard's alarm is.
type windowsHostPINDisplay struct{}

func newHostPINDisplay() hostPINDisplay { return windowsHostPINDisplay{} }

func (windowsHostPINDisplay) Ready() (bool, string) {
	session, _, _ := procWTSGetActiveConsoleSessionId.Call()
	if uint32(session) == noConsoleUser {
		return false, "no console session"
	}
	if consoleSessionUser(uint32(session)) == "" {
		return false, "nobody is signed in at the console"
	}
	return true, ""
}

func (windowsHostPINDisplay) Show(title, body string) error {
	session, _, _ := procWTSGetActiveConsoleSessionId.Call()
	if uint32(session) == noConsoleUser {
		return errors.New("no console session")
	}
	titlePtr, err := syscall.UTF16FromString(title)
	if err != nil {
		return err
	}
	bodyPtr, err := syscall.UTF16FromString(strings.ReplaceAll(body, "\n", "\r\n"))
	if err != nil {
		return err
	}
	var response uint32
	ok, _, callErr := procWTSSendMessageW.Call(0, session,
		uintptr(unsafe.Pointer(&titlePtr[0])), uintptr((len(titlePtr)-1)*2),
		uintptr(unsafe.Pointer(&bodyPtr[0])), uintptr((len(bodyPtr)-1)*2),
		mbIconInformation|mbSetForeground|mbTopmost, hostPINBoxSeconds, uintptr(unsafe.Pointer(&response)), 0)
	if ok == 0 {
		return fmt.Errorf("WTSSendMessageW: %v", callErr)
	}
	return nil
}

// consoleSessionUser is the user signed in to the session, or "".
func consoleSessionUser(session uint32) string {
	var buf *uint16
	var size uint32
	ok, _, _ := procWTSQuerySessionInformationW.Call(0, uintptr(session), wtsUserName,
		uintptr(unsafe.Pointer(&buf)), uintptr(unsafe.Pointer(&size)))
	if ok == 0 || buf == nil {
		return ""
	}
	defer procWTSFreeMemory.Call(uintptr(unsafe.Pointer(buf)))
	return syscall.UTF16ToString(unsafe.Slice(buf, size/2))
}
