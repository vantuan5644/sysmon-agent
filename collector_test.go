package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type panicOnceMetricsCollector struct {
	metrics Metrics
	calls   int
}

func (p *panicOnceMetricsCollector) Collect(ctx context.Context) (Metrics, error) {
	p.calls++
	if p.calls == 1 {
		panic("collector boom")
	}
	return p.metrics, nil
}

type sequenceMetricsCollector struct {
	metrics []Metrics
	errs    []error
	calls   int
}

func (s *sequenceMetricsCollector) Collect(ctx context.Context) (Metrics, error) {
	index := s.calls
	s.calls++
	if index < len(s.errs) && s.errs[index] != nil {
		return Metrics{}, s.errs[index]
	}
	if index < len(s.metrics) {
		return s.metrics[index], nil
	}
	return Metrics{}, nil
}

func TestCachedMetricsCollectorReusesFreshSuccessfulCollection(t *testing.T) {
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	inner := &sequenceMetricsCollector{metrics: []Metrics{
		baseMetrics("first"),
		baseMetrics("second"),
	}}
	cache := &cachedMetricsCollector{inner: inner, ttl: time.Second, now: func() time.Time { return now }}

	first, err := cache.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(500 * time.Millisecond)
	second, err := cache.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if first.Hostname != "first" || second.Hostname != "first" {
		t.Fatalf("cached metrics hostnames = %q/%q, want first/first", first.Hostname, second.Hostname)
	}
	if inner.calls != 1 {
		t.Fatalf("inner calls = %d, want one cached collection", inner.calls)
	}
}

func TestCachedMetricsCollectorExpiresAfterTTL(t *testing.T) {
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	inner := &sequenceMetricsCollector{metrics: []Metrics{
		baseMetrics("first"),
		baseMetrics("second"),
	}}
	cache := &cachedMetricsCollector{inner: inner, ttl: time.Second, now: func() time.Time { return now }}

	first, err := cache.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	second, err := cache.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if first.Hostname != "first" || second.Hostname != "second" {
		t.Fatalf("metrics hostnames = %q/%q, want first/second after TTL", first.Hostname, second.Hostname)
	}
	if inner.calls != 2 {
		t.Fatalf("inner calls = %d, want TTL refresh", inner.calls)
	}
}

func TestCachedMetricsCollectorDoesNotCacheErrors(t *testing.T) {
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	inner := &sequenceMetricsCollector{
		metrics: []Metrics{{}, baseMetrics("recovered")},
		errs:    []error{errors.New("boom")},
	}
	cache := &cachedMetricsCollector{inner: inner, ttl: time.Second, now: func() time.Time { return now }}

	_, err := cache.Collect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("first collection error = %v, want boom", err)
	}
	metrics, err := cache.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if metrics.Hostname != "recovered" {
		t.Fatalf("second collection hostname = %q, want recovered", metrics.Hostname)
	}
	if inner.calls != 2 {
		t.Fatalf("inner calls = %d, want retry after error", inner.calls)
	}
}

func TestCoalescingMetricsCollectorClearsInFlightAfterPanic(t *testing.T) {
	inner := &panicOnceMetricsCollector{metrics: completeCoreMetrics()}
	collector := newCoalescingMetricsCollector(inner)

	_, err := collector.Collect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "metrics collection panicked: collector boom") {
		t.Fatalf("first collection error = %v, want collector panic error", err)
	}

	metrics, err := collector.Collect(context.Background())
	if err != nil {
		t.Fatalf("second collection returned error: %v", err)
	}
	if metrics.Hostname != "test-host" {
		t.Fatalf("second collection hostname = %q, want test-host", metrics.Hostname)
	}
	if inner.calls != 2 {
		t.Fatalf("inner collector calls = %d, want 2", inner.calls)
	}
}

func TestCollectMetricAsyncStoresResultAndRecoversPanic(t *testing.T) {
	var wg sync.WaitGroup
	got := ""
	collectMetricAsync(&wg, &got, func() string {
		return "ok"
	}, func(recovered any) string {
		return "panic"
	})
	wg.Wait()
	if got != "ok" {
		t.Fatalf("successful async metric = %q, want ok", got)
	}

	collectMetricAsync(&wg, &got, func() string {
		panic("collector boom")
	}, func(recovered any) string {
		return "recovered: " + recovered.(string)
	})
	wg.Wait()
	if got != "recovered: collector boom" {
		t.Fatalf("recovered async metric = %q", got)
	}
}

func TestNonEmptyStringCacheCachesSuccessfulValue(t *testing.T) {
	var cache nonEmptyStringCache
	calls := 0

	got := cache.Get(func() string {
		calls++
		return "Microsoft Windows 11"
	})
	if got != "Microsoft Windows 11" {
		t.Fatalf("first cached value = %q, want Windows platform", got)
	}

	got = cache.Get(func() string {
		calls++
		return "unexpected"
	})
	if got != "Microsoft Windows 11" {
		t.Fatalf("second cached value = %q, want original Windows platform", got)
	}
	if calls != 1 {
		t.Fatalf("loader calls = %d, want one", calls)
	}
}

