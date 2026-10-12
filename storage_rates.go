package main

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// Per-drive throughput.
//
// The storage panel knew each drive's capacity and temperature but not whether
// it was doing anything, and the App details page knows which process moves
// data but not which drive it lands on. Both OSes keep cumulative byte
// counters per physical disk that cost next to nothing to read: Linux
// /proc/diskstats (one read for every disk), Windows IOCTL_DISK_PERFORMANCE on
// each \\.\PhysicalDriveN. The rate is the counter delta between two slow-lane
// passes.

// diskIOCounter is one disk's cumulative bytes read and written.
type diskIOCounter struct {
	read  uint64
	write uint64
}

// diskRateTracker keeps the previous pass's counters. Carries its own lock.
type diskRateTracker struct {
	mu     sync.Mutex
	prev   map[string]diskIOCounter
	prevAt time.Time
}

// apply fills every device's read/write rate from counters keyed by device
// name (StorageDevice.Name). readErr degrades every device with the same
// reason; a device with no counter, or seen for the first time, degrades on
// its own.
func (t *diskRateTracker) apply(set *StorageSet, counters map[string]diskIOCounter, readErr error, now time.Time) {
	t.mu.Lock()
	prev, prevAt := t.prev, t.prevAt
	if readErr == nil {
		t.prev, t.prevAt = counters, now
	}
	t.mu.Unlock()

	if !set.Available {
		return
	}
	elapsed := now.Sub(prevAt).Seconds()
	for i := range set.Devices {
		device := &set.Devices[i]
		if readErr != nil {
			reason := "disk I/O counters unavailable: " + readErr.Error()
			device.ReadBytesPerSecond = unavailableNumber("B/s", reason)
			device.WriteBytesPerSecond = unavailableNumber("B/s", reason)
			continue
		}
		current, ok := counters[device.Name]
		if !ok {
			device.ReadBytesPerSecond = unavailableNumber("B/s", "no I/O counters for this device")
			device.WriteBytesPerSecond = unavailableNumber("B/s", "no I/O counters for this device")
			continue
		}
		previous, seen := prev[device.Name]
		if !seen || prevAt.IsZero() || elapsed <= 0 {
			device.ReadBytesPerSecond = unavailableNumber("B/s", "disk I/O sampler is warming up")
			device.WriteBytesPerSecond = unavailableNumber("B/s", "disk I/O sampler is warming up")
			continue
		}
		device.ReadBytesPerSecond = diskCounterRate(current.read, previous.read, elapsed)
		device.WriteBytesPerSecond = diskCounterRate(current.write, previous.write, elapsed)
	}
}

// diskCounterRate degrades a counter that went backwards (a re-enumerated
// drive, a driver reload) to unavailable for one pass instead of reporting an
// absurd rate.
func diskCounterRate(current, previous uint64, elapsed float64) NumberMetric {
	if current < previous {
		return unavailableNumber("B/s", "disk I/O counter reset")
	}
	return availableNumber(float64(current-previous)/elapsed, "B/s")
}

// parseLinuxDiskstats reads /proc/diskstats lines ("major minor name reads
// merged sectors_read ms_reading writes merged sectors_written ..."). Sectors
// there are always 512 bytes whatever the device's own sector size
// (Documentation/admin-guide/iostats.rst).
func parseLinuxDiskstats(data string) map[string]diskIOCounter {
	counters := map[string]diskIOCounter{}
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}
		read, readErr := strconv.ParseUint(fields[5], 10, 64)
		write, writeErr := strconv.ParseUint(fields[9], 10, 64)
		if readErr != nil || writeErr != nil {
			continue
		}
		counters[fields[2]] = diskIOCounter{read: read * 512, write: write * 512}
	}
	return counters
}

// storageRateTotals sums every drive's rates for the history; ok is false
// until at least one drive has a rate.
func storageRateTotals(storage StorageSet) (read, write float64, ok bool) {
	if !storage.Available {
		return 0, 0, false
	}
	for _, device := range storage.Devices {
		if device.ReadBytesPerSecond.Available {
			read += device.ReadBytesPerSecond.Value
			ok = true
		}
		if device.WriteBytesPerSecond.Available {
			write += device.WriteBytesPerSecond.Value
			ok = true
		}
	}
	return read, write, ok
}
