package main

import (
	"math"
	"runtime"
	"strings"
	"time"
)

type NumberMetric struct {
	Available bool    `json:"available"`
	Value     float64 `json:"value"`
	Unit      string  `json:"unit,omitempty"`
	Error     string  `json:"error,omitempty"`
}

type CapacityMetric struct {
	Available  bool    `json:"available"`
	UsedBytes  uint64  `json:"used_bytes"`
	TotalBytes uint64  `json:"total_bytes"`
	Percent    float64 `json:"percent"`
	Error      string  `json:"error,omitempty"`
}

type DiskMetric struct {
	Name       string         `json:"name"`
	Mountpoint string         `json:"mountpoint"`
	FSType     string         `json:"fs_type,omitempty"`
	Capacity   CapacityMetric `json:"capacity"`
}

type NetworkInterfaceMetric struct {
	Name             string       `json:"name"`
	RXBytesTotal     uint64       `json:"rx_bytes_total"`
	TXBytesTotal     uint64       `json:"tx_bytes_total"`
	RXBytesPerSecond NumberMetric `json:"rx_bytes_per_second"`
	TXBytesPerSecond NumberMetric `json:"tx_bytes_per_second"`
}

type NetworkSet struct {
	Available  bool                     `json:"available"`
	Interfaces []NetworkInterfaceMetric `json:"interfaces"`
	Error      string                   `json:"error,omitempty"`
}

type TemperatureMetric struct {
	Name    string       `json:"name"`
	Celsius NumberMetric `json:"celsius"`
}

type TemperatureSet struct {
	Available bool                `json:"available"`
	Sensors   []TemperatureMetric `json:"sensors"`
	Error     string              `json:"error,omitempty"`
}

type GPUMetric struct {
	Name        string         `json:"name"`
	Usage       NumberMetric   `json:"usage_percent"`
	Power       NumberMetric   `json:"power_watts"`
	Memory      CapacityMetric `json:"memory"`
	Temperature NumberMetric   `json:"temperature_celsius"`
}

type GPUSet struct {
	Available bool        `json:"available"`
	Devices   []GPUMetric `json:"devices"`
	Error     string      `json:"error,omitempty"`
}

// TailscaleStatus reports the host's Tailscale daemon state: whether the node
// is online to the coordination server and whether it is routing through an
// exit node. It degrades independently (a missing/offline daemon reports
// Available:false) and is collected from `tailscale status --json`.
type TailscaleStatus struct {
	Available       bool   `json:"available"`
	Online          bool   `json:"online"`            // Self.Online: logged in and reachable to the control plane
	ExitNodeEnabled bool   `json:"exit_node_enabled"` // currently routing through a selected exit node
	Error           string `json:"error,omitempty"`
}

type Metrics struct {
	Hostname             string          `json:"hostname"`
	OS                   string          `json:"os"`
	Arch                 string          `json:"arch"`
	Platform             string          `json:"platform,omitempty"`
	Timestamp            time.Time       `json:"timestamp"`
	CPU                  NumberMetric    `json:"cpu_percent"`
	CPUPower             NumberMetric    `json:"cpu_power"`
	CPUClock             NumberMetric    `json:"cpu_clock"`
	CPUClockMax          NumberMetric    `json:"cpu_clock_max"`
	CPUTemperature       NumberMetric    `json:"cpu_temperature"`
	PSUOutputPower       NumberMetric    `json:"psu_output_power"`
	Memory               CapacityMetric  `json:"memory"`
	MemorySwap           CapacityMetric  `json:"memory_swap"`
	Disks                []DiskMetric    `json:"disks"`
	Network              NetworkSet      `json:"network"`
	Tailscale            TailscaleStatus `json:"tailscale"`
	Temperatures         TemperatureSet  `json:"temperatures"`
	GPU                  GPUSet          `json:"gpu"`
	CollectionDurationMS int64           `json:"collection_duration_ms"`
	CollectionErrors     []string        `json:"collection_errors,omitempty"`
}

