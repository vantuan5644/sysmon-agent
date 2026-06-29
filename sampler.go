package main

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"
)

// laneCollector is implemented by platform collectors that support split-rate
// sampling. The fast lane gathers the cheap, syscall-only metrics (CPU + RAM)
// that the dashboard animates live; the slow lane gathers the expensive ones
// (power, clocks, disks, network, temperatures, GPU) that need process spawns
// or sysfs/RAPL walks. Each method performs its own collection (no shared lock
// held during I/O) and returns a patch closure that the sampler applies to the
// shared working snapshot under a short lock. Returning a closure keeps field
// ownership inside the platform collector so the sampler never has to know which
// Metrics fields belong to which lane.
type laneCollector interface {
	CollectFast(ctx context.Context) func(*Metrics)
	CollectSlow(ctx context.Context) func(*Metrics)
}

// metricsStreamer is implemented by collectors that can push warm snapshots to
// Server-Sent Events subscribers. The HTTP layer registers GET /api/stream only
// when the wrapped collector implements this, so collectors without a sampler
// (or platforms without lane support) simply fall back to polling.
type metricsStreamer interface {
	// Subscribe registers a subscriber and returns a channel that immediately
	// receives the current snapshot JSON (if any) followed by every subsequent
	// published snapshot, plus an unsubscribe func that must be called when the
	// consumer is done. The channel coalesces to the latest snapshot: a slow
	// consumer never blocks the sampler and never sees a backlog.
	Subscribe() (<-chan []byte, func())
}

const (
	defaultFastSampleInterval = 200 * time.Millisecond
	defaultSlowSampleInterval = 1500 * time.Millisecond
	minFastSampleInterval     = 100 * time.Millisecond
	minSlowSampleInterval     = 500 * time.Millisecond
	// samplerIdleAfter stops the expensive slow lane when nobody is watching:
	// the slow loop pauses once there has been no SSE subscriber and no
	// /api/metrics fetch for this long, and resumes immediately on demand. The
	// cheap fast lane keeps running so the published snapshot's timestamp stays
	// fresh and /readyz never trips on staleness.
	samplerIdleAfter = 30 * time.Second
)

type subscriber struct {
	ch chan []byte
}

