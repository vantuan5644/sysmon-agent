//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// Windows process creation flags used when spawning the detached apply-update
// helper. The helper must outlive the agent process (stopping the service kills
// the agent) and must not flash a console window:
//   - CREATE_NEW_PROCESS_GROUP: the helper does not inherit the agent's CTRL
//     event group, so the SCM's STOP signal to the agent does not propagate.
//   - DETACHED_PROCESS: the helper runs without a console (no window flash).
//   - CREATE_BREAKAWAY_FROM_JOB: if the service is in a Job object, the helper
//     is not killed when the job (and therefore the agent) is torn down.
const (
	updateHelperCreateNewProcessGroup = 0x00000200
	updateHelperDetachedProcess       = 0x00000008
	updateHelperBreakawayFromJob      = 0x01000000
)

// updateApplyReadyPollTimeout bounds the helper's post-restart /readyz wait.
// Generous enough to absorb a cold LibreHardwareMonitor driver load on the
// first restart sample; short enough that a genuinely broken binary rolls back
// promptly.
const updateApplyReadyPollTimeout = 60 * time.Second

// updateApplyReadyPollInterval is the cadence at which the helper polls
// /readyz after restarting the service.
const updateApplyReadyPollInterval = 1500 * time.Millisecond

// updateServiceStopTimeout bounds how long the helper waits for sc.exe stop to
// actually reach STOPPED. sc stop is asynchronous; the wait (not the call) is
// what releases the exe lock so the rename-swap can succeed.
const updateServiceStopTimeout = 30 * time.Second

// updatePlatformSelfUpdateSupported confines the in-dashboard self-update to
// the Windows service. The check is "running as the service" (session-0 + SCM
// handshake), not merely "on Windows", so a console run on Windows still
// reports 501 and the user is pointed at install-windows.ps1.
func updatePlatformSelfUpdateSupported() bool {
	return runningAsWindowsService()
}

// spawnApplyHelper resolves the agent's own exe + service port and launches the
// detached `sysmon-agent.exe --apply-update <tag> <verified-exe> <ready-url>`
// helper. The helper performs the swap+rollback (see runApplyUpdate); the agent
// returns 202 immediately and is then killed by the helper's sc stop.
func spawnApplyHelper(ctx context.Context, tag, verifiedExe string) error {
	selfExe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve agent exe: %w", err)
	}
	if _, err := os.Stat(verifiedExe); err != nil {
		return fmt.Errorf("verified exe missing: %w", err)
	}
	readyURL, err := updateReadyURL(selfExe)
	if err != nil {
		return err
	}
	// The service name the SCM launched *this* process under is the only
	// reliable one — the release installer registers "SysmonAgent" and the
	// monorepo script "HomelabSysmonAgent". Resolve it here, in the agent, and
	// hand it to the helper.
	svcName := resolvedServiceName()
	if err := ctx.Err(); err != nil {
		return err
	}
	// Deliberately exec.Command, not exec.CommandContext: this helper must
	// outlive the agent (and therefore ctx, which the caller cancels as soon as
	// it returns 202). CommandContext would attach a watchdog that kills the
	// helper on cancel.
	cmd := exec.Command(selfExe, "--apply-update", tag, verifiedExe, readyURL, svcName)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: updateHelperCreateNewProcessGroup | updateHelperDetachedProcess | updateHelperBreakawayFromJob,
		HideWindow:    true,
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start helper: %w", err)
	}
	// Release the helper so it does not become a zombie when the agent exits
	// without ever calling Wait.
	_ = cmd.Process.Release()
	return nil
}

// updateReadyURL builds the http://127.0.0.1:<port>/readyz URL the apply helper
// polls after restarting the service. The port is resolved from the agent's
// own command line (`os.Args`), falling back to the default listen port.
func updateReadyURL(selfExe string) (string, error) {
	port := defaultUpdateReadyPort
	for i, arg := range os.Args {
		if arg == "-port" && i+1 < len(os.Args) {
			port = strings.TrimSpace(os.Args[i+1])
			break
		}
		if strings.HasPrefix(arg, "-port=") {
			port = strings.TrimSpace(strings.TrimPrefix(arg, "-port="))
			break
		}
	}
	if port == "" {
		port = defaultUpdateReadyPort
	}
	return "http://127.0.0.1:" + port + "/readyz", nil
}

// defaultUpdateReadyPort is the fallback port the apply helper polls when the
// agent's command line cannot be parsed for -port. It matches the agent's
// default SYSMON_PORT.
const defaultUpdateReadyPort = "9099"

