//go:build linux

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// linuxHostPINDisplay is a desktop notification. Only the desktop's
// systemctl --user agent has a session bus; a system unit (a server) has none
// and only logs the -pair command.
type linuxHostPINDisplay struct{}

func newHostPINDisplay() hostPINDisplay { return linuxHostPINDisplay{} }

func (linuxHostPINDisplay) Ready() (bool, string) {
	if _, err := exec.LookPath("notify-send"); err != nil {
		return false, "notify-send is not installed"
	}
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") != "" {
		return true, ""
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		if _, err := os.Stat(filepath.Join(dir, "bus")); err == nil {
			return true, ""
		}
	}
	return false, "no desktop session; a system service has none"
}

// Show keeps the notification up until it is closed (critical urgency):
// a normal one goes to the tray after a few seconds on most desktops.
func (linuxHostPINDisplay) Show(title, body string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "notify-send", "-a", "Sysmon Agent", "-u", "critical", "-i", "dialog-password", title, body)
	_, err := trackedCombinedOutput(cmd)
	return err
}
