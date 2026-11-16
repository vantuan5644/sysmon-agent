//go:build !linux && !windows

package main

import (
	"context"
	"errors"
	"io"
)

// Self-update covers the Windows service and Linux. Elsewhere (macOS) the
// dashboard shows the release link and the binary is replaced by hand.

func updatePlatformMode() string { return updateModeNone }

func updateUnsupportedReason() string {
	return "self-update covers the Windows service and Linux; on this OS replace the binary with the new release by hand"
}

func updateCommandHint() string { return "" }

func requestRootUpdate(context.Context, string) error {
	return errors.New("the root updater is Linux-only")
}

func lastUpdaterResult() *updaterResult { return nil }

func runRootUpdateCommand(io.Writer) error {
	return errors.New("-update is for Linux; on this OS replace the binary with the new release by hand")
}

func runUpdaterSetup(string, io.Writer) error {
	return errors.New("-update-setup is for Linux")
}
