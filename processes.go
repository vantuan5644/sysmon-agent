package main

import (
	"sort"
	"strings"
)

// defaultProcessTopN is the number of rows each view (Apps / Processes) ships to
// the dashboard by default. The client re-sorts within the sent rows, so the
// server sends a union of the per-column leaders (see selectTop*) so re-sorting
// by any column surfaces that column's true leaders.
const defaultProcessTopN = 15

// processTopHardCap bounds the top-N union so the payload stays small even when
// the per-column leaders disagree (the top CPU consumers may differ from the
// top RAM consumers). 2*defaultProcessTopN is a comfortable ceiling for four
// overlapping top-N selections.
const processTopHardCap = 30

// unavailableProcessSet is the degraded ProcessSet used by the platform
// collectors' slow-lane panic fallbacks and by platforms that cannot enumerate
// processes. It is a whole-set unavailable with an explanatory Error, mirroring
// the other *Set metrics' graceful degradation.
func unavailableProcessSet(message string) ProcessSet {
	return ProcessSet{Available: false, Error: message}
}

// buildProcessSet aggregates raw per-process rows into apps and selects the
// top-N union for each view. total is the full host process count (for the page
// header); an empty raw slice still reports Available with Total set so the
// dashboard can show "0 processes" rather than an error.
func buildProcessSet(raw []ProcessMetric, total int) ProcessSet {
	if len(raw) == 0 {
		return ProcessSet{Available: true, Total: total}
	}
	apps := aggregateByApp(raw)
	return ProcessSet{
		Available: true,
		Total:     total,
		Apps:      selectTopApps(apps, defaultProcessTopN),
		Processes: selectTopProcesses(raw, defaultProcessTopN),
	}
}

// aggregateByApp groups raw processes by their executable (lowercased base
// name with a .exe suffix stripped), sums the numeric fields (a summed field is
// available if at least one contributing PID reported it), and records the PID
// count. CPU% values are whole-host-normalized per process, so summing them
// yields the app's whole-host %, directly comparable to the aggregated CPU
// gauge. Order is preserved by first appearance for stable output.
func aggregateByApp(raw []ProcessMetric) []AppMetric {
	groups := map[string][]ProcessMetric{}
	order := make([]string, 0, len(raw))
	for _, p := range raw {
		key := appKey(p.Name)
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], p)
	}
	apps := make([]AppMetric, 0, len(order))
	for _, key := range order {
		procs := groups[key]
		apps = append(apps, AppMetric{
			Name:      appDisplayName(procs, key),
			Count:     len(procs),
			CPU:       clampPercent(sumProcessField(procs, func(p ProcessMetric) NumberMetric { return p.CPU }, "%")),
			Memory:    sumProcessField(procs, func(p ProcessMetric) NumberMetric { return p.Memory }, "B"),
			GPUMemory: sumProcessField(procs, func(p ProcessMetric) NumberMetric { return p.GPUMemory }, "B"),
			DiskRead:  sumProcessField(procs, func(p ProcessMetric) NumberMetric { return p.DiskRead }, "B/s"),
			DiskWrite: sumProcessField(procs, func(p ProcessMetric) NumberMetric { return p.DiskWrite }, "B/s"),
		})
	}
	return apps
}

// sumProcessField sums one NumberMetric field across a group of processes. A
// field is available when at least one contributing PID reported it; an
// all-unavailable field degrades to unavailable so the row never silently
// shows a misleading 0.
func sumProcessField(procs []ProcessMetric, get func(ProcessMetric) NumberMetric, unit string) NumberMetric {
	var total float64
	any := false
	for _, p := range procs {
		m := get(p)
		if m.Available && isFinite(m.Value) {
			total += m.Value
			any = true
		}
	}
	if !any {
		return unavailableNumber(unit, "no process reported this field")
	}
	return availableNumber(total, unit)
}

