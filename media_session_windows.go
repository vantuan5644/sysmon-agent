//go:build windows

package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

//go:embed media-session-windows.ps1
var mediaSessionScript []byte

type mediaSessionResult struct {
	Handled bool   `json:"handled"`
	Applied bool   `json:"applied"`
	Error   string `json:"error"`
}

func applyMediaSessionResult(output []byte, sendKey func() error) error {
	var result mediaSessionResult
	if err := json.Unmarshal(bytes.TrimSpace(output), &result); err != nil {
		return fmt.Errorf("parse Windows media session result: %w", err)
	}
	if !result.Handled {
		return sendKey()
	}
	if !result.Applied {
		if result.Error == "" {
			result.Error = "current media session rejected play/pause"
		}
		return fmt.Errorf("Windows media session: %s", result.Error)
	}
	return nil
}

// The service launches a native helper into the user's session first. That
// helper starts PowerShell locally on the same desktop to access media sessions.
func emitMediaSessionControl() error {
	file, err := os.CreateTemp("", "sysmon-media-session-*.ps1")
	if err != nil {
		return fmt.Errorf("create media session script: %w", err)
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(mediaSessionScript)
	closeErr := file.Close()
	if writeErr != nil {
		return fmt.Errorf("write media session script: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close media session script: %w", closeErr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3500*time.Millisecond)
	defer cancel()
	// WinRT support here requires Windows PowerShell's .NET Framework host.
	cmd := exec.CommandContext(ctx, windowsPowerShellCommand(exec.LookPath),
		"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", file.Name())
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Stdin = strings.NewReader("")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("Windows media session helper: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return applyMediaSessionResult(output, func() error { return emitMediaKey(sendWindowsInput) })
}
