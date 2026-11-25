//go:build linux

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// AMD and Intel GPU utilization from DRM fdinfo.
//
// The kernel publishes, for every open DRM file (a "client"), how long each
// engine class has spent on that client's work, in
// /proc/<pid>/fdinfo/<fd> (Documentation/gpu/drm-usage-stats.rst):
//
//	drm-driver:	amdgpu
//	drm-pdev:	0000:03:00.0
//	drm-client-id:	22
//	drm-engine-gfx:	15985792485 ns
//
// i915 and amdgpu count nanoseconds (drm-engine-<class>), and i915 divides a
// class by drm-engine-capacity-<class> when it has several engines of it; xe
// counts GPU cycles instead (drm-cycles-<class> against
// drm-total-cycles-<class>). Two reads give a class's share of the interval,
// the classes summed over clients give the device's engines, and the
// busiest class is its load. That is the only usage figure an Intel iGPU has
// (i915 and xe have no gpu_busy_percent), and the only per-process GPU figure
// on Linux without NVML.
//
// Finding the files means reading every process's fd table, so the full walk
// runs at most every drmFdinfoWalkEvery and is capped (drmFdinfoMaxReads
// fdinfo reads, drmFdinfoMaxFds descriptors); between walks only the fdinfo
// files already known to be DRM clients are read again. It runs in the slow
// lane, so it parks with the lane when nobody is watching, and the first pass
// after a park only takes baselines (drmFdinfoMaxGap), never a spike.
//
// A process whose fd table the agent cannot read (another user's, for an
// agent not running as root) is reported as unavailable rather than 0%, and
// so is one started after the last walk.

const (
	drmFdinfoWalkEvery = 30 * time.Second
	drmFdinfoMaxReads  = 4096
	drmFdinfoMaxFds    = 200000
	drmFdinfoMaxBytes  = 64 << 10
	// drmFdinfoMaxGap is the longest interval a delta is taken over; a
	// longer one (the slow lane parked) rebaselines instead.
	drmFdinfoMaxGap = 20 * time.Second
)

// drmEngineCounter is one engine class of one client.
type drmEngineCounter struct {
	busy     uint64 // ns, or cycles when cycles is set
	total    uint64 // total cycles (xe)
	cycles   bool
	capacity uint64
}

// drmFdinfoRecord is one parsed fdinfo file of a DRM client.
type drmFdinfoRecord struct {
	driver   string
	pdev     string
	clientID string
	engines  map[string]drmEngineCounter
}

// parseDRMFdinfo reads the drm-* keys of an fdinfo file. ok is false for a
// file that is not a DRM client (no drm-client-id).
func parseDRMFdinfo(r io.Reader) (drmFdinfoRecord, bool) {
	record := drmFdinfoRecord{engines: map[string]drmEngineCounter{}}
	ns := map[string]uint64{}
	cycles := map[string]uint64{}
	totals := map[string]uint64{}
	capacity := map[string]uint64{}
	scanner := bufio.NewScanner(io.LimitReader(r, drmFdinfoMaxBytes))
	scanner.Buffer(make([]byte, 0, 4096), 4096)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ":")
		if !ok || !strings.HasPrefix(key, "drm-") {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch {
		case key == "drm-driver":
			record.driver = value
		case key == "drm-pdev":
			record.pdev = strings.ToLower(value)
		case key == "drm-client-id":
			record.clientID = value
		case strings.HasPrefix(key, "drm-engine-capacity-"):
			if n, err := strconv.ParseUint(value, 10, 64); err == nil {
				capacity[strings.TrimPrefix(key, "drm-engine-capacity-")] = n
			}
		case strings.HasPrefix(key, "drm-engine-"):
			number, unit, _ := strings.Cut(value, " ")
			if strings.TrimSpace(unit) != "ns" {
				continue
			}
			if n, err := strconv.ParseUint(number, 10, 64); err == nil {
				ns[strings.TrimPrefix(key, "drm-engine-")] = n
			}
		case strings.HasPrefix(key, "drm-total-cycles-"):
			if n, err := strconv.ParseUint(value, 10, 64); err == nil {
				totals[strings.TrimPrefix(key, "drm-total-cycles-")] = n
			}
		case strings.HasPrefix(key, "drm-cycles-"):
			if n, err := strconv.ParseUint(value, 10, 64); err == nil {
				cycles[strings.TrimPrefix(key, "drm-cycles-")] = n
			}
		}
	}
	if record.clientID == "" {
		return drmFdinfoRecord{}, false
	}
	for class, busy := range ns {
		record.engines[class] = drmEngineCounter{busy: busy, capacity: capacity[class]}
	}
	for class, busy := range cycles {
		if total, ok := totals[class]; ok {
			record.engines[class] = drmEngineCounter{busy: busy, total: total, cycles: true, capacity: capacity[class]}
		}
	}
	return record, true
}