// clampPercent caps an available percent metric at 100. An app's CPU is the sum
// of its processes' whole-host percentages; sequential per-PID sampling (Linux
// walks hundreds of /proc/[pid]/stat files while wall-elapsed stays fixed) can
// push that sum a few percent past 100 under sustained full load. An app cannot
// use more than the whole host, so clamping keeps the value honest AND keeps the
// strict >100 readiness shape check (validateProcessNumberMetric) from flipping
// /readyz to 503 over a sampling artifact -- per the never-fail-the-response
// invariant. Per-process CPU is already capped in processCPUPercent.
func clampPercent(m NumberMetric) NumberMetric {
	if m.Available && isFinite(m.Value) && m.Value > 100 {
		m.Value = 100
	}
	return m
}

// appKey normalizes a process name into a grouping key: the lowercased base
// name with a .exe suffix stripped. Windows "Chrome.exe" and "chrome" thus
// share a group; the bare base avoids path/noise differences.
func appKey(name string) string {
	key := strings.ToLower(stripExe(strings.TrimSpace(name)))
	if key == "" {
		return "unknown"
	}
	return key
}

// appDisplayName picks the display name for an app group: the first-seen
// process name with its .exe suffix stripped (preserving the original casing),
// falling back to the normalized key when none of the names carry a value.
func appDisplayName(procs []ProcessMetric, key string) string {
	for _, p := range procs {
		if display := stripExe(strings.TrimSpace(p.Name)); display != "" {
			return display
		}
	}
	return key
}

// stripExe removes a trailing .exe (case-insensitive) from a process name. It
// leaves other names untouched.
func stripExe(name string) string {
	if len(name) >= len(".exe") && strings.EqualFold(name[len(name)-len(".exe"):], ".exe") {
		return name[:len(name)-len(".exe")]
	}
	return name
}

// selectTopProcesses returns up to n process rows selected as the union of the
// top-N by each available numeric column (CPU, Memory, GPUMemory, and combined
// Disk I/O), capped at processTopHardCap and sorted by CPU descending. Taking
// the union means a client re-sort by any column still sees that column's true
// leaders rather than only the CPU leaders.
func selectTopProcesses(raw []ProcessMetric, n int) []ProcessMetric {
	if len(raw) == 0 {
		return nil
	}
	if len(raw) <= n {
		return sortedProcessesByCPU(raw)
	}
	picked := map[int]ProcessMetric{}
	consider := func(less func(a, b ProcessMetric) bool) {
		ordered := append([]ProcessMetric(nil), raw...)
		sort.SliceStable(ordered, func(i, j int) bool { return less(ordered[i], ordered[j]) })
		added := 0
		for _, p := range ordered {
			if added >= n {
				break
			}
			if _, dup := picked[p.PID]; dup {
				continue
			}
			picked[p.PID] = p
			added++
		}
	}
	consider(func(a, b ProcessMetric) bool { return metricValueOrZero(a.CPU) > metricValueOrZero(b.CPU) })
	consider(func(a, b ProcessMetric) bool { return metricValueOrZero(a.Memory) > metricValueOrZero(b.Memory) })
	consider(func(a, b ProcessMetric) bool { return metricValueOrZero(a.GPUMemory) > metricValueOrZero(b.GPUMemory) })
	consider(func(a, b ProcessMetric) bool { return processDiskTotal(a) > processDiskTotal(b) })
	out := make([]ProcessMetric, 0, len(picked))
	for _, p := range picked {
		out = append(out, p)
	}
	out = trimProcessesByCPU(out, processTopHardCap)
	return sortedProcessesByCPU(out)
}

