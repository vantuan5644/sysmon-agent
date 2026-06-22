//go:build !linux && !windows

package main

import "context"

type unsupportedController struct{}

// NewSystemController returns a controller that reports every host control as
// unavailable. Used on platforms without a native control implementation.
func NewSystemController() SystemController { return unsupportedController{} }

func (unsupportedController) Capabilities() []ControlCapability {
	return capabilitiesFor(func(ControlAction) bool { return false })
}

func (unsupportedController) Apply(_ context.Context, action ControlAction) ControlResult {
	return unavailableControlResult(action, "host controls are not supported on this platform")
}
