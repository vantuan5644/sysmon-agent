// Package gpuhotspot defines the bounded file contract between the privileged
// Linux sensor reader and the unprivileged dashboard agent.
package gpuhotspot

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const MaxDevices = 64
const MaxBytes = 64 * 1024
const FreshFor = 6 * time.Second

type Device struct {
	PCIBusID string   `json:"pci_bus_id"`
	Celsius  *float64 `json:"celsius"`
	Error    string   `json:"error,omitempty"`
}

type Snapshot struct {
	Timestamp time.Time `json:"timestamp"`
	Devices   []Device  `json:"devices"`
	Error     string    `json:"error,omitempty"`
}

// NVIDIA prints an eight-digit PCI domain; sysfs prints four digits.
func NormalizePCIBusID(raw string) (string, error) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(raw)), ":")
	if len(parts) != 3 {
		return "", fmt.Errorf("invalid PCI address")
	}
	slot := strings.Split(parts[2], ".")
	if len(slot) != 2 {
		return "", fmt.Errorf("invalid PCI slot")
	}
	values := make([]uint64, 4)
	for i, part := range []string{parts[0], parts[1], slot[0], slot[1]} {
		if part == "" || strings.ContainsAny(part, "+-x ") {
			return "", fmt.Errorf("invalid PCI component")
		}
		value, err := strconv.ParseUint(part, 16, 32)
		if err != nil {
			return "", err
		}
		values[i] = value
	}
	if values[0] > 0xffff || values[1] > 255 || values[2] > 31 || values[3] > 7 {
		return "", fmt.Errorf("PCI address out of range")
	}
	return fmt.Sprintf("%04x:%02x:%02x.%x", values[0], values[1], values[2], values[3]), nil
}

// Ampere/Ada register decoding follows ThomasBaruzier/gddr6-core-junction-vram-temps,
// src/sensor.c at 6d8c5ecf633a8658d205fb2c24531bf87164912f (Apache-2.0).
// The selected register is die hotspot; VRAM junction uses a different register.
func Decode(raw uint32) (float64, error) {
	value := float64((raw >> 8) & 0xff)
	if raw == math.MaxUint32 || value <= 0 || value >= 150 {
		return 0, fmt.Errorf("invalid GPU hotspot register reading")
	}
	return value, nil
}
