//go:build windows

package main

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed lhm-bridge-daemon.ps1
var lhmBridgeDaemonFS embed.FS

// lhmDaemonScriptPath resolves to a temp copy of the embedded long-lived
// LibreHardwareMonitor bridge daemon, written once per process (same pattern as
// lhmBridgePath). Editing lhm-bridge-daemon.ps1 requires a Go rebuild because it
// is //go:embed-ed, not read from disk at runtime.
var (
	lhmDaemonOnce    sync.Once
	lhmDaemonFile    string
	lhmDaemonFileErr error
)

func lhmDaemonScriptPath() (string, error) {
	lhmDaemonOnce.Do(func() {
		src, err := lhmBridgeDaemonFS.ReadFile("lhm-bridge-daemon.ps1")
		if err != nil {
			lhmDaemonFileErr = fmt.Errorf("read embedded lhm-bridge-daemon.ps1: %w", err)
			return
		}
		lhmDaemonFile = filepath.Join(os.TempDir(), "sysmon-lhm-bridge-daemon.ps1")
		if err := os.WriteFile(lhmDaemonFile, src, 0o644); err != nil {
			lhmDaemonFileErr = fmt.Errorf("write lhm-bridge-daemon temp copy: %w", err)
		}
	})
	return lhmDaemonFile, lhmDaemonFileErr
}

// Sentinel transport errors for the daemon. fetchLhmBridge distinguishes
// errLhmDaemonDisabled (pwsh / DLL missing -> fall back to one-shot) from the
// transient failures (timeout / EOF / restart-backoff), which degrade the
// current sample and let the next slow pass heal.
var (
	errLhmDaemonDisabled       = errors.New("LibreHardwareMonitor daemon disabled")
	errLhmDaemonRestartBackoff = errors.New("LibreHardwareMonitor daemon restarting (backoff)")
	errLhmDaemonTimeout        = errors.New("LibreHardwareMonitor daemon read timed out")
	errLhmDaemonEOF            = errors.New("LibreHardwareMonitor daemon process exited")
)

// Daemon read deadlines and recycling policy. The cold read reuses the one-shot
// lhmBridgeTimeout (12 s) so the very first read after a (re)start - which still
// pays Computer.Open() (~2-4 s cold) inside the daemon's startup - has the same
// headroom as the one-shot fallback. Warm reads drop to a sub-second-ish budget;
// a single Update() + sensor walk is well under that. Recycling re-Opens after a
// generous count / age so hot-plugged hardware (USB PSU) is picked up and to
// shed any long-lived driver/handle drift, without churning cold re-Opens.
//
// These are package vars (not consts) so unit tests can shorten the deadlines
// and backoff to run in milliseconds without waiting on real timers.
var (
	lhmDaemonColdReadTimeout = lhmBridgeTimeout
	lhmDaemonWarmReadTimeout = 3 * time.Second
	lhmDaemonRestartBackoff  = 2 * time.Second
	lhmDaemonDisableFailures = 6
	lhmDaemonRecycleReads    = 240
	lhmDaemonRecycleAge      = 30 * time.Minute
)

// lhmDaemonProcess is the spawn contract the daemon talks over. The production
// implementation launches a long-lived pwsh running the embedded daemon script;
// unit tests substitute an in-memory transport that echoes canned JSON lines so
// the read / timeout / restart / disable logic is exercised without a real
// PowerShell + LHM host.
type lhmDaemonProcess interface {
	// Start launches the bridge process and returns its stdin (requests), stdout
	// (responses), and a kill function that releases the process regardless of
	// state. Start is called again after the daemon is torn down for a restart
	// or a recycle.
	Start() (stdin io.WriteCloser, stdout io.Reader, kill func() error, err error)
}

// newLhmDaemonProcess is the production spawn. It is a package var (like
// lookupInstalledPwsh) so tests can substitute an in-memory transport.
var newLhmDaemonProcess = defaultNewLhmDaemonProcess

func defaultNewLhmDaemonProcess() (lhmDaemonProcess, error) {
	scriptPath, err := lhmDaemonScriptPath()
	if err != nil {
		return nil, err
	}
	return &pwshLhmDaemonProcess{scriptPath: scriptPath}, nil
}

