package gpuguard

import (
	"fmt"
	"strconv"
	"strings"
)

// SMIQuery is the nvidia-smi query the Linux helper streams with -lms: one
// line per interval from a single long-lived process, instead of a new
// nvidia-smi every two seconds.
const SMIQuery = "--query-gpu=name,pci.bus_id,temperature.gpu,power.draw"

// SMIReading is one line of SMIQuery output.
type SMIReading struct {
	Name     string
	PCIBusID string
	Core     *float64
	Power    *float64
}

// ParseSMILine reads "NVIDIA GeForce RTX 4090, 00000000:01:00.0, 55, 65.48"
// (csv,noheader,nounits). The name is taken as everything before the last
// three fields, so a comma in it cannot shift the numbers. A field nvidia-smi
// cannot read ("[N/A]", "[Unknown Error]") leaves that reading nil.
func ParseSMILine(line string) (SMIReading, error) {
	fields := strings.Split(strings.TrimSpace(line), ",")
	if len(fields) < 4 {
		return SMIReading{}, fmt.Errorf("unexpected nvidia-smi line %q", strings.TrimSpace(line))
	}
	n := len(fields)
	reading := SMIReading{
		Name:     strings.TrimSpace(strings.Join(fields[:n-3], ",")),
		PCIBusID: strings.TrimSpace(fields[n-3]),
	}
	number := func(raw string, max float64) *float64 {
		v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil || v <= 0 || v > max {
			return nil
		}
		return &v
	}
	reading.Core = number(fields[n-2], 150)
	reading.Power = number(fields[n-1], 2000)
	if reading.Core == nil && reading.Power == nil {
		return reading, fmt.Errorf("nvidia-smi reported no temperature or power: %q", strings.TrimSpace(line))
	}
	return reading, nil
}
