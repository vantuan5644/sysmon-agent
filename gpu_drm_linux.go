//go:build linux

package main

import (
	"bufio"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// AMD and Intel GPU details from DRM sysfs, beyond the busy/VRAM/edge/power
// basics gpu_linux.go started with. Everything here is a plain sysfs read in
// the slow lane; nothing spawns.
//
//   - Name: device/product_name when the board carries one, else the PCI ID
//     looked up in pci.ids (hwdata), else "AMD GPU card0".
//   - Temperatures by hwmon label: "edge" is the card's temperature,
//     "junction" its hotspot and "mem" (amdgpu) or "vram" (xe) its memory.
//     A card without labels keeps the first temp*_input as its temperature.
//   - Power: power1_average, then power1_input (RDNA3 and APUs have only the
//     latter), plus power1_cap as the board's limit.
//   - Clock: the starred pp_dpm_sclk level (amdgpu), else hwmon freq1_input,
//     or Intel's gt_cur_freq_mhz (i915) / tile0/gt0/freq0/cur_freq (xe).
//   - Memory: an APU's VRAM is a BIOS carve-out of a few hundred MiB that
//     fills at once, while the rest of what the GPU uses is GTT (system RAM
//     it maps). An integrated AMD GPU reports both together, with
//     memory_kind "shared".
//   - A runtime-suspended GPU (power/runtime_status "suspended", the dGPU of
//     a hybrid laptop) is idle; its other files are left unread, because on
//     older kernels an amdgpu sysfs read resumes the device.

// linuxPCIIDsPaths are the pci.ids copies tried in order (hwdata first).
var linuxPCIIDsPaths = []string{"/usr/share/hwdata/pci.ids", "/usr/share/misc/pci.ids", "/usr/share/pci.ids"}

const (
	// pciIDsMaxBytes bounds one lookup's read; the file is about 1.5 MB.
	pciIDsMaxBytes = 8 << 20
	// pciIDsCacheMax bounds the lookup cache (one entry per distinct GPU).
	pciIDsCacheMax = 64
)

type pciIDsKey struct {
	path                         string
	vendor, device, subV, subDev string
}

var pciIDsCache = struct {
	sync.Mutex
	names map[pciIDsKey]string
}{names: map[pciIDsKey]string{}}

// lookupPCIName returns the marketing name of a PCI device from pci.ids, or
// "" when no copy knows it. Each distinct ID is scanned for once and cached,
// misses included.
func lookupPCIName(vendor, device, subVendor, subDevice string) string {
	vendor, device = normalizePCIHex(vendor), normalizePCIHex(device)
	subVendor, subDevice = normalizePCIHex(subVendor), normalizePCIHex(subDevice)
	if vendor == "" || device == "" {
		return ""
	}
	for _, path := range linuxPCIIDsPaths {
		key := pciIDsKey{path, vendor, device, subVendor, subDevice}
		pciIDsCache.Lock()
		name, cached := pciIDsCache.names[key]
		pciIDsCache.Unlock()
		if !cached {
			var err error
			name, err = scanPCIIDs(path, vendor, device, subVendor, subDevice)
			if err != nil && errors.Is(err, os.ErrNotExist) {
				continue
			}
			pciIDsCache.Lock()
			if len(pciIDsCache.names) >= pciIDsCacheMax {
				pciIDsCache.names = map[pciIDsKey]string{}
			}
			pciIDsCache.names[key] = name
			pciIDsCache.Unlock()
		}
		if name != "" {
			return name
		}
	}
	return ""
}

// normalizePCIHex turns "0x1002" or "1002" into "1002"; anything else is "".
func normalizePCIHex(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "0x")
	if len(value) != 4 {
		return ""
	}
	if _, err := strconv.ParseUint(value, 16, 16); err != nil {
		return ""
	}
	return value
}

// scanPCIIDs reads one pci.ids file until the vendor's block ends. The device
// line ("\t1681  Rembrandt [Radeon 680M]") gives the name; when it lists a
// family ("Radeon RX 7900 XT/7900 XTX/..."), the subsystem line under it can
// say which card this is.
func scanPCIIDs(path, vendor, device, subVendor, subDevice string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(io.LimitReader(file, pciIDsMaxBytes), 64<<10)
	inVendor, inDevice := false, false
	deviceName := ""
	for {
		line, err := reader.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if line != "" && line[0] != '#' {
			switch {
			case line[0] != '\t':
				if inVendor {
					return pciDisplayName(deviceName, ""), nil
				}
				inVendor = strings.HasPrefix(strings.ToLower(line), vendor+"  ")
			case !inVendor:
			case strings.HasPrefix(line, "\t\t"):
				if inDevice && subVendor != "" && subDevice != "" {
					fields := strings.TrimPrefix(line, "\t\t")
					if strings.HasPrefix(strings.ToLower(fields), subVendor+" "+subDevice+"  ") {
						return pciDisplayName(deviceName, strings.TrimSpace(fields[len("xxxx xxxx  "):])), nil
					}
				}
			default:
				if inDevice {
					return pciDisplayName(deviceName, ""), nil
				}
				body := line[1:]
				if strings.HasPrefix(strings.ToLower(body), device+"  ") {
					inDevice = true
					deviceName = strings.TrimSpace(body[len("xxxx  "):])
				}
			}
		}
		if err != nil {
			return pciDisplayName(deviceName, ""), nil
		}
	}
}

