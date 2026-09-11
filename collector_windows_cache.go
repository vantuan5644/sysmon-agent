//go:build windows

package main

import (
	"context"
	"time"
)

const (
	windowsSwapCacheTTL       = 60 * time.Second
	windowsStorageCacheTTL    = 5 * time.Minute
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

func (c *systemCollector) cachedWindowsStorageRows(ctx context.Context) ([]windowsPhysicalDisk, error) {
	now := time.Now()
	c.mu.Lock()
	rows, cachedErr, at := c.storageRows, c.storageRowsErr, c.storageRowsAt
	c.mu.Unlock()
	if !at.IsZero() && now.Sub(at) < windowsStorageCacheTTL {
		return rows, cachedErr
	}

	var fresh []windowsPhysicalDisk
	err := runPowerShellJSONArray(ctx, windowsStorageScript, &fresh)
	c.mu.Lock()
	c.storageRows, c.storageRowsErr, c.storageRowsAt = fresh, err, time.Now()
	c.mu.Unlock()
	return fresh, err
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