// drmFdinfoReadResult says what reading one fdinfo path found.
type drmFdinfoReadResult int

const (
	drmFdinfoClient     drmFdinfoReadResult = iota // a DRM client
	drmFdinfoGone                                  // closed, exited, or no longer DRM
	drmFdinfoUnreadable                            // permission denied or another error
)

func readDRMFdinfoFile(path string) (drmFdinfoRecord, drmFdinfoReadResult) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			return drmFdinfoRecord{}, drmFdinfoGone
		}
		return drmFdinfoRecord{}, drmFdinfoUnreadable
	}
	defer file.Close()
	record, ok := parseDRMFdinfo(file)
	if !ok {
		return drmFdinfoRecord{}, drmFdinfoGone
	}
	return record, drmFdinfoClient
}

// drmFdinfoPath is a known DRM client fd.
type drmFdinfoPath struct {
	path string
	pid  int
}

// drmClientReading is one client's counters at one read.
type drmClientReading struct {
	pid    int
	at     time.Time
	record drmFdinfoRecord
}

// drmDeviceUsage is one device's load from fdinfo.
type drmDeviceUsage struct {
	Load    NumberMetric
	Engines []GPUEngine
}

// drmUsageSummary is one sample of every AMD/Intel device's clients.
type drmUsageSummary struct {
	Available bool
	Error     string
	// Devices is keyed by PCI address (drm-pdev).
	Devices map[string]drmDeviceUsage
	// Processes holds every process measured this sample, idle ones at 0.
	Processes map[int]processGPUUsage
	// Scanned is every process whose fd table the last walk read.
	Scanned map[int]bool
	// Unreadable is every process whose fd table or fdinfo was denied.
	Unreadable map[int]bool
	// Capped is set when the last walk stopped at a cap.
	Capped bool
}

func unavailableDRMUsageSummary(message string) drmUsageSummary {
	return drmUsageSummary{Available: false, Error: message}
}

// processUsage is a process row's GPU figure from this summary.
func (s drmUsageSummary) processUsage(pid int) (NumberMetric, string) {
	if !s.Available {
		return unavailableNumber("%", firstNonEmpty(s.Error, "GPU usage sampler unavailable")), ""
	}
	if s.Unreadable[pid] {
		return unavailableNumber("%", "GPU file descriptors not readable by the agent"), ""
	}
	if usage, ok := s.Processes[pid]; ok {
		return availableNumber(usage.Percent, "%"), usage.Engine
	}
	if s.Scanned[pid] {
		return availableNumber(0, "%"), ""
	}
	if s.Capped {
		return unavailableNumber("%", "GPU client scan stopped at its cap"), ""
	}
	return unavailableNumber("%", "process not scanned for GPU clients yet"), ""
}

// drmFdinfoSampler finds DRM clients and meters them between samples.
type drmFdinfoSampler struct {
	mu        sync.Mutex
	procRoot  string
	walkEvery time.Duration
	maxReads  int
	maxFds    int
	maxGap    time.Duration
	now       func() time.Time

	walked     bool
	lastWalk   time.Time
	known      []drmFdinfoPath
	scanned    map[int]bool
	unreadable map[int]bool
	capped     bool
	meter      drmUsageMeter
}

