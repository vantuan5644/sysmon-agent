package main

import (
	"context"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// macOS uses the sampler's demand-aware whole-collection lane. No commands run
// in HTTP handlers, and no privileged helpers or persistent services are added.
type macSystemCollector struct {
	hostname  string
	mu        sync.Mutex
	network   map[string]macNetCounter
	networkAt time.Time
}

func NewSystemCollector() MetricsCollector {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "unknown"
	}
	return &macSystemCollector{hostname: hostname}
}

func (c *macSystemCollector) Hostname() string { return c.hostname }

func macRead(ctx context.Context, executable string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	data, err := cmd.Output()
	return string(data), err
}

func (c *macSystemCollector) Collect(ctx context.Context) (Metrics, error) {
	started := time.Now()
	m := baseMetrics(c.hostname)
	const unavailable = "not exposed by the unprivileged macOS collector"
	m.CPUCores = unavailableCPUCores(unavailable)
	m.CPUPower = unavailableNumber("W", unavailable)
	unavailableCPUPowerRails(&m, unavailable)
	degradedCPUClockSet(unavailable).applyTo(&m)
	m.CPUTemperature = unavailableNumber("C", unavailable)
	m.PSUOutputPower = unavailableNumber("W", unavailable)
	m.MemoryCommit = unavailableCapacity(unavailable)
	m.MemorySwap = unavailableCapacity(unavailable)
	m.Pressure = unavailablePressure(unavailable)
	m.Storage = unavailableStorage(unavailable)
	m.Tailscale = TailscaleStatus{Error: unavailable}
	m.Temperatures = TemperatureSet{Error: unavailable}
	m.GPU = GPUSet{Error: unavailable}
	m.Processes = unavailableProcessSet(unavailable)
	if output, err := macRead(ctx, "/usr/bin/top", "-l", "2", "-s", "1", "-n", "0"); err == nil {
		m.CPU = parseMacCPU(output)
	} else {
		m.CPU = unavailableNumber("%", "macOS top failed: "+err.Error())
	}
	totalText, totalErr := macRead(ctx, "/usr/sbin/sysctl", "-n", "hw.memsize")
	total, parseErr := strconv.ParseUint(strings.TrimSpace(totalText), 10, 64)
	if output, err := macRead(ctx, "/usr/bin/vm_stat"); err == nil && totalErr == nil && parseErr == nil {
		m.Memory = parseMacMemory(output, total)
	} else {
		m.Memory = unavailableCapacity("macOS memory counters unavailable")
	}
	// APFS Data holds user data on modern macOS. Older systems use the root.
	var stat syscall.Statfs_t
	mount := "/System/Volumes/Data"
	err := syscall.Statfs(mount, &stat)
	if err != nil {
		mount = "/"
		err = syscall.Statfs(mount, &stat)
	}
	if err == nil {
		total := stat.Blocks * uint64(stat.Bsize)
		free := stat.Bfree * uint64(stat.Bsize)
		m.Disks = []DiskMetric{{Name: "System disk", Mountpoint: mount, Capacity: availableCapacityFromTotalFree(total, free, "invalid macOS disk capacity")}}
	} else {
		m.Disks = unavailableDisk(err.Error())
	}
	output, err := macRead(ctx, "/usr/sbin/netstat", "-ibn")
	if err == nil {
		m.Network = c.networkRates(output, time.Now())
	} else {
		m.Network = NetworkSet{Error: "macOS network counters unavailable"}
	}
	return finishMetrics(m, started), nil
}

func (c *macSystemCollector) networkRates(output string, now time.Time) NetworkSet {
	counters, err := parseMacNetwork(output)
	if err != nil {
		return NetworkSet{Error: err.Error()}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	elapsed := now.Sub(c.networkAt).Seconds()
	result := NetworkSet{Available: true, Interfaces: []NetworkInterfaceMetric{}, Uplink: NetworkUplink{Error: "macOS uplink identification unavailable"}}
	for name, current := range counters {
		entry := NetworkInterfaceMetric{Name: name, RXBytesTotal: current.rx, TXBytesTotal: current.tx,
			RXBytesPerSecond: unavailableNumber("B/s", "waiting for network interval"), TXBytesPerSecond: unavailableNumber("B/s", "waiting for network interval")}
		if previous, ok := c.network[name]; ok && elapsed > 0 && elapsed <= 15 && current.rx >= previous.rx && current.tx >= previous.tx {
			entry.RXBytesPerSecond = availableNumber(float64(current.rx-previous.rx)/elapsed, "B/s")
			entry.TXBytesPerSecond = availableNumber(float64(current.tx-previous.tx)/elapsed, "B/s")
		}
		result.Interfaces = append(result.Interfaces, entry)
	}
	sort.Slice(result.Interfaces, func(i, j int) bool { return result.Interfaces[i].Name < result.Interfaces[j].Name })
	c.network, c.networkAt = counters, now
	return result
}
