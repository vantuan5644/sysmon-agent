//go:build windows

package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	procGetLogicalDrives      = kernel32.NewProc("GetLogicalDrives")
	procGetDriveTypeW         = kernel32.NewProc("GetDriveTypeW")
	procGetDiskFreeSpaceExW   = kernel32.NewProc("GetDiskFreeSpaceExW")
	procGetVolumeInformationW = kernel32.NewProc("GetVolumeInformationW")
)

const driveFixed = 3

// ioctlDiskPerformance is IOCTL_DISK_PERFORMANCE: CTL_CODE(IOCTL_DISK_BASE,
// 0x0008, METHOD_BUFFERED, FILE_ANY_ACCESS). FILE_ANY_ACCESS is what lets a
// handle opened with no access rights ask for it.
const ioctlDiskPerformance = 0x70020

// diskPerformance mirrors DISK_PERFORMANCE (winioctl.h). Go lays it out like
// the C struct: 88 bytes, the int64 fields 8-byte aligned.
type diskPerformance struct {
	BytesRead           int64
	BytesWritten        int64
	ReadTime            int64
	WriteTime           int64
	IdleTime            int64
	ReadCount           uint32
	WriteCount          uint32
	QueueDepth          uint32
	SplitCount          uint32
	QueryTime           int64
	StorageDeviceNumber uint32
	StorageManagerName  [8]uint16
}

// applyWindowsStorageRates reads each physical drive's cumulative byte
// counters and fills its throughput. Storage rows are named PhysicalDriveN by
// the discovery script, which is exactly the device path to open.
//
// The handle is opened with zero access rights. That needs no privilege and
// sends nothing to the device: the disk class driver answers
// IOCTL_DISK_PERFORMANCE from counters it keeps anyway. That distinction
// matters on BBLWIN, where LibreHardwareMonitor's storage detection (vendor
// pass-through commands) used to reset a USB card reader; this path issues no
// such command.
func (c *systemCollector) applyWindowsStorageRates(set *StorageSet) {
	if !set.Available {
		return
	}
	counters := make(map[string]diskIOCounter, len(set.Devices))
	for _, device := range set.Devices {
		if counter, err := readWindowsDiskPerformance(device.Name); err == nil {
			counters[device.Name] = counter
		}
	}
	c.diskRates.apply(set, counters, nil, time.Now())
}

func readWindowsDiskPerformance(name string) (diskIOCounter, error) {
	if !strings.HasPrefix(name, "PhysicalDrive") {
		return diskIOCounter{}, fmt.Errorf("%q is not a physical drive", name)
	}
	path, err := syscall.UTF16PtrFromString(`\\.\` + name)
	if err != nil {
		return diskIOCounter{}, err
	}
	handle, err := syscall.CreateFile(path, 0, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil, syscall.OPEN_EXISTING, 0, 0)
	if err != nil {
		return diskIOCounter{}, fmt.Errorf("open %s: %w", name, err)
	}
	defer syscall.CloseHandle(handle)
	var perf diskPerformance
	var returned uint32
	if err := syscall.DeviceIoControl(handle, ioctlDiskPerformance, nil, 0, (*byte)(unsafe.Pointer(&perf)), uint32(unsafe.Sizeof(perf)), &returned, nil); err != nil {
		return diskIOCounter{}, fmt.Errorf("IOCTL_DISK_PERFORMANCE on %s: %w", name, err)
	}
	if perf.BytesRead < 0 || perf.BytesWritten < 0 {
		return diskIOCounter{}, fmt.Errorf("negative byte counters on %s", name)
	}
	return diskIOCounter{read: uint64(perf.BytesRead), write: uint64(perf.BytesWritten)}, nil
}

// windowsDisks reads fixed-volume capacity and identity through kernel32. Disk
// capacity changes continuously enough to be useful on the dashboard, but it
// does not justify a new CIM host process on every slow sample.
func windowsDisks(ctx context.Context) []DiskMetric {
	mask, _, callErr := procGetLogicalDrives.Call()
	if mask == 0 {
		return unavailableDisk(fmt.Sprintf("GetLogicalDrives failed: %v", callErr))
	}
	disks := make([]DiskMetric, 0, 4)
	for drive := 0; drive < 26; drive++ {
		if mask&(uintptr(1)<<drive) == 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return unavailableDisk(err.Error())
		}
		mountpoint := fmt.Sprintf("%c:", 'A'+drive)
		root, err := syscall.UTF16PtrFromString(mountpoint + `\`)
		if err != nil {
			continue
		}
		if kind, _, _ := procGetDriveTypeW.Call(uintptr(unsafe.Pointer(root))); kind != driveFixed {
			continue
		}

		metric := DiskMetric{Name: mountpoint, Mountpoint: mountpoint}
		var freeAvailable, total, totalFree uint64
		if ok, _, err := procGetDiskFreeSpaceExW.Call(
			uintptr(unsafe.Pointer(root)),
			uintptr(unsafe.Pointer(&freeAvailable)),
			uintptr(unsafe.Pointer(&total)),
			uintptr(unsafe.Pointer(&totalFree)),
		); ok == 0 {
			metric.Capacity = unavailableCapacity(fmt.Sprintf("GetDiskFreeSpaceExW failed: %v", err))
		} else {
			metric.Capacity = availableCapacityFromTotalFree(total, totalFree, "invalid Windows disk capacity counters")
		}

		var label [261]uint16
		var filesystem [261]uint16
		if ok, _, _ := procGetVolumeInformationW.Call(
			uintptr(unsafe.Pointer(root)),
			uintptr(unsafe.Pointer(&label[0])), uintptr(len(label)),
			0, 0, 0,
			uintptr(unsafe.Pointer(&filesystem[0])), uintptr(len(filesystem)),
		); ok != 0 {
			if value := syscall.UTF16ToString(label[:]); value != "" {
				metric.Name += " " + value
			}
			metric.FSType = strings.ToLower(syscall.UTF16ToString(filesystem[:]))
		}
		disks = append(disks, metric)
	}
	sort.Slice(disks, func(i, j int) bool { return disks[i].Mountpoint < disks[j].Mountpoint })
	return ensureDiskMetrics(disks, "no fixed disks found")
}