// pwshLhmDaemonProcess spawns pwsh on the embedded daemon script on each Start.
type pwshLhmDaemonProcess struct {
	scriptPath string
}

func (p *pwshLhmDaemonProcess) Start() (io.WriteCloser, io.Reader, func() error, error) {
	cmd := exec.Command(
		windowsPowerShellCoreCommand(exec.LookPath),
		"-NoLogo",
		"-NoProfile",
		"-NonInteractive",
		"-ExecutionPolicy",
		"Bypass",
		"-Command",
		fmt.Sprintf("& '%s'", p.scriptPath),
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("lhm daemon stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, nil, nil, fmt.Errorf("lhm daemon stdout pipe: %w", err)
	}
	// Discard stderr: the daemon writes diagnostics nowhere useful there and we
	// do not want a stray PowerShell verbose line to be mistaken for protocol.
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, nil, nil, fmt.Errorf("start lhm daemon: %w", err)
	}
	kill := func() error {
		// Close stdin first so the daemon's ReadLine() sees EOF and exits 0
		// cleanly; then wait briefly for it before forcing a kill. Best-effort:
		// the LHM ring0 driver teardown can lag, so cap the graceful window and
		// kill to guarantee the child is gone (the daemon is single-use once
		// killed; a fresh Start re-spawns).
		_ = stdin.Close()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
			return nil
		case <-time.After(1500 * time.Millisecond):
			_ = cmd.Process.Kill()
			<-done
			return nil
		}
	}
	return stdin, stdout, kill, nil
}

// lhmDaemon owns a single long-lived bridge process and serializes one
// request/response at a time over its stdio channel (the daemon is a strict
// line-by-line request/response protocol, so only one request may be in flight).
// It handles cold-vs-warm deadlines, restart with backoff after a transport
// failure, periodic recycle (re-Open) for hot-plugged hardware, and permanent
// disable when the daemon can never start (pwsh / DLL missing) so the caller can
// fall back to the one-shot bridge.
type lhmDaemon struct {
	mu      sync.Mutex
	process lhmDaemonProcess

	stdin  io.WriteCloser
	stdout *bufio.Reader
	kill   func() error

	alive          bool
	disabled       bool
	coldRead       bool // true until the first success after a (re)start
	everSucceeded  bool // true once the daemon has produced any good read
	reads          int
	startedAt      time.Time
	consecFailures int
	nextRestartAt  time.Time
}

// read issues one bridge read against the persistent daemon, (re)starting it as
// needed. It returns the parsed result on success. On a transport failure it
// returns a sentinel error: errLhmDaemonDisabled means the caller should fall
// back to the one-shot bridge (the daemon can never work); any other error is a
// transient failure for which the caller should degrade the current sample and
// let the next pass heal.
func (d *lhmDaemon) read(ctx context.Context) (lhmBridgeResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.disabled {
		return lhmBridgeResult{}, errLhmDaemonDisabled
	}
	if err := d.ensureAliveLocked(ctx); err != nil {
		return lhmBridgeResult{}, err
	}

	deadline := lhmDaemonWarmReadTimeout
	if d.coldRead {
		// The first read after a (re)start still pays Computer.Open(), which the
		// daemon performs at its own startup before it reads the request line.
		deadline = lhmDaemonColdReadTimeout
	}
	result, err := d.requestLocked(ctx, deadline)
	if err != nil {
		d.handleFailureLocked()
		return lhmBridgeResult{}, err
	}
	d.handleSuccessLocked()
	return result, nil
}

// Close tears the daemon down and marks it disabled so further reads fail fast
// instead of re-spawning. Called from the collector's Close() on agent shutdown.
func (d *lhmDaemon) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.disabled = true
	d.teardownLocked()
	return nil
}

