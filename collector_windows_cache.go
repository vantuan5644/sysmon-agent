//go:build windows

package main

import (
	"context"
	"fmt"
	"time"
)

const (
	windowsSwapCacheTTL    = 60 * time.Second
	windowsStorageCacheTTL = 5 * time.Minute
	// windowsStorageRetryAfter replaces the 5-minute TTL once a discovery pass
	// has failed. Holding a failure for the full TTL meant one timed-out query
	// blanked the storage panel - and raised a "storage:" entry in
	// collection_errors - for five minutes, long after the condition that
	// caused it had passed.
	windowsStorageRetryAfter = 45 * time.Second
	// windowsStorageStaleGrace bounds how long the last successful discovery
	// keeps being served while fresh passes keep failing. Drive identity and
	// capacity move slowly and the temperatures layered onto these rows come
	// from the bridge on every pass, so stale rows are worth far more than an
	// empty panel - but not forever, or a removed drive would haunt the
	// dashboard indefinitely.
	windowsStorageStaleGrace = 15 * time.Minute
	// windowsStorageDiscoveryWait is how long the first sample waits for the
	// first discovery, and how long a later refresh may run before it is
	// reported as unfinished. It covers windowsDeviceReadBudget, the longest a
	// discovery waits on a slow drive.
	windowsStorageDiscoveryWait = 6 * time.Second

	windowsTailscaleTTL       = 15 * time.Second
	windowsUplinkCacheTTL     = 60 * time.Second
	windowsGPUCacheTTL        = 5 * time.Second
	windowsProcessGPUCacheTTL = 15 * time.Second
	windowsACPICacheTTL       = 5 * time.Minute
)

func (c *systemCollector) resolveWindowsClockBase(ctx context.Context) (NumberMetric, float64, bool) {
	c.clockBaseOnce.Do(func() {
		var result struct {
			MaxClockSpeed *float64
		}
		if err := runPowerShellJSON(ctx, `Get-CimInstance Win32_Processor | Select-Object -First 1 MaxClockSpeed`, &result); err != nil {
			c.clockBase = unavailableNumber("MHz", err.Error())
			return
		}
		if value, ok := windowsClockMHz(result.MaxClockSpeed); ok {
			c.clockBase = availableNumber(value, "MHz")
			c.clockBaseMHz = value
			c.clockBaseOK = true
			return
		}
		c.clockBase = unavailableNumber("MHz", "Win32_Processor did not report MaxClockSpeed")
	})
	return c.clockBase, c.clockBaseMHz, c.clockBaseOK
}

func (c *systemCollector) cachedWindowsSwap(ctx context.Context) CapacityMetric {
	now := time.Now()
	c.mu.Lock()
	value, at := c.swapCached, c.swapCachedAt
	c.mu.Unlock()
	if !at.IsZero() && now.Sub(at) < windowsSwapCacheTTL {
		return value
	}
	value = windowsSwap(ctx)
	c.mu.Lock()
	c.swapCached, c.swapCachedAt = value, time.Now()
	c.mu.Unlock()
	return value
}

// cachedWindowsStorageRows serves physical-disk discovery from a cache that is
// refreshed in the background on a long TTL. When a refresh fails it keeps
// serving the last successful rows for a bounded grace period rather than
// reporting the whole set unavailable, and retries well before the success TTL
// so a momentary miss does not look like a broken collector.
//
// The slow lane never waits on a refresh. It used to run inline, so a
// discovery stalled by one slow drive held back power, temperatures, GPU and
// network with it: 8 s of every 45 s while BBLWIN's ZEPHYR recorder was
// plugged in (see collector_windows_storage.go). Only the very first discovery
// is waited for, briefly, so the first sample and -self-check have rows.
func (c *systemCollector) cachedWindowsStorageRows(ctx context.Context) ([]windowsPhysicalDisk, error) {
	c.mu.Lock()
	c.startWindowsStorageRefreshLocked(time.Now())
	pending, first := c.storageRefresh, c.storageAttemptAt.IsZero()
	c.mu.Unlock()

	if pending != nil && first {
		timer := time.NewTimer(windowsStorageDiscoveryWait)
		select {
		case <-pending:
		case <-timer.C:
		case <-ctx.Done():
		}
		timer.Stop()
	}

	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.storageRowsErr
	// A refresh that outlives the first-sample wait is the current story, so
	// it replaces the last completed attempt's error. It still only surfaces
	// once the last good rows are past windowsStorageStaleGrace.
	if c.storageRefresh != nil && (c.storageAttemptAt.IsZero() || now.Sub(c.storageRefreshAt) >= windowsStorageDiscoveryWait) {
		err = fmt.Errorf("storage discovery has not finished after %s", now.Sub(c.storageRefreshAt).Round(100*time.Millisecond))
	}
	return windowsStorageRowsOrError(c.storageRows, err, c.storageRowsAt, now)
}

