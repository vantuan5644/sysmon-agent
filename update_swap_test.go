package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeService models just enough of a Windows service for the swap tests: it
// tracks running/stopped and records the call order. Windows refuses to replace
// the exe of a running service (ERROR_SHARING_VIOLATION), which is not
// reproducible on a Unix CI run — so the tests assert on the *call order*
// instead: a rollback that never stops the service is a rollback that cannot
// work on the real thing, whatever the filesystem lets us get away with here.
type fakeService struct {
	name     string
	running  bool
	calls    []string
	startErr error
	readyErr error
	stopErr  error
}

func (f *fakeService) ops() updateSwapOps {
	return updateSwapOps{
		StopAndWait: func(name string) error {
			f.calls = append(f.calls, "stop:"+name)
			if f.stopErr != nil {
				return f.stopErr
			}
			f.running = false
			return nil
		},
		Start: func(name string) error {
			f.calls = append(f.calls, "start:"+name)
			if f.startErr != nil {
				return f.startErr
			}
			f.running = true
			return nil
		},
		WaitReady: func(url string, timeout time.Duration) error {
			f.calls = append(f.calls, "ready:"+url)
			return f.readyErr
		},
	}
}

func writeExe(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func swapFixture(t *testing.T) (live, verified string, svc *fakeService) {
	t.Helper()
	dir := t.TempDir()
	live = filepath.Join(dir, "sysmon-agent.exe")
	verified = filepath.Join(dir, "sysmon-agent.update-v9.9.9.exe")
	writeExe(t, live, "OLD BINARY")
	writeExe(t, verified, "NEW BINARY")
	return live, verified, &fakeService{name: "SysmonAgent", running: true}
}

func TestApplyVerifiedSwapHappyPath(t *testing.T) {
	live, verified, svc := swapFixture(t)

	if err := applyVerifiedSwap(svc.name, live, verified, "http://127.0.0.1:9099/readyz", time.Second, svc.ops()); err != nil {
		t.Fatalf("applyVerifiedSwap returned %v, want nil", err)
	}

	if got := readFile(t, live); got != "NEW BINARY" {
		t.Errorf("live exe = %q, want the new binary", got)
	}
	if _, err := os.Stat(live + updateBackupSuffix); !os.IsNotExist(err) {
		t.Errorf("rollback copy should be deleted after a successful swap, stat err = %v", err)
	}
	if _, err := os.Stat(verified); !os.IsNotExist(err) {
		t.Errorf("staged verified file should be deleted after a successful swap, stat err = %v", err)
	}
	if !svc.running {
		t.Error("service should be running after a successful swap")
	}
	wantOrder := "stop:SysmonAgent,start:SysmonAgent,ready:http://127.0.0.1:9099/readyz"
	if got := strings.Join(svc.calls, ","); got != wantOrder {
		t.Errorf("call order = %q, want %q", got, wantOrder)
	}
}

// TestApplyVerifiedSwapUsesRemoveBackupHook guards a defect found only by
// running the real thing on a live host: the helper is spawned from the live
// exe and renames that path to <exe>.old.exe, so it holds the backup as its own
// loaded image and a plain os.Remove can never delete it. Every successful
// update silently left a ~12 MB orphan behind. The cleanup must go through the
// RemoveBackup hook (Windows: delete, else schedule for reboot).
func TestApplyVerifiedSwapUsesRemoveBackupHook(t *testing.T) {
	live, verified, svc := swapFixture(t)
	ops := svc.ops()
	removed := ""
	ops.RemoveBackup = func(path string) error {
		removed = path
		return os.Remove(path)
	}

	if err := applyVerifiedSwap(svc.name, live, verified, "http://127.0.0.1:9099/readyz", time.Second, ops); err != nil {
		t.Fatalf("applyVerifiedSwap returned %v, want nil", err)
	}
	if want := live + updateBackupSuffix; removed != want {
		t.Errorf("RemoveBackup called with %q, want %q — the success path must not bypass the hook", removed, want)
	}
}

// A backup that cannot be deleted (locked, as it always is on Windows) must not
// fail the update: the swap already succeeded and the new binary is serving.
func TestApplyVerifiedSwapSucceedsWhenBackupCannotBeRemoved(t *testing.T) {
	live, verified, svc := swapFixture(t)
	ops := svc.ops()
	ops.RemoveBackup = func(string) error { return errors.New("in use by another process") }

	if err := applyVerifiedSwap(svc.name, live, verified, "http://127.0.0.1:9099/readyz", time.Second, ops); err != nil {
		t.Fatalf("applyVerifiedSwap returned %v, want nil (a stuck backup is untidy, not a failure)", err)
	}
	if got := readFile(t, live); got != "NEW BINARY" {
		t.Errorf("live exe = %q, want the new binary", got)
	}
}

// TestApplyVerifiedSwapRollsBackWhenNewBinaryNeverBecomesReady is the
// regression test for the original bug: the rollback removed the new exe
// without stopping the service first, so on Windows the remove/rename failed
// and the helper reported "rolled back" while the bad binary kept running.
func TestApplyVerifiedSwapRollsBackWhenNewBinaryNeverBecomesReady(t *testing.T) {
	live, verified, svc := swapFixture(t)
	svc.readyErr = errors.New("readyz never returned 200")

	err := applyVerifiedSwap(svc.name, live, verified, "http://127.0.0.1:9099/readyz", time.Second, svc.ops())
	if err == nil {
		t.Fatal("applyVerifiedSwap returned nil, want a rollback error")
	}
	if !strings.Contains(err.Error(), "rolled back") {
		t.Errorf("error = %v, want it to report a rollback", err)
	}

	if got := readFile(t, live); got != "OLD BINARY" {
		t.Errorf("live exe = %q, want the previous binary restored", got)
	}
	if _, statErr := os.Stat(live + updateBackupSuffix); !os.IsNotExist(statErr) {
		t.Errorf("backup should be consumed by the rollback, stat err = %v", statErr)
	}
	if !svc.running {
		t.Error("service should be running again after a rollback")
	}

	// The service must be stopped a second time before the exe is replaced —
	// this is the assertion that fails against the original implementation.
	stops := 0
	for _, call := range svc.calls {
		if strings.HasPrefix(call, "stop:") {
			stops++
		}
	}
	if stops != 2 {
		t.Errorf("stop was called %d time(s) (%v); the rollback must stop the service before replacing a running exe", stops, svc.calls)
	}
}

func TestApplyVerifiedSwapRollsBackWhenServiceFailsToStart(t *testing.T) {
	live, verified, svc := swapFixture(t)
	svc.startErr = errors.New("sc start failed: the service did not respond")

	err := applyVerifiedSwap(svc.name, live, verified, "http://127.0.0.1:9099/readyz", time.Second, svc.ops())
	if err == nil {
		t.Fatal("applyVerifiedSwap returned nil, want an error")
	}
	if got := readFile(t, live); got != "OLD BINARY" {
		t.Errorf("live exe = %q, want the previous binary restored", got)
	}
	// Start also fails during the rollback, so the error must say so rather
	// than claiming a clean rollback.
	if !strings.Contains(err.Error(), "restarting it failed") {
		t.Errorf("error = %v, want it to report that the restart failed", err)
	}
}

// A rollback that cannot stop the service must say so instead of reporting
// success — silently leaving a bad binary running is the failure mode this
// whole path exists to prevent.
func TestApplyVerifiedSwapReportsRollbackFailure(t *testing.T) {
	live, verified, svc := swapFixture(t)
	svc.readyErr = errors.New("readyz never returned 200")

	stopCalls := 0
	ops := svc.ops()
	realStop := ops.StopAndWait
	ops.StopAndWait = func(name string) error {
		stopCalls++
		if stopCalls > 1 {
			return errors.New("sc stop timed out")
		}
		return realStop(name)
	}

	err := applyVerifiedSwap(svc.name, live, verified, "http://127.0.0.1:9099/readyz", time.Second, ops)
	if err == nil {
		t.Fatal("applyVerifiedSwap returned nil, want an error")
	}
	if !strings.Contains(err.Error(), "rollback failed") {
		t.Errorf("error = %v, want it to report that the rollback failed", err)
	}
	if strings.Contains(err.Error(), "rolled back to the previous binary:") {
		t.Errorf("error = %v, must not claim a successful rollback", err)
	}
	// The previous binary must still be recoverable by hand.
	if got := readFile(t, live+updateBackupSuffix); got != "OLD BINARY" {
		t.Errorf("backup = %q, want the previous binary preserved for manual recovery", got)
	}
}

func TestApplyVerifiedSwapRollsBackWhenVerifiedFileIsMissing(t *testing.T) {
	live, verified, svc := swapFixture(t)
	if err := os.Remove(verified); err != nil {
		t.Fatalf("remove staged file: %v", err)
	}

	err := applyVerifiedSwap(svc.name, live, verified, "http://127.0.0.1:9099/readyz", time.Second, svc.ops())
	if err == nil {
		t.Fatal("applyVerifiedSwap returned nil, want an error")
	}
	if got := readFile(t, live); got != "OLD BINARY" {
		t.Errorf("live exe = %q, want the previous binary restored", got)
	}
	if !svc.running {
		t.Error("service should be running again after the rollback")
	}
}

func TestApplyVerifiedSwapAbortsWhenServiceWillNotStop(t *testing.T) {
	live, verified, svc := swapFixture(t)
	svc.stopErr = errors.New("sc stop timed out")

	err := applyVerifiedSwap(svc.name, live, verified, "http://127.0.0.1:9099/readyz", time.Second, svc.ops())
	if err == nil {
		t.Fatal("applyVerifiedSwap returned nil, want an error")
	}
	// Nothing may be touched if we could not release the exe lock.
	if got := readFile(t, live); got != "OLD BINARY" {
		t.Errorf("live exe = %q, want it untouched when the service will not stop", got)
	}
	if got := readFile(t, verified); got != "NEW BINARY" {
		t.Errorf("staged file = %q, want it preserved when the swap never started", got)
	}
}

// A stale .old.exe from an interrupted earlier run must not block the swap —
// on Windows, renaming onto an existing file fails.
func TestApplyVerifiedSwapClearsStaleBackup(t *testing.T) {
	live, verified, svc := swapFixture(t)
	writeExe(t, live+updateBackupSuffix, "STALE BACKUP")

	if err := applyVerifiedSwap(svc.name, live, verified, "http://127.0.0.1:9099/readyz", time.Second, svc.ops()); err != nil {
		t.Fatalf("applyVerifiedSwap returned %v, want nil", err)
	}
	if got := readFile(t, live); got != "NEW BINARY" {
		t.Errorf("live exe = %q, want the new binary", got)
	}
}

func TestApplyVerifiedSwapRequiresCompleteOps(t *testing.T) {
	live, verified, svc := swapFixture(t)
	if err := applyVerifiedSwap(svc.name, live, verified, "http://x/readyz", time.Second, updateSwapOps{}); err == nil {
		t.Fatal("applyVerifiedSwap with empty ops returned nil, want an error")
	}
}
