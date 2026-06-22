package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const bytesPerMiB = 1024 * 1024

func collectNVIDIAGPU(ctx context.Context) GPUSet {
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		return GPUSet{Available: false, Error: "nvidia-smi not found"}
	}

	queryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(
		queryCtx,
		"nvidia-smi",
		"--query-gpu=name,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw",
		"--format=csv,noheader,nounits",
	)
	out, err := cmd.Output()
	if err != nil {
		return GPUSet{Available: false, Error: fmt.Sprintf("nvidia-smi failed: %v", err)}
	}
	return parseNVIDIAGPUCSV(string(out))
}

func parseNVIDIAGPUCSV(out string) GPUSet {
	reader := csv.NewReader(strings.NewReader(out))
	reader.TrimLeadingSpace = true
	rows, err := reader.ReadAll()
	if err != nil {
		return GPUSet{Available: false, Error: fmt.Sprintf("failed to parse nvidia-smi output: %v", err)}
	}

	devices := make([]GPUMetric, 0, len(rows))
	for _, row := range rows {
		if len(row) < 5 {
			continue
		}
		name := strings.TrimSpace(row[0])
		usage := parseOptionalFloat(row[1], "%")
		memUsedMiB, memUsedOK := parseNonNegativeFloat(row[2])
		memTotalMiB, memTotalOK := parseNonNegativeFloat(row[3])
		temp := parseOptionalFloat(row[4], "C")
		power := nvidiaPowerMetric(row[5:])

		memory := unavailableCapacity("nvidia-smi did not report memory")
		if memUsedOK && memTotalOK {
			memory = nvidiaMemoryCapacity(memUsedMiB, memTotalMiB)
		}

		devices = append(devices, GPUMetric{
			Name:        name,
			Usage:       usage,
			Power:       power,
			Memory:      memory,
			Temperature: temp,
		})
	}

	if len(devices) == 0 {
		return GPUSet{Available: false, Error: "nvidia-smi returned no GPU rows"}
	}
	return GPUSet{Available: true, Devices: devices}
}

func nvidiaMemoryCapacity(usedMiB, totalMiB float64) CapacityMetric {
	used, usedOK := mibFloatToBytes(usedMiB)
	total, totalOK := mibFloatToBytes(totalMiB)
	if !usedOK || !totalOK || total == 0 || used > total {
		return unavailableCapacity("invalid nvidia-smi memory counters")
	}
	return availableCapacity(used, total)
}

func mibFloatToBytes(value float64) (uint64, bool) {
	if !isFinite(value) || value < 0 {
		return 0, false
	}
	bytes := value * bytesPerMiB
	if !isFinite(bytes) || bytes > float64(^uint64(0)) {
		return 0, false
	}
	return uint64(bytes), true
}

func parseOptionalFloat(raw, unit string) NumberMetric {
	value, ok := parseFloat(raw)
	if !ok {
		return unavailableNumber(unit, "not reported")
	}
	return availableNumber(value, unit)
}

// nvidiaPowerMetric parses the optional power.draw column emitted by nvidia-smi.
// Older drivers or some board models report "[N/A]" / omit the column entirely,
// in which case power is reported as unavailable instead of failing the device.
func nvidiaPowerMetric(columns []string) NumberMetric {
	if len(columns) == 0 || strings.TrimSpace(columns[0]) == "" {
		return unavailableNumber("W", "nvidia-smi did not report power draw")
	}
	power := parseOptionalFloat(columns[0], "W")
	if power.Available && power.Value < 0 {
		return unavailableNumber("W", "invalid nvidia-smi power draw")
	}
	return power
}

func parseFloat(raw string) (float64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "N/A") || strings.EqualFold(raw, "[Not Supported]") {
		return 0, false
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || !isFinite(value) {
		return 0, false
	}
	return value, true
}

func parseNonNegativeFloat(raw string) (float64, bool) {
	value, ok := parseFloat(raw)
	if !ok || value < 0 {
		return 0, false
	}
	return value, true
}
