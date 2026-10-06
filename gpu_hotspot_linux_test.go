//go:build linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sysmon-agent/internal/gpuhotspot"
)

func TestLinuxGPUHotspotFileContract(t *testing.T) {
	now := time.Now().UTC()
	value := 82.0
	path := filepath.Join(t.TempDir(), "readings.json")
	gpu := GPUSet{Available: true, Devices: []GPUMetric{{Name: "4090", PCIBusID: "00000000:02:00.0"}, {Name: "3090", PCIBusID: "00000000:01:00.0", Usage: availableNumber(5, "%")}}}
	snapshot := gpuhotspot.Snapshot{Timestamp: now, Devices: []gpuhotspot.Device{{PCIBusID: "0000:01:00.0", Celsius: &value}}}
	write := func() {
		t.Helper()
		data, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	write()
	got := mergeLinuxGPUHotspots(gpu, path, now)
	if got.Devices[0].HotspotTemperature.Available || got.Devices[1].HotspotTemperature.Value != 82 {
		t.Fatalf("association: %+v", got)
	}
	if gpu.Devices[1].HotspotTemperature.Available {
		t.Fatal("mutated original")
	}
	for _, tc := range []struct {
		name   string
		change func()
	}{
		{"stale", func() { snapshot.Timestamp = now.Add(-7 * time.Second); write() }},
		{"future", func() { snapshot.Timestamp = now.Add(time.Hour); write() }},
		{"duplicate", func() {
			snapshot.Timestamp = now
			snapshot.Devices = append(snapshot.Devices, snapshot.Devices[0])
			write()
		}},
		{"corrupt", func() { os.WriteFile(path, []byte("{"), 0644) }},
		{"oversized", func() { os.WriteFile(path, []byte(strings.Repeat(" ", gpuhotspot.MaxBytes+1)), 0644) }},
		{"missing", func() { os.Remove(path) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.change()
			got := mergeLinuxGPUHotspots(got, path, now)
			if got.Devices[1].HotspotTemperature.Available || !got.Devices[1].Usage.Available {
				t.Fatalf("failed contract: %+v", got)
			}
		})
	}
}
