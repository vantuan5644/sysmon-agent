//go:build !linux && !windows

package main

import (
	"context"
	"os"
	"time"
)

type unsupportedCollector struct {
	hostname string
}

func NewSystemCollector() MetricsCollector {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	return unsupportedCollector{hostname: hostname}
}

func (c unsupportedCollector) Collect(ctx context.Context) (Metrics, error) {
	started := time.Now()
	metrics := baseMetrics(c.hostname)
	metrics.CPU = unavailableNumber("%", "unsupported operating system")
	metrics.CPUCores = unavailableCPUCores("unsupported operating system")
	metrics.CPUPower = unavailableNumber("W", "unsupported operating system")
	metrics.CPUClock = unavailableNumber("MHz", "unsupported operating system")
	metrics.CPUClockMax = unavailableNumber("MHz", "unsupported operating system")
	metrics.CPUTemperature = unavailableNumber("C", "unsupported operating system")
	metrics.PSUOutputPower = unavailableNumber("W", "unsupported operating system")
	metrics.Memory = unavailableCapacity("unsupported operating system")
	metrics.Disks = unavailableDisk("unsupported operating system")
	metrics.Network = NetworkSet{Available: false, Error: "unsupported operating system"}
	metrics.Tailscale = TailscaleStatus{Available: false, Error: "unsupported operating system"}
	metrics.Temperatures = TemperatureSet{Available: false, Error: "unsupported operating system"}
	metrics.GPU = GPUSet{Available: false, Error: "unsupported operating system"}
	return finishMetrics(metrics, started), nil
}
