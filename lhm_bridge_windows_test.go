//go:build windows

package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// goodBridgeJSON is a representative LHM daemon response exercising all four
// payload fields (CPU package power, CPU clock, PSU output power, temperatures).
const goodBridgeJSON = `{"available":true,"power":{"available":true,"value":88.08},"cpu_clock":{"available":true,"value":4500},"psu_output_power":{"available":true,"value":312.4},"temperatures":[{"name":"CPU Package","value":55.5}]}`

// fakeAction controls what a fake daemon instance does for a given request.
type fakeAction int

const (
	fakeRespond fakeAction = iota // write the response line and keep serving
	fakeHang                      // read the request then block until killed
	fakeClose                     // close stdout without responding (=> EOF)
)

// fakeDaemonProcess is an in-memory lhmDaemonProcess. Each Start() creates a
// fresh pair of os.Pipes and launches a serve goroutine that reads request lines
// and writes canned responses, mirroring real process I/O (including EOF on
// close). Its behavior is controlled per Start() via behaviors and globally via
// alwaysStartErr, so tests can model "first start hangs, second heals",
// "stdout closes mid-stream", and "can never start".
type fakeDaemonProcess struct {
	alwaysStartErr error // if non-nil, every Start returns it
	behaviors      []func(line string) (string, fakeAction)

	mu        sync.Mutex
	starts    int
	instances []*fakeInstance
}

func (p *fakeDaemonProcess) startCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.starts
}

func (p *fakeDaemonProcess) Start() (io.WriteCloser, io.Reader, func() error, error) {
	if p.alwaysStartErr != nil {
		p.mu.Lock()
		p.starts++
		p.mu.Unlock()
		return nil, nil, nil, p.alwaysStartErr
	}
	stdinRead, stdinWrite, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		_ = stdinRead.Close()
		_ = stdinWrite.Close()
		return nil, nil, nil, err
	}
	p.mu.Lock()
	idx := p.starts
	p.starts++
	p.mu.Unlock()
	behavior := func(line string) (string, fakeAction) { return goodBridgeJSON, fakeRespond }
	switch {
	case idx < len(p.behaviors):
		behavior = p.behaviors[idx]
	case len(p.behaviors) > 0:
		behavior = p.behaviors[len(p.behaviors)-1]
	}
	inst := &fakeInstance{
		behavior:    behavior,
		stdinRead:   stdinRead,
		stdoutWrite: stdoutWrite,
		done:        make(chan struct{}),
	}
	p.mu.Lock()
	p.instances = append(p.instances, inst)
	p.mu.Unlock()
	go inst.serve()
	kill := func() error {
		inst.close()
		return nil
	}
	return stdinWrite, stdoutRead, kill, nil
}

type fakeInstance struct {
	behavior    func(line string) (string, fakeAction)
	stdinRead   *os.File
	stdoutWrite *os.File
	done        chan struct{}
	once        sync.Once
}

func (inst *fakeInstance) close() {
	inst.once.Do(func() { close(inst.done) })
	_ = inst.stdinRead.Close()
	_ = inst.stdoutWrite.Close()
}

func (inst *fakeInstance) serve() {
	reader := bufio.NewReader(inst.stdinRead)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		resp, action := inst.behavior(strings.TrimRight(line, "\n"))
		switch action {
		case fakeHang:
			<-inst.done // block until the daemon kills this instance
			return
		case fakeClose:
			_ = inst.stdoutWrite.Close()
			return
		case fakeRespond:
			if _, werr := io.WriteString(inst.stdoutWrite, resp+"\n"); werr != nil {
				return
			}
		}
	}
}

// daemonTestEnv saves/restores the daemon timing/policy vars and shortens them so
// the transport tests run in milliseconds instead of seconds.
type daemonTestEnv struct {
	cold       time.Duration
	warm       time.Duration
	backoff    time.Duration
	disable    int
	recycle    int
	recycleAge time.Duration
	spawn      func() (lhmDaemonProcess, error)
}

func shortenDaemonTimings(t *testing.T) *daemonTestEnv {
	t.Helper()
	saved := &daemonTestEnv{
		cold:       lhmDaemonColdReadTimeout,
		warm:       lhmDaemonWarmReadTimeout,
		backoff:    lhmDaemonRestartBackoff,
		disable:    lhmDaemonDisableFailures,
		recycle:    lhmDaemonRecycleReads,
		recycleAge: lhmDaemonRecycleAge,
		spawn:      newLhmDaemonProcess,
	}
	lhmDaemonColdReadTimeout = 80 * time.Millisecond
	lhmDaemonWarmReadTimeout = 80 * time.Millisecond
	lhmDaemonRestartBackoff = time.Millisecond
	lhmDaemonDisableFailures = 3
	t.Cleanup(func() {
		lhmDaemonColdReadTimeout = saved.cold
		lhmDaemonWarmReadTimeout = saved.warm
		lhmDaemonRestartBackoff = saved.backoff
		lhmDaemonDisableFailures = saved.disable
		lhmDaemonRecycleReads = saved.recycle
		lhmDaemonRecycleAge = saved.recycleAge
		newLhmDaemonProcess = saved.spawn
	})
	return saved
}

