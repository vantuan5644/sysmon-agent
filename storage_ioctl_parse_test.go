package main

import (
	"encoding/binary"
	"slices"
	"testing"
	"unicode/utf16"
)

// storageDescriptorFixture lays out a STORAGE_DEVICE_DESCRIPTOR the way the
// class driver returns it: the fixed 36-byte header, then the strings it
// points at. An empty string gets offset 0, as an NVMe drive's vendor does.
func storageDescriptorFixture(vendor, product string, busType uint32, removable bool) []byte {
	buf := make([]byte, 36)
	if removable {
		buf[10] = 1
	}
	binary.LittleEndian.PutUint32(buf[28:], busType)
	for _, field := range []struct {
		at    int
		value string
	}{{12, vendor}, {16, product}} {
		if field.value == "" {
			continue
		}
		binary.LittleEndian.PutUint32(buf[field.at:], uint32(len(buf)))
		buf = append(append(buf, field.value...), 0)
	}
	return buf
}

// TestParseStorageDeviceDescriptorReadsBBLWINsDrives uses the strings the four
// BBLWIN drives returned on 2026-10-22, including the space padding SCSI
// inquiry data carries.
func TestParseStorageDeviceDescriptorReadsBBLWINsDrives(t *testing.T) {
	cases := []struct {
		vendor, product string
		busType         uint32
		removable       bool
		model           string
	}{
		{"", "CT4000T705SSD3", 17, false, "CT4000T705SSD3"},
		{"ZEPHYR  ", "ZEPHYR USB DISK ", 7, true, "ZEPHYR ZEPHYR USB DISK"},
		{"Msft    ", "Virtual Disk    ", 15, false, "Msft Virtual Disk"},
	}
	for _, tc := range cases {
		got, err := parseStorageDeviceDescriptor(storageDescriptorFixture(tc.vendor, tc.product, tc.busType, tc.removable))
		if err != nil {
			t.Fatal(err)
		}
		if model := windowsDiskModel(got.Vendor, got.Product); model != tc.model {
			t.Fatalf("model = %q, want %q", model, tc.model)
		}
		if got.BusType != uint16(tc.busType) || got.Removable != tc.removable {
			t.Fatalf("%s: bus %d removable %t, want %d %t", tc.model, got.BusType, got.Removable, tc.busType, tc.removable)
		}
	}
}

func TestParseStorageDeviceDescriptorRejectsBadOffsets(t *testing.T) {
	if _, err := parseStorageDeviceDescriptor(make([]byte, 20)); err == nil {
		t.Fatal("a truncated descriptor was accepted")
	}
	buf := storageDescriptorFixture("", "", 17, false)
	binary.LittleEndian.PutUint32(buf[12:], 0xFFFFFFFF)
	binary.LittleEndian.PutUint32(buf[16:], uint32(len(buf)+10))
	got, err := parseStorageDeviceDescriptor(buf)
	if err != nil || got.Vendor != "" || got.Product != "" {
		t.Fatalf("out-of-range offsets = %+v, %v; want empty strings", got, err)
	}
}

