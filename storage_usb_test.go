package main

import "testing"

// TestStorageTemperatureUnavailableReasonNamesTheBridge pins the point of the
// message: a USB-attached drive's missing temperature must read as a diagnosed
// limitation of the bridge, not as a generic sensor fault. Every other bus keeps
// the plain wording - an internal drive with no reading really is unexplained.
func TestStorageTemperatureUnavailableReasonNamesTheBridge(t *testing.T) {
	if got := storageTemperatureUnavailableReason(storageBusTypeUSB); got != usbEnclosureTemperatureReason {
		t.Fatalf("USB reason = %q, want %q", got, usbEnclosureTemperatureReason)
	}
	// 17 NVMe, 11 SATA, 3 ATA, 10 SAS, 0 unknown.
	for _, busType := range []uint16{0, 3, 10, 11, 17} {
		got := storageTemperatureUnavailableReason(busType)
		if got == usbEnclosureTemperatureReason {
			t.Fatalf("bus type %d claimed a USB enclosure", busType)
		}
		if got == "" {
			t.Fatalf("bus type %d produced an empty reason", busType)
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