// selectTopApps mirrors selectTopProcesses for the aggregated AppMetric view.
func selectTopApps(apps []AppMetric, n int) []AppMetric {
	if len(apps) == 0 {
		return nil
	}
	if len(apps) <= n {
		return sortedAppsByCPU(apps)
	}
	picked := map[string]AppMetric{}
	consider := func(less func(a, b AppMetric) bool) {
		ordered := append([]AppMetric(nil), apps...)
		sort.SliceStable(ordered, func(i, j int) bool { return less(ordered[i], ordered[j]) })
		added := 0
		for _, a := range ordered {
			if added >= n {
				break
			}
			if _, dup := picked[a.Name]; dup {
				continue
			}
			picked[a.Name] = a
			added++
		}
	}
	consider(func(a, b AppMetric) bool { return metricValueOrZero(a.CPU) > metricValueOrZero(b.CPU) })
	consider(func(a, b AppMetric) bool { return metricValueOrZero(a.Memory) > metricValueOrZero(b.Memory) })
	consider(func(a, b AppMetric) bool { return metricValueOrZero(a.GPUMemory) > metricValueOrZero(b.GPUMemory) })
	consider(func(a, b AppMetric) bool { return appDiskTotal(a) > appDiskTotal(b) })
	out := make([]AppMetric, 0, len(picked))
	for _, a := range picked {
		out = append(out, a)
	}
	out = trimAppsByCPU(out, processTopHardCap)
	return sortedAppsByCPU(out)
}

func processDiskTotal(p ProcessMetric) float64 {
	return metricValueOrZero(p.DiskRead) + metricValueOrZero(p.DiskWrite)
}

func appDiskTotal(a AppMetric) float64 {
	return metricValueOrZero(a.DiskRead) + metricValueOrZero(a.DiskWrite)
}

func metricValueOrZero(m NumberMetric) float64 {
	if !m.Available || !isFinite(m.Value) {
		return 0
	}
	return m.Value
}

func sortedProcessesByCPU(in []ProcessMetric) []ProcessMetric {
	out := append([]ProcessMetric(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		return metricValueOrZero(out[i].CPU) > metricValueOrZero(out[j].CPU)
	})
	return out
}

func sortedAppsByCPU(in []AppMetric) []AppMetric {
	out := append([]AppMetric(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		return metricValueOrZero(out[i].CPU) > metricValueOrZero(out[j].CPU)
	})
	return out
}

// trimProcessesByCPU returns at most cap rows, dropping the lowest-CPU rows
// first so the kept rows are the CPU leaders (matching the default sort).
func trimProcessesByCPU(in []ProcessMetric, cap int) []ProcessMetric {
	if len(in) <= cap {
		return in
	}
	out := append([]ProcessMetric(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		return metricValueOrZero(out[i].CPU) > metricValueOrZero(out[j].CPU)
	})
	return out[:cap]
}

// trimAppsByCPU mirrors trimProcessesByCPU for the AppMetric view.
func trimAppsByCPU(in []AppMetric, cap int) []AppMetric {
	if len(in) <= cap {
		return in
	}
	out := append([]AppMetric(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		return metricValueOrZero(out[i].CPU) > metricValueOrZero(out[j].CPU)
	})
	return out[:cap]
}

// processCPUPercent turns two cumulative CPU-second samples into a whole-host
// utilization percentage for one process, normalized by the core count so the
// value is comparable to the aggregated CPU gauge (one fully-busy core on an
// N-core host reads 100/N % here). A counter reset (a new process reusing a
// PID) or a negative delta degrades to unavailable rather than a negative
// figure. Shared by both platform collectors because the delta math is
// identical.
func processCPUPercent(current, previous float64, elapsedSeconds float64, numCPU int) NumberMetric {
	if elapsedSeconds <= 0 || numCPU <= 0 {
		return unavailableNumber("%", "invalid process CPU sample interval")
	}
	delta := current - previous
	if delta < 0 {
		return unavailableNumber("%", "process CPU counter reset")
	}
	pct := delta / elapsedSeconds / float64(numCPU) * 100
	if pct > 100 {
		pct = 100
	}
	return availableNumber(pct, "%")
}
