//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
)

// updateApplySpawnTimeout is declared in update.go (cross-platform) and is
// not redeclared here on non-Windows. spawnApplyHelper below is the no-op stub.

// updatePlatformSelfUpdateSupported reports whether the in-dashboard
// self-update is supported on this host. Non-Windows hosts always return false
// — the published artifact is the Windows installer, and Linux self-update
// stays on systemd / package managers for v1 (see auto-update.md Non-goals).
func updatePlatformSelfUpdateSupported() bool {
	return false
}

// spawnApplyHelper is the no-op stub for non-Windows. ApplyNow gates on
// selfUpdateSupported first and returns errUpdateUnsupported before reaching
// here, but the stub keeps the build green.
func spawnApplyHelper(ctx context.Context, tag, verifiedExe string) error {
	_ = ctx
	_ = tag
	_ = verifiedExe
	return errors.New("apply-update helper is Windows-only")
}

// runApplyUpdate is the no-op stub for non-Windows. main.go routes the
// --apply-update subcommand here; on non-Windows it is unreachable (the
// detached helper is spawned only by the Windows-side spawnApplyHelper), and
// the standalone -apply-update invocation prints a clear error and exits.
func runApplyUpdate(tag, verifiedExe, readyURL, svcName string) error {
	_ = verifiedExe
	_ = readyURL
	_ = svcName
	return fmt.Errorf("apply-update %q not supported on this platform (Windows-only)", tag)
}

// resolvedServiceName mirrors the Windows helper so cross-platform callers do
// not need a build tag. There is no SCM off Windows, so the compile-time
// constant is always the answer.
func resolvedServiceName() string {
	return serviceName
}
