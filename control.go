package main

import (
	"context"
	"time"
)

// ControlAction identifies a host-side control that the dashboard toolbar can
// trigger. The set is intentionally small and host-fixed: the device glance is a
// remote, not a settings panel. Refresh rate and warning thresholds are
// configured on the host (CLI flags / env), not from the device.
type ControlAction string

const (
	// ControlMicMute toggles the mute state of every active capture endpoint
	// (all microphones) on the host.
	ControlMicMute ControlAction = "mic_mute"
	// ControlMediaToggle sends a play/pause media key to the active session.
	ControlMediaToggle ControlAction = "media_toggle"
	// ControlVolumeMute toggles mute on the default playback (speaker) endpoint.
	ControlVolumeMute ControlAction = "volume_mute"
	// ControlLockScreen locks the interactive workstation.
	ControlLockScreen ControlAction = "lock_screen"
)

// controlActionOrder is the canonical order controls are reported in /api/status
// and rendered in the toolbar.
var controlActionOrder = []ControlAction{
	ControlMicMute,
	ControlMediaToggle,
	ControlVolumeMute,
	ControlLockScreen,
}

var controlActionLabels = map[ControlAction]string{
	ControlMicMute:     "Mic mute",
	ControlMediaToggle: "Play/Pause",
	ControlVolumeMute:  "Speaker mute",
	ControlLockScreen:  "Lock screen",
}

func isKnownControlAction(action ControlAction) bool {
	_, ok := controlActionLabels[action]
	return ok
}

// ControlRequest is the POST /api/control request body.
type ControlRequest struct {
	Action ControlAction `json:"action"`
}

// ControlResult is returned to the dashboard after applying (or failing to
// apply) a control. Mirroring the metrics layer's graceful degradation, a
// control that cannot be applied degrades into available=false / applied=false
// with an Error rather than failing the HTTP request.
type ControlResult struct {
	Action    ControlAction `json:"action"`
	Available bool          `json:"available"`
	Applied   bool          `json:"applied"`
	State     string        `json:"state,omitempty"`
	Message   string        `json:"message,omitempty"`
	Error     string        `json:"error,omitempty"`
}

// ControlCapability advertises whether a control is supported on this host so
// the toolbar can disable buttons it cannot drive.
type ControlCapability struct {
	Action    ControlAction `json:"action"`
	Label     string        `json:"label"`
	Available bool          `json:"available"`
}

// SystemController applies host controls. Implementations are platform-specific
// (build tags), mirroring NewSystemCollector.
type SystemController interface {
	Capabilities() []ControlCapability
	Apply(ctx context.Context, action ControlAction) ControlResult
}

// controlTimeout bounds a single control invocation. The Windows bridge compiles
// inline C# on first run (Add-Type) and may launch a helper into the active
// console session, so this is generous.
const controlTimeout = 8 * time.Second

// capabilitiesFor builds the ordered capability list from a support predicate.
func capabilitiesFor(available func(ControlAction) bool) []ControlCapability {
	caps := make([]ControlCapability, 0, len(controlActionOrder))
	for _, action := range controlActionOrder {
		caps = append(caps, ControlCapability{
			Action:    action,
			Label:     controlActionLabels[action],
			Available: available(action),
		})
	}
	return caps
}

// unavailableControlResult is a helper for controllers that cannot apply a
// control on this platform/configuration.
func unavailableControlResult(action ControlAction, message string) ControlResult {
	return ControlResult{Action: action, Available: false, Applied: false, Error: message}
}
