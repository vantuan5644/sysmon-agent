//go:build linux

package gpuhotspot

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Both registers sit in BAR0. The VRAM one is the GDDR6/GDDR6X temperature
// ADC, read the same way on GA102 (RTX 3090) and AD102 (RTX 4090).
const (
	hotspotOffset = uint64(0x2046c)
	vramOffset    = uint64(0xe2a8)
)

func supportedDevice(vendor, device string) bool {
	return strings.TrimSpace(vendor) == "0x10de" && (strings.TrimSpace(device) == "0x2204" || strings.TrimSpace(device) == "0x2684")
}

// BAR0 is resolved afresh each pass, so a bus reset never reuses an old mapping.
func ReadDevices(sysRoot, memoryPath string) ([]Device, error) {
	entries, err := os.ReadDir(filepath.Join(sysRoot, "bus", "pci", "devices"))
	if err != nil {
		return nil, err
	}
	devices := make([]Device, 0)
	for _, entry := range entries {
		id, err := NormalizePCIBusID(entry.Name())
		if err != nil {
			continue
		}
		path := filepath.Join(sysRoot, "bus", "pci", "devices", entry.Name())
		vendor, vendorErr := os.ReadFile(filepath.Join(path, "vendor"))
		device, deviceErr := os.ReadFile(filepath.Join(path, "device"))
		if vendorErr != nil || deviceErr != nil || !supportedDevice(string(vendor), string(device)) {
			continue
		}
		if len(devices) == MaxDevices {
			return nil, fmt.Errorf("too many supported GPUs")
		}
		reading := Device{PCIBusID: id}
		resource, err := os.ReadFile(filepath.Join(path, "resource"))
		var bar0 uint64
		if err == nil {
			bar0, err = barStart(string(resource))
		}
		if err != nil {
			reading.Error = "GPU hotspot register unavailable: " + err.Error()
			reading.VRAMError = "GPU VRAM register unavailable: " + err.Error()
			devices = append(devices, reading)
			continue
		}
		if value, err := readRegister(memoryPath, bar0+hotspotOffset, Decode); err != nil {
			reading.Error = "GPU hotspot register unavailable: " + err.Error()
		} else {
			reading.Celsius = &value
		}
		if value, err := readRegister(memoryPath, bar0+vramOffset, DecodeVRAM); err != nil {
			reading.VRAMError = "GPU VRAM register unavailable: " + err.Error()
		} else {
			reading.VRAMCelsius = &value
		}
		devices = append(devices, reading)
	}
	return devices, nil
}

// barStart returns BAR0's start once BAR0 is a memory BAR large enough to
// hold both registers.
func barStart(resource string) (uint64, error) {
	fields := strings.Fields(strings.SplitN(resource, "\n", 2)[0])
	if len(fields) != 3 {
		return 0, fmt.Errorf("invalid BAR0 resource")
	}
	values := [3]uint64{}
	for i, field := range fields {
		value, err := strconv.ParseUint(strings.TrimPrefix(field, "0x"), 16, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid BAR0 counters")
		}
		values[i] = value
	}
	start, end, flags := values[0], values[1], values[2]
	// IORESOURCE_MEM=0x200. Never map I/O ports, empty BARs, or an overflowing
	// range. hotspotOffset is the higher of the two registers.
	if start == 0 || end < start || flags&0x200 == 0 || end-start < hotspotOffset+3 || start > math.MaxInt64-hotspotOffset-3 {
		return 0, fmt.Errorf("BAR0 does not contain the sensor registers")
	}
	return start, nil
}

func readRegister(path string, address uint64, decode func(uint32) (float64, error)) (float64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	pageSize := uint64(os.Getpagesize())
	base := address - address%pageSize
	offset := int(address - base)
	length := offset + 4
	if length > int(pageSize) || base > math.MaxInt64 {
		return 0, fmt.Errorf("invalid register mapping range")
	}
	mapping, err := syscall.Mmap(int(file.Fd()), int64(base), length, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return 0, err
	}
	defer syscall.Munmap(mapping)
	return decode(binary.LittleEndian.Uint32(mapping[offset : offset+4]))
}