func newDRMFdinfoSampler(procRoot string) *drmFdinfoSampler {
	return &drmFdinfoSampler{
		procRoot:  procRoot,
		walkEvery: drmFdinfoWalkEvery,
		maxReads:  drmFdinfoMaxReads,
		maxFds:    drmFdinfoMaxFds,
		maxGap:    drmFdinfoMaxGap,
		now:       time.Now,
	}
}

// Sample reads the clients of the devices in wanted (PCI addresses) and
// returns their usage since the previous sample.
func (s *drmFdinfoSampler) Sample(wanted map[string]bool) drmUsageSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(wanted) == 0 {
		return unavailableDRMUsageSummary("no AMD/Intel GPU with a PCI address")
	}
	started := s.now()
	var readings []drmClientReading
	if !s.walked || started.Sub(s.lastWalk) >= s.walkEvery || started.Before(s.lastWalk) {
		readings = s.walk(wanted)
		s.walked = true
		s.lastWalk = started
	} else {
		readings = s.reread(wanted)
	}
	return s.meter.measure(readings, wanted, s.maxGap, len(s.unreadable) > 0 || s.capped, s.summaryShell())
}

// summaryShell copies the walk's sets, which the next sample replaces.
func (s *drmFdinfoSampler) summaryShell() drmUsageSummary {
	copySet := func(set map[int]bool) map[int]bool {
		out := make(map[int]bool, len(set))
		for pid := range set {
			out[pid] = true
		}
		return out
	}
	return drmUsageSummary{Available: true, Scanned: copySet(s.scanned), Unreadable: copySet(s.unreadable), Capped: s.capped}
}

// walk reads every process's fd table, in pid order, up to the caps.
// Known clients of processes the walk did not reach (it stopped at a cap)
// are read again so they keep their baselines.
func (s *drmFdinfoSampler) walk(wanted map[string]bool) []drmClientReading {
	scanned := map[int]bool{}
	unreadable := map[int]bool{}
	capped := false
	var known []drmFdinfoPath
	var readings []drmClientReading

	entries, _ := os.ReadDir(s.procRoot)
	pids := make([]int, 0, len(entries))
	for _, entry := range entries {
		if pid, ok := parsePID(entry.Name()); ok {
			pids = append(pids, pid)
		}
	}
	sort.Ints(pids)

	reads, fds := 0, 0
	lastVisited := 0
	for _, pid := range pids {
		pidDir := filepath.Join(s.procRoot, strconv.Itoa(pid))
		names, err := readDirNamesCapped(filepath.Join(pidDir, "fd"), s.maxFds-fds)
		if err != nil {
			if errors.Is(err, errDirCapped) {
				capped = true
				break
			}
			if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ESRCH) {
				unreadable[pid] = true
			}
			lastVisited = pid
			continue
		}
		fds += len(names)
		pidCapped := false
		var pidReadings []drmClientReading
		var pidKnown []drmFdinfoPath
		pidDenied := false
		for _, name := range names {
			if link, err := os.Readlink(filepath.Join(pidDir, "fd", name)); err == nil && !strings.HasPrefix(link, "/dev/dri/") {
				continue
			}
			if reads >= s.maxReads {
				pidCapped = true
				break
			}
			reads++
			path := filepath.Join(pidDir, "fdinfo", name)
			record, result := readDRMFdinfoFile(path)
			switch result {
			case drmFdinfoUnreadable:
				pidDenied = true
			case drmFdinfoClient:
				if !wanted[record.pdev] {
					continue
				}
				pidKnown = append(pidKnown, drmFdinfoPath{path: path, pid: pid})
				pidReadings = append(pidReadings, drmClientReading{pid: pid, at: s.now(), record: record})
			}
		}
		if pidCapped {
			capped = true
			break
		}
		lastVisited = pid
		known = append(known, pidKnown...)
		readings = append(readings, pidReadings...)
		if pidDenied {
			unreadable[pid] = true
		} else {
			scanned[pid] = true
		}
	}

	if capped {
		for _, entry := range s.known {
			if entry.pid <= lastVisited {
				continue
			}
			record, result := readDRMFdinfoFile(entry.path)
			switch result {
			case drmFdinfoClient:
				if wanted[record.pdev] {
					known = append(known, entry)
					readings = append(readings, drmClientReading{pid: entry.pid, at: s.now(), record: record})
				}
			case drmFdinfoUnreadable:
				known = append(known, entry)
				unreadable[entry.pid] = true
			}
		}
	}
	s.known, s.scanned, s.unreadable, s.capped = known, scanned, unreadable, capped
	return readings
}

