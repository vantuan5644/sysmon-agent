package main

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

type MetricsCollector interface {
	Collect(ctx context.Context) (Metrics, error)
}

const metricsCacheTTL = 250 * time.Millisecond

type cachedMetricsCollector struct {
	inner MetricsCollector
	ttl   time.Duration
	now   func() time.Time

	mu          sync.Mutex
	hasMetrics  bool
	metrics     Metrics
	collectedAt time.Time
}

func newCachedMetricsCollector(inner MetricsCollector, ttl time.Duration) MetricsCollector {
	return &cachedMetricsCollector{inner: inner, ttl: ttl, now: time.Now}
}

func (c *cachedMetricsCollector) Collect(ctx context.Context) (Metrics, error) {
	now := c.now()
	c.mu.Lock()
	if c.hasMetrics && now.Sub(c.collectedAt) < c.ttl {
		metrics := c.metrics
		c.mu.Unlock()
		return metrics, nil
	}
	c.mu.Unlock()

	metrics, err := c.inner.Collect(ctx)
	if err != nil {
		return Metrics{}, err
	}

	c.mu.Lock()
	c.hasMetrics = true
	c.metrics = metrics
	c.collectedAt = c.now()
	c.mu.Unlock()

	return metrics, nil
}

type coalescingMetricsCollector struct {
	inner    MetricsCollector
	mu       sync.Mutex
	inFlight *metricsCollection
}

type metricsCollection struct {
	done    chan struct{}
	metrics Metrics
	err     error
}

func newCoalescingMetricsCollector(inner MetricsCollector) MetricsCollector {
	return &coalescingMetricsCollector{inner: inner}
}

func collectMetricAsync[T any](wg *sync.WaitGroup, target *T, collect func() T, fallback func(any) T) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if recovered := recover(); recovered != nil {
				*target = fallback(recovered)
			}
		}()
		*target = collect()
	}()
}

func (c *coalescingMetricsCollector) Collect(ctx context.Context) (metrics Metrics, err error) {
	c.mu.Lock()
	if c.inFlight != nil {
		inFlight := c.inFlight
		c.mu.Unlock()
		select {
		case <-inFlight.done:
			return inFlight.metrics, inFlight.err
		case <-ctx.Done():
			return Metrics{}, ctx.Err()
		}
	}

	inFlight := &metricsCollection{done: make(chan struct{})}
	c.inFlight = inFlight
	c.mu.Unlock()

	defer func() {
		recovered := recover()
		if recovered != nil {
			metrics = Metrics{}
			err = fmt.Errorf("metrics collection panicked: %v", recovered)
		}
		inFlight.metrics = metrics
		inFlight.err = err

		c.mu.Lock()
		if c.inFlight == inFlight {
			c.inFlight = nil
		}
		close(inFlight.done)
		c.mu.Unlock()
	}()

	metrics, err = c.inner.Collect(ctx)
	return metrics, err
}

const minNetworkSampleSeconds = 0.2

type nonEmptyStringCache struct {
	mu    sync.Mutex
	value string
}

func (c *nonEmptyStringCache) Get(load func() string) string {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.value != "" {
		return c.value
	}
	value := load()
	if value != "" {
		c.value = value
	}
	return value
}

func waitForSample(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func networkSampleElapsedSeconds(previousAt, currentAt time.Time, fallback time.Duration) float64 {
	elapsed := currentAt.Sub(previousAt).Seconds()
	if elapsed > 0 {
		return elapsed
	}
	return fallback.Seconds()
}

func sortNetworkInterfacesByActivity(interfaces []NetworkInterfaceMetric) {
	sort.SliceStable(interfaces, func(i, j int) bool {
		leftRate, leftKnown := networkInterfaceRate(interfaces[i])
		rightRate, rightKnown := networkInterfaceRate(interfaces[j])
		if leftKnown != rightKnown {
			return leftKnown
		}
		if leftRate != rightRate {
			return leftRate > rightRate
		}
		return interfaces[i].Name < interfaces[j].Name
	})
}

func networkInterfaceRate(iface NetworkInterfaceMetric) (float64, bool) {
	total := 0.0
	known := false
	for _, metric := range []NumberMetric{iface.RXBytesPerSecond, iface.TXBytesPerSecond} {
		if metric.Available && isFinite(metric.Value) {
			total += metric.Value
			known = true
		}
	}
	return total, known
}

func networkCounterRate(previous, current uint64, seconds float64) NumberMetric {
	if seconds <= 0 {
		return unavailableNumber("B/s", "invalid network sample interval")
	}
	if current < previous {
		return unavailableNumber("B/s", "network counter reset")
	}
	return availableNumber(float64(current-previous)/seconds, "B/s")
}
