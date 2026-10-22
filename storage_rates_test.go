package main

import (
	"errors"
	"testing"
	"time"
)

func TestParseLinuxDiskstatsReadsSectorsAsBytes(t *testing.T) {
	data := `   8       0 sda 1200 30 4096 900 800 20 2048 700 0 1500 1600 0 0 0 0 0 0
 259       0 nvme0n1 5000 0 1000000 3000 9000 0 2000000 8000 0 9000 11000 0 0 0 0 0 0
   7       0 loop0 1 2 3
garbage line`
	counters := parseLinuxDiskstats(data)
	if got := counters["sda"]; got.read != 4096*512 || got.write != 2048*512 {
		t.Fatalf("sda = %+v", got)
	}
	if got := counters["nvme0n1"]; got.read != 1_000_000*512 || got.write != 2_000_000*512 {
		t.Fatalf("nvme0n1 = %+v", got)
	}
	if _, ok := counters["loop0"]; ok {
		t.Fatal("a short line was parsed")
	}
}

// TestDiskRateTrackerTurnsCountersIntoRates covers each way a drive's rate can
// be missing as well as the normal delta.
func TestDiskRateTrackerTurnsCountersIntoRates(t *testing.T) {
	set := func() StorageSet {
		return StorageSet{Available: true, Devices: []StorageDevice{{Name: "nvme0n1"}, {Name: "sdb"}}}
	}
	var tracker diskRateTracker
	start := time.Unix(1_760_000_000, 0)

	first := set()
	tracker.apply(&first, map[string]diskIOCounter{"nvme0n1": {read: 1000, write: 5000}}, nil, start)
	if first.Devices[0].ReadBytesPerSecond.Available || first.Devices[0].ReadBytesPerSecond.Error != "disk I/O sampler is warming up" {
		t.Fatalf("first pass = %+v, want warming up", first.Devices[0].ReadBytesPerSecond)
	}
	if first.Devices[1].ReadBytesPerSecond.Error != "no I/O counters for this device" {
		t.Fatalf("device without counters = %+v", first.Devices[1].ReadBytesPerSecond)
	}

	second := set()
	tracker.apply(&second, map[string]diskIOCounter{"nvme0n1": {read: 5000, write: 25000}}, nil, start.Add(2*time.Second))
	if r, w := second.Devices[0].ReadBytesPerSecond, second.Devices[0].WriteBytesPerSecond; r.Value != 2000 || w.Value != 10000 || r.Unit != "B/s" {
		t.Fatalf("rates = read %+v write %+v, want 2000 and 10000 B/s", r, w)
	}

	third := set()
	tracker.apply(&third, map[string]diskIOCounter{"nvme0n1": {read: 10, write: 30000}}, nil, start.Add(4*time.Second))
	if third.Devices[0].ReadBytesPerSecond.Available || third.Devices[0].ReadBytesPerSecond.Error != "disk I/O counter reset" {
		t.Fatalf("counter going backwards = %+v", third.Devices[0].ReadBytesPerSecond)
	}
	if !third.Devices[0].WriteBytesPerSecond.Available {
		t.Fatalf("a reset read counter took the write rate with it: %+v", third.Devices[0].WriteBytesPerSecond)
	}

	failed := set()
	tracker.apply(&failed, nil, errors.New("permission denied"), start.Add(6*time.Second))
	if failed.Devices[0].ReadBytesPerSecond.Available || failed.Devices[1].WriteBytesPerSecond.Error == "" {
		t.Fatalf("read error did not degrade every device: %+v", failed.Devices)
	}

	// A failed read keeps the last good counters, so the next pass still has a
	// baseline and does not fall back to warming up.
	after := set()
	tracker.apply(&after, map[string]diskIOCounter{"nvme0n1": {read: 2010, write: 30000}}, nil, start.Add(8*time.Second))
	if r := after.Devices[0].ReadBytesPerSecond; !r.Available || r.Value != 500 {
		t.Fatalf("rate after a failed read = %+v, want 500 B/s over the 4 s since the last good read", r)
	}
}

func TestStorageRateTotalsFeedHistory(t *testing.T) {
	m := historyMetricsAt(time.UnixMilli(1_760_000_000_000), 1, 1, 1, 1)
	m.Storage = StorageSet{Available: true, Devices: []StorageDevice{
		{Name: "a", ReadBytesPerSecond: availableNumber(1000, "B/s"), WriteBytesPerSecond: availableNumber(500, "B/s")},
		{Name: "b", ReadBytesPerSecond: unavailableNumber("B/s", "warming up"), WriteBytesPerSecond: availableNumber(250, "B/s")},
	}}
	h := newMetricsHistory(5*time.Second, time.Minute)
	h.observe(m, true)
	if got := h.snapshot().Points[0].Disk; got == nil || *got != 1750 {
		t.Fatalf("history disk = %v, want 1750 B/s", got)
	}
	h2 := newMetricsHistory(5*time.Second, time.Minute)
	h2.observe(m, false)
	if got := h2.snapshot().Points[0].Disk; got != nil {
		t.Fatalf("parked slow lane recorded disk %v", *got)
	}
}
