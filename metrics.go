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

// CPUCoreSet reports per-logical-core utilization so the dashboard can show how
// many cores are carrying real load -- the signal for whether a workload is
// single- or multi-threaded, which the aggregate cpu_percent average hides (one
// pegged core on a 16-core host reads as ~6%). It degrades as a whole
// (Available:false + Error) like the other *Set metrics, since every core comes
// from one read. Cores holds each core's busy percent ordered by core index;
// Busy counts cores at or above BusyThreshold; Count is len(Cores).
type CPUCoreSet struct {
	Available     bool      `json:"available"`
	Cores         []float64 `json:"cores,omitempty"`
	Count         int       `json:"count"`
	Busy          int       `json:"busy"`
	BusyThreshold float64   `json:"busy_threshold"`
	Error         string    `json:"error,omitempty"`
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
	Uplink     NetworkUplink            `json:"uplink"`
	Error      string                   `json:"error,omitempty"`
}

// NetworkUplink names the host's active default-route network so the NET card
// can show what it is connected to: a Wi-Fi SSID or a wired link. Kind is
// "wifi" or "ethernet"; Name is the SSID or a wired label (e.g. "Ethernet" with
// an optional link speed). It degrades independently (Available:false + Error)
// when there is no default route or the SSID cannot be read.
type NetworkUplink struct {
	Available bool   `json:"available"`
	Kind      string `json:"kind,omitempty"`
	Name      string `json:"name,omitempty"`
	Error     string `json:"error,omitempty"`
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

// StorageDevice is one physical block device (NVMe/SATA SSD, HDD) carrying its
// own capacity and temperature, mirroring the GPUSet/GPUMetric precedent of
// "one device with its own metrics". Capacity aggregates only the MOUNTED
// filesystems backed by that device (a drive with nothing mounted degrades to
// unavailable rather than reporting a bogus 0%-of-physical-size); SizeBytes is
// the physical drive size, reported separately. Temperature degrades
// independently (a device with no hwmon still renders its model + capacity).
type StorageDevice struct {
	Name        string         `json:"name"`                  // "nvme0n1"
	Model       string         `json:"model"`                 // "CT4000T705SSD3"
	SizeBytes   uint64         `json:"size_bytes"`            // physical drive capacity
	Mountpoints []string       `json:"mountpoints,omitempty"` // mounted filesystems backing this device
	Capacity    CapacityMetric `json:"capacity"`              // aggregated over MOUNTED filesystems
	Temperature NumberMetric   `json:"temperature_celsius"`
}

// StorageSet is the per-drive storage view served to the dashboard's storage
// panel. It degrades as a whole (Available:false + Error) on platforms that
// cannot enumerate block devices, and per-field within each device. Mirrors the
// GPUSet shape so the same tolerant validator pattern applies.
type StorageSet struct {
	Available bool            `json:"available"`
	Devices   []StorageDevice `json:"devices"`
	Error     string          `json:"error,omitempty"`
}

// ProcessMetric is one row of the per-process "mini Task Manager + nvitop"
// page: a single PID with its CPU% (whole-host normalized 0..100), RAM
// (RSS/WorkingSet), GPU memory (CUDA/compute processes only), and Disk I/O
// rates. Each numeric field degrades independently per the core invariant, so
// a process the agent lacks privilege to read (e.g. another user's PID under a
// --user service) degrades just the unread fields rather than the row.
type ProcessMetric struct {
	PID       int          `json:"pid"`
	Name      string       `json:"name"`
	CPU       NumberMetric `json:"cpu_percent"`  // whole-host %, 0..100
	Memory    NumberMetric `json:"memory_bytes"` // RSS/WorkingSet, unit "B"
	GPUMemory NumberMetric `json:"gpu_memory"`   // bytes; unavailable for non-CUDA processes
	DiskRead  NumberMetric `json:"disk_read"`    // bytes/sec
	DiskWrite NumberMetric `json:"disk_write"`   // bytes/sec
}

// AppMetric is the Task-Manager-style aggregate of every ProcessMetric that
// shares an executable (grouped by the lowercased base name, with a .exe suffix
// stripped). Count is the number of PIDs merged. Each summed field is available
// if at least one contributing PID reported it.
type AppMetric struct {
	Name      string       `json:"name"`
	Count     int          `json:"count"`
	CPU       NumberMetric `json:"cpu_percent"`
	Memory    NumberMetric `json:"memory_bytes"`
	GPUMemory NumberMetric `json:"gpu_memory"`
	DiskRead  NumberMetric `json:"disk_read"`
	DiskWrite NumberMetric `json:"disk_write"`
}

// ProcessSet is the per-process view served to the new dashboard page. It
// carries both an Apps breakdown (processes grouped by executable) and a
// Processes view (one row per PID), each already trimmed to the top resource
// consumers by a union of per-column leaders (see processes.go). Total is the
// full host process count for the page header. It degrades as a whole
// (Available:false + Error) on platforms that cannot enumerate processes, and
// per-field within each row. Intentionally NOT rolled into collection_errors,
// like Tailscale: it is an optional subsystem whose absence on some platforms
// would be permanent noise; the Error string still ships in the JSON for the
// process page to render.
type ProcessSet struct {
	Available bool            `json:"available"`
	Total     int             `json:"total"`
	Apps      []AppMetric     `json:"apps,omitempty"`
	Processes []ProcessMetric `json:"processes,omitempty"`
	Error     string          `json:"error,omitempty"`
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
	Hostname   string       `json:"hostname"`
	OS         string       `json:"os"`
	Arch       string       `json:"arch"`
	Platform   string       `json:"platform,omitempty"`
	Timestamp  time.Time    `json:"timestamp"`
	CPUName    string       `json:"cpu_name,omitempty"`
	MemoryName string       `json:"memory_name,omitempty"`
	CPU        NumberMetric `json:"cpu_percent"`
	// CPUPower is whole-socket power. CPUCorePower/CPUSocPower/CPUMiscPower are
	// its per-rail breakdown, currently AMD-only (read from the SMU power table
	// via the LibreHardwareMonitor bridge) and unavailable elsewhere. The
	// distinction is worth keeping visible: AMD Adrenalin and Ryzen Master label
	// the cores-only rail "CPU power", which on a chiplet part runs ~30 W below
	// the socket total because it excludes the IO die (SoC) and misc rails.
	CPUPower     NumberMetric `json:"cpu_power"`
	CPUCorePower NumberMetric `json:"cpu_core_power"`
	CPUSocPower  NumberMetric `json:"cpu_soc_power"`
	CPUMiscPower NumberMetric `json:"cpu_misc_power"`
	// The four clock fields answer four different questions and are deliberately
	// NOT interchangeable -- conflating them is what made the same 7950X report a
	// 5.9 GHz ceiling on Linux and 5.5 GHz on Windows:
	//
	//   CPUClock         live clock, averaged across every core. On a 16-core part
	//                    this sits ~1 GHz below whichever core is boosting.
	//   CPUClockPeakCore live clock of the single fastest core right now. This is
	//                    the number that answers "does my CPU actually hit its
	//                    rated boost?"; the average never will.
	//   CPUClockMax      observed boost ceiling: a peak-hold of CPUClockPeakCore,
	//                    seeded near base and ratcheted up, never down. Same
	//                    definition on every platform, so the dashboard ring
	//                    scales identically everywhere. Process state -- it
	//                    re-seeds on restart (see cpuClockPeakTracker).
	//   CPUClockRated    the ceiling the firmware *declares*. Linux-only
	//                    (cpuinfo_max_freq); reported because on amd-pstate it is
	//                    an extrapolation of the CPPC perf table, not an
	//                    achievable clock -- on a 7950X it computes to
	//                    nominal_freq x highest_perf/nominal_perf = 4501 x 166/127
	//                    = 5883 MHz, ~180 MHz above the rated 5.7 GHz boost. It is
	//                    kept visible but is NOT what the ring scales to.
	CPUClock         NumberMetric `json:"cpu_clock"`
	CPUClockPeakCore NumberMetric `json:"cpu_clock_peak_core"`
	CPUClockMax      NumberMetric `json:"cpu_clock_max"`
	CPUClockRated    NumberMetric `json:"cpu_clock_rated"`
	CPUClockBase     NumberMetric `json:"cpu_clock_base"`
	CPUTemperature   NumberMetric `json:"cpu_temperature"`
	// CPUTemperatureSensor names the sensor CPUTemperature was read from, so a
	// reading is self-describing and two hosts can be checked for comparability
	// instead of assumed comparable. Empty when no sensor was resolved.
	CPUTemperatureSensor string          `json:"cpu_temperature_sensor,omitempty"`
	CPUCores             CPUCoreSet      `json:"cpu_cores"`
	PSUOutputPower       NumberMetric    `json:"psu_output_power"`
	Memory               CapacityMetric  `json:"memory"`
	MemorySwap           CapacityMetric  `json:"memory_swap"`
	Disks                []DiskMetric    `json:"disks"`
	Storage              StorageSet      `json:"storage"`
	Network              NetworkSet      `json:"network"`
	Tailscale            TailscaleStatus `json:"tailscale"`
	Temperatures         TemperatureSet  `json:"temperatures"`
	GPU                  GPUSet          `json:"gpu"`
	Processes            ProcessSet      `json:"processes"`
	CollectionDurationMS int64           `json:"collection_duration_ms"`
	CollectionErrors     []string        `json:"collection_errors,omitempty"`
}

// pickCPUTemperature returns the canonical whole-CPU die reading from a
// temperature set so the dashboard can show a dedicated CPU temperature next to
// CPU usage instead of only the global hottest sensor.
//
// It deliberately does NOT return the warmest CPU-ish sensor, which is what it
// used to do. Taking a max made `cpu_temperature` mean a different physical
// quantity per platform, because the candidate set is not comparable: Linux
// hwmon offers roughly `k10temp Tctl` + `Tccd1/2` -- one die -- while
// LibreHardwareMonitor offers ten survivors spanning four locations. Measured on
// a 7950X at idle, the max picked `Core (Tctl/Tdie)` at 59.5 C while the same
// tree also carried `Package` 43.34 C, `IOD Hotspot` 43.5 C, `CCD1 (Tdie)` 37 C,
// `L3 (CCD1)` 32.75 C and a board super-IO `... NCT6799D CPU` at 52 C. Comparing
// that number against a Linux host compares two different registers -- the same
// defect cpu_clock_peak.go documents for cpu_clock_max.
//
// So the picker ranks by what the sensor *is* (see cpuDieTemperatureRank) and
// only breaks ties within a rank by temperature, which still covers a genuine
// multi-socket or multi-die host. Sensors that are CPU-adjacent but are not the
// whole-CPU die -- per-CCD, L3, IO die, pre-averaged aggregates, and board
// super-IO socket sensors -- are excluded outright rather than allowed to win by
// being hot.
//
// Eligibility still comes from isCPUTemperatureSensor, which is unchanged and
// mirrored by isPrimaryCardTemperatureSensor in static/app.js; the ranking is
// layered on top so that mirror stays accurate.
func pickCPUTemperature(temps TemperatureSet) NumberMetric {
	value, _ := pickCPUTemperatureSensor(temps)
	return value
}

// cpuCanonicalTemperatureSensor names one platform's authoritative whole-CPU die
// sensor, so a host the agent knows about always reports the same register
// rather than whatever the ranking heuristics happen to select.
//
// Match mode differs by source because the naming does. Linux hwmon names are
// composed by collectHWMONTemperatures as "<chip> <label>" and both halves are
// stable, so those match exactly. LibreHardwareMonitor prefixes the CPU model
// ("AMD Ryzen 9 7950X Core (Tctl/Tdie)"), so only the tail is stable and a
// literal would break on the next CPU -- those match by suffix.
type cpuCanonicalTemperatureSensor struct {
	match  string
	suffix bool
	source string
}

// cpuCanonicalTemperatureSensors is consulted in order; the first entry with any
// matching sensor wins regardless of temperature. Adding a platform here is the
// preferred fix when a host picks a surprising sensor -- extend this table
// rather than loosening cpuDieTemperatureRank.
var cpuCanonicalTemperatureSensors = []cpuCanonicalTemperatureSensor{
	{match: "core (tctl/tdie)", suffix: true, source: "LibreHardwareMonitor, AMD"},
	{match: "k10temp tctl", source: "Linux hwmon, AMD"},
	{match: "coretemp package id 0", source: "Linux hwmon, Intel"},
	{match: "cpu package", suffix: true, source: "LibreHardwareMonitor, Intel"},
}

// pickCPUTemperatureSensor resolves the CPU temperature and also returns the
// name of the sensor it came from, which is published as
// Metrics.CPUTemperatureSensor. Reporting the name is the actual guard against
// this metric silently changing meaning again: whatever the selection rules do,
// two hosts (or two points in time) can be checked for comparability directly
// instead of being assumed comparable.
func pickCPUTemperatureSensor(temps TemperatureSet) (NumberMetric, string) {
	if !temps.Available {
		return unavailableNumber("C", temps.Error), ""
	}
	// Exact table first. Within one entry, ties go to the warmest so a
	// multi-socket host reports its hottest package rather than the first one
	// enumerated.
	for _, canonical := range cpuCanonicalTemperatureSensors {
		var best *NumberMetric
		bestName := ""
		for i := range temps.Sensors {
			sensor := temps.Sensors[i]
			if !sensor.Celsius.Available {
				continue
			}
			n := strings.ToLower(strings.TrimSpace(sensor.Name))
			matched := n == canonical.match
			if canonical.suffix {
				matched = strings.HasSuffix(n, canonical.match)
			}
			if !matched {
				continue
			}
			candidate := sensor.Celsius
			if best == nil || candidate.Value > best.Value {
				best = &candidate
				bestName = sensor.Name
			}
		}
		if best != nil {
			return *best, bestName
		}
	}
	return rankCPUTemperature(temps)
}

// rankCPUTemperature is the fallback for a host with no canonical sensor in the
// table: an unknown CPU, an unusual hwmon driver, or a future LHM rename. It
// still refuses to simply take the warmest CPU-ish sensor.
func rankCPUTemperature(temps TemperatureSet) (NumberMetric, string) {
	var best *NumberMetric
	bestName := ""
	bestRank := cpuTempRankNone
	// Fallback across eligible-but-not-die sensors, used only when a host
	// exposes no whole-CPU die reading at all (e.g. a board that reports just a
	// socket sensor). Degrading to the old warmest-wins behaviour there beats
	// reporting nothing.
	var fallback *NumberMetric
	fallbackName := ""
	for i := range temps.Sensors {
		sensor := temps.Sensors[i]
		if !sensor.Celsius.Available {
			continue
		}
		if !isCPUTemperatureSensor(sensor.Name) {
			continue
		}
		candidate := sensor.Celsius
		rank := cpuDieTemperatureRank(sensor.Name)
		if rank == cpuTempRankNone {
			if fallback == nil || candidate.Value > fallback.Value {
				fallback = &candidate
				fallbackName = sensor.Name
			}
			continue
		}
		if best == nil || rank < bestRank || (rank == bestRank && candidate.Value > best.Value) {
			best = &candidate
			bestRank = rank
			bestName = sensor.Name
		}
	}
	if best != nil {
		return *best, bestName
	}
	if fallback != nil {
		return *fallback, fallbackName
	}
	return unavailableNumber("C", "no CPU temperature sensor reported"), ""
}

// Ranks for cpuDieTemperatureRank, best first. Ordering encodes which sensor is
// the canonical whole-CPU die reading on each vendor: AMD publishes Tctl/Tdie,
// Intel publishes a package sensor (`coretemp Package id 0`), and a per-core
// reading is the last resort that still describes actual CPU silicon.
const (
	cpuTempRankTctl    = iota // AMD "Core (Tctl/Tdie)", "k10temp Tctl"
	cpuTempRankPackage        // Intel "coretemp Package id 0", AMD LHM "Package"
	cpuTempRankCore           // "coretemp Core 0"
	cpuTempRankGeneric        // bare "CPU", vendor model strings
	cpuTempRankNone           // not a whole-CPU die reading; never picked over the above
)

// cpuDieTemperatureRank classifies an already-eligible sensor name as a
// whole-CPU die reading and ranks it, or returns cpuTempRankNone for a sensor
// that describes something narrower than the whole CPU.
//
// Tctl outranks Package deliberately. On AMD the two are distinct registers that
// disagree by ~16 C at idle, so folding them into one tier and taking the max
// would reintroduce exactly the arbitrariness this function exists to remove.
func cpuDieTemperatureRank(name string) int {
	n := strings.ToLower(name)
	// Fragments here are matched as substrings, so each one is chosen to be
	// unambiguous: "soc" would match "socket", "iod" would match "diode", and
	// "nct" would match "junction", each silently disqualifying a legitimate CPU
	// die sensor. Prefer a longer, unmistakable fragment over a short one.
	for _, fragment := range []string{
		// Narrower than the whole CPU: one core-complex die, cache, IO die, or a
		// value LHM has already averaged/maxed for display. The IO die needs no
		// fragment of its own -- LHM names it "IOD Hotspot", caught below.
		"ccd", "l3", "cache", "hotspot", "hot spot", "average",
		// Board super-IO chips expose a socket-adjacent sensor also called "CPU".
		// It reads the socket, not the die, and on this ASUS X670E board it sat
		// 7 C below Tctl -- close enough to look plausible, far enough to poison
		// a cross-host comparison.
		"nuvoton", "nct6", "it87", "w83", "f718",
		// Rails and regulators, not silicon.
		"vrm", "vddcr", "vcore",
	} {
		if strings.Contains(n, fragment) {
			return cpuTempRankNone
		}
	}
	switch {
	case strings.Contains(n, "tctl"), strings.Contains(n, "tdie"):
		return cpuTempRankTctl
	case strings.Contains(n, "package"):
		return cpuTempRankPackage
	case strings.Contains(n, "core"):
		return cpuTempRankCore
	default:
		return cpuTempRankGeneric
	}
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

// unavailableCPUPowerRails marks the per-rail CPU power breakdown
// (core/SoC/misc) unavailable with a single message. Those rails are AMD SMU
// telemetry read through the Windows LibreHardwareMonitor bridge, so every other
// platform reports them explicitly absent rather than leaving the fields at
// their zero value with no Error to explain why.
func unavailableCPUPowerRails(m *Metrics, message string) {
	m.CPUCorePower = unavailableNumber("W", message)
	m.CPUSocPower = unavailableNumber("W", message)
	m.CPUMiscPower = unavailableNumber("W", message)
}

// cpuCoreBusyPercent is the per-core utilization at or above which a logical
// core counts as "busy" in CPUCoreSet.Busy. 80% counts a saturated core without
// flicker from background scheduler noise on otherwise-idle cores.
const cpuCoreBusyPercent = 80.0

// availableCPUCores builds a populated CPUCoreSet from per-core busy percentages
// (ordered by core index), counting how many sit at or above cpuCoreBusyPercent.
// An empty slice degrades to unavailable so the dashboard never renders a 0/0
// grid.
func availableCPUCores(cores []float64) CPUCoreSet {
	if len(cores) == 0 {
		return unavailableCPUCores("no per-core utilization reported")
	}
	busy := 0
	for _, percent := range cores {
		if percent >= cpuCoreBusyPercent {
			busy++
		}
	}
	return CPUCoreSet{
		Available:     true,
		Cores:         cores,
		Count:         len(cores),
		Busy:          busy,
		BusyThreshold: cpuCoreBusyPercent,
	}
}

func unavailableCPUCores(message string) CPUCoreSet {
	return CPUCoreSet{Available: false, BusyThreshold: cpuCoreBusyPercent, Error: message}
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

// unavailableStorage marks the whole StorageSet unavailable with an error. It
// is the platform-absent / read-failure fallback, mirroring unavailableDisk.
func unavailableStorage(message string) StorageSet {
	return StorageSet{Available: false, Error: message}
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
	// Storage degrades as a whole (unavailable set) or per-field within each
	// device (a drive with no mounted filesystem degrades only its capacity, not
	// its temperature). Roll every degraded field up so a missing storage sensor
	// is visible in collection_errors -- otherwise the panel silently shows gaps.
	if !metrics.Storage.Available {
		add("storage", metrics.Storage.Error)
	} else {
		for _, device := range metrics.Storage.Devices {
			label := firstNonEmpty(device.Model, device.Name, "unknown")
			if !device.Capacity.Available {
				add("storage "+label+" capacity", device.Capacity.Error)
			}
			if !device.Temperature.Available {
				add("storage "+label+" temperature", device.Temperature.Error)
			}
		}
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
