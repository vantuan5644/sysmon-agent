//go:build windows

package main

import (
	"context"
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
	windowsStorageStaleGrace  = 15 * time.Minute
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

// cachedWindowsStorageRows refreshes physical-disk discovery on a long TTL and,
// when a refresh fails, keeps serving the last successful rows for a bounded
// grace period rather than reporting the whole set unavailable. Failures are
// also retried well before the success TTL: the observed failure here was a
// transient one (the query measured 2.0 s against its budget on an idle host
// and exited 0), so holding it for five minutes made a momentary miss look like
// a broken collector.
func (c *systemCollector) cachedWindowsStorageRows(ctx context.Context) ([]windowsPhysicalDisk, error) {
	now := time.Now()
	c.mu.Lock()
	rows, lastErr, goodAt, attemptAt := c.storageRows, c.storageRowsErr, c.storageRowsAt, c.storageAttemptAt
	c.mu.Unlock()

	ttl := windowsStorageCacheTTL
	if lastErr != nil {
		ttl = windowsStorageRetryAfter
	}
	if !attemptAt.IsZero() && now.Sub(attemptAt) < ttl {
		return windowsStorageRowsOrError(rows, lastErr, goodAt, now)
	}

	var fresh []windowsPhysicalDisk
	err := runPowerShellJSONArrayWithTimeout(ctx, windowsStorageScript, &fresh, windowsStorageQueryTimeout)
	completed := time.Now()
	c.mu.Lock()
	c.storageRowsErr, c.storageAttemptAt = err, completed
	if err == nil {
		c.storageRows, c.storageRowsAt = fresh, completed
	}
	rows, goodAt = c.storageRows, c.storageRowsAt
	c.mu.Unlock()
	return windowsStorageRowsOrError(rows, err, goodAt, completed)
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