// pickCPUTemperature returns the best CPU package/core reading from a temperature
// set so the dashboard can show a dedicated CPU temperature next to CPU usage
// instead of only the global hottest sensor. GPU die sensors are excluded
// because each GPU device already carries its own temperature_celsius field;
// the picker then prefers a CPU die by name and returns the warmest plausible
// candidate so a stuck low reading does not mask a real hot core.
func pickCPUTemperature(temps TemperatureSet) NumberMetric {
	if !temps.Available {
		return unavailableNumber("C", temps.Error)
	}
	var best *NumberMetric
	for i := range temps.Sensors {
		sensor := temps.Sensors[i]
		if !sensor.Celsius.Available {
			continue
		}
		if !isCPUTemperatureSensor(sensor.Name) {
			continue
		}
		candidate := sensor.Celsius
		if best == nil || candidate.Value > best.Value {
			best = &candidate
		}
	}
	if best != nil {
		return *best
	}
	return unavailableNumber("C", "no CPU temperature sensor reported")
}

// isCPUTemperatureSensor classifies a sensor name as a CPU die reading. It first
// rejects obviously non-CPU sensors (GPU, RAM, disks, board, water) and then
// matches CPU-specific substrings covering Linux hwmon (k10temp/coretemp) and
// LibreHardwareMonitor naming ("AMD Ryzen ... Package", "Core (Tctl/Tdie)").
func isCPUTemperatureSensor(name string) bool {
	n := strings.ToLower(name)
	for _, fragment := range []string{
		"gpu", "nvidia", "geforce", "radeon", "arc",
		"dimm", "ram", "memory",
		"water", "ambient", "board", "chipset", "motherboard",
		"hdd", "ssd", "nvme", "disk",
		"psu", "battery",
	} {
		if strings.Contains(n, fragment) {
			return false
		}
	}
	for _, fragment := range []string{
		"cpu", "core", "package", "socket",
		"tctl", "tdie", "tcase",
		"k10temp", "coretemp", "k8temp",
		"ryzen", "xeon", "epyc", "threadripper",
	} {
		if strings.Contains(n, fragment) {
			return true
		}
	}
	return false
}

func baseMetrics(hostname string) Metrics {
	return Metrics{
		Hostname:  hostname,
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		Timestamp: time.Now().UTC(),
	}
}

func availableNumber(value float64, unit string) NumberMetric {
	if !isFinite(value) {
		return unavailableNumber(unit, "invalid numeric value")
	}
	return NumberMetric{Available: true, Value: round(value, 2), Unit: unit}
}

func unavailableNumber(unit, message string) NumberMetric {
	return NumberMetric{Available: false, Unit: unit, Error: message}
}

func availableCapacity(used, total uint64) CapacityMetric {
	if total == 0 {
		return unavailableCapacity("capacity total is zero")
	}
	if used > total {
		used = total
	}
	percent := (float64(used) / float64(total)) * 100
	return CapacityMetric{
		Available:  true,
		UsedBytes:  used,
		TotalBytes: total,
		Percent:    round(percent, 2),
	}
}

func availableCapacityFromTotalFree(total, free uint64, invalidMessage string) CapacityMetric {
	if free > total {
		return unavailableCapacity(invalidMessage)
	}
	return availableCapacity(total-free, total)
}

func kibToBytes(value uint64) (uint64, bool) {
	if value > ^uint64(0)/1024 {
		return 0, false
	}
	return value * 1024, true
}

func sumUint64(values ...uint64) (uint64, bool) {
	var total uint64
	for _, value := range values {
		if total > ^uint64(0)-value {
			return 0, false
		}
		total += value
	}
	return total, true
}

func unavailableCapacity(message string) CapacityMetric {
	return CapacityMetric{Available: false, Error: message}
}

func unavailableDisk(message string) []DiskMetric {
	return []DiskMetric{{
		Name:     "unavailable",
		Capacity: unavailableCapacity(message),
	}}
}

func ensureDiskMetrics(disks []DiskMetric, unavailableMessage string) []DiskMetric {
	if len(disks) > 0 {
		return disks
	}
	return unavailableDisk(unavailableMessage)
}

