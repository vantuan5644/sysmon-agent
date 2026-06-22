package main

import (
	"strings"
	"testing"
	"time"
)

func TestCheckClientCheckValidatesHistoryEndpoint(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	if err := checkClientCheck(handler); err != nil {
		t.Fatalf("checkClientCheck returned %v, want history endpoint validation to pass", err)
	}
}

func TestValidateMetricsShapeAcceptsUnavailableOptionalTelemetry(t *testing.T) {
	metrics := completeCoreMetrics()
	metrics.Network = NetworkSet{Available: false, Error: "network sampler is warming up"}
	metrics.Temperatures = TemperatureSet{Available: false, Error: "no supported temperature sensors found"}
	metrics.GPU = GPUSet{Available: false, Error: "nvidia-smi not found"}

	if err := validateMetricsShape(metrics); err != nil {
		t.Fatalf("validateMetricsShape rejected degraded metrics: %v", err)
	}
}

func TestValidateMetricsShapeAcceptsAvailableOptionalTelemetry(t *testing.T) {
	metrics := completeCoreMetrics()
	metrics.Network = NetworkSet{
		Available: true,
		Interfaces: []NetworkInterfaceMetric{{
			Name:             "eth0",
			RXBytesTotal:     1000,
			TXBytesTotal:     500,
			RXBytesPerSecond: availableNumber(10, "B/s"),
			TXBytesPerSecond: availableNumber(5, "B/s"),
		}},
	}
	metrics.Temperatures = TemperatureSet{
		Available: true,
		Sensors: []TemperatureMetric{
			{Name: "CPU", Celsius: availableNumber(42, "C")},
			{Name: "Chipset", Celsius: unavailableNumber("C", "sensor temporarily unavailable")},
		},
	}
	metrics.GPU = GPUSet{
		Available: true,
		Devices: []GPUMetric{{
			Name:        "GPU",
			Usage:       availableNumber(20, "%"),
			Memory:      unavailableCapacity("live VRAM usage unavailable"),
			Temperature: unavailableNumber("C", "GPU temperature unavailable"),
		}},
	}

	if err := validateMetricsShape(metrics); err != nil {
		t.Fatalf("validateMetricsShape rejected available optional metrics: %v", err)
	}
}

func TestValidateMetricsShapeRequiresUnavailableTemperatureSensorErrors(t *testing.T) {
	metrics := completeCoreMetrics()
	metrics.Temperatures = TemperatureSet{
		Available: true,
		Sensors:   []TemperatureMetric{{Name: "CPU", Celsius: unavailableNumber("C", "")}},
	}

	err := validateMetricsShape(metrics)
	if err == nil || !strings.Contains(err.Error(), "temperatures.sensors[0].celsius unavailable without an error") {
		t.Fatalf("validateMetricsShape error = %v, want unavailable temperature sensor error failure", err)
	}
}

func TestValidateMetricsShapeRejectsUnavailableCoreMetrics(t *testing.T) {
	metrics := completeCoreMetrics()
	metrics.CPU = unavailableNumber("%", "missing")

	err := validateMetricsShape(metrics)
	if err == nil || !strings.Contains(err.Error(), "cpu_percent") {
		t.Fatalf("validateMetricsShape error = %v, want cpu_percent failure", err)
	}
}

func TestValidateMetricsShapeRejectsPercentOutOfRange(t *testing.T) {
	metrics := completeCoreMetrics()
	metrics.CPU = NumberMetric{Available: true, Value: 101, Unit: "%"}

	err := validateMetricsShape(metrics)
	if err == nil || !strings.Contains(err.Error(), "cpu_percent percent out of range") {
		t.Fatalf("validateMetricsShape error = %v, want percent range failure", err)
	}
}

func TestValidateMetricsShapeRequiresUnavailableErrors(t *testing.T) {
	metrics := completeCoreMetrics()
	metrics.GPU = GPUSet{Available: false}

	err := validateMetricsShape(metrics)
	if err == nil || !strings.Contains(err.Error(), "gpu unavailable without an error") {
		t.Fatalf("validateMetricsShape error = %v, want unavailable GPU error failure", err)
	}
}

func TestValidateMetricsShapeRejectsEmptyCollectionErrors(t *testing.T) {
	metrics := completeCoreMetrics()
	metrics.CollectionErrors = []string{"network: warming", " "}

	err := validateMetricsShape(metrics)
	if err == nil || !strings.Contains(err.Error(), "collection_errors[1] is empty") {
		t.Fatalf("validateMetricsShape error = %v, want empty collection error failure", err)
	}
}

func TestValidateMetricsShapeRejectsNegativeCollectionDuration(t *testing.T) {
	metrics := completeCoreMetrics()
	metrics.CollectionDurationMS = -1

	err := validateMetricsShape(metrics)
	if err == nil || !strings.Contains(err.Error(), "collection_duration_ms must be non-negative") {
		t.Fatalf("validateMetricsShape error = %v, want collection_duration_ms failure", err)
	}
}

func TestValidateMetricsShapeRequiresRuntimeMetadata(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*Metrics)
		wantErr string
	}{
		{
			name:    "hostname",
			mutate:  func(metrics *Metrics) { metrics.Hostname = "" },
			wantErr: "hostname is required",
		},
		{
			name:    "os",
			mutate:  func(metrics *Metrics) { metrics.OS = "" },
			wantErr: "os is required",
		},
		{
			name:    "arch",
			mutate:  func(metrics *Metrics) { metrics.Arch = "" },
			wantErr: "arch is required",
		},
		{
			name:    "timestamp",
			mutate:  func(metrics *Metrics) { metrics.Timestamp = time.Time{} },
			wantErr: "timestamp is required",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metrics := completeCoreMetrics()
			tc.mutate(&metrics)

			err := validateMetricsShape(metrics)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateMetricsShape error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateMetricsTimestampFreshness(t *testing.T) {
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		timestamp time.Time
		wantErr   string
	}{
		{name: "current", timestamp: now},
		{name: "small future skew", timestamp: now.Add(5 * time.Second)},
		{name: "empty", timestamp: time.Time{}, wantErr: "is empty"},
		{name: "future", timestamp: now.Add(6 * time.Second), wantErr: "is in the future"},
		{name: "stale", timestamp: now.Add(-61 * time.Second), wantErr: "is stale"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateMetricsTimestampFreshness(tc.timestamp, now)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateMetricsTimestampFreshness returned %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateMetricsTimestampFreshness error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func completeCoreMetrics() Metrics {
	metrics := baseMetrics("test-host")
	metrics.CPU = availableNumber(12.5, "%")
	metrics.Memory = availableCapacity(512, 1024)
	metrics.Disks = []DiskMetric{{
		Name:       "root",
		Mountpoint: "/",
		FSType:     "ext4",
		Capacity:   availableCapacity(50, 100),
	}}
	metrics.Network = NetworkSet{Available: false, Error: "network sampler is warming up"}
	metrics.Temperatures = TemperatureSet{Available: false, Error: "no supported temperature sensors found"}
	metrics.GPU = GPUSet{Available: false, Error: "GPU telemetry unavailable"}
	return metrics
}
