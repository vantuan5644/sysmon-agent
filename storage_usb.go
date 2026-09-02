package main

import (
	"os"
	"path/filepath"
	"strings"
)

// usbEnclosureTemperatureReason is the degraded-metric message for a drive that
// reports no live temperature because it sits behind a USB-to-NVMe/SATA bridge.
//
// This is not "USB cannot carry temperatures". The SMART log page is there and
// other tools read it: on BBLWIN the ROG ESD-S1CL enclosure (ASMedia bridge,
// USB\VID_0B05&PID_1932, UAS) reports 35 C to CrystalDiskInfo and to
// smartctl -d sntasmedia, while LibreHardwareMonitor reports 0. Reaching it
// means tunnelling an NVMe admin command inside a bridge-vendor-specific SCSI
// opcode - ASMedia, JMicron and Realtek each need their own. LHM on Windows and
// the kernel hwmon path on Linux only issue the generic query, which a UAS
// bridge does not answer, so both come back empty.
//
// The message exists so the empty reading is self-describing: "no storage
// temperature sensor reported" reads as a fault, this reads as a known limit of
// the path and names what would have to change to lift it. Same reasoning as
// publishing cpu_temperature_sensor - a degraded field that explains itself can
// be checked rather than guessed at.
const usbEnclosureTemperatureReason = "USB enclosure: bridge does not expose live SMART temperature"

// storageBusTypeUSB is MSFT_PhysicalDisk.BusType 7, the USB member of the
// Windows storage BusType enumeration (17 is NVMe, 11 SATA, 3 ATA, 10 SAS).
const storageBusTypeUSB = 7

// storageTemperatureUnavailableReason picks the explanation for a physical disk
// the LibreHardwareMonitor bridge reported no usable temperature for. Untagged
// (not in collector_windows.go) so the branch is unit-tested on every platform,
// the same reason update_swap.go carries no build tag.
func storageTemperatureUnavailableReason(busType uint16) string {
	if busType == storageBusTypeUSB {
		return usbEnclosureTemperatureReason
	}
	return "no storage temperature sensor reported"
}

// linuxBlockDeviceIsUSB reports whether <blockRoot>/<disk> resolves through a
// USB host controller. /sys/block/<disk> is a symlink into the device tree, and
// a USB-attached disk's path runs through a "usb<N>" node:
//
//	../devices/pci0000:00/0000:00:14.0/usb2/2-4/2-4:1.0/host6/.../block/sda
//
// Matched as a whole path segment of "usb" + digits, never as a bare "usb"
// substring: short fragments are traps here (a model or vendor directory may
// well contain "usb"), the same trap documented for cpuDieTemperatureRank.
func linuxBlockDeviceIsUSB(disk, blockRoot string) bool {
	if disk == "" {
		return false
	}
	target, err := os.Readlink(filepath.Join(blockRoot, disk))
	if err != nil {
		return false
	}
	for _, segment := range strings.Split(filepath.ToSlash(target), "/") {
		if isUSBControllerNode(segment) {
			return true
		}
	}
	return false
}

// isUSBControllerNode matches a sysfs USB host-controller directory name:
// literally "usb" followed by at least one digit and nothing else.
func isUSBControllerNode(segment string) bool {
	rest, ok := strings.CutPrefix(segment, "usb")
	if !ok || rest == "" {
		return false
	}
	for _, r := range rest {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