// startWindowsStorageRefreshLocked starts a background discovery when the
// cached one is due and none is running. One at a time: a discovery stuck on
// a hung device blocks only the next refresh, never piles up goroutines. The
// caller holds c.mu.
func (c *systemCollector) startWindowsStorageRefreshLocked(now time.Time) {
	if c.storageRefresh != nil {
		return
	}
	ttl := windowsStorageCacheTTL
	if c.storageRowsErr != nil {
		ttl = windowsStorageRetryAfter
	}
	if !c.storageAttemptAt.IsZero() && now.Sub(c.storageAttemptAt) < ttl {
		return
	}
	discover := c.storageDiscover
	if discover == nil {
		exclude := c.storageExclude
		discover = func() ([]windowsPhysicalDisk, error) { return discoverWindowsPhysicalDisks(exclude) }
	}
	done := make(chan struct{})
	c.storageRefresh, c.storageRefreshAt = done, now
	go c.runWindowsStorageRefresh(discover, done)
}

func (c *systemCollector) runWindowsStorageRefresh(discover func() ([]windowsPhysicalDisk, error), done chan struct{}) {
	var fresh []windowsPhysicalDisk
	var err error
	func() {
		// Nothing above this goroutine recovers a panic, and an unrecovered one
		// would take the whole agent down.
		defer func() {
			if recovered := recover(); recovered != nil {
				fresh, err = nil, fmt.Errorf("storage discovery panicked: %v", recovered)
			}
		}()
		fresh, err = discover()
	}()
	completed := time.Now()
	c.mu.Lock()
	c.storageRowsErr, c.storageAttemptAt = err, completed
	if err == nil {
		c.storageRows, c.storageRowsAt = fresh, completed
	}
	c.storageRefresh = nil
	c.mu.Unlock()
	close(done)
}

// windowsStorageRowsOrError picks between the last good discovery and the error
// from the pass that just failed.
func windowsStorageRowsOrError(rows []windowsPhysicalDisk, err error, goodAt, now time.Time) ([]windowsPhysicalDisk, error) {
	if err == nil {
		return rows, nil
	}
	if len(rows) > 0 && !goodAt.IsZero() && now.Sub(goodAt) <= windowsStorageStaleGrace {
		return rows, nil
	}
	return nil, err
}

func (c *systemCollector) cachedTailscaleStatus(ctx context.Context) TailscaleStatus {
	now := time.Now()
	c.mu.Lock()
	value, at := c.tailscaleCached, c.tailscaleAt
	c.mu.Unlock()
	if !at.IsZero() && now.Sub(at) < windowsTailscaleTTL {
		return value
	}
	value = readTailscaleStatus(ctx)
	c.mu.Lock()
	c.tailscaleCached, c.tailscaleAt = value, time.Now()
	c.mu.Unlock()
	return value
}

func (c *systemCollector) cachedWindowsGPU(ctx context.Context) GPUSet {
	now := time.Now()
	c.mu.Lock()
	value, at := c.gpuCached, c.gpuCachedAt
	c.mu.Unlock()
	if !at.IsZero() && now.Sub(at) < windowsGPUCacheTTL {
		return value
	}
	c.nvidiaMu.Lock()
	defer c.nvidiaMu.Unlock()
	value = c.windowsGPU(ctx)
	c.mu.Lock()
	c.gpuCached, c.gpuCachedAt = value, time.Now()
	c.mu.Unlock()
	return value
}

func (c *systemCollector) cachedNVIDIAProcessGPUMemory(ctx context.Context) map[int]int64 {
	now := time.Now()
	c.mu.Lock()
	value, at := c.processGPUMemory, c.processGPUMemoryAt
	c.mu.Unlock()
	if !at.IsZero() && now.Sub(at) < windowsProcessGPUCacheTTL {
		return value
	}
	c.nvidiaMu.Lock()
	defer c.nvidiaMu.Unlock()
	value = nvidiaProcessGPUMemory(ctx)
	c.mu.Lock()
	c.processGPUMemory, c.processGPUMemoryAt = value, time.Now()
	c.mu.Unlock()
	return value
}

func (c *systemCollector) cachedWindowsACPITemperatures(ctx context.Context) []TemperatureMetric {
	now := time.Now()
	c.mu.Lock()
	value, at := c.acpiSensors, c.acpiSensorsAt
	c.mu.Unlock()
	if !at.IsZero() && now.Sub(at) < windowsACPICacheTTL {
		return value
	}
	value = windowsACPITemperatureSensors(ctx)
	c.mu.Lock()
	c.acpiSensors, c.acpiSensorsAt = value, time.Now()
	c.mu.Unlock()
	return value
}
