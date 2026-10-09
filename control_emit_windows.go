//go:build windows

package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

// Hidden -control-emit actions. The Windows host-control bridge, running as the
// session-0 LocalSystem service, injects "sysmon-agent.exe -control-emit <action>"
// into the active console session via CreateProcessAsUser. A native PE launches
// and runs reliably across that session boundary; powershell.exe does not (it is
// created but dies in early CLR/console init before executing), which is why the
// previous powershell -EncodedCommand media path silently did nothing. Routing
// the input through the agent's own native binary fixes that.
const (
	controlEmitMediaPlayPause = "media_play_pause"
	controlEmitLockScreen     = "lock_screen"
)

var (
	user32 = syscall.NewLazyDLL("user32.dll")

	procSendInput       = user32.NewProc("SendInput")
	procLockWorkStation = user32.NewProc("LockWorkStation")
)

const (
	vkMediaPlayPause     = 0xB3
	mediaPlayPauseScan   = 0x22
	keyeventfExtendedKey = 0x0001
	keyeventfKeyUp       = 0x0002
)

type windowsKeyboardInput struct {
	VirtualKey uint16
	ScanCode   uint16
	Flags      uint32
	Time       uint32
	ExtraInfo  uintptr
}

// INPUT's union is sized for MOUSEINPUT, which is eight bytes larger than
// KEYBDINPUT on both 32-bit and 64-bit Windows. uintptr provides its alignment.
type windowsInput struct {
	Type     uint32
	Keyboard windowsKeyboardInput
	Padding  [8]byte
}

func sendWindowsInput(inputs []windowsInput) (uint32, error) {
	sent, _, err := procSendInput.Call(uintptr(len(inputs)), uintptr(unsafe.Pointer(&inputs[0])), unsafe.Sizeof(inputs[0]))
	return uint32(sent), err
}

func emitMediaKey(send func([]windowsInput) (uint32, error)) error {
	down := windowsInput{Type: 1, Keyboard: windowsKeyboardInput{
		VirtualKey: vkMediaPlayPause, ScanCode: mediaPlayPauseScan, Flags: keyeventfExtendedKey,
	}}
	up := down
	up.Keyboard.Flags |= keyeventfKeyUp
	sent, err := send([]windowsInput{down, up})
	if sent == 2 {
		return nil
	}
	if sent == 1 {
		// Release a partially delivered press so a failed toggle cannot leave
		// the media key held down. Never retry the whole toggle.
		_, _ = send([]windowsInput{up})
	}
	return fmt.Errorf("SendInput accepted %d of 2 media key events (desktop may be locked or input blocked): %v", sent, err)
}

// emitControlInput applies one media or lock action in the current Windows
// session. The service launches this short-lived child on the user's desktop;
// media session access and legacy keyboard input therefore run as that user.
func emitControlInput(action string) error {
	switch action {
	case controlEmitMediaPlayPause:
		if startedByServiceControlManager() {
			return fmt.Errorf("media input requires the logged-in user's desktop, not service session 0")
		}
		return emitMediaSessionControl()
	case controlEmitLockScreen:
		// LockWorkStation locks the session it is called from.
		r, _, err := procLockWorkStation.Call()
		if r == 0 {
			return fmt.Errorf("LockWorkStation failed: %v", err)
		}
		return nil
	default:
		return fmt.Errorf("unknown control-emit action %q", action)
	}
}
