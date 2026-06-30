//go:build !windows

package main

import "fmt"

// emitControlInput is a no-op stub on non-Windows platforms. The -control-emit
// flag exists only to support the Windows host-control bridge's native
// active-session injection (see control_emit_windows.go); other platforms apply
// media/lock controls directly in their own collectors/controllers.
func emitControlInput(action string) error {
	return fmt.Errorf("control-emit %q not supported on this platform", action)
}
