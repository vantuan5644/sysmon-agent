//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCollectLinuxDRMGPUAMD(t *testing.T) {
	root := t.TempDir()
	device := filepath.Join(root, "class", "drm", "card0", "device")
	files := map[string]string{
		filepath.Join(device, "vendor"):                         "0x1002\n",
		filepath.Join(device, "gpu_busy_percent"):               "17\n",
		filepath.Join(device, "mem_info_vram_used"):             "1073741824\n",
		filepath.Join(device, "mem_info_vram_total"):            "4294967296\n",
		filepath.Join(device, "hwmon", "hwmon3", "temp1_input"): "65000\n",
		filepath.Join(device, "hwmon", "hwmon3", "power1_average"): "225000000\n",
	}
	for path, value := range files {
		if err := writeTestFile(path, value); err != nil {
			t.Fatal(err)
		}
	}

	got := collectLinuxDRMGPU(root)
	if !got.Available || len(got.Devices) != 1 {
		t.Fatalf("GPU set = %+v", got)
	}
	deviceMetric := got.Devices[0]
	if deviceMetric.Name != "AMD GPU card0" {
		t.Fatalf("name = %q", deviceMetric.Name)
	}
	if !deviceMetric.Usage.Available || deviceMetric.Usage.Value != 17 {
		t.Fatalf("usage = %+v", deviceMetric.Usage)
	}
	if !deviceMetric.Memory.Available || deviceMetric.Memory.Percent != 25 {
		t.Fatalf("memory = %+v", deviceMetric.Memory)
	}
	if !deviceMetric.Temperature.Available || deviceMetric.Temperature.Value != 65 {
		t.Fatalf("temperature = %+v", deviceMetric.Temperature)
	}
	if !deviceMetric.Power.Available || deviceMetric.Power.Value != 225 || deviceMetric.Power.Unit != "W" {
		t.Fatalf("power = %+v, want 225 W", deviceMetric.Power)
	}
}

func TestCollectLinuxDRMGPUIntelPartial(t *testing.T) {
	root := t.TempDir()
	device := filepath.Join(root, "class", "drm", "card1", "device")
	if err := writeTestFile(filepath.Join(device, "vendor"), "0x8086\n"); err != nil {
		t.Fatal(err)
	}

	got := collectLinuxDRMGPU(root)
	if !got.Available || len(got.Devices) != 1 {
		t.Fatalf("GPU set = %+v", got)
	}
	deviceMetric := got.Devices[0]
	if deviceMetric.Name != "Intel GPU card1" {
		t.Fatalf("name = %q", deviceMetric.Name)
	}
	if deviceMetric.Usage.Available {
		t.Fatalf("usage unexpectedly available: %+v", deviceMetric.Usage)
	}
	if deviceMetric.Memory.Available {
		t.Fatalf("memory unexpectedly available: %+v", deviceMetric.Memory)
	}
}

func TestCollectLinuxDRMGPUFiltersConnectorsAndNVIDIA(t *testing.T) {
	root := t.TempDir()
	nvidia := filepath.Join(root, "class", "drm", "card0", "device")
	connector := filepath.Join(root, "class", "drm", "card0-HDMI-A-1", "device")
	if err := writeTestFile(filepath.Join(nvidia, "vendor"), "0x10de\n"); err != nil {
		t.Fatal(err)
	}
	if err := writeTestFile(filepath.Join(connector, "vendor"), "0x1002\n"); err != nil {
		t.Fatal(err)
	}

	got := collectLinuxDRMGPU(root)
	if got.Available {
		t.Fatalf("expected no DRM GPU devices, got %+v", got)
	}
}

func writeTestFile(path, value string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(value), 0644)
}
