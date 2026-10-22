package main

import "testing"

// TestStorageExclusionsMatchTheRecorderOnBothOSes: one entry has to catch the
// drive under the name each OS gives it, and leave the real drives alone.
func TestStorageExclusionsMatchTheRecorderOnBothOSes(t *testing.T) {
	exclude := parseStorageExclusions(" zephyr  usb disk ,, ")
	if len(exclude) != 1 {
		t.Fatalf("parsed %q, want one entry", exclude)
	}
	for _, model := range []string{"ZEPHYR ZEPHYR USB DISK", "ZEPHYR USB DISK", "Zephyr USB  Disk"} {
		if !exclude.excludes(model) {
			t.Fatalf("%q not excluded", model)
		}
	}
	for _, model := range []string{"CT4000T710SSD8", "Msft Virtual Disk", "ZEPHYR", ""} {
		if exclude.excludes(model) {
			t.Fatalf("%q wrongly excluded", model)
		}
	}
	if parseStorageExclusions("").excludes("ZEPHYR USB DISK") {
		t.Fatal("an empty list excluded a drive")
	}
}