// runApplyUpdate is the hidden --apply-update subcommand entry point. It runs
// detached from the agent, performs the binary swap, and restarts the service.
// It is invoked with: sysmon-agent.exe --apply-update <tag> <verified-exe>
// <ready-url> [service-name]. The agent process is the parent; once this helper
// stops the service, the agent is gone, but the helper is in its own process
// group and survives.
//
// The swap+rollback sequence itself lives in the platform-neutral
// applyVerifiedSwap (update_swap.go) so it is unit-tested; this function only
// resolves the paths and supplies the sc.exe-backed service operations.
//
// svcName is the name the *agent* resolved from the SCM (see
// resolvedServiceName): the same binary is installed as "SysmonAgent" by the
// public release installer and "HomelabSysmonAgent" by the monorepo script, so
// the compile-time constant is not a safe default here. An empty svcName falls
// back to whatever this process can resolve.
func runApplyUpdate(tag, verifiedExe, readyURL, svcName string) error {
	_ = tag
	selfExe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve agent exe: %w", err)
	}
	if strings.TrimSpace(svcName) == "" {
		svcName = resolvedServiceName()
	}
	if !isExecutableOnPath("sc.exe") {
		return errors.New("sc.exe not available on PATH")
	}
	if _, err := serviceState(svcName); err != nil {
		return fmt.Errorf("service %q is not queryable; refusing to swap the binary: %w", svcName, err)
	}

	return applyVerifiedSwap(
		svcName,
		filepath.Clean(selfExe),
		filepath.Clean(verifiedExe),
		readyURL,
		updateApplyReadyPollTimeout,
		updateSwapOps{
			StopAndWait:  stopServiceAndWait,
			Start:        startService,
			WaitReady:    pollReadyURL,
			RemoveBackup: removeOrScheduleDelete,
		},
	)
}

// moveFileExDelayUntilReboot is MOVEFILE_DELAY_UNTIL_REBOOT: with a nil
// destination, MoveFileExW queues the file for deletion at the next boot.
const moveFileExDelayUntilReboot = 0x00000004

var procMoveFileExW = kernel32.NewProc("MoveFileExW")

// removeOrScheduleDelete deletes path, and if that fails because the file is
// still locked, queues it for deletion at the next reboot.
//
// This exists for one specific case: the update helper runs from the very exe
// it renames to <exe>.old.exe. Windows permits renaming a running image but
// never unlinking one, so the success-path cleanup of the rollback copy is
// guaranteed to fail while the helper is alive — and the helper has to outlive
// the cleanup to do it at all. Rather than leave a ~12 MB orphan after every
// update, hand it to the OS. Keeping the backup until reboot is also mildly
// useful: a manual rollback stays possible until then, and the next update's
// stale-backup sweep removes it anyway.
func removeOrScheduleDelete(path string) error {
	if err := os.Remove(path); err == nil || os.IsNotExist(err) {
		return nil
	}
	from, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	r, _, callErr := procMoveFileExW.Call(
		uintptr(unsafe.Pointer(from)),
		0, // nil destination + DELAY_UNTIL_REBOOT == delete on reboot
		uintptr(moveFileExDelayUntilReboot),
	)
	if r == 0 {
		return fmt.Errorf("schedule %s for deletion at reboot: %w", path, callErr)
	}
	return nil
}

// stopServiceAndWait issues `sc.exe stop` and then polls `sc.exe query` until
// the service reports STOPPED or the timeout expires. sc stop is async; the
// wait is what releases the file lock on the agent exe.
func stopServiceAndWait(name string) error {
	// sc.exe stop on an already-stopped service returns a non-zero exit; treat
	// that as success and verify via the query loop.
	_ = exec.Command("sc.exe", "stop", name).Run()
	deadline := time.Now().Add(updateServiceStopTimeout)
	for {
		status, err := serviceState(name)
		if err != nil {
			return err
		}
		if status == "STOPPED" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("service %s did not reach STOPPED within %s (state=%s)", name, updateServiceStopTimeout, status)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// startService issues `sc.exe start` and returns any error from the SCM. A
// short re-check follows the SCM-start timeout pattern from install-windows.ps1
// (the very first boot can race the LHM driver load).
func startService(name string) error {
	if err := exec.Command("sc.exe", "start", name).Run(); err != nil {
		time.Sleep(2 * time.Second)
		status, queryErr := serviceState(name)
		if queryErr != nil {
			return err
		}
		if status != "RUNNING" && status != "START_PENDING" {
			return fmt.Errorf("sc start reported %w and state is %s", err, status)
		}
	}
	return nil
}

// serviceState returns the SERVICE_STATUS current-state string from
// `sc.exe query <name>` (e.g. "RUNNING", "STOPPED", "START_PENDING"). Empty
// string + error if the service is missing or query fails.
func serviceState(name string) (string, error) {
	out, err := exec.Command("sc.exe", "query", name).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("sc query %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "STATE") {
			continue
		}
		// "STATE              : 4  RUNNING (or other)"
		if _, after, ok := strings.Cut(line, ":"); ok {
			fields := strings.Fields(strings.TrimSpace(after))
			if len(fields) >= 2 {
				return fields[1], nil
			}
		}
	}
	return "", fmt.Errorf("sc query %s: could not parse STATE", name)
}

// pollReadyURL polls a /readyz URL until it returns {"status":"ok","metrics":true}
// or the timeout expires. Used by the apply helper after restarting the service.
func pollReadyURL(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 4 * time.Second}
	for {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("readyz never responded within %s: %w", timeout, err)
			}
			return fmt.Errorf("readyz never returned 200 within %s", timeout)
		}
		time.Sleep(updateApplyReadyPollInterval)
	}
}

// runningAsWindowsService is the authoritative gate for self-update support.
// It combines the session-0 heuristic (startedByServiceControlManager) with
// the dispatcher handshake: a process that the SCM actually launched and that
// connected to the dispatcher is running as a service; a console run in
// session 0 (rare but possible) is not.
func runningAsWindowsService() bool {
	if !startedByServiceControlManager() {
		return false
	}
	svcMu.Lock()
	defer svcMu.Unlock()
	return svcHandle != 0
}
