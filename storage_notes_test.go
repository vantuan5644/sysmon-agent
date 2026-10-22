package main

import (
	"reflect"
	"testing"
)

// TestExpectedStorageConditionsAreNotesNotIssues pins the BBLWIN case that
// motivated collection_notes: an unmounted foreign drive, a file-backed virtual
// disk and a USB enclosure each degrade a storage field forever, and used to pin
// three permanent entries in the issues panel. They must move to
// collection_notes - still reported, labelled the same way - while a genuine
// storage miss on the same snapshot stays an issue.
func TestExpectedStorageConditionsAreNotesNotIssues(t *testing.T) {
	metrics := baseMetrics("BBLWIN")
	metrics.CPU = availableNumber(5, "%")
	metrics.Memory = availableCapacity(1, 2)
	metrics.Storage = StorageSet{Available: true, Devices: []StorageDevice{
		{Name: "PhysicalDrive0", Model: "CT4000T705SSD3", Capacity: unavailableCapacity(noMountedFilesystemsReason), Temperature: availableNumber(53, "C")},
		{Name: "PhysicalDrive2", Model: "Msft Virtual Disk", Capacity: availableCapacity(1, 2), Temperature: unavailableNumber("C", virtualDiskTemperatureReason)},
		{Name: "PhysicalDrive3", Model: "Realtek RTL9210 NVME", Capacity: availableCapacity(1, 2), Temperature: unavailableNumber("C", usbEnclosureTemperatureReason)},
		{Name: "PhysicalDrive4", Model: "Samsung SSD 990 PRO", Capacity: availableCapacity(1, 2), Temperature: unavailableNumber("C", "no storage temperature sensor reported")},
	}}

	errors := summarizeCollectionErrors(metrics)
	wantErrors := []string{"storage Samsung SSD 990 PRO temperature: no storage temperature sensor reported"}
	if !reflect.DeepEqual(errors, wantErrors) {
		t.Fatalf("collection_errors = %#v, want %#v", errors, wantErrors)
	}

	notes := summarizeCollectionNotes(metrics)
	wantNotes := []string{
		"storage CT4000T705SSD3 capacity: no mounted filesystems",
		"storage Msft Virtual Disk temperature: " + virtualDiskTemperatureReason,
		"storage Realtek RTL9210 NVME temperature: " + usbEnclosureTemperatureReason,
	}
	if !reflect.DeepEqual(notes, wantNotes) {
		t.Fatalf("collection_notes = %#v, want %#v", notes, wantNotes)
	}

	finished := finishMetrics(metrics, metrics.Timestamp)
	if !reflect.DeepEqual(finished.CollectionNotes, wantNotes) {
		t.Fatalf("finishMetrics did not populate collection_notes: %#v", finished.CollectionNotes)
	}
}

// TestIsExpectedStorageConditionMatchesExactMessagesOnly: the generic misses,
// and an expected message embedded in a longer error, must stay issues.
func TestIsExpectedStorageConditionMatchesExactMessagesOnly(t *testing.T) {
	for _, message := range []string{noMountedFilesystemsReason, usbEnclosureTemperatureReason, virtualDiskTemperatureReason, unreadBusTemperatureReason, noDriveTemperatureReason, " " + noMountedFilesystemsReason + " "} {
		if !isExpectedStorageCondition(message) {
			t.Fatalf("%q not treated as expected", message)
		}
	}
	for _, message := range []string{
		"",
		"no storage temperature sensor reported",
		"storage temperature unavailable",
		"no block-device temperature sensor exposed",
		"statfs denied: " + noMountedFilesystemsReason,
	} {
		if isExpectedStorageCondition(message) {
			t.Fatalf("%q wrongly treated as expected", message)
		}
	}
}

// TestStorageTemperatureUnavailableReasonNamesAVirtualDisk: Windows reports a
// mounted VHDX (BBLWIN's E: Dev Drive) as BusType 15 and a Hyper-V or Storage
// Spaces virtual disk as 14. Neither has a sensor.
func TestStorageTemperatureUnavailableReasonNamesAVirtualDisk(t *testing.T) {
	for _, busType := range []uint16{storageBusTypeVirtual, storageBusTypeFileBackedVirtual} {
		if got := storageTemperatureUnavailableReason(busType); got != virtualDiskTemperatureReason {
			t.Fatalf("bus type %d reason = %q, want %q", busType, got, virtualDiskTemperatureReason)
		}
	}
	for _, busType := range []uint16{0, 3, 7, 11} {
		if got := storageTemperatureUnavailableReason(busType); got == virtualDiskTemperatureReason {
			t.Fatalf("bus type %d claimed to be a virtual disk", busType)
		}
	}
}
