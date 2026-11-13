//go:build windows

package main

import (
	"context"
	"strings"
	"sync"
	"testing"

	"sysmon-agent/internal/gpuguard"
)

func TestGPUGuardSampleUsesTheGPUOnlyDaemonRequest(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []string
	)
	fake := &fakeDaemonProcess{behaviors: []func(string) (string, fakeAction){func(line string) (string, fakeAction) {
		mu.Lock()
		requests = append(requests, line)
		mu.Unlock()
		return `{"available":true,"temperatures":[],"gpu":[` +
			`{"name":"Intel(R) UHD Graphics","temperature_celsius":{"available":true,"value":40,"unit":"C"}},` +
			`{"name":"NVIDIA GeForce RTX 4090","usage_percent":{"available":true,"value":97},"temperature_celsius":{"available":true,"value":71,"unit":"C"},` +
			`"hotspot_temperature_celsius":{"available":true,"value":84.5,"unit":"C"},"power_watts":{"available":true,"value":431.2,"unit":"W"}}]}`, fakeRespond
	}}}
	installFakeDaemon(t, fake)
	c := &systemCollector{useDaemon: true}
	t.Cleanup(func() {
		if c.daemon != nil {
			c.daemon.Close()
		}
	})

	sample := c.ReadGPUGuardSample(context.Background())
	if sample.Error != "" || sample.GPU != "NVIDIA GeForce RTX 4090" {
		t.Fatalf("sample %+v", sample)
	}
	if sample.Core == nil || *sample.Core != 71 || sample.Hotspot == nil || *sample.Hotspot != 84.5 || sample.Power == nil || *sample.Power != 431.2 {
		t.Fatalf("readings core %v hotspot %v power %v", sample.Core, sample.Hotspot, sample.Power)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(requests, ",") != "gpu" {
		t.Fatalf("daemon requests %v, want one gpu request", requests)
	}
	// Every two seconds without end: it must not count toward the full
	// reads that schedule a PSU rescan.
	if c.daemon.reads != 0 {
		t.Fatalf("gpu request counted as %d full reads", c.daemon.reads)
	}
}

func TestGPUGuardSampleSaysWhyWhenTheBridgeFails(t *testing.T) {
	fake := &fakeDaemonProcess{behaviors: []func(string) (string, fakeAction){func(string) (string, fakeAction) {
		return `{"available":false,"error":"LibreHardwareMonitorLib.dll not found"}`, fakeRespond
	}}}
	installFakeDaemon(t, fake)
	c := &systemCollector{useDaemon: true}
	t.Cleanup(func() {
		if c.daemon != nil {
			c.daemon.Close()
		}
	})
	sample := c.ReadGPUGuardSample(context.Background())
	if sample.Hotspot != nil || !strings.Contains(sample.Error, "LibreHardwareMonitorLib.dll not found") {
		t.Fatalf("sample %+v", sample)
	}
}

func TestPickGuardGPU(t *testing.T) {
	hot := availableNumber(80, "C")
	if _, ok := pickGuardGPU([]GPUMetric{{Name: "AMD Radeon"}, {Name: "Intel UHD"}}); ok {
		t.Fatal("picked a GPU of unknown make among two")
	}
	if device, ok := pickGuardGPU([]GPUMetric{{Name: "Some GPU", HotspotTemperature: hot}}); !ok || device.Name != "Some GPU" {
		t.Fatal("a lone GPU with a hotspot was not taken")
	}
	if _, ok := pickGuardGPU([]GPUMetric{{Name: "Some GPU"}}); ok {
		t.Fatal("a lone GPU without a hotspot was taken")
	}
	sample := guardSampleFromGPU(gpuguard.Sample{}, GPUMetric{Name: "NVIDIA", Temperature: unavailableNumber("C", "no core"), HotspotTemperature: unavailableNumber("C", "no hotspot")})
	if sample.Core != nil || sample.Hotspot != nil || sample.Error != "no core no hotspot" {
		t.Fatalf("sample %+v", sample)
	}
}