func summarizeCollectionErrors(metrics Metrics) []string {
	var errors []string
	add := func(name, message string) {
		message = strings.TrimSpace(message)
		if message == "" {
			return
		}
		errors = append(errors, name+": "+message)
	}

	if !metrics.CPU.Available {
		add("cpu_percent", metrics.CPU.Error)
	}
	if !metrics.CPUPower.Available {
		add("cpu_power", metrics.CPUPower.Error)
	}
	if !metrics.CPUClock.Available {
		add("cpu_clock", metrics.CPUClock.Error)
	}
	if !metrics.CPUTemperature.Available {
		add("cpu_temperature", metrics.CPUTemperature.Error)
	}
	if !metrics.PSUOutputPower.Available {
		add("psu_output_power", metrics.PSUOutputPower.Error)
	}
	if !metrics.Memory.Available {
		add("memory", metrics.Memory.Error)
	}
	// Swap is surfaced on the RAM card's detail line and degrades independently.
	// The common "no swap configured" state (and the unset zero value) is
	// intentionally NOT rolled into collection_errors -- many hosts legitimately
	// run without swap or use zram, so reporting it as a permanent error is
	// noise, just like an absent Tailscale daemon. A genuine read failure still
	// surfaces here.
	if !metrics.MemorySwap.Available && metrics.MemorySwap.Error != "" &&
		metrics.MemorySwap.Error != "no swap configured" {
		add("swap", metrics.MemorySwap.Error)
	}
	for _, disk := range metrics.Disks {
		if disk.Capacity.Available {
			continue
		}
		label := firstNonEmpty(disk.Mountpoint, disk.Name, "unknown")
		add("disk "+label, disk.Capacity.Error)
	}
	if !metrics.Network.Available {
		add("network", metrics.Network.Error)
	}
	// Tailscale is intentionally NOT rolled into collection_errors. Unlike a
	// sensor (CPU/disk/network), it is an optional status indicator: many hosts
	// legitimately have no Tailscale daemon installed, and surfacing "tailscale
	// CLI not found" as a permanent issue on every such host is pure noise. Its
	// state is conveyed by the NET card's status pill (online/offline/absent);
	// the Error string still ships in the JSON for direct API consumers and
	// validateMetricsShape keeps the field's shape honest.
	if !metrics.Temperatures.Available {
		add("temperatures", metrics.Temperatures.Error)
	} else {
		for _, sensor := range metrics.Temperatures.Sensors {
			if sensor.Celsius.Available {
				continue
			}
			add("temperature "+firstNonEmpty(sensor.Name, "unknown"), sensor.Celsius.Error)
		}
	}
	if strings.TrimSpace(metrics.GPU.Error) != "" {
		add("gpu", metrics.GPU.Error)
	} else if !metrics.GPU.Available {
		add("gpu", metrics.GPU.Error)
	}
	if metrics.GPU.Available {
		for _, device := range metrics.GPU.Devices {
			label := firstNonEmpty(device.Name, "unknown")
			if !device.Usage.Available {
				add("gpu "+label+" usage", device.Usage.Error)
			}
			if !device.Memory.Available {
				add("gpu "+label+" memory", device.Memory.Error)
			}
			if !device.Power.Available {
				add("gpu "+label+" power", device.Power.Error)
			}
			if !device.Temperature.Available {
				add("gpu "+label+" temperature", device.Temperature.Error)
			}
		}
	}
	if len(errors) == 0 {
		return nil
	}
	return errors
}

func finishMetrics(metrics Metrics, started time.Time) Metrics {
	metrics.CollectionDurationMS = collectionDurationMS(started)
	metrics.CollectionErrors = summarizeCollectionErrors(metrics)
	return metrics
}

func collectionDurationMS(started time.Time) int64 {
	if started.IsZero() {
		return 0
	}
	elapsed := time.Since(started)
	if elapsed < 0 {
		return 0
	}
	return elapsed.Milliseconds()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func round(value float64, places int) float64 {
	if !isFinite(value) {
		return 0
	}
	if places <= 0 {
		return math.Round(value)
	}
	mul := 1.0
	for i := 0; i < places; i++ {
		mul *= 10
		if !isFinite(mul) {
			return value
		}
	}
	scaled := value * mul
	if !isFinite(scaled) {
		return value
	}
	return math.Round(scaled) / mul
}

func isFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
