package main

import (
	"os"
	"path/filepath"
	"strings"
)

// Expected storage conditions. Each of these degrades a field for a reason that
// is a property of how the drive is attached or used while the collector itself
// works, and none of them will ever clear on its own:
//
//   - a drive with nothing mounted on this OS (the Linux SSD seen from Windows,
//     a spare drive, the other half of a dual boot);
//   - a USB enclosure whose bridge does not pass the SMART log through
//     (usbEnclosureTemperatureReason, storage_usb.go);
//   - a virtual disk, which has no physical sensor to read at all.
//
// They used to be rolled into collection_errors with everything else, which
// pinned three permanent entries in BBLWIN's issues panel (2026-10-12): the
// "All clear" state could never appear, and a new, real fault had to be spotted
// among messages that were there every day. They are reported as
// collection_notes instead, and the storage row itself shows the reason, so
// nothing goes silent - an expected gap is described where it occurs rather
// than raised as an issue.
//
// Only exact messages qualify. A generic "no storage temperature sensor
// reported" stays an issue: it means a physical drive the agent expected to
// read and could not.
const (
	noMountedFilesystemsReason   = "no mounted filesystems"
	virtualDiskTemperatureReason = "virtual disk: no physical temperature sensor"
)

// storageBusTypeVirtual and storageBusTypeFileBackedVirtual are the
// MSFT_PhysicalDisk.BusType values for a virtual disk (Hyper-V, Storage Spaces
// virtual disk) and a file-backed one (a mounted VHD/VHDX such as BBLWIN's E:
// Dev Drive, which Windows names "Msft Virtual Disk").
const (
	storageBusTypeVirtual           = 14
	storageBusTypeFileBackedVirtual = 15
)

// isExpectedStorageCondition reports whether a degraded storage field's message
// names one of the expected conditions above.
func isExpectedStorageCondition(message string) bool {
	switch strings.TrimSpace(message) {
	case noMountedFilesystemsReason, usbEnclosureTemperatureReason, virtualDiskTemperatureReason:
		return true
	}
	return false
}

// linuxBlockDeviceIsVirtual reports whether <blockRoot>/<disk> resolves through
// a virtio device node, i.e. the disk a hypervisor presents to a guest
// (/sys/block/vda -> ../devices/pci0000:00/0000:00:04.0/virtio2/block/vda).
// Matched as a whole "virtio" + digits segment for the same reason
// isUSBControllerNode refuses a bare substring.
func linuxBlockDeviceIsVirtual(disk, blockRoot string) bool {
	if disk == "" {
		return false
	}
	target, err := os.Readlink(filepath.Join(blockRoot, disk))
	if err != nil {
		return false
	}
	for _, segment := range strings.Split(filepath.ToSlash(target), "/") {
		rest, ok := strings.CutPrefix(segment, "virtio")
		if !ok || rest == "" {
			continue
		}
		digits := true
		for _, r := range rest {
			if r < '0' || r > '9' {
				digits = false
				break
			}
		}
		if digits {
			return true
		}
	}
	return false
}