func installFakeDaemon(t *testing.T, fake *fakeDaemonProcess) {
	t.Helper()
	shortenDaemonTimings(t)
	orig := newLhmDaemonProcess
	newLhmDaemonProcess = func() (lhmDaemonProcess, error) { return fake, nil }
	t.Cleanup(func() { newLhmDaemonProcess = orig })
}

func TestLhmDaemonHappyPath(t *testing.T) {
	fake := &fakeDaemonProcess{}
	installFakeDaemon(t, fake)
	d := &lhmDaemon{}

	result, err := d.read(context.Background())
	if err != nil {
		t.Fatalf("read returned error: %v", err)
	}
	if !result.Available {
		t.Fatalf("result unavailable: %+v", result)
	}
	if result.Power == nil || result.Power.Value != 88.08 {
		t.Fatalf("power = %+v, want 88.08 W", result.Power)
	}
	if result.CPUClock == nil || result.CPUClock.Value != 4500 {
		t.Fatalf("cpu_clock = %+v, want 4500 MHz", result.CPUClock)
	}
	if result.PSUOutputPower == nil || result.PSUOutputPower.Value != 312.4 {
		t.Fatalf("psu_output_power = %+v, want 312.4 W", result.PSUOutputPower)
	}
	if len(result.Temperatures) != 1 || result.Temperatures[0].Value != 55.5 {
		t.Fatalf("temperatures = %+v, want one entry at 55.5 C", result.Temperatures)
	}

	// A second read reuses the same process (no restart) and stays warm.
	if _, err := d.read(context.Background()); err != nil {
		t.Fatalf("second read returned error: %v", err)
	}
	if got := fake.startCount(); got != 1 {
		t.Fatalf("process start count = %d, want 1 (no restart between warm reads)", got)
	}
}

func TestLhmDaemonErrorObjectPassthrough(t *testing.T) {
	errJSON := `{"available":false,"error":"sensor warm-up","power":null,"cpu_clock":null,"psu_output_power":null,"temperatures":[]}`
	fake := &fakeDaemonProcess{
		behaviors: []func(string) (string, fakeAction){
			func(string) (string, fakeAction) { return errJSON, fakeRespond },
		},
	}
	installFakeDaemon(t, fake)
	d := &lhmDaemon{}

	result, err := d.read(context.Background())
	if err != nil {
		t.Fatalf("transport error on an error-object response: %v", err)
	}
	if result.Available {
		t.Fatalf("result should be unavailable, got %+v", result)
	}
	if result.Error != "sensor warm-up" {
		t.Fatalf("error = %q, want sensor warm-up", result.Error)
	}
}

func TestLhmDaemonTimeoutRestartsAndHeals(t *testing.T) {
	// First instance hangs (never responds) so the cold read times out; the
	// second instance responds and the daemon heals on the next read.
	fake := &fakeDaemonProcess{
		behaviors: []func(string) (string, fakeAction){
			func(string) (string, fakeAction) { return "", fakeHang },
			func(string) (string, fakeAction) { return goodBridgeJSON, fakeRespond },
		},
	}
	installFakeDaemon(t, fake)
	d := &lhmDaemon{}

	// First read: cold timeout on the hung instance -> transient error.
	start := time.Now()
	if _, err := d.read(context.Background()); err == nil {
		t.Fatal("first read should fail with a timeout, got nil")
	} else if !errors.Is(err, errLhmDaemonTimeout) {
		t.Fatalf("first read error = %v, want errLhmDaemonTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cold read took %s, should have been bounded by the short test timeout", elapsed)
	}

	// Second read (after the short backoff): a fresh process responds and heals.
	waitUntilBackoffElapsed(t)
	result, err := d.read(context.Background())
	if err != nil {
		t.Fatalf("second read should heal, got error: %v", err)
	}
	if !result.Available {
		t.Fatalf("second read result unavailable: %+v", result)
	}
	if got := fake.startCount(); got != 2 {
		t.Fatalf("process start count = %d, want 2 (restart after timeout)", got)
	}
}

