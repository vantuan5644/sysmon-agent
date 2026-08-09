package main

import "testing"

func mhz(value float64) NumberMetric { return availableNumber(value, "MHz") }

func naClock() NumberMetric { return unavailableNumber("MHz", "no reading") }

// The seed exists so the dashboard ring has a sane upper bound on the very first
// sample, before any boost has been observed.
func TestCPUClockPeakSeedsFromBase(t *testing.T) {
	var tracker cpuClockPeakTracker
	got := tracker.observe(naClock(), naClock(), 4500, true)
	if !got.Available {
		t.Fatalf("ceiling unavailable with a known base clock: %+v", got)
	}
	if got.Value != 4860 { // 4500 * 1.08
		t.Fatalf("seeded ceiling = %v, want 4860", got.Value)
	}
}

// With no base and no readings there is nothing to scale a ring against, so the
// metric degrades rather than inventing a number.
func TestCPUClockPeakUnavailableWithNothingToGoOn(t *testing.T) {
	var tracker cpuClockPeakTracker
	if got := tracker.observe(naClock(), naClock(), 0, false); got.Available {
		t.Fatalf("ceiling should be unavailable with no base and no sample, got %+v", got)
	}
}

// The whole point of the change: the ceiling tracks the fastest single core, not
// the cross-core average. Feeding it the average is what capped the Windows
// dashboard around 5.5 GHz on a part rated for 5.7.
func TestCPUClockPeakPrefersPeakCoreOverAverage(t *testing.T) {
	var tracker cpuClockPeakTracker
	got := tracker.observe(mhz(5750), mhz(3700), 4500, true)
	if got.Value != 5750 {
		t.Fatalf("ceiling = %v, want 5750 (the boosting core, not the 3700 average)", got.Value)
	}
}

// Hosts that report only an aggregate clock still get a ceiling.
func TestCPUClockPeakFallsBackToAverage(t *testing.T) {
	var tracker cpuClockPeakTracker
	got := tracker.observe(naClock(), mhz(5200), 4500, true)
	if got.Value != 5200 {
		t.Fatalf("ceiling = %v, want 5200 from the average fallback", got.Value)
	}
}

// A boost clock observed once is a real capability of the part. Letting the
// ceiling decay would make the ring's scale drift with load, so the same reading
// would render at a different fill depending on what ran a minute ago.
func TestCPUClockPeakRatchetsUpOnly(t *testing.T) {
	var tracker cpuClockPeakTracker
	tracker.observe(mhz(5750), naClock(), 4500, true)
	got := tracker.observe(mhz(3200), naClock(), 4500, true)
	if got.Value != 5750 {
		t.Fatalf("ceiling = %v, want 5750 held after a lower sample", got.Value)
	}
}

// A sensor glitch or a unit mixup must never ratchet the ceiling somewhere it can
// never come back down from, since the peak-hold only rises. A kHz value read as
// MHz (5_750_000) or a GHz value read as MHz (5.75) both land outside the bounds.
func TestCPUClockPeakRejectsImplausibleSamples(t *testing.T) {
	for _, sample := range []float64{5.75, 199, 8001, 5750000} {
		var tracker cpuClockPeakTracker
		got := tracker.observe(mhz(sample), naClock(), 4500, true)
		if got.Value != 4860 {
			t.Fatalf("sample %v moved the ceiling to %v; want the 4860 seed held", sample, got.Value)
		}
	}
}

// When the per-core peak is implausible the average is still worth trying --
// but a plausible peak must shut the average out entirely, so a stuck-high
// average can never override a good per-core reading.
func TestCPUClockPeakIgnoresAverageWhenPeakCoreIsUsable(t *testing.T) {
	var tracker cpuClockPeakTracker
	got := tracker.observe(mhz(5000), mhz(7900), 4500, true)
	if got.Value != 5000 {
		t.Fatalf("ceiling = %v, want 5000; a usable peak-core reading must win outright", got.Value)
	}
}

func TestDegradedCPUClockSetMarksEveryFieldUnavailable(t *testing.T) {
	var m Metrics
	degradedCPUClockSet("collector panicked").applyTo(&m)
	for name, metric := range map[string]NumberMetric{
		"cpu_clock":           m.CPUClock,
		"cpu_clock_peak_core": m.CPUClockPeakCore,
		"cpu_clock_max":       m.CPUClockMax,
		"cpu_clock_rated":     m.CPUClockRated,
		"cpu_clock_base":      m.CPUClockBase,
	} {
		if metric.Available || metric.Error != "collector panicked" || metric.Unit != "MHz" {
			t.Fatalf("%s = %+v, want unavailable MHz with the given error", name, metric)
		}
	}
}