// reread reads the known client paths again, dropping closed ones.
func (s *drmFdinfoSampler) reread(wanted map[string]bool) []drmClientReading {
	known := s.known[:0]
	var readings []drmClientReading
	for _, entry := range s.known {
		record, result := readDRMFdinfoFile(entry.path)
		switch result {
		case drmFdinfoClient:
			if !wanted[record.pdev] {
				continue
			}
			known = append(known, entry)
			readings = append(readings, drmClientReading{pid: entry.pid, at: s.now(), record: record})
		case drmFdinfoUnreadable:
			known = append(known, entry)
			if s.unreadable == nil {
				s.unreadable = map[int]bool{}
			}
			s.unreadable[entry.pid] = true
		}
	}
	s.known = known
	return readings
}

var errDirCapped = errors.New("directory exceeds the remaining fd cap")

// readDirNamesCapped lists a directory in batches, failing with errDirCapped
// rather than reading more than limit names.
func readDirNamesCapped(dir string, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, errDirCapped
	}
	file, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var names []string
	for {
		batch, err := file.Readdirnames(256)
		names = append(names, batch...)
		if len(names) > limit {
			return nil, errDirCapped
		}
		if err == io.EOF {
			return names, nil
		}
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			return names, nil
		}
	}
}

type drmClientKey struct{ pdev, client string }

type drmClassKey struct {
	drmClientKey
	class string
}

type drmBaseline struct {
	busy, total uint64
	at          time.Time
	sample      uint64
}

type drmClientSeen struct {
	at     time.Time
	sample uint64
}

// drmUsageMeter turns counter readings into shares of each interval. Only a
// baseline from the immediately previous sample counts, so a client that
// vanished for a sample, or a sample after a long park, starts over.
type drmUsageMeter struct {
	sample    uint64
	baselines map[drmClassKey]drmBaseline
	clients   map[drmClientKey]drmClientSeen
}