// TestWindowsDiskModelDropsBusNamesFromTheVendor: a "NVMe" or "ATA" prefix
// would break the temperature match, which looks for this model inside the
// LibreHardwareMonitor sensor name.
func TestWindowsDiskModelDropsBusNamesFromTheVendor(t *testing.T) {
	cases := map[[2]string]string{
		{"NVMe", "Samsung SSD 990 PRO 2TB"}: "Samsung SSD 990 PRO 2TB",
		{"ATA", "Samsung SSD 870 EVO 1TB"}:  "Samsung SSD 870 EVO 1TB",
		{"ROG", "ESD-S1CL"}:                 "ROG ESD-S1CL",
		{"SanDisk", ""}:                     "SanDisk",
		{"", ""}:                            "",
	}
	for in, want := range cases {
		if got := windowsDiskModel(in[0], in[1]); got != want {
			t.Fatalf("windowsDiskModel(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

// temperatureDescriptorFixture lays out a STORAGE_TEMPERATURE_DATA_DESCRIPTOR
// with the given (index, celsius) entries and the drive's warning and critical
// limits in the header.
func temperatureDescriptorFixture(warning, critical int16, entries ...[2]int16) []byte {
	buf := make([]byte, 24+16*len(entries))
	binary.LittleEndian.PutUint16(buf[8:], uint16(critical))
	binary.LittleEndian.PutUint16(buf[10:], uint16(warning))
	binary.LittleEndian.PutUint16(buf[12:], uint16(len(entries)))
	for i, entry := range entries {
		binary.LittleEndian.PutUint16(buf[24+16*i:], uint16(entry[0]))
		binary.LittleEndian.PutUint16(buf[24+16*i+2:], uint16(entry[1]))
	}
	return buf
}

// TestParseStorageTemperatureDescriptorReadsTheComposite uses BBLWIN's replies
// from 2026-10-22: the T705 returned one entry (48 C, limits 87/89), the T710
// two (29 C each, limits 86/88). Index 0 is the Composite, wherever it sits.
func TestParseStorageTemperatureDescriptorReadsTheComposite(t *testing.T) {
	cases := []struct {
		buf  []byte
		want float64
	}{
		{temperatureDescriptorFixture(87, 89, [2]int16{0, 48}), 48},
		{temperatureDescriptorFixture(86, 88, [2]int16{0, 29}, [2]int16{1, 29}), 29},
		{temperatureDescriptorFixture(86, 88, [2]int16{1, 41}, [2]int16{0, 35}), 35},
		{temperatureDescriptorFixture(86, 88, [2]int16{2, 44}), 44},
	}
	for _, tc := range cases {
		got, err := parseStorageTemperatureDescriptor(tc.buf)
		if err != nil || got != tc.want {
			t.Fatalf("temperature = %v, %v; want %v", got, err, tc.want)
		}
	}
}

func TestParseStorageTemperatureDescriptorRejectsNonReadings(t *testing.T) {
	for name, buf := range map[string][]byte{
		"truncated":      make([]byte, 20),
		"no sensors":     temperatureDescriptorFixture(86, 88),
		"zero":           temperatureDescriptorFixture(86, 88, [2]int16{0, 0}),
		"below absolute": temperatureDescriptorFixture(86, 88, [2]int16{0, -274}),
		"cut entries":    temperatureDescriptorFixture(86, 88, [2]int16{0, 40})[:30],
	} {
		if got, err := parseStorageTemperatureDescriptor(buf); err == nil {
			t.Fatalf("%s: accepted %v C", name, got)
		}
	}
}

func TestParseVolumeDiskExtentsListsEachDiskOnce(t *testing.T) {
	extents := func(disks ...uint32) []byte {
		buf := make([]byte, 8+24*len(disks))
		binary.LittleEndian.PutUint32(buf, uint32(len(disks)))
		for i, disk := range disks {
			binary.LittleEndian.PutUint32(buf[8+24*i:], disk)
		}
		return buf
	}
	got, err := parseVolumeDiskExtents(extents(1))
	if err != nil || !slices.Equal(got, []uint32{1}) {
		t.Fatalf("simple volume = %v, %v", got, err)
	}
	got, err = parseVolumeDiskExtents(extents(2, 3, 2))
	if err != nil || !slices.Equal(got, []uint32{2, 3}) {
		t.Fatalf("spanned volume = %v, %v; want each disk once", got, err)
	}
	if _, err := parseVolumeDiskExtents(extents(1, 2)[:20]); err == nil {
		t.Fatal("a truncated extent list was accepted")
	}
}

func TestPhysicalDriveNumbersPicksDrivesFromTheDosDeviceList(t *testing.T) {
	list := utf16.Encode([]rune("C:\x00PhysicalDrive3\x00Volume{x}\x00PhysicalDrive0\x00PhysicalDriveX\x00PhysicalDrive12\x00\x00"))
	if got := physicalDriveNumbers(list); !slices.Equal(got, []uint32{0, 3, 12}) {
		t.Fatalf("physicalDriveNumbers = %v, want [0 3 12]", got)
	}
}