var pciBracketName = regexp.MustCompile(`\[([^\]]+)\]`)

// pciDisplayName prefers the marketing name in brackets ("Rembrandt [Radeon
// 680M]" -> "Radeon 680M"). When that names a family, a subsystem name that
// mentions one Radeon or Arc model narrows it ("TUF Gaming Radeon RX 7900
// XTX OC" -> "Radeon RX 7900 XTX OC"). Board names that are only a laptop or
// a maker's code ("ThinkPad E595") are not used.
func pciDisplayName(deviceName, subsystemName string) string {
	deviceName = strings.TrimSpace(deviceName)
	if deviceName == "" {
		return ""
	}
	name := deviceName
	if matches := pciBracketName.FindAllStringSubmatch(deviceName, -1); len(matches) > 0 {
		name = strings.TrimSpace(matches[len(matches)-1][1])
	}
	if strings.Contains(name, "/") && subsystemName != "" {
		if refined := pciModelFromSubsystem(subsystemName); refined != "" {
			return refined
		}
	}
	return name
}

func pciModelFromSubsystem(subsystemName string) string {
	var candidates []string
	if matches := pciBracketName.FindAllStringSubmatch(subsystemName, -1); len(matches) > 0 {
		for _, match := range matches {
			candidates = append(candidates, match[1])
		}
	}
	candidates = append(candidates, pciBracketName.ReplaceAllString(subsystemName, ""))
	for _, candidate := range candidates {
		candidate = strings.Join(strings.Fields(candidate), " ")
		if strings.Contains(candidate, "/") {
			continue
		}
		for _, marker := range []string{"Radeon ", "Arc "} {
			if i := strings.Index(candidate, marker); i >= 0 {
				return strings.TrimSpace(candidate[i:])
			}
		}
		if i := strings.Index(candidate, "RX "); i >= 0 {
			return "Radeon " + strings.TrimSpace(candidate[i:])
		}
	}
	return ""
}

// linuxDRMGPUNameFor names a card: product_name, then pci.ids, then the
// card fallback. A vendor word is added when the name lacks one.
func linuxDRMGPUNameFor(cardName, vendorID, devicePath string) string {
	name := strings.Join(strings.Fields(readFirstLine(filepath.Join(devicePath, "product_name"))), " ")
	if name == "" {
		name = lookupPCIName(vendorID,
			readFirstLine(filepath.Join(devicePath, "device")),
			readFirstLine(filepath.Join(devicePath, "subsystem_vendor")),
			readFirstLine(filepath.Join(devicePath, "subsystem_device")))
	}
	if name == "" {
		return linuxDRMGPUName(cardName, vendorID)
	}
	lower := strings.ToLower(name)
	switch strings.ToLower(vendorID) {
	case "0x1002":
		if !strings.Contains(lower, "amd") && !strings.Contains(lower, "ati ") {
			name = "AMD " + name
		}
	case "0x8086":
		if !strings.Contains(lower, "intel") {
			name = "Intel " + name
		}
	}
	return name
}

// linuxHwmonSensors is what a card's hwmon directories carry, read once.
type linuxHwmonSensors struct {
	firstTemp    NumberMetric
	edge         NumberMetric
	junction     NumberMetric
	memory       NumberMetric
	junctionCrit NumberMetric
	memoryCrit   NumberMetric
	powerAverage NumberMetric
	powerInput   NumberMetric
	powerCap     NumberMetric
	fanRPM       NumberMetric
	sclkMHz      NumberMetric
	hasFan       bool
	hasMemTemp   bool
	found        bool
}