// sampler is the resident background collector. It keeps a single working
// Metrics snapshot warm by running a fast lane (CPU/RAM, ~5 Hz) and a slow lane
// (everything else, ~0.7 Hz) against the platform laneCollector, publishing a
// merged snapshot after every tick. Collect() returns the warm snapshot
// instantly (no process spawns on the request path), which is what lets the
// dashboard stream at high rates and fixes the request-timeout that plain
// per-request collection caused on Windows.
type sampler struct {
	inner     MetricsCollector
	lanes     laneCollector
	hostname  string
	fastEvery time.Duration
	slowEvery time.Duration
	idleAfter time.Duration

	mu           sync.Mutex
	working      Metrics
	snapshot     Metrics
	snapshotJSON []byte
	haveSnapshot bool
	lastDemandAt time.Time
	subs         map[*subscriber]struct{}

	started bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func newSampler(inner MetricsCollector, fastEvery, slowEvery time.Duration) *sampler {
	lanes, _ := inner.(laneCollector)
	s := &sampler{
		inner:     inner,
		lanes:     lanes,
		hostname:  collectorHostname(inner),
		fastEvery: clampInterval(fastEvery, minFastSampleInterval, defaultFastSampleInterval),
		slowEvery: clampInterval(slowEvery, minSlowSampleInterval, defaultSlowSampleInterval),
		idleAfter: samplerIdleAfter,
		subs:      map[*subscriber]struct{}{},
	}
	s.working = warmingMetrics(s.hostname)
	return s
}

func clampInterval(value, min, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	if value < min {
		return min
	}
	return value
}

// collectorHostname recovers the hostname the platform collector already
// resolved so the warming placeholder snapshot carries it before the first
// lane pass. Platform collectors expose it via a hostnamer; otherwise resolve it
// directly (mirroring what the collectors do) so the warming snapshot never
// carries an empty hostname, which validateMetricsShape rejects.
func collectorHostname(inner MetricsCollector) string {
	if h, ok := inner.(interface{ Hostname() string }); ok {
		if name := h.Hostname(); name != "" {
			return name
		}
	}
	if name, err := os.Hostname(); err == nil && name != "" {
		return name
	}
	return "unknown"
}

// warmingMetrics is the snapshot served before the lanes have produced real
// data. CPU and memory are filled by the first fast pass (which validateMetrics
// requires to be available); the slow fields carry an error so they degrade
// gracefully and pass the readiness shape check during the brief warmup window.
func warmingMetrics(hostname string) Metrics {
	m := baseMetrics(hostname)
	const warming = "sampler is warming up"
	m.CPU = unavailableNumber("%", warming)
	m.CPUPower = unavailableNumber("W", warming)
	m.CPUClock = unavailableNumber("MHz", warming)
	m.CPUClockMax = unavailableNumber("MHz", warming)
	m.CPUTemperature = unavailableNumber("C", warming)
	m.PSUOutputPower = unavailableNumber("W", warming)
	m.Memory = unavailableCapacity(warming)
	m.Disks = unavailableDisk(warming)
	m.Network = NetworkSet{Available: false, Error: warming}
	m.Tailscale = TailscaleStatus{Available: false, Error: warming}
	m.Temperatures = TemperatureSet{Available: false, Error: warming}
	m.GPU = GPUSet{Available: false, Error: warming}
	return m
}

// Start launches the background sampling loops. It is safe to call once; further
// calls are no-ops. When the platform collector does not support lane splitting,
// a single loop runs the whole collector at the slow interval so streaming still
// works (every field just refreshes at the slow rate).
func (s *sampler) Start() {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.mu.Unlock()

	// Enable the persistent platform bridge (the LibreHardwareMonitor daemon on
	// Windows) now that the sampler is entering steady-state serving. This is
	// gated on Start() rather than lazy creation so one-shot modes such as
	// -self-check - which never Start() the sampler - keep the per-sample bridge
	// and spawn nothing long-lived.
	if br, ok := s.inner.(interface{ EnablePersistentBridge() }); ok {
		br.EnablePersistentBridge()
	}

	if s.lanes == nil {
		s.wg.Add(1)
		go s.runWholeLoop(ctx)
		return
	}
	s.wg.Add(2)
	go s.runFastLoop(ctx)
	go s.runSlowLoop(ctx)
}

// Stop cancels the loops and waits for them to exit.
func (s *sampler) Stop() {
	s.mu.Lock()
	cancel := s.cancel
	started := s.started
	s.mu.Unlock()
	if !started || cancel == nil {
		return
	}
	cancel()
	s.wg.Wait()
	// Tear down persistent resources the inner collector owns (the LHM daemon on
	// Windows) so a graceful stop / service STOP leaves no orphaned child.
	if closer, ok := s.inner.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
}

func (s *sampler) runFastLoop(ctx context.Context) {
	defer s.wg.Done()
	for {
		started := time.Now()
		patch := s.lanes.CollectFast(ctx)
		s.applyAndPublish(patch, started)
		if sleepCanceled(ctx, s.fastEvery) {
			return
		}
	}
}

func (s *sampler) runSlowLoop(ctx context.Context) {
	defer s.wg.Done()
	// One eager pass at startup so the first snapshot carries full sensor data
	// regardless of whether a client is connected yet.
	started := time.Now()
	patch := s.lanes.CollectSlow(ctx)
	s.applyAndPublish(patch, started)
	for {
		if sleepCanceled(ctx, s.slowEvery) {
			return
		}
		if !s.hasDemand() {
			continue
		}
		started := time.Now()
		patch := s.lanes.CollectSlow(ctx)
		s.applyAndPublish(patch, started)
	}
}

// runWholeLoop is the fallback for collectors without lane support: it runs the
// full Collect() at the slow interval and publishes the result so SSE clients
// still receive updates.
func (s *sampler) runWholeLoop(ctx context.Context) {
	defer s.wg.Done()
	for {
		started := time.Now()
		metrics, err := s.inner.Collect(ctx)
		if err == nil {
			s.publishWhole(metrics)
		}
		if sleepCanceled(ctx, s.slowEvery) {
			return
		}
		if !s.hasDemand() {
			// Park until demand returns so an idle host is not collected forever.
			if waitForDemand(ctx, s, s.slowEvery) {
				return
			}
		}
		_ = started
	}
}

func sleepCanceled(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-timer.C:
		return false
	}
}