func (m *drmUsageMeter) measure(readings []drmClientReading, wanted map[string]bool, maxGap time.Duration, someUnknown bool, summary drmUsageSummary) drmUsageSummary {
	previous := m.sample
	m.sample++
	baselines := map[drmClassKey]drmBaseline{}
	clients := map[drmClientKey]drmClientSeen{}

	type accumulator struct {
		classes  map[string]float64
		measured bool
		clients  int
	}
	devices := map[string]*accumulator{}
	for pdev := range wanted {
		devices[pdev] = &accumulator{classes: map[string]float64{}}
	}
	pids := map[int]*accumulator{}
	share := func(value float64) float64 { return math.Max(0, math.Min(1, value)) }

	for _, reading := range readings {
		key := drmClientKey{reading.record.pdev, reading.record.clientID}
		if _, dup := clients[key]; dup {
			continue // the same client in a dup'd fd or a forked child
		}
		clients[key] = drmClientSeen{at: reading.at, sample: m.sample}
		device := devices[key.pdev]
		if device == nil {
			continue
		}
		device.clients++
		process := pids[reading.pid]
		if process == nil {
			process = &accumulator{classes: map[string]float64{}}
			pids[reading.pid] = process
		}
		seen, seenBefore := m.clients[key]
		seenPrevious := seenBefore && previous > 0 && seen.sample == previous && reading.at.Sub(seen.at) <= maxGap && reading.at.After(seen.at)
		measured := seenPrevious // a client with no busy classes is idle
		for class, counter := range reading.record.engines {
			classKey := drmClassKey{key, class}
			baselines[classKey] = drmBaseline{busy: counter.busy, total: counter.total, at: reading.at, sample: m.sample}
			base, ok := m.baselines[classKey]
			switch {
			case ok && base.sample == previous && reading.at.Sub(base.at) <= maxGap && reading.at.After(base.at):
			case !ok && seenPrevious && !counter.cycles:
				// amdgpu omits a class until its first busy nanosecond, so a
				// class that appears on a known client started from zero.
				base = drmBaseline{at: seen.at}
			default:
				continue
			}
			var value float64
			if counter.cycles {
				if counter.total <= base.total || counter.busy < base.busy {
					continue
				}
				value = float64(counter.busy-base.busy) / float64(counter.total-base.total)
			} else {
				wall := reading.at.Sub(base.at)
				if counter.busy < base.busy || wall <= 0 {
					continue
				}
				value = float64(counter.busy-base.busy) / float64(wall.Nanoseconds())
				if counter.capacity > 1 {
					value /= float64(counter.capacity)
				}
			}
			value = share(value)
			measured = true
			device.classes[class] += value
			process.classes[class] += value
		}
		if measured {
			device.measured = true
			process.measured = true
		}
	}
	m.baselines, m.clients = baselines, clients

	summary.Devices = map[string]drmDeviceUsage{}
	for pdev, device := range devices {
		switch {
		case device.measured:
			load, _ := busiestDRMClass(device.classes)
			engines := make([]GPUEngine, 0, len(device.classes))
			for class, value := range device.classes {
				engines = append(engines, GPUEngine{Name: drmEngineDisplayName(class), Usage: availableNumber(roundTenth(clampEnginePercent(value*100)), "%")})
			}
			sortGPUEngines(engines)
			summary.Devices[pdev] = drmDeviceUsage{Load: availableNumber(roundTenth(clampEnginePercent(load*100)), "%"), Engines: engines}
		case device.clients == 0 && !someUnknown:
			// Every process was read and none has the device open.
			summary.Devices[pdev] = drmDeviceUsage{Load: availableNumber(0, "%")}
		case device.clients == 0:
			summary.Devices[pdev] = drmDeviceUsage{Load: unavailableNumber("%", "no GPU clients readable by the agent")}
		default:
			summary.Devices[pdev] = drmDeviceUsage{Load: unavailableNumber("%", "GPU usage sampler is warming up")}
		}
	}
	summary.Processes = map[int]processGPUUsage{}
	for pid, process := range pids {
		if !process.measured {
			continue
		}
		load, class := busiestDRMClass(process.classes)
		usage := processGPUUsage{Percent: roundTenth(clampEnginePercent(load * 100))}
		if usage.Percent > 0 {
			usage.Engine = drmEngineDisplayName(class)
		}
		summary.Processes[pid] = usage
	}
	return summary
}

// busiestDRMClass is the class with the largest summed share; ties go to
// the alphabetically first class, so the result is stable.
func busiestDRMClass(classes map[string]float64) (float64, string) {
	best, bestClass := -1.0, ""
	for class, value := range classes {
		if value > best || (value == best && class < bestClass) {
			best, bestClass = value, class
		}
	}
	if best < 0 {
		return 0, ""
	}
	return best, bestClass
}

func roundTenth(value float64) float64 {
	return math.Round(value*10) / 10
}

// drmEngineDisplayName labels a DRM engine class the way the Windows
// counters label engines: amdgpu gfx and Intel render/rcs are "3D", the copy
// engines (dma, copy, bcs) "Copy", and so on. "enc_1" is "Video Encode 1".
func drmEngineDisplayName(class string) string {
	base, instance := class, ""
	if i := strings.LastIndexByte(class, '_'); i > 0 {
		if _, err := strconv.Atoi(class[i+1:]); err == nil {
			base, instance = class[:i], class[i+1:]
		}
	}
	names := map[string]string{
		"gfx": "3D", "render": "3D", "rcs": "3D",
		"dma": "Copy", "sdma": "Copy", "copy": "Copy", "bcs": "Copy",
		"video": "Video", "vcs": "Video",
		"video-enhance": "Video Enhance", "vecs": "Video Enhance",
		"compute": "Compute", "ccs": "Compute",
		"dec": "Video Decode", "enc": "Video Encode",
		"jpeg": "JPEG", "vpe": "VPE",
	}
	name, ok := names[strings.ToLower(base)]
	if !ok {
		return gpuEngineDisplayName(strings.ReplaceAll(class, "-", " "))
	}
	if instance != "" {
		name += " " + instance
	}
	return name
}

