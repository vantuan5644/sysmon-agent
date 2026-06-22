//go:build windows

package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

//go:embed control-bridge.ps1
var controlBridgeFS embed.FS

// controlBridgePath resolves to a temp copy of the embedded host-control bridge
// script, created once per process (same pattern as lhmBridgePath). Editing
// control-bridge.ps1 therefore requires a Go rebuild to take effect.
var (
	controlBridgeOnce sync.Once
	controlBridgeFile string
	controlBridgeErr  error
)

func controlBridgePath() (string, error) {
	controlBridgeOnce.Do(func() {
		src, err := controlBridgeFS.ReadFile("control-bridge.ps1")
		if err != nil {
			controlBridgeErr = fmt.Errorf("read embedded control-bridge.ps1: %w", err)
			return
		}
		controlBridgeFile = filepath.Join(os.TempDir(), "sysmon-control-bridge.ps1")
		if err := os.WriteFile(controlBridgeFile, src, 0o644); err != nil {
			controlBridgeErr = fmt.Errorf("write control-bridge temp copy: %w", err)
			return
		}
	})
	return controlBridgeFile, controlBridgeErr
}

type windowsController struct{}

// NewSystemController returns the Windows host controller. Mic and speaker mute
// use Core Audio endpoint properties, which work from the LocalSystem service
// session. Media play/pause and lock screen are injected into the active console
// session via CreateProcessAsUser when the agent runs non-interactively.
func NewSystemController() SystemController { return windowsController{} }

// windowsControlActionArg maps a public action to the bridge -Action argument.
func windowsControlActionArg(action ControlAction) (string, bool) {
	switch action {
	case ControlMicMute:
		return "mic_mute", true
	case ControlVolumeMute:
		return "volume_mute", true
	case ControlMediaToggle:
		return "media_toggle", true
	case ControlLockScreen:
		return "lock_screen", true
	default:
		return "", false
	}
}

func (windowsController) Capabilities() []ControlCapability {
	return capabilitiesFor(func(action ControlAction) bool {
		_, ok := windowsControlActionArg(action)
		return ok
	})
}

type windowsControlBridgeResult struct {
	Action    string `json:"action"`
	Available bool   `json:"available"`
	Applied   bool   `json:"applied"`
	State     string `json:"state"`
	Message   string `json:"message"`
	Error     string `json:"error"`
}

func (windowsController) Apply(ctx context.Context, action ControlAction) ControlResult {
	arg, ok := windowsControlActionArg(action)
	if !ok {
		return unavailableControlResult(action, "unknown control action")
	}
	scriptPath, err := controlBridgePath()
	if err != nil {
		return unavailableControlResult(action, err.Error())
	}

	cmd := exec.CommandContext(
		ctx,
		windowsPowerShellCommand(exec.LookPath),
		"-NoLogo",
		"-NoProfile",
		"-NonInteractive",
		"-ExecutionPolicy",
		"Bypass",
		"-File",
		scriptPath,
		"-Action",
		arg,
	)
	// An empty, immediately-EOF stdin keeps the -File host from blocking on a
	// null device when the agent runs without a console (service session).
	cmd.Stdin = strings.NewReader("")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" && len(stdout) > 0 {
			detail = strings.TrimSpace(string(stdout))
		}
		if detail == "" {
			detail = err.Error()
		}
		return ControlResult{Action: action, Available: true, Applied: false, Error: "control bridge failed: " + detail}
	}

	var parsed windowsControlBridgeResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout), &parsed); err != nil {
		return ControlResult{Action: action, Available: true, Applied: false, Error: "parse control bridge output: " + err.Error()}
	}
	return ControlResult{
		Action:    action,
		Available: parsed.Available,
		Applied:   parsed.Applied,
		State:     parsed.State,
		Message:   parsed.Message,
		Error:     parsed.Error,
	}
}