// waitForDemand polls until there is demand or the context is canceled, so an
// idle no-lane host parks instead of spinning the full collector. Returns true
// if the context was canceled.
func waitForDemand(ctx context.Context, s *sampler, poll time.Duration) bool {
	for {
		if s.hasDemand() {
			return false
		}
		if sleepCanceled(ctx, poll) {
			return true
		}
	}
}

func (s *sampler) hasDemand() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.subs) > 0 {
		return true
	}
	return !s.lastDemandAt.IsZero() && time.Since(s.lastDemandAt) < s.idleAfter
}

// applyAndPublish applies a lane patch to the working snapshot, finalizes it,
// stores it as the current snapshot, and broadcasts the JSON to subscribers.
func (s *sampler) applyAndPublish(patch func(*Metrics), started time.Time) {
	if patch == nil {
		return
	}
	s.mu.Lock()
	patch(&s.working)
	s.working.Timestamp = time.Now().UTC()
	working := s.working
	s.mu.Unlock()

	final := finishMetrics(working, started)
	data, err := json.Marshal(final)
	if err != nil {
		return
	}

	s.mu.Lock()
	s.snapshot = final
	s.snapshotJSON = data
	s.haveSnapshot = true
	s.mu.Unlock()

	s.broadcast(data)
}

// publishWhole stores and broadcasts a full Metrics produced by the fallback
// loop (collectors without lane support).
func (s *sampler) publishWhole(metrics Metrics) {
	data, err := json.Marshal(metrics)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.snapshot = metrics
	s.snapshotJSON = data
	s.haveSnapshot = true
	s.mu.Unlock()
	s.broadcast(data)
}

func (s *sampler) broadcast(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub := range s.subs {
		coalesceSend(sub.ch, data)
	}
}

// coalesceSend delivers data on a size-1 channel, dropping any older pending
// value first so the subscriber always observes the latest snapshot and the
// sampler never blocks on a slow reader.
func coalesceSend(ch chan []byte, data []byte) {
	select {
	case ch <- data:
		return
	default:
	}
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- data:
	default:
	}
}

func (s *sampler) Collect(ctx context.Context) (Metrics, error) {
	s.mu.Lock()
	s.lastDemandAt = time.Now()
	snapshot := s.snapshot
	have := s.haveSnapshot
	s.mu.Unlock()
	if have {
		return snapshot, nil
	}
	// No snapshot yet (called before the first lane pass): fall back to a direct
	// collection so early /readyz and /api/metrics requests still succeed.
	return s.inner.Collect(ctx)
}

func (s *sampler) Subscribe() (<-chan []byte, func()) {
	sub := &subscriber{ch: make(chan []byte, 1)}
	s.mu.Lock()
	s.subs[sub] = struct{}{}
	s.lastDemandAt = time.Now()
	if s.haveSnapshot {
		// Channel was just created with capacity 1, so this never blocks.
		sub.ch <- s.snapshotJSON
	}
	s.mu.Unlock()

	cancel := func() {
		s.mu.Lock()
		if _, ok := s.subs[sub]; ok {
			delete(s.subs, sub)
			close(sub.ch)
		}
		s.mu.Unlock()
	}
	return sub.ch, cancel
}
