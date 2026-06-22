package main

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"
)

// fakeLaneCollector implements both MetricsCollector (the direct-collection
// fallback) and laneCollector (the split-rate lanes the sampler drives). The
// fast patch stamps CPU with the call count so a snapshot can be told apart
// from the warming placeholder; the slow patch only touches GPU so the two
// lanes never clobber each other's fields.
type fakeLaneCollector struct {
	hostname      string
	fast          int64
	slow          int64
	fallbackCalls int64
}

func (f *fakeLaneCollector) Hostname() string { return f.hostname }

func (f *fakeLaneCollector) Collect(ctx context.Context) (Metrics, error) {
	atomic.AddInt64(&f.fallbackCalls, 1)
	m := baseMetrics(f.hostname)
	m.CPU = availableNumber(1, "%")
	m.Memory = availableCapacity(10, 100)
	return m, nil
}

func (f *fakeLaneCollector) CollectFast(ctx context.Context) func(*Metrics) {
	n := atomic.AddInt64(&f.fast, 1)
	return func(m *Metrics) {
		m.CPU = availableNumber(float64(n), "%")
		m.Memory = availableCapacity(20, 100)
	}
}

func (f *fakeLaneCollector) CollectSlow(ctx context.Context) func(*Metrics) {
	atomic.AddInt64(&f.slow, 1)
	return func(m *Metrics) {
		m.GPU = GPUSet{Available: false, Error: "no gpu in test"}
	}
}

func waitForCond(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

func TestClampInterval(t *testing.T) {
	ms := time.Millisecond
	check := func(value, min, fallback, want time.Duration) {
		t.Helper()
		if got := clampInterval(value, min, fallback); got != want {
			t.Fatalf("clampInterval(%v, %v, %v) = %v, want %v", value, min, fallback, got, want)
		}
	}
	// Non-positive values fall back; values below min are raised to min; values
	// already in range pass through unchanged.
	check(0, 100*ms, 200*ms, 200*ms)
	check(-5*ms, 100*ms, 200*ms, 200*ms)
	check(40*ms, 100*ms, 200*ms, 100*ms)
	check(150*ms, 100*ms, 200*ms, 150*ms)
}

func TestSamplerCollectFallsBackBeforeFirstSnapshot(t *testing.T) {
	lane := &fakeLaneCollector{hostname: "test-host"}
	s := newSampler(lane, 0, 0)
	// Not started: there is no warm snapshot yet, so Collect must fall back to a
	// direct collection so early /readyz and /api/metrics requests still succeed.
	m, err := s.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect returned error: %v", err)
	}
	if got := atomic.LoadInt64(&lane.fallbackCalls); got != 1 {
		t.Fatalf("fallback Collect calls = %d, want 1", got)
	}
	if !m.CPU.Available || m.CPU.Value != 1 {
		t.Fatalf("fallback CPU = %+v, want available value 1", m.CPU)
	}
}

func TestSamplerServesWarmSnapshotAfterFastPass(t *testing.T) {
	lane := &fakeLaneCollector{hostname: "test-host"}
	s := newSampler(lane, 0, 0)
	s.fastEvery = 20 * time.Millisecond
	s.slowEvery = 30 * time.Millisecond
	s.Start()
	defer s.Stop()

	// Wait until a fast pass has published a real CPU reading (not the warming
	// placeholder). Inspect the snapshot directly so the warmup polling does not
	// itself trip the direct-collection fallback.
	waitForCond(t, 2*time.Second, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.haveSnapshot && s.snapshot.CPU.Available
	})

	before := atomic.LoadInt64(&lane.fallbackCalls)
	m, err := s.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect returned error: %v", err)
	}
	if got := atomic.LoadInt64(&lane.fallbackCalls); got != before {
		t.Fatalf("warm Collect triggered %d fallback calls, want 0 (should serve snapshot)", got-before)
	}
	if !m.CPU.Available {
		t.Fatalf("warm snapshot CPU unavailable: %+v", m.CPU)
	}
	if m.Hostname != "test-host" {
		t.Fatalf("warm snapshot hostname = %q, want test-host", m.Hostname)
	}
}

func TestSamplerSubscribeSeedsAndStreams(t *testing.T) {
	lane := &fakeLaneCollector{hostname: "test-host"}
	s := newSampler(lane, 0, 0)
	s.fastEvery = 20 * time.Millisecond
	s.slowEvery = 30 * time.Millisecond
	s.Start()
	defer s.Stop()

	waitForCond(t, 2*time.Second, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.haveSnapshot
	})

	ch, cancel := s.Subscribe()
	defer cancel()

	// The first read is the seed snapshot delivered synchronously at Subscribe.
	select {
	case data := <-ch:
		var m Metrics
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("seed snapshot not valid JSON: %v", err)
		}
		if m.Hostname != "test-host" {
			t.Fatalf("seed snapshot hostname = %q, want test-host", m.Hostname)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber did not receive seed snapshot")
	}

	// A subsequent fast publish must reach the subscriber.
	select {
	case data := <-ch:
		if len(data) == 0 {
			t.Fatal("subscriber received empty live payload")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber did not receive a live update")
	}
}

func TestSamplerUnsubscribeStopsDelivery(t *testing.T) {
	lane := &fakeLaneCollector{hostname: "test-host"}
	s := newSampler(lane, 0, 0)
	s.fastEvery = 20 * time.Millisecond
	s.slowEvery = 30 * time.Millisecond
	s.Start()
	defer s.Stop()

	ch, cancel := s.Subscribe()
	cancel()

	// After cancel the subscriber is removed and its channel is closed; draining
	// must terminate (a closed channel yields ok=false), not block forever.
	done := make(chan struct{})
	go func() {
		for range ch {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("channel was not closed on unsubscribe")
	}

	s.mu.Lock()
	subs := len(s.subs)
	s.mu.Unlock()
	if subs != 0 {
		t.Fatalf("subscriber count after cancel = %d, want 0", subs)
	}
}

func TestSamplerSlowLaneParksWithoutDemandAndResumes(t *testing.T) {
	lane := &fakeLaneCollector{hostname: "test-host"}
	s := newSampler(lane, 0, 0)
	s.fastEvery = 20 * time.Millisecond
	s.slowEvery = 25 * time.Millisecond
	s.idleAfter = 60 * time.Millisecond
	s.Start()
	defer s.Stop()

	// The slow lane always runs one eager startup pass so the first snapshot
	// carries full sensor data even with nobody watching.
	waitForCond(t, time.Second, func() bool {
		return atomic.LoadInt64(&lane.slow) >= 1
	})

	// With no subscriber and no /api/metrics fetch, the slow lane parks after the
	// eager pass: many slow intervals elapse but the count stays at 1.
	time.Sleep(250 * time.Millisecond)
	if got := atomic.LoadInt64(&lane.slow); got != 1 {
		t.Fatalf("idle slow-lane passes = %d, want 1 (only the eager startup pass)", got)
	}

	// A Collect registers demand, so the slow lane must resume.
	if _, err := s.Collect(context.Background()); err != nil {
		t.Fatalf("Collect returned error: %v", err)
	}
	waitForCond(t, time.Second, func() bool {
		return atomic.LoadInt64(&lane.slow) >= 2
	})
}
