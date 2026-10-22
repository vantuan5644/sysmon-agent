package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Byte-level parsing for the Windows storage discovery in
// collector_windows_storage.go. Untagged so the layouts are unit-tested on
// every platform, the same reason storage_usb.go carries no build tag.

// storageDeviceDescriptor is the part of STORAGE_DEVICE_DESCRIPTOR (winioctl.h)
// that discovery uses.
type storageDeviceDescriptor struct {
	Vendor    string
	Product   string
	BusType   uint16
	Removable bool
}

// parseStorageDeviceDescriptor reads the IOCTL_STORAGE_QUERY_PROPERTY
// (StorageDeviceProperty) result. Fixed layout: RemovableMedia at byte 10, the
// vendor/product string offsets at 12/16, BusType (STORAGE_BUS_TYPE, the same
// enumeration MSFT_PhysicalDisk.BusType uses) at 28.
func parseStorageDeviceDescriptor(buf []byte) (storageDeviceDescriptor, error) {
	if len(buf) < 32 {
		return storageDeviceDescriptor{}, fmt.Errorf("storage device descriptor is %d bytes, want at least 32", len(buf))
	}
	return storageDeviceDescriptor{
		Vendor:    descriptorString(buf, binary.LittleEndian.Uint32(buf[12:])),
		Product:   descriptorString(buf, binary.LittleEndian.Uint32(buf[16:])),
		BusType:   uint16(binary.LittleEndian.Uint32(buf[28:])),
		Removable: buf[10] != 0,
	}, nil
}

// descriptorString reads the NUL-terminated ASCII string at offset. Offset 0
// means the field is absent (an NVMe drive reports no vendor), and some
// drivers write 0xFFFFFFFF for the same thing, which the bounds check covers.
func descriptorString(buf []byte, offset uint32) string {
	if offset == 0 || uint64(offset) >= uint64(len(buf)) {
		return ""
	}
	field := buf[offset:]
	if end := strings.IndexByte(string(field), 0); end >= 0 {
		field = field[:end]
	}
	return strings.TrimSpace(string(field))
}

// windowsDiskModel composes the name Windows' storage provider shows as
// MSFT_PhysicalDisk.FriendlyName, which is what LibreHardwareMonitor's drive
// sensors are matched against: "<vendor> <product>" when the vendor is real,
// the product alone otherwise. Measured on BBLWIN: vendor "ZEPHYR" + product
// "ZEPHYR USB DISK" is "ZEPHYR ZEPHYR USB DISK", "Msft" + "Virtual Disk" is
// "Msft Virtual Disk", and the NVMe drives carry no vendor at all. "ATA" and
// "NVMe" are the bus names some drivers put in the vendor field; prefixing
// them would break the temperature match, which looks for this name inside
// the sensor's.
func windowsDiskModel(vendor, product string) string {
	vendor, product = strings.TrimSpace(vendor), strings.TrimSpace(product)
	switch strings.ToLower(vendor) {
	case "", "ata", "nvme":
		return product
	}
	if product == "" {
		return vendor
	}
	return vendor + " " + product
}

// errNoTemperatureSensors is a temperature reply that lists no sensors: the
// drive answered, and has nothing to report.
var errNoTemperatureSensors = errors.New("drive reported no temperature sensors")

// parseStorageTemperatureDescriptor reads the drive temperature from a
// STORAGE_TEMPERATURE_DATA_DESCRIPTOR (IOCTL_STORAGE_QUERY_PROPERTY with
// StorageDeviceTemperatureProperty): InfoCount at byte 12, then 16-byte
// STORAGE_TEMPERATURE_INFO entries from byte 24, each an Index and a signed
// Celsius Temperature. Index 0 is the whole-drive reading (an NVMe drive's
// Composite Temperature); the other indexes are individual sensors. The
// drive's warning and critical limits sit in the header, apart from the
// readings, so a limit can never be taken for one.
func parseStorageTemperatureDescriptor(buf []byte) (float64, error) {
	if len(buf) < 24 {
		return 0, fmt.Errorf("temperature descriptor is %d bytes, want at least 24", len(buf))
	}
	count := int(binary.LittleEndian.Uint16(buf[12:]))
	if count == 0 {
		return 0, errNoTemperatureSensors
	}
	var celsius int16
	found := false
	for i := 0; i < count && 24+16*(i+1) <= len(buf); i++ {
		entry := buf[24+16*i:]
		if index := binary.LittleEndian.Uint16(entry); index == 0 || !found {
			celsius, found = int16(binary.LittleEndian.Uint16(entry[2:])), true
			if index == 0 {
				break
			}
		}
	}
	if !found {
		return 0, fmt.Errorf("temperature descriptor reports %d sensors in %d bytes", count, len(buf))
	}
	// A running drive never reads exactly 0 C; that is an unpopulated channel,
	// the same sentinel the LHM bridge drops.
	if celsius == 0 || celsius < -50 || celsius > 150 {
		return 0, fmt.Errorf("drive reported an implausible %d C", celsius)
	}
	return float64(celsius), nil
}

// parseVolumeDiskExtents returns the distinct disk numbers in a
// VOLUME_DISK_EXTENTS buffer: a DWORD count, padding to 8, then 24-byte
// DISK_EXTENT entries whose first DWORD is the disk number.
func parseVolumeDiskExtents(buf []byte) ([]uint32, error) {
	if len(buf) < 4 {
		return nil, fmt.Errorf("volume disk extents are %d bytes, want at least 4", len(buf))
	}
	count := int(binary.LittleEndian.Uint32(buf))
	if need := 8 + 24*count; count > 0 && len(buf) < need {
		return nil, fmt.Errorf("volume reports %d extents in %d bytes, want %d", count, len(buf), need)
	}
	disks := make([]uint32, 0, count)
	for i := 0; i < count; i++ {
		disk := binary.LittleEndian.Uint32(buf[8+24*i:])
		if !slices.Contains(disks, disk) {
			disks = append(disks, disk)
		}
	}
	return disks, nil
}

// physicalDriveNumbers picks the PhysicalDriveN entries out of the
// NUL-separated device-name list QueryDosDeviceW(NULL) returns, sorted.
func physicalDriveNumbers(list []uint16) []uint32 {
	var numbers []uint32
	for _, name := range strings.Split(string(utf16.Decode(list)), "\x00") {
		digits, ok := strings.CutPrefix(name, "PhysicalDrive")
		if !ok {
			continue
		}
		number, err := strconv.ParseUint(digits, 10, 32)
		if err != nil {
			continue
		}
		numbers = append(numbers, uint32(number))
	}
	slices.Sort(numbers)
	return numbers
}
