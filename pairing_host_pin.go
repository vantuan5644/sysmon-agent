package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// A PIN on the host's own screen, once.
//
// Pairing is required by default. An agent that requires it only by default
// (no -pairing flag and no SYSMON_PAIRING) was either upgraded from a release
// older than pairing, whose installer wrote no -pairing (the dashboard's
// Update button keeps the service's command line), or installed on Linux from
// a unit that leaves the setting out. Either way no device is paired, every
// dashboard now asks for a PIN, and nobody was told where to get one. So on
// the first start like that, the agent opens a pairing window and shows its
// PIN on the host's screen: a message box on the Windows console, a desktop
// notification from a Linux user unit. A host with no screen to show it on
// (a system unit, nobody signed in) only logs the -pair command.
//
// The marker file beside the paired devices keeps it to once: after that the
// usual ways to get a PIN apply (-pair, Pair a device in the Start menu, a
// dashboard already paired).

const hostPINMarkerName = "host-pin.json"

// hostPINDisplay shows a PIN on the host itself.
type hostPINDisplay interface {
	// Ready reports whether there is a screen to show it on, and why not.
	Ready() (bool, string)
	Show(title, body string) error
}

type hostPINInput struct {
	Store *pairingStore
	Dir   string
	// FromDefault is true when pairing is required only because that is the
	// default.
	FromDefault bool
	// Command is the -pair command for a PIN later, as the host should run it.
	Command string
	Display hostPINDisplay
	Logf    func(string, ...any)
}

// maybeShowHostPIN shows a PIN when the agent requires pairing by default,
// keeps its pairings in a folder, has none and has not shown one before. It
// reports whether it showed one.
func maybeShowHostPIN(in hostPINInput) bool {
	logf := in.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if !in.FromDefault || in.Store == nil || in.Dir == "" || in.Display == nil {
		return false
	}
	if len(in.Store.Devices()) > 0 || in.Store.WindowOpen() {
		return false
	}
	if _, err := os.Lstat(filepath.Join(in.Dir, hostPINMarkerName)); err == nil {
		return false
	}
	if ok, why := in.Display.Ready(); !ok {
		logf("pairing: required and no device is paired yet; there is no screen here to show a PIN on (%s). For a PIN, run: %s", why, in.Command)
		return false
	}
	pin, expires, err := in.Store.OpenWindow()
	if err != nil {
		logf("pairing: could not open a pairing window to show on this host: %v", err)
		return false
	}
	until := expires.Local().Format("15:04")
	if err := in.Display.Show("Sysmon Agent: pair a device", hostPINBody(pin, until, in.Command)); err != nil {
		logf("pairing: could not show a PIN on this host's screen: %v. For a PIN, run: %s", err, in.Command)
		return false
	}
	marker, _ := json.Marshal(struct {
		ShownAt time.Time `json:"shown_at"`
		Until   time.Time `json:"until"`
	}{time.Now().UTC(), expires.UTC()})
	if err := writePrivateFile(in.Dir, hostPINMarkerName, append(marker, '\n')); err != nil {
		logf("pairing: showed a PIN, but could not record it (%v); it may show again on the next start", err)
	}
	// The PIN itself stays out of the log.
	logf("pairing: showed a pairing PIN on this host's screen, valid until %s", until)
	return true
}

func hostPINBody(pin, until, command string) string {
	return fmt.Sprintf("PIN: %s\n\nThe Sysmon Agent dashboard now asks for a PIN before it shows this host. Type this one on the device you want to pair. It works for one device until %s (%d minutes).\n\nFor another PIN later, run:\n%s",
		formatPIN(pin), until, int(pairingWindowTTL/time.Minute), command)
}

// hostPairCommand is the -pair command for this agent, quoted for the host's
// own shell.
func hostPairCommand(exe, settingsPath string) string {
	if abs, err := filepath.Abs(settingsPath); err == nil {
		settingsPath = abs
	}
	if runtime.GOOS == "windows" {
		return windowsQuoteArg(exe) + " -pair -settings " + windowsQuoteArg(settingsPath) + "   (in a terminal run as administrator)"
	}
	return posixQuoteArg(exe) + " -pair -settings " + posixQuoteArg(settingsPath) + "   (as the agent's own user)"
}

// pairingRequiredByDefault reports whether neither -pairing nor SYSMON_PAIRING
// chose the pairing mode, the same test envString applies.
func pairingRequiredByDefault() bool {
	if os.Getenv("SYSMON_PAIRING") != "" {
		return false
	}
	set := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "pairing" {
			set = true
		}
	})
	return !set
}

func windowsQuoteArg(s string) string {
	if s == "" || strings.ContainsAny(s, " \t&()^,;") {
		return `"` + s + `"`
	}
	return s
}

func posixQuoteArg(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t'\"$`\\!*?[]{}()<>|&;#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
