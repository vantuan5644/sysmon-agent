//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCollectStorageLeavesOutExcludedDrives: an excluded drive is dropped by
// its sysfs model before its size or temperature is read, and the others stay.
func TestCollectStorageLeavesOutExcludedDrives(t *testing.T) {
	sysRoot := t.TempDir()
	for name, model := range map[string]string{"nvme0n1": "CT4000T710SSD8", "sda": "ZEPHYR USB DISK"} {
		device := filepath.Join(sysRoot, "block", name, "device")
		if err := os.MkdirAll(device, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(device, "model"), []byte(model+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sysRoot, "block", name, "size"), []byte("7814037168\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	set := collectStorageFrom(sysRoot, parseStorageExclusions("ZEPHYR USB DISK"))
	if !set.Available || len(set.Devices) != 1 || set.Devices[0].Name != "nvme0n1" {
		t.Fatalf("storage = %+v, want only nvme0n1", set)
	}
	if all := collectStorageFrom(sysRoot, nil); len(all.Devices) != 2 {
		t.Fatalf("without exclusions = %+v, want both drives", all)
	}
}
