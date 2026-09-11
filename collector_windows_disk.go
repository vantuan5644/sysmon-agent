//go:build windows

package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"syscall"
	"unsafe"
)

var (
	procGetLogicalDrives      = kernel32.NewProc("GetLogicalDrives")
	procGetDriveTypeW         = kernel32.NewProc("GetDriveTypeW")
	procGetDiskFreeSpaceExW   = kernel32.NewProc("GetDiskFreeSpaceExW")
	procGetVolumeInformationW = kernel32.NewProc("GetVolumeInformationW")
)

const driveFixed = 3

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
