package main

import "sync"

// cpuClockPeakTracker maintains the observed CPU boost ceiling the dashboard's
// clock ring scales to (Metrics.CPUClockMax). It is deliberately free of build
// tags: every platform collector owns one, so `cpu_clock_max` means exactly the
// same thing everywhere.
//
// Why a peak-hold rather than a firmware value: neither OS exposes a usable
// rated boost clock.
//
//   - Windows: Win32_Processor.MaxClockSpeed is the rated BASE clock on every
//     modern CPU (4500 on a 7950X), not the turbo ceiling. There is no second
//     WMI/CIM field carrying the boost number.
//   - Linux: cpufreq's cpuinfo_max_freq looks authoritative but on amd-pstate is
//     computed as nominal_freq x highest_perf/nominal_perf, which extrapolates
//     the CPPC perf table past its linear range. On a 7950X that yields 5883 MHz
//     against a rated 5.7 GHz boost -- a clock the silicon never reaches, so a
//     ring scaled to it can never fill. It is still reported, as
//     Metrics.CPUClockRated, but it does not drive the ring.
//
// Peak-holding what the hardware was actually observed doing sidesteps both and
// converges on the real boost clock (~5.75 GHz on that part) under either OS.
//
// The tracker is fed the per-core peak, not the cross-core average, because the
// average on a many-core part is dragged down by idle cores and can never reach
// the boost clock: a 16-core chip with one core at 5.7 GHz and fifteen at 3.6
// averages 3.7. That aggregation choice -- not a hardware difference -- is why
// the Windows dashboard used to top out around 5.5 GHz.
type cpuClockPeakTracker struct {
	mu      sync.Mutex
	peakMHz float64
}

// cpuClockPeakSeedFactor scales the rated base clock into the initial ceiling so
// the ring has a sane upper bound on the very first sample, before any boost has
// been seen. Every modern desktop part boosts at least this far above base, so
// the seed is immediately overtaken by real readings; it exists only to keep the
// ring from rendering against a zero (or base-equal) span for a few seconds.
const cpuClockPeakSeedFactor = 1.08

// Plausibility bounds for a live clock reading. A sensor glitch or a unit mixup
// (kHz read as MHz, MHz read as GHz) must never ratchet the ceiling somewhere it
// can never come back down from, since the peak-hold only rises.
const (
	cpuClockPeakMinMHz = 200
	cpuClockPeakMaxMHz = 8000
)

// observe folds one sample into the ceiling and returns the current value.
//
// peakCore is preferred over current: current is a cross-core average and would
// hold the ceiling well below the real boost clock. current is the fallback for
// hosts that report only an aggregate clock. baseMHz seeds the ceiling on the
// first call when the rated base clock is known.
//
// The ceiling only ever rises. A boost clock observed once is a real capability
// of the part, and letting it decay would make the ring's scale drift with load
// -- the same reading would render at a different fill depending on what ran a
// minute ago.
func (t *cpuClockPeakTracker) observe(peakCore, current NumberMetric, baseMHz float64, baseOK bool) NumberMetric {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.peakMHz == 0 && baseOK && baseMHz > 0 {
		t.peakMHz = round(baseMHz*cpuClockPeakSeedFactor, 0)
	}
	for _, sample := range []NumberMetric{peakCore, current} {
		if !plausibleCPUClock(sample) {
			continue
		}
		if sample.Value > t.peakMHz {
			t.peakMHz = sample.Value
		}
		break
	}
	if t.peakMHz > 0 {
		return availableNumber(t.peakMHz, "MHz")
	}
	return unavailableNumber("MHz", "CPU boost ceiling not yet observed")
}

// plausibleCPUClock reports whether a live clock reading is sane enough to
// ratchet the peak-hold with.
func plausibleCPUClock(metric NumberMetric) bool {
	return metric.Available &&
		isFinite(metric.Value) &&
		metric.Value >= cpuClockPeakMinMHz &&
		metric.Value <= cpuClockPeakMaxMHz
}

// cpuClockSet groups the five clock metrics that are always resolved together --
// they come from one collection pass and the peak-hold ceiling is derived from
// the others, so splitting them across separate async slots would let a partial
// failure publish an inconsistent set. Both platform collectors fill one of
// these and apply it wholesale.
type cpuClockSet struct {
	Current  NumberMetric
	PeakCore NumberMetric
	Max      NumberMetric
	Rated    NumberMetric
	Base     NumberMetric
}

func (s cpuClockSet) applyTo(m *Metrics) {
	m.CPUClock = s.Current
	m.CPUClockPeakCore = s.PeakCore
	m.CPUClockMax = s.Max
	m.CPUClockRated = s.Rated
	m.CPUClockBase = s.Base
}

// degradedCPUClockSet marks every clock unavailable with one message, for the
// panic fallbacks and the whole-lane degraded patches.
func degradedCPUClockSet(message string) cpuClockSet {
	return cpuClockSet{
		Current:  unavailableNumber("MHz", message),
		PeakCore: unavailableNumber("MHz", message),
		Max:      unavailableNumber("MHz", message),
		Rated:    unavailableNumber("MHz", message),
		Base:     unavailableNumber("MHz", message),
	}
}
