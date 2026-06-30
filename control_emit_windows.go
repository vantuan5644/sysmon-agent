//go:build windows

package main

import (
	"fmt"
	"syscall"
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

	procKeybdEvent      = user32.NewProc("keybd_event")
	procLockWorkStation = user32.NewProc("LockWorkStation")
)

const (
	vkMediaPlayPause     = 0xB3
	keyeventfExtendedKey = 0x0001
	keyeventfKeyUp       = 0x0002
)

// emitControlInput performs a single host input in the current Windows session
// and returns. It runs in a short-lived child the control bridge launches into
// the active console session, so the keystroke / lock lands on the logged-in
// user's interactive desktop even though the agent service lives in session 0.
func emitControlInput(action string) error {
	switch action {
	case controlEmitMediaPlayPause:
		// Press then release the play/pause media key. keybd_event injects into
		// the input desktop of the session this process runs in (the user's).
		procKeybdEvent.Call(vkMediaPlayPause, 0, keyeventfExtendedKey, 0)
		procKeybdEvent.Call(vkMediaPlayPause, 0, keyeventfExtendedKey|keyeventfKeyUp, 0)
		return nil
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
