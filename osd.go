package main

import (
	"context"
	"fmt"
	"time"
)

// OSD hardware is sampled independently from the dashboard's full slow lane.
// CPU utilization remains owned by the existing fast lane.
type osdHardware struct {
	Timestamp       time.Time
	CPUTemperature  NumberMetric
	TemperatureName string
	GPU             GPUSet
}

type osdHardwareCollector interface {
	CollectOSDHardware(context.Context) osdHardware
}

type osdMetrics struct {
	Timestamp            time.Time    `json:"timestamp"`
	CPU                  NumberMetric `json:"cpu_percent"`
	CPUTemperature       NumberMetric `json:"cpu_temperature"`
	CPUTemperatureSensor string       `json:"cpu_temperature_sensor,omitempty"`
	GPU                  GPUSet       `json:"gpu"`
}

const osdSampleInterval = time.Second
const osdFreshFor = 3500 * time.Millisecond

func collectOSDSafely(ctx context.Context, collector osdHardwareCollector) (hardware osdHardware) {
	defer func() {
		if failure := recover(); failure != nil {
			message := fmt.Sprintf("OSD hardware collection failed: %v", failure)
			hardware = osdHardware{CPUTemperature: unavailableNumber("C", message), GPU: GPUSet{Error: message}}
		}
	}()
	return collector.CollectOSDHardware(ctx)
}

func (s *sampler) OSDSnapshot() osdMetrics {
	now := time.Now()
	s.mu.Lock()
	s.lastOSDDemandAt = now
	cpu, timestamp, hardware := s.snapshot.CPU, s.snapshot.Timestamp, s.osdHardware
	s.mu.Unlock()
	if timestamp.IsZero() || now.Sub(timestamp) > osdFreshFor {
		cpu = unavailableNumber("%", "CPU sample unavailable or stale")
	}
	if hardware.Timestamp.IsZero() || now.Sub(hardware.Timestamp) > osdFreshFor {
		hardware.CPUTemperature = unavailableNumber("C", "OSD hardware sample unavailable or stale")
		hardware.TemperatureName = ""
		hardware.GPU = GPUSet{Available: false, Error: "OSD hardware sample unavailable or stale"}
	}
	return osdMetrics{Timestamp: now.UTC(), CPU: cpu, CPUTemperature: hardware.CPUTemperature,
		CPUTemperatureSensor: hardware.TemperatureName, GPU: hardware.GPU}
}

func (s *sampler) runOSDLoop(ctx context.Context, collector osdHardwareCollector) {
	defer s.wg.Done()
	for {
		s.mu.Lock()
		active := !s.lastOSDDemandAt.IsZero() && time.Since(s.lastOSDDemandAt) < s.idleAfter
		s.mu.Unlock()
		if active {
			hardware := collectOSDSafely(ctx, collector)
			hardware.Timestamp = time.Now().UTC()
			s.mu.Lock()
			s.osdHardware = hardware
			s.mu.Unlock()
		}
		if sleepCanceled(ctx, osdSampleInterval) {
			return
		}
	}
}