// readLinuxGPUHwmon reads the labelled sensors of every hwmon directory
// under a card (normally one); the first reading of each kind wins.
func readLinuxGPUHwmon(devicePath string) linuxHwmonSensors {
	var s linuxHwmonSensors
	dirs, _ := filepath.Glob(filepath.Join(devicePath, "hwmon", "hwmon*"))
	sort.Strings(dirs)
	setOnce := func(metric *NumberMetric, value float64, unit string) {
		if !metric.Available {
			*metric = availableNumber(value, unit)
		}
	}
	for _, dir := range dirs {
		s.found = true
		inputs, _ := filepath.Glob(filepath.Join(dir, "temp*_input"))
		sort.Slice(inputs, func(i, j int) bool { return hwmonIndex(inputs[i]) < hwmonIndex(inputs[j]) })
		for _, input := range inputs {
			value, ok := readTemperatureMilliC(input)
			if !ok {
				continue
			}
			setOnce(&s.firstTemp, value, "C")
			prefix := strings.TrimSuffix(input, "_input")
			label := strings.ToLower(readFirstLine(prefix + "_label"))
			crit, critOK := readTemperatureMilliC(prefix + "_crit")
			switch label {
			case "edge":
				setOnce(&s.edge, value, "C")
			case "junction", "hotspot":
				setOnce(&s.junction, value, "C")
				if critOK && crit > 0 {
					setOnce(&s.junctionCrit, crit, "C")
				}
			case "mem", "vram":
				s.hasMemTemp = true
				setOnce(&s.memory, value, "C")
				if critOK && crit > 0 {
					setOnce(&s.memoryCrit, crit, "C")
				}
			}
		}
		if microwatts, ok := readUint64File(filepath.Join(dir, "power1_average")); ok && microwatts > 0 {
			setOnce(&s.powerAverage, float64(microwatts)/1e6, "W")
		}
		if microwatts, ok := readUint64File(filepath.Join(dir, "power1_input")); ok && microwatts > 0 {
			setOnce(&s.powerInput, float64(microwatts)/1e6, "W")
		}
		if microwatts, ok := readUint64File(filepath.Join(dir, "power1_cap")); ok && microwatts > 0 {
			setOnce(&s.powerCap, float64(microwatts)/1e6, "W")
		}
		if rpm, ok := readUint64File(filepath.Join(dir, "fan1_input")); ok {
			s.hasFan = true
			setOnce(&s.fanRPM, float64(rpm), "RPM")
		}
		if strings.EqualFold(readFirstLine(filepath.Join(dir, "freq1_label")), "sclk") {
			if hz, ok := readUint64File(filepath.Join(dir, "freq1_input")); ok && hz > 0 {
				setOnce(&s.sclkMHz, float64(hz)/1e6, "MHz")
			}
		}
	}
	return s
}

// hwmonIndex is N in ".../tempN_input", so temp10 sorts after temp2.
func hwmonIndex(path string) int {
	base := strings.TrimSuffix(filepath.Base(path), "_input")
	digits := strings.TrimLeft(base, "abcdefghijklmnopqrstuvwxyz")
	n, err := strconv.Atoi(digits)
	if err != nil {
		return 1 << 30
	}
	return n
}

// parseDPMCurrentMHz returns the starred level of a pp_dpm_* table
// ("2: 2200Mhz *"). amdgpu can star several levels of an APU's table; the
// first wins.
func parseDPMCurrentMHz(text string) (float64, bool) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasSuffix(line, "*") {
			continue
		}
		_, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		rest = strings.TrimSpace(strings.TrimSuffix(rest, "*"))
		lower := strings.ToLower(rest)
		unit := 1.0
		switch {
		case strings.HasSuffix(lower, "mhz"):
			rest = rest[:len(rest)-3]
		case strings.HasSuffix(lower, "ghz"):
			rest, unit = rest[:len(rest)-3], 1000
		default:
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
		if err != nil || value <= 0 {
			continue
		}
		return value * unit, true
	}
	return 0, false
}

// readSmallFile reads a sysfs file of at most 4 KiB.
func readSmallFile(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	data, _ := io.ReadAll(io.LimitReader(file, 4<<10))
	return string(data)
}

// linuxDRMGPUClock is the card's current graphics clock in MHz.
func linuxDRMGPUClock(cardPath, devicePath, vendorID string, hwmon linuxHwmonSensors) (NumberMetric, bool) {
	if strings.EqualFold(vendorID, "0x8086") {
		for _, path := range []string{
			filepath.Join(cardPath, "gt_cur_freq_mhz"),
			filepath.Join(cardPath, "gt", "gt0", "rps_cur_freq_mhz"),
			filepath.Join(devicePath, "tile0", "gt0", "freq0", "cur_freq"),
		} {
			if mhz, ok := readUint64File(path); ok && mhz > 0 {
				return availableNumber(float64(mhz), "MHz"), true
			}
		}
		return NumberMetric{}, false
	}
	if mhz, ok := parseDPMCurrentMHz(readSmallFile(filepath.Join(devicePath, "pp_dpm_sclk"))); ok {
		return availableNumber(mhz, "MHz"), true
	}
	if hwmon.sclkMHz.Available {
		return hwmon.sclkMHz, true
	}
	return NumberMetric{}, false
}