func (d *lhmDaemon) ensureAliveLocked(ctx context.Context) error {
	if d.alive {
		return nil
	}
	if !d.nextRestartAt.IsZero() && time.Now().Before(d.nextRestartAt) {
		// Backing off after a failure; do not hot-loop process spawns.
		return errLhmDaemonRestartBackoff
	}
	if d.process == nil {
		proc, err := newLhmDaemonProcess()
		if err != nil {
			// Could not even resolve the script (embed/write failure): permanent.
			d.disabled = true
			return err
		}
		d.process = proc
	}
	stdin, stdout, kill, err := d.process.Start()
	if err != nil {
		d.recordFailureLocked()
		d.nextRestartAt = time.Now().Add(lhmDaemonRestartBackoff)
		return fmt.Errorf("start LibreHardwareMonitor daemon: %w", err)
	}
	d.stdin = stdin
	d.stdout = bufio.NewReader(stdout)
	d.kill = kill
	d.alive = true
	d.coldRead = true
	d.startedAt = time.Now()
	d.reads = 0
	return nil
}

func (d *lhmDaemon) requestLocked(ctx context.Context, deadline time.Duration) (lhmBridgeResult, error) {
	if _, err := d.stdin.Write([]byte("read\n")); err != nil {
		return lhmBridgeResult{}, fmt.Errorf("write LibreHardwareMonitor daemon request: %w", err)
	}
	line, err := d.readLineLocked(deadline, ctx)
	if err != nil {
		return lhmBridgeResult{}, err
	}
	var result lhmBridgeResult
	if err := json.Unmarshal(bytes.TrimSpace([]byte(line)), &result); err != nil {
		return lhmBridgeResult{}, fmt.Errorf("parse LibreHardwareMonitor daemon response: %w", err)
	}
	return result, nil
}

// readLineLocked reads one newline-terminated line with a deadline. Windows
// pipes have no read deadline, so the blocking ReadString runs in a goroutine
// delivering to a channel while we select on the deadline and the caller's
// context. On timeout the caller kills the process (handleFailureLocked), which
// closes the pipe and the leaked goroutine returns promptly; the channel has
// capacity 1 so the send never blocks.
func (d *lhmDaemon) readLineLocked(deadline time.Duration, ctx context.Context) (string, error) {
	type readResult struct {
		line string
		err  error
	}
	// Capture the reader under the caller's lock before spawning: teardownLocked
	// nils d.stdout after a timeout/ctx failure, and the goroutine runs without
	// d.mu, so reading the field inside the goroutine would race that write (and
	// could nil-deref if the goroutine is not scheduled until after teardown).
	reader := d.stdout
	ch := make(chan readResult, 1)
	go func() {
		line, err := reader.ReadString('\n')
		ch <- readResult{line: line, err: err}
	}()
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	select {
	case r := <-ch:
		if r.err != nil {
			if errors.Is(r.err, io.EOF) || strings.Contains(r.err.Error(), "EOF") {
				return "", errLhmDaemonEOF
			}
			return "", r.err
		}
		return r.line, nil
	case <-timer.C:
		return "", errLhmDaemonTimeout
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (d *lhmDaemon) handleFailureLocked() {
	d.teardownLocked()
	d.recordFailureLocked()
	d.nextRestartAt = time.Now().Add(lhmDaemonRestartBackoff)
}

// recordFailureLocked tracks consecutive transport failures and permanently
// disables the daemon once the threshold is reached AND it has never produced a
// good read - the signature of a host where the daemon can never start (pwsh /
// DLL missing). A daemon that has worked before is never disabled here; it just
// keeps restarting with backoff, trusting the next Open() to self-heal.
func (d *lhmDaemon) recordFailureLocked() {
	d.consecFailures++
	if !d.everSucceeded && d.consecFailures >= lhmDaemonDisableFailures {
		d.disabled = true
	}
}

func (d *lhmDaemon) handleSuccessLocked() {
	d.everSucceeded = true
	d.consecFailures = 0
	d.coldRead = false
	d.reads++
	if d.reads >= lhmDaemonRecycleReads || time.Since(d.startedAt) >= lhmDaemonRecycleAge {
		// Recycle: tear down so the next read re-spawns with a fresh Open(),
		// re-enumerating any hot-plugged hardware and shedding driver drift.
		// coldRead stays false here, but ensureAliveLocked resets it on the
		// restart so the next (cold) read gets the long deadline again.
		d.teardownLocked()
	}
}

func (d *lhmDaemon) teardownLocked() {
	d.alive = false
	if d.kill != nil {
		_ = d.kill()
	}
	d.stdin = nil
	d.stdout = nil
	d.kill = nil
}
