package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Memory pressure.
//
// A RAM percentage says how much memory is in use. Whether anything is short
// of it is a separate question, and the one a person glancing at a dashboard
// is usually asking: a host can sit at 85% for days with nothing waiting, and
// a Windows host can fail allocations with physical RAM free, because what
// runs out there is the commit limit.
//
// Linux answers that directly: pressure stall information (PSI, /proc/pressure)
// reports the share of wall time in which tasks were stalled on CPU, memory or
// I/O. "some" means at least one runnable task was waiting; "full" means every
// non-idle task was, so no useful work happened at all. avg10 is the kernel's
// 10-second running average, refreshed every 2 s, which is why this rides the
// slow lane.
//
// Windows has no pressure accounting. Its nearest honest figure is commit
// charge against the commit limit (RAM plus page files), from the same
// GlobalMemoryStatusEx call the fast lane already makes for RAM. Linux reports
// commit as unavailable: its default heuristic overcommit does not enforce
// CommitLimit, so Committed_AS against it is not a measure of anything that
// can run out.

// PressureSet is the PSI avg10 percentages. Each field degrades on its own:
// kernels before 5.13 report no system-wide CPU "full" line, for example.
type PressureSet struct {
	Available  bool         `json:"available"`
	CPUSome    NumberMetric `json:"cpu_some"`
	MemorySome NumberMetric `json:"memory_some"`
	MemoryFull NumberMetric `json:"memory_full"`
	IOSome     NumberMetric `json:"io_some"`
	IOFull     NumberMetric `json:"io_full"`
	Error      string       `json:"error,omitempty"`
}

const (
	windowsNoPressureMessage = "Windows keeps no pressure stall accounting; see memory_commit"
	linuxNoCommitMessage     = "Linux overcommits by default, so commit charge has no enforced limit; see pressure"
	pressureRoot             = "/proc/pressure"
)

func unavailablePressure(message string) PressureSet {
	return PressureSet{
		Available:  false,
		CPUSome:    unavailableNumber("%", message),
		MemorySome: unavailableNumber("%", message),
		MemoryFull: unavailableNumber("%", message),
		IOSome:     unavailableNumber("%", message),
		IOFull:     unavailableNumber("%", message),
		Error:      message,
	}
}

// collectPressureFrom reads <root>/{cpu,memory,io}. The set is available when
// any resource could be read.
func collectPressureFrom(root string) PressureSet {
	set := PressureSet{}
	var firstErr error
	read := func(resource string) (some, full NumberMetric) {
		data, err := os.ReadFile(filepath.Join(root, resource))
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			message := pressureReadError(err)
			return unavailableNumber("%", message), unavailableNumber("%", message)
		}
		some, full, err = parsePSI(string(data))
		if err != nil && firstErr == nil {
			firstErr = err
		}
		return some, full
	}
	set.CPUSome, _ = read("cpu")
	set.MemorySome, set.MemoryFull = read("memory")
	set.IOSome, set.IOFull = read("io")
	set.Available = set.CPUSome.Available || set.MemorySome.Available || set.IOSome.Available
	if !set.Available && firstErr != nil {
		set.Error = pressureReadError(firstErr)
	}
	return set
}

// pressureReadError explains the usual reasons PSI is missing: CONFIG_PSI off,
// or psi=0 on the kernel command line, which leaves the files present but
// makes reads fail with EOPNOTSUPP.
func pressureReadError(err error) string {
	if errors.Is(err, os.ErrNotExist) {
		return "kernel exposes no /proc/pressure (CONFIG_PSI off)"
	}
	if strings.Contains(strings.ToLower(err.Error()), "not supported") {
		return "pressure stall information disabled (psi=0 on the kernel command line)"
	}
	return "read pressure: " + err.Error()
}

// parsePSI reads the avg10 of the "some" and "full" lines of one
// /proc/pressure file:
//
//	some avg10=0.12 avg60=0.08 avg300=0.02 total=123456
//	full avg10=0.00 avg60=0.00 avg300=0.00 total=789
func parsePSI(data string) (some, full NumberMetric, err error) {
	some = unavailableNumber("%", "no \"some\" line")
	full = unavailableNumber("%", "no \"full\" line")
	found := false
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || (fields[0] != "some" && fields[0] != "full") {
			continue
		}
		value, ok := psiField(fields[1:], "avg10")
		if !ok {
			continue
		}
		found = true
		if fields[0] == "some" {
			some = availableNumber(value, "%")
		} else {
			full = availableNumber(value, "%")
		}
	}
	if !found {
		return some, full, fmt.Errorf("unrecognized pressure format")
	}
	return some, full, nil
}

func psiField(fields []string, key string) (float64, bool) {
	for _, field := range fields {
		name, raw, ok := strings.Cut(field, "=")
		if !ok || name != key {
			continue
		}
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || value < 0 || !isFinite(value) {
			return 0, false
		}
		if value > 100 {
			value = 100
		}
		return value, true
	}
	return 0, false
}