func TestNonEmptyStringCacheRetriesEmptyValue(t *testing.T) {
	var cache nonEmptyStringCache
	values := []string{"", "Microsoft Windows 11"}
	calls := 0

	if got := cache.Get(func() string {
		value := values[calls]
		calls++
		return value
	}); got != "" {
		t.Fatalf("first cached value = %q, want empty", got)
	}

	if got := cache.Get(func() string {
		value := values[calls]
		calls++
		return value
	}); got != "Microsoft Windows 11" {
		t.Fatalf("second cached value = %q, want retry value", got)
	}
	if calls != 2 {
		t.Fatalf("loader calls = %d, want retry after empty value", calls)
	}
}

func TestWaitForSampleHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := waitForSample(ctx, time.Second)
	if err == nil {
		t.Fatal("waitForSample returned nil for canceled context")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("waitForSample took %s after cancellation", elapsed)
	}
}

func TestWaitForSampleWaitsForDelay(t *testing.T) {
	start := time.Now()
	if err := waitForSample(context.Background(), 10*time.Millisecond); err != nil {
		t.Fatalf("waitForSample returned %v", err)
	}
	if elapsed := time.Since(start); elapsed < 8*time.Millisecond {
		t.Fatalf("waitForSample returned before delay: %s", elapsed)
	}
}

func TestNetworkSampleElapsedSecondsUsesActualElapsed(t *testing.T) {
	start := time.Unix(100, 0)
	got := networkSampleElapsedSeconds(start, start.Add(750*time.Millisecond), 250*time.Millisecond)
	if got != 0.75 {
		t.Fatalf("networkSampleElapsedSeconds = %v, want 0.75", got)
	}
}

func TestNetworkSampleElapsedSecondsFallsBackForNonAdvancingClock(t *testing.T) {
	start := time.Unix(100, 0)
	got := networkSampleElapsedSeconds(start, start.Add(-time.Second), 250*time.Millisecond)
	if got != 0.25 {
		t.Fatalf("networkSampleElapsedSeconds = %v, want 0.25", got)
	}
}

func TestSortNetworkInterfacesByActivity(t *testing.T) {
	interfaces := []NetworkInterfaceMetric{
		{
			Name:             "zzz-idle",
			RXBytesPerSecond: availableNumber(0, "B/s"),
			TXBytesPerSecond: availableNumber(0, "B/s"),
		},
		{
			Name:             "mmm-warming",
			RXBytesPerSecond: unavailableNumber("B/s", "warming up"),
			TXBytesPerSecond: unavailableNumber("B/s", "warming up"),
		},
		{
			Name:             "bbb-medium",
			RXBytesPerSecond: availableNumber(15, "B/s"),
			TXBytesPerSecond: unavailableNumber("B/s", "not reported"),
		},
		{
			Name:             "aaa-fast",
			RXBytesPerSecond: availableNumber(10, "B/s"),
			TXBytesPerSecond: availableNumber(20, "B/s"),
		},
	}

	sortNetworkInterfacesByActivity(interfaces)

	got := []string{interfaces[0].Name, interfaces[1].Name, interfaces[2].Name, interfaces[3].Name}
	want := []string{"aaa-fast", "bbb-medium", "zzz-idle", "mmm-warming"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sorted interfaces = %+v, want %+v", got, want)
		}
	}
}

func TestNetworkCounterRate(t *testing.T) {
	got := networkCounterRate(100, 250, 0.5)
	if !got.Available || got.Value != 300 || got.Unit != "B/s" {
		t.Fatalf("networkCounterRate valid sample = %+v, want 300 B/s", got)
	}
}

func TestNetworkCounterRateRejectsInvalidSamples(t *testing.T) {
	for _, tc := range []struct {
		name     string
		previous uint64
		current  uint64
		seconds  float64
		wantErr  string
	}{
		{name: "reset", previous: 250, current: 100, seconds: 1, wantErr: "network counter reset"},
		{name: "zero interval", previous: 100, current: 250, seconds: 0, wantErr: "invalid network sample interval"},
		{name: "negative interval", previous: 100, current: 250, seconds: -1, wantErr: "invalid network sample interval"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := networkCounterRate(tc.previous, tc.current, tc.seconds)
			if got.Available {
				t.Fatalf("networkCounterRate = %+v, want unavailable", got)
			}
			if got.Unit != "B/s" || !strings.Contains(got.Error, tc.wantErr) {
				t.Fatalf("networkCounterRate = %+v, want %q error", got, tc.wantErr)
			}
		})
	}
}
