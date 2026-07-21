package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// The binary swap + rollback contract, shared by the two independent
// implementations of it:
//
//   - this file, used by the agent's detached --apply-update helper, and
//   - install-windows.ps1's Invoke-VerifiedSwap.
//
// They cannot literally share code (the PowerShell engine must work without the
// agent binary), so the sequence is kept identical on purpose:
//
//  1. Stop the service — this is what releases the lock on the live exe.
//  2. Rename the live exe to <exe>.old.exe (the rollback copy).
//  3. Copy the verified exe into the live path.
//  4. Start the service.
//  5. Poll /readyz; on ready, delete the rollback copy and the staged file.
//  6. On any failure, stop the service, restore the rollback copy, start again.
//
// The orchestration lives here — free of sc.exe and of any Windows build tag —
// so the ordering (and especially the rollback paths) is unit-testable on every
// platform with fake service ops. The platform-specific side effects are
// injected via updateSwapOps.

// updateSwapOps are the service-lifecycle side effects the swap needs. The real
// implementation shells out to sc.exe (see update_apply_windows.go); tests
// substitute fakes so every branch — including the rollback paths that only
// trigger when a bad binary is installed — is exercised without a real service.
type updateSwapOps struct {
	// StopAndWait must block until the service has actually reached STOPPED.
	// `sc stop` is asynchronous, and it is the *wait*, not the call, that
	// releases the exe lock and makes the rename below possible.
	StopAndWait func(name string) error
	// Start launches the service.
	Start func(name string) error
	// WaitReady polls the agent's /readyz until it answers or the timeout
	// expires.
	WaitReady func(url string, timeout time.Duration) error
	// RemoveBackup deletes the rollback copy after a successful swap. It is a
	// separate hook because a plain os.Remove cannot do it on Windows: the
	// helper is spawned from the live exe, renames that same path to
	// <exe>.old.exe (allowed while running), and is therefore holding the
	// backup as its own loaded image — the delete fails with a sharing
	// violation every time. The Windows implementation falls back to scheduling
	// the delete for the next reboot. Optional; nil means plain os.Remove.
	RemoveBackup func(path string) error
}

// updateBackupSuffix is the suffix on the rollback copy of the live exe. The
// apply helper restores it when the new binary fails readiness; a successful
// swap deletes it.
const updateBackupSuffix = ".old.exe"

// applyVerifiedSwap performs the swap described above. current is the live exe
// path, verified is the already-downloaded-and-checksum-verified replacement,
// and readyURL is polled after the restart to decide success.
//
// On a successful return the new binary is installed and serving. On error the
// previous binary has been restored and restarted whenever that was possible;
// the error text distinguishes "rolled back" from "rollback also failed" so the
// caller never reports a rollback that did not happen.
func applyVerifiedSwap(svcName, current, verified, readyURL string, readyTimeout time.Duration, ops updateSwapOps) error {
	if ops.StopAndWait == nil || ops.Start == nil || ops.WaitReady == nil {
		return errors.New("update swap: incomplete service ops")
	}
	backup := current + updateBackupSuffix

	if err := ops.StopAndWait(svcName); err != nil {
		return fmt.Errorf("stop service: %w", err)
	}

	// Rename onto an existing file fails on Windows, so clear any stale backup
	// left behind by an earlier interrupted run before moving the live exe.
	if err := os.Remove(backup); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale backup %s: %w", backup, err)
	}
	if err := os.Rename(current, backup); err != nil {
		// Nothing has been modified yet; put the service back the way we found it.
		_ = ops.Start(svcName)
		return fmt.Errorf("back up live exe: %w", err)
	}

	if err := copyFileContents(verified, current); err != nil {
		return rollback(svcName, current, backup, ops, fmt.Errorf("install verified exe: %w", err))
	}

	if err := ops.Start(svcName); err != nil {
		return rollback(svcName, current, backup, ops, fmt.Errorf("start service after swap: %w", err))
	}

	if err := ops.WaitReady(readyURL, readyTimeout); err != nil {
		return rollback(svcName, current, backup, ops, fmt.Errorf("new binary did not become ready: %w", err))
	}

	// Success: drop the rollback copy and the staged download. Both are
	// best-effort — a leftover file is untidy, not a failed update. The backup
	// goes through RemoveBackup because on Windows it is the running helper's
	// own image and cannot be unlinked until the process (or the OS) lets go.
	removeBackup := ops.RemoveBackup
	if removeBackup == nil {
		removeBackup = os.Remove
	}
	_ = removeBackup(backup)
	_ = os.Remove(verified)
	return nil
}

// rollback restores the previous binary after cause made the new one
// unusable. It stops the service first: the most likely failure is a binary
// that starts but never becomes ready, which means the service is RUNNING and
// Windows will refuse to replace a running exe. Skipping the stop is how a
// rollback silently does nothing while still reporting success.
func rollback(svcName, current, backup string, ops updateSwapOps, cause error) error {
	if stopErr := ops.StopAndWait(svcName); stopErr != nil {
		return fmt.Errorf("%w; rollback failed: could not stop the service to restore %s: %v", cause, backup, stopErr)
	}
	if err := os.Remove(current); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("%w; rollback failed: could not remove the new exe: %v (previous binary kept at %s)", cause, err, backup)
	}
	if err := os.Rename(backup, current); err != nil {
		return fmt.Errorf("%w; rollback failed: could not restore %s: %v", cause, backup, err)
	}
	if err := ops.Start(svcName); err != nil {
		return fmt.Errorf("%w; rolled back to the previous binary, but restarting it failed: %v", cause, err)
	}
	return fmt.Errorf("rolled back to the previous binary: %w", cause)
}

// copyFileContents copies src over dst, truncating dst. We copy rather than
// rename so a failure part-way through leaves the verified staged file intact
// for inspection (and the caller rolls back from the backup copy).
func copyFileContents(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