var intelDiscretePattern = regexp.MustCompile(`(?i)\b(DG1|DG2|Alchemist|Battlemage|Arc (Pro )?[AB]\d)`)

// linuxDRMGPUIntegrated says whether a card is an integrated GPU. AMD dGPUs
// publish their VRAM vendor (mem_info_vram_vendor), a fan or a memory
// temperature, none of which an APU has; Intel GPUs are integrated unless
// pci.ids names a discrete part (DG1, DG2/Alchemist, Battlemage, Arc A/B).
func linuxDRMGPUIntegrated(vendorID, devicePath, name string, hwmon linuxHwmonSensors) bool {
	switch strings.ToLower(vendorID) {
	case "0x1002":
		if _, err := os.Stat(filepath.Join(devicePath, "mem_info_vram_vendor")); err == nil {
			return false
		}
		return !hwmon.hasFan && !hwmon.hasMemTemp
	case "0x8086":
		pciName := lookupPCIName(vendorID, readFirstLine(filepath.Join(devicePath, "device")), "", "")
		return !intelDiscretePattern.MatchString(name) && !intelDiscretePattern.MatchString(pciName)
	}
	return false
}

// linuxDRMGPUMemory is VRAM, or for an integrated GPU with GTT the carve-out
// plus GTT (kind "shared"). An integrated GPU without VRAM files but with GTT
// reports GTT alone, also "shared".
func linuxDRMGPUMemory(devicePath string, integrated bool) (CapacityMetric, string) {
	vram := linuxDRMGPUVRAM(devicePath)
	if !integrated {
		return vram, ""
	}
	gttUsed, usedOK := readUint64File(filepath.Join(devicePath, "mem_info_gtt_used"))
	gttTotal, totalOK := readUint64File(filepath.Join(devicePath, "mem_info_gtt_total"))
	if !usedOK || !totalOK || gttTotal == 0 {
		return vram, ""
	}
	used, total := gttUsed, gttTotal
	if vram.Available {
		used += vram.UsedBytes
		total += vram.TotalBytes
	}
	return availableCapacity(used, total), "shared"
}

// linuxDRMRuntimeSuspended reports a device the kernel has powered down.
func linuxDRMRuntimeSuspended(devicePath string) bool {
	return readFirstLine(filepath.Join(devicePath, "power", "runtime_status")) == "suspended"
}

// linuxDRMPCISlot is the card's PCI address as DRM fdinfo spells it
// (drm-pdev: 0000:e6:00.0), from uevent, else the device link's target.
func linuxDRMPCISlot(devicePath string) string {
	for _, line := range strings.Split(readSmallFile(filepath.Join(devicePath, "uevent")), "\n") {
		if slot, ok := strings.CutPrefix(strings.TrimSpace(line), "PCI_SLOT_NAME="); ok && slot != "" {
			return strings.ToLower(slot)
		}
	}
	if target, err := filepath.EvalSymlinks(devicePath); err == nil {
		base := strings.ToLower(filepath.Base(target))
		if strings.Count(base, ":") == 2 && strings.Contains(base, ".") {
			return base
		}
	}
	return ""
}

func numberPtr(metric NumberMetric) *NumberMetric {
	if !metric.Available {
		return nil
	}
	return &metric
}

// drmCardIndex is N of "cardN", for a stable order.
func drmCardIndex(cardName string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(cardName, "card"))
	if err != nil {
		return 1 << 30
	}
	return n
}

// sortLinuxDRMDevices orders discrete before integrated, then by name and
// card number, and disambiguates two cards that share a name.
func sortLinuxDRMDevices(devices []linuxDRMDevice) {
	sort.SliceStable(devices, func(i, j int) bool {
		a, b := devices[i], devices[j]
		if a.metric.Integrated != b.metric.Integrated {
			return !a.metric.Integrated
		}
		if a.metric.Name != b.metric.Name {
			return a.metric.Name < b.metric.Name
		}
		return drmCardIndex(a.card) < drmCardIndex(b.card)
	})
	count := map[string]int{}
	for _, device := range devices {
		count[device.metric.Name]++
	}
	for i := range devices {
		if count[devices[i].metric.Name] > 1 {
			devices[i].metric.Name += " (" + devices[i].card + ")"
		}
	}
}

// linuxDRMDevice is one AMD/Intel card while the set is being built.
type linuxDRMDevice struct {
	card      string
	pdev      string
	suspended bool
	metric    GPUMetric
}
