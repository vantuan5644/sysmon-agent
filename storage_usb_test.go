package main

import "testing"

// TestStorageTemperatureUnavailableReasonNamesTheBridge pins the point of the
// message: a USB-attached drive's missing temperature must read as a diagnosed
// limitation of the bridge, not as a generic sensor fault. A drive on another
// bus whose temperature is not read says which ones are.
func TestStorageTemperatureUnavailableReasonNamesTheBridge(t *testing.T) {
	if got := storageTemperatureUnavailableReason(storageBusTypeUSB); got != usbEnclosureTemperatureReason {
		t.Fatalf("USB reason = %q, want %q", got, usbEnclosureTemperatureReason)
	}
	// 10 SAS, 12 SD, 0 unknown.
	for _, busType := range []uint16{0, 10, 12} {
		if got := storageTemperatureUnavailableReason(busType); got != unreadBusTemperatureReason {
			t.Fatalf("bus type %d reason = %q, want %q", busType, got, unreadBusTemperatureReason)
		}
	}
}

// TestDriveTemperatureReadableCoversNVMeAndSATA: the buses Windows is asked
// about, and the ones it is not.
func TestDriveTemperatureReadableCoversNVMeAndSATA(t *testing.T) {
	for _, busType := range []uint16{storageBusTypeNVMe, storageBusTypeSATA, storageBusTypeATA} {
		if !driveTemperatureReadable(busType) {
			t.Fatalf("bus type %d not read", busType)
		}
	}
	for _, busType := range []uint16{0, storageBusTypeUSB, 10, storageBusTypeVirtual, storageBusTypeFileBackedVirtual} {
		if driveTemperatureReadable(busType) {
			t.Fatalf("bus type %d read", busType)
		}
	}
}

// TestIsUSBControllerNodeRejectsSubstringTraps guards the fragment trap that
// cpuDieTemperatureRank documents: sysfs path segments are attacker-free but not
// well-behaved, and a bare "usb" substring test would call a Corsair "usb-c"
// enclosure directory or a "usbcore" node a host controller.
func TestIsUSBControllerNodeRejectsSubstringTraps(t *testing.T) {
	for _, segment := range []string{"usb1", "usb2", "usb10"} {
		if !isUSBControllerNode(segment) {
			t.Fatalf("%q not recognized as a USB host controller", segment)
		}
	}
	for _, segment := range []string{"", "usb", "usbcore", "usb-c", "usb1x", "1-usb2", "nvme0", "pci0000:00"} {
		if isUSBControllerNode(segment) {
			t.Fatalf("%q wrongly recognized as a USB host controller", segment)
		}
	}
}