func TestLhmDaemonEOFRestarts(t *testing.T) {
	// First instance closes stdout without responding -> read sees EOF; the
	// second instance responds and the daemon recovers.
	fake := &fakeDaemonProcess{
		behaviors: []func(string) (string, fakeAction){
			func(string) (string, fakeAction) { return "", fakeClose },
			func(string) (string, fakeAction) { return goodBridgeJSON, fakeRespond },
		},
	}
	installFakeDaemon(t, fake)
	d := &lhmDaemon{}

	if _, err := d.read(context.Background()); err == nil {
		t.Fatal("first read should fail with EOF, got nil")
	} else if !errors.Is(err, errLhmDaemonEOF) {
		t.Fatalf("first read error = %v, want errLhmDaemonEOF", err)
	}

	waitUntilBackoffElapsed(t)
	result, err := d.read(context.Background())
	if err != nil {
		t.Fatalf("second read should heal after EOF, got error: %v", err)
	}
	if !result.Available {
		t.Fatalf("second read result unavailable: %+v", result)
	}
	if got := fake.startCount(); got != 2 {
		t.Fatalf("process start count = %d, want 2 (restart after EOF)", got)
	}
}

func TestLhmDaemonDisablesAfterRepeatedStartFailures(t *testing.T) {
	fake := &fakeDaemonProcess{alwaysStartErr: errors.New("pwsh not found")}
	installFakeDaemon(t, fake)
	d := &lhmDaemon{}

	// Drive reads until the daemon gives up. With the short test backoff this
	// resolves in well under a second; guard with an overall deadline anyway.
	deadline := time.Now().Add(3 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		_, err := d.read(context.Background())
		if errors.Is(err, errLhmDaemonDisabled) {
			lastErr = err
			break
		}
		lastErr = err
		time.Sleep(2 * time.Millisecond)
	}
	if !errors.Is(lastErr, errLhmDaemonDisabled) {
		t.Fatalf("daemon did not disable after repeated start failures; last error = %v", lastErr)
	}
	if got := fake.startCount(); got != lhmDaemonDisableFailures {
		t.Fatalf("start attempts before disable = %d, want %d", got, lhmDaemonDisableFailures)
	}
}

func TestLhmDaemonNeverDisablesAfterItHasSucceeded(t *testing.T) {
	// A daemon that has produced at least one good read must keep restarting
	// (with backoff) rather than permanently disable, so transient driver hiccups
	// self-heal instead of stranding the host on the slow one-shot fallback. The
	// first instance responds once (-> success) then hangs on its next request;
	// the next instance also hangs; a third instance heals. Two consecutive
	// timeouts after a success must NOT trip the disable threshold.
	var instance0Calls int32
	respondThenHang := func(string) (string, fakeAction) {
		if atomic.AddInt32(&instance0Calls, 1) == 1 {
			return goodBridgeJSON, fakeRespond
		}
		return "", fakeHang
	}
	hang := func(string) (string, fakeAction) { return "", fakeHang }
	good := func(string) (string, fakeAction) { return goodBridgeJSON, fakeRespond }
	fake := &fakeDaemonProcess{
		behaviors: []func(string) (string, fakeAction){respondThenHang, hang, good},
	}
	installFakeDaemon(t, fake)
	d := &lhmDaemon{}

	if _, err := d.read(context.Background()); err != nil {
		t.Fatalf("first read should succeed: %v", err)
	}
	// Two consecutive timeouts after a success: degrade but keep going.
	if _, err := d.read(context.Background()); !errors.Is(err, errLhmDaemonTimeout) {
		t.Fatalf("second read error = %v, want timeout", err)
	}
	waitUntilBackoffElapsed(t)
	if _, err := d.read(context.Background()); !errors.Is(err, errLhmDaemonTimeout) {
		t.Fatalf("third read error = %v, want timeout", err)
	}
	waitUntilBackoffElapsed(t)
	// Fourth read heals.
	result, err := d.read(context.Background())
	if err != nil {
		t.Fatalf("fourth read should heal, got %v", err)
	}
	if !result.Available {
		t.Fatalf("fourth read result unavailable: %+v", result)
	}
	if d.disabled {
		t.Fatal("daemon disabled itself after a success, should keep self-healing")
	}
}

func TestLhmDaemonRecycleRestartsAfterReadThreshold(t *testing.T) {
	lhmDaemonRecycleReads = 2 // recycle after every 2 successful reads
	good := func(string) (string, fakeAction) { return goodBridgeJSON, fakeRespond }
	fake := &fakeDaemonProcess{behaviors: []func(string) (string, fakeAction){good}}
	installFakeDaemon(t, fake)
	d := &lhmDaemon{}

	// Two successful reads hit the recycle threshold; the process is torn down so
	// the next read spawns a fresh one (a fresh Computer.Open() re-enumerates
	// hardware). After the recycle, coldRead is re-armed for the cold re-Open.
	for i := 0; i < lhmDaemonRecycleReads; i++ {
		if _, err := d.read(context.Background()); err != nil {
			t.Fatalf("read #%d error: %v", i+1, err)
		}
	}
	if got := fake.startCount(); got != 1 {
		t.Fatalf("start count before recycle-read = %d, want 1", got)
	}
	// The next read recycles: the recycled read uses the long cold deadline again
	// (lhmDaemonColdReadTimeout), and a fresh process is started.
	lhmDaemonColdReadTimeout = 5 * time.Second
	if _, err := d.read(context.Background()); err != nil {
		t.Fatalf("recycle read error: %v", err)
	}
	if got := fake.startCount(); got != 2 {
		t.Fatalf("start count after recycle-read = %d, want 2 (fresh Open())", got)
	}
}

