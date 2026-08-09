package main

import "sync"

// cpuPowerSmoother turns the CPU power rails into a short rolling mean and
// enforces the one relationship the hardware guarantees: no rail draws more
// than the package that contains it. Like cpuClockPeakTracker it is deliberately
// free of build tags -- every platform collector owns one, so cpu_power means
// the same statistic everywhere.
//
// Why it exists. On Windows the LibreHardwareMonitor bridge reads `CPU PPT` and
// the per-rail `Core`/`SOC`/`Misc Power` sensors out of the AMD SMU in one pass,
// but the SMU does not update them on a common cadence, so the readings are not
// coherent samples of one instant. Measured on a 7950X at idle: `Core Power`
// 41.50 + `SOC` 21.41 + `Misc` 9.09 = 72.00 W reported inside a 50.40 W package,
// and in another sample `cpu_core_power` 54.34 W inside a `cpu_power` of
// 53.90 W. Cores cannot draw more than their package; the dashboard rendered it
// anyway.
//
// Why it also runs on Linux, where RAPL is already an integrated energy delta
// over the sample interval and needs no denoising: applying it on one platform
// only would make cpu_power a mean-over-one-interval there and a
// mean-over-N-instants here -- two different statistics under one field name,
// which is the exact defect cpu_clock_peak.go and pickCPUTemperature were fixed
// for. Smoothing both keeps them symmetric.
//
// What this does NOT fix, and must not be claimed to: the 44 <-> 74 W bimodal
// swing measured on BBLWIN persists in 10-20 s plateaus (see
// docs/windows-idle-tuning.md). A window long enough to flatten that would lag
// the dashboard by half a minute. This removes sample-to-sample noise and the
// impossible readings; it does not make the reported package power true. The
// PSU cross-check remains the way to test whether a power change is real.
type cpuPowerSmoother struct {
	mu     sync.Mutex
	pkg    cpuPowerWindow
	core   cpuPowerWindow
	soc    cpuPowerWindow
	misc   cpuPowerWindow
	psuOut cpuPowerWindow
}

// cpuPowerWindowSize is the number of slow-lane samples averaged. At the default
// -slow-ms of 1500 that is a 7.5 s window: long enough to absorb the per-sample
// incoherence between the SMU rails, short enough that the dashboard still
// visibly responds to a workload starting. Raising it trades responsiveness for
// smoothness and does not buy correctness -- see the note above about the
// bimodal swing.
const cpuPowerWindowSize = 5

// cpuPowerSample groups the power fields that are resolved together in one
// collection pass, so the smoother sees a consistent set and the clamp has a
// package to clamp against. Mirrors the cpuClockSet grouping.
type cpuPowerSample struct {
	Package NumberMetric
	Core    NumberMetric
	Soc     NumberMetric
	Misc    NumberMetric
	PSUOut  NumberMetric
}

// observe folds one collection pass into the rolling means and returns the
// smoothed, clamped set.
//
// Unavailable fields pass straight through and contribute nothing to their
// window, so a sensor that drops out degrades exactly as before instead of
// being masked by a stale average -- the per-metric degradation invariant.
func (s *cpuPowerSmoother) observe(in cpuPowerSample) cpuPowerSample {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := cpuPowerSample{
		Package: s.pkg.observe(in.Package),
		Core:    s.core.observe(in.Core),
		Soc:     s.soc.observe(in.Soc),
		Misc:    s.misc.observe(in.Misc),
		PSUOut:  s.psuOut.observe(in.PSUOut),
	}
	clampCPUPowerRails(&out)
	return out
}

// clampCPUPowerRails caps each per-rail reading at the package total. A rail is
// a subset of the package by construction, so a rail above it is always a
// telemetry fault rather than a real measurement.
//
// Capping is chosen over discarding because the rails carry the useful signal
// (the BBLWIN idle investigation turned on SOC and Misc being flat while Core
// moved), and over scaling all rails proportionally because that would silently
// rewrite readings that were individually fine. Only the impossible value is
// touched.
func clampCPUPowerRails(sample *cpuPowerSample) {
	if !sample.Package.Available || !isFinite(sample.Package.Value) {
		return
	}
	limit := sample.Package.Value
	for _, rail := range []*NumberMetric{&sample.Core, &sample.Soc, &sample.Misc} {
		if !rail.Available || !isFinite(rail.Value) {
			continue
		}
		if rail.Value > limit {
			rail.Value = limit
		}
	}
}

// cpuPowerWindow is a fixed-size ring of the most recent available readings for
// one rail.
type cpuPowerWindow struct {
	values [cpuPowerWindowSize]float64
	next   int
	filled int
}

// observe adds one reading and returns the mean of the window. An unavailable
// or non-finite reading is returned untouched and leaves the window alone, so a
// transient bridge failure neither poisons the mean nor resets it.
func (w *cpuPowerWindow) observe(metric NumberMetric) NumberMetric {
	if !metric.Available || !isFinite(metric.Value) {
		return metric
	}
	w.values[w.next] = metric.Value
	w.next = (w.next + 1) % cpuPowerWindowSize
	if w.filled < cpuPowerWindowSize {
		w.filled++
	}
	sum := 0.0
	for i := 0; i < w.filled; i++ {
		sum += w.values[i]
	}
	smoothed := metric
	smoothed.Value = round(sum/float64(w.filled), 2)
	return smoothed
}