// drmUsageFuture is a DRM fdinfo sample taken alongside the rest of a pass:
// the GPU collector and the process collector both wait for it.
type drmUsageFuture struct {
	done    chan struct{}
	summary drmUsageSummary
	// nvidia is set on a host with an NVIDIA GPU (see applyDRMProcessGPU).
	nvidia bool
}

func (f *drmUsageFuture) wait() drmUsageSummary {
	if f == nil {
		return unavailableDRMUsageSummary("DRM fdinfo usage not sampled")
	}
	<-f.done
	return f.summary
}

// linuxDRMCardScan is what a cheap look at /sys/class/drm finds.
type linuxDRMCardScan struct {
	pdevs  map[string]bool // AMD/Intel cards that are awake
	nvidia bool
}

// scanLinuxDRMCards lists the AMD/Intel cards to meter, by PCI address, and
// whether an NVIDIA GPU is present. A runtime-suspended card has no clients
// doing work and is left out.
func scanLinuxDRMCards(sysRoot, procRoot string) linuxDRMCardScan {
	scan := linuxDRMCardScan{pdevs: map[string]bool{}}
	matches, _ := filepath.Glob(filepath.Join(sysRoot, "class", "drm", "card*", "device"))
	for _, devicePath := range matches {
		if !isDRMCardName(filepath.Base(filepath.Dir(devicePath))) {
			continue
		}
		switch strings.ToLower(readFirstLine(filepath.Join(devicePath, "vendor"))) {
		case "0x10de":
			scan.nvidia = true
		case "0x1002", "0x8086":
			if linuxDRMRuntimeSuspended(devicePath) {
				continue
			}
			if pdev := linuxDRMPCISlot(devicePath); pdev != "" {
				scan.pdevs[pdev] = true
			}
		}
	}
	if _, err := os.Stat(filepath.Join(procRoot, "driver", "nvidia", "gpus")); err == nil {
		scan.nvidia = true
	}
	return scan
}

// startDRMUsage starts this pass's DRM fdinfo sample in the background.
func (c *systemCollector) startDRMUsage() *drmUsageFuture {
	future := &drmUsageFuture{done: make(chan struct{})}
	scan := scanLinuxDRMCards("/sys", "/proc")
	future.nvidia = scan.nvidia
	if len(scan.pdevs) == 0 {
		future.summary = unavailableDRMUsageSummary("no awake AMD/Intel GPU to meter")
		close(future.done)
		return future
	}
	c.drmUsageOnce.Do(func() { c.drmUsage = newDRMFdinfoSampler("/proc") })
	go func() {
		defer close(future.done)
		defer func() {
			if recovered := recover(); recovered != nil {
				future.summary = unavailableDRMUsageSummary(fmt.Sprintf("DRM fdinfo sampler panicked: %v", recovered))
			}
		}()
		future.summary = c.drmUsage.Sample(scan.pdevs)
	}()
	return future
}

// applyDRMProcessGPU fills each process row's GPU utilization from the DRM
// fdinfo sample. On a host with an NVIDIA GPU the rows keep a note instead.
func applyDRMProcessGPU(raw []ProcessMetric, future *drmUsageFuture) {
	if future == nil {
		return
	}
	if future.nvidia {
		for i := range raw {
			raw[i].GPU = unavailableNumber("%", linuxNVIDIAProcessGPUMessage)
			raw[i].GPUEngine = ""
		}
		return
	}
	summary := future.wait()
	if !summary.Available {
		return
	}
	for i := range raw {
		raw[i].GPU, raw[i].GPUEngine = summary.processUsage(raw[i].PID)
	}
}