func TestLhmDaemonCloseKillsAndDisables(t *testing.T) {
	fake := &fakeDaemonProcess{}
	installFakeDaemon(t, fake)
	d := &lhmDaemon{}

	if _, err := d.read(context.Background()); err != nil {
		t.Fatalf("read error: %v", err)
	}
	if got := fake.startCount(); got != 1 {
		t.Fatalf("start count = %d, want 1", got)
	}

	if err := d.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}
	if !d.disabled {
		t.Fatal("Close should disable further reads")
	}
	// A read after Close must fail fast with the disabled sentinel, not respawn.
	if _, err := d.read(context.Background()); !errors.Is(err, errLhmDaemonDisabled) {
		t.Fatalf("read after Close = %v, want errLhmDaemonDisabled", err)
	}
	if got := fake.startCount(); got != 1 {
		t.Fatalf("start count after Close = %d, want 1 (Close must not respawn)", got)
	}
}

func TestSystemCollectorEnablePersistentBridgeGate(t *testing.T) {
	t.Run("default on", func(t *testing.T) {
		t.Setenv("SYSMON_LHM_DAEMON", "")
		c := &systemCollector{}
		c.EnablePersistentBridge()
		if !c.useDaemon {
			t.Fatal("useDaemon = false, want true when SYSMON_LHM_DAEMON is unset")
		}
	})
	t.Run("explicitly disabled", func(t *testing.T) {
		t.Setenv("SYSMON_LHM_DAEMON", "false")
		c := &systemCollector{}
		c.EnablePersistentBridge()
		if c.useDaemon {
			t.Fatal("useDaemon = true, want false when SYSMON_LHM_DAEMON=false")
		}
	})
}

func TestFetchLhmBridgeFallsBackToOneShotWhenDaemonDisabled(t *testing.T) {
	// A permanently-disabled daemon must hand off to the one-shot bridge instead
	// of stranding every metric on unavailable. Stub runLhmBridge so the test
	// does not spawn real pwsh.
	stub := lhmBridgeResult{
		Available: true,
		Power:     &lhmBridgeValue{Available: true, Value: 42},
	}
	orig := runLhmBridge
	runLhmBridge = func(context.Context) (lhmBridgeResult, error) { return stub, nil }
	t.Cleanup(func() { runLhmBridge = orig })

	c := &systemCollector{useDaemon: true, daemon: &lhmDaemon{disabled: true}}
	result, err := c.fetchLhmBridge(context.Background())
	if err != nil {
		t.Fatalf("fetchLhmBridge error: %v", err)
	}
	if !result.Available || result.Power == nil || result.Power.Value != 42 {
		t.Fatalf("result = %+v, want the one-shot fallback payload", result)
	}
}

func TestFetchLhmBridgeDegradesOnTransientDaemonFailure(t *testing.T) {
	// A transient transport failure (timeout) must degrade just this sample to
	// an unavailable result rather than spawning the one-shot pwsh.
	fake := &fakeDaemonProcess{
		behaviors: []func(string) (string, fakeAction){
			func(string) (string, fakeAction) { return "", fakeHang },
		},
	}
	installFakeDaemon(t, fake)
	called := false
	orig := runLhmBridge
	runLhmBridge = func(context.Context) (lhmBridgeResult, error) {
		called = true
		return lhmBridgeResult{Available: true}, nil
	}
	t.Cleanup(func() { runLhmBridge = orig })

	c := &systemCollector{useDaemon: true}
	result, err := c.fetchLhmBridge(context.Background())
	if err != nil {
		t.Fatalf("fetchLhmBridge error: %v", err)
	}
	if result.Available {
		t.Fatalf("result should be unavailable on transient failure, got %+v", result)
	}
	if !strings.Contains(result.Error, "daemon read failed") {
		t.Fatalf("error = %q, want a daemon-read-failed message", result.Error)
	}
	if called {
		t.Fatal("transient failure must NOT fall back to the one-shot bridge (would race the same driver)")
	}
}

// waitUntilBackoffElapsed sleeps just past the (shortened) restart backoff so a
// follow-up read can spawn a fresh process instead of bouncing off backoff.
func waitUntilBackoffElapsed(t *testing.T) {
	t.Helper()
	time.Sleep(lhmDaemonRestartBackoff + 5*time.Millisecond)
}
