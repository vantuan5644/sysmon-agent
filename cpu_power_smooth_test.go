package main

import "testing"

func w(value float64) NumberMetric { return availableNumber(value, "W") }

func TestCPUPowerSmootherAveragesOverTheWindow(t *testing.T) {
	var s cpuPowerSmoother
	// First sample has nothing to average with, so it passes through.
	got := s.observe(cpuPowerSample{Package: w(40)})
	if got.Package.Value != 40 {
		t.Fatalf("first sample = %v, want 40 (nothing to average yet)", got.Package.Value)
	}
	got = s.observe(cpuPowerSample{Package: w(80)})
	if got.Package.Value != 60 {
		t.Fatalf("second sample = %v, want 60 (mean of 40, 80)", got.Package.Value)
	}
	got = s.observe(cpuPowerSample{Package: w(60)})
	if got.Package.Value != 60 {
		t.Fatalf("third sample = %v, want 60 (mean of 40, 80, 60)", got.Package.Value)
	}
}

// Once full, the window must forget the oldest reading rather than growing.
func TestCPUPowerSmootherWindowEvictsOldest(t *testing.T) {
	var s cpuPowerSmoother
	for i := 0; i < cpuPowerWindowSize; i++ {
		s.observe(cpuPowerSample{Package: w(100)})
	}
	got := s.observe(cpuPowerSample{Package: w(0)})
	want := round(float64(100*(cpuPowerWindowSize-1))/float64(cpuPowerWindowSize), 2)
	if got.Package.Value != want {
		t.Fatalf("after eviction = %v, want %v", got.Package.Value, want)
	}
	// Enough further zero samples must drive it all the way down, proving the
	// ring wraps instead of permanently weighting the first readings.
	for i := 0; i < cpuPowerWindowSize; i++ {
		got = s.observe(cpuPowerSample{Package: w(0)})
	}
	if got.Package.Value != 0 {
		t.Fatalf("after a full window of zeros = %v, want 0", got.Package.Value)
	}
}

// The measured BBLWIN fault: cpu_core_power 54.34 W reported inside a cpu_power
// of 53.90 W. Cores cannot draw more than their package.
func TestCPUPowerSmootherClampsRailAbovePackage(t *testing.T) {
	var s cpuPowerSmoother
	got := s.observe(cpuPowerSample{
		Package: w(53.90),
		Core:    w(54.34),
		Soc:     w(20.00),
	})
	if got.Core.Value != 53.90 {
		t.Fatalf("core = %v, want clamped to the 53.90 W package", got.Core.Value)
	}
	// A rail already below the package must be left exactly alone.
	if got.Soc.Value != 20.00 {
		t.Fatalf("soc = %v, want the original 20.00 W untouched", got.Soc.Value)
	}
}

// The other measured shape of the same fault is rails that are each below the
// package but SUM above it (41.50 + 21.41 + 9.09 = 72.00 inside 50.40 W). That
// is deliberately NOT clamped: rescuing it means choosing which rail to shrink,
// and every choice rewrites a reading that is individually plausible. The
// per-rail clamp fixes only what is unambiguously impossible; the sum staying
// inconsistent is a known limit recorded in docs/windows-idle-tuning.md.
func TestCPUPowerSmootherLeavesOverSummingRailsAlone(t *testing.T) {
	var s cpuPowerSmoother
	got := s.observe(cpuPowerSample{
		Package: w(50.40),
		Core:    w(41.50),
		Soc:     w(21.41),
		Misc:    w(9.09),
	})
	if got.Core.Value != 41.50 || got.Soc.Value != 21.41 || got.Misc.Value != 9.09 {
		t.Fatalf("rails = %v/%v/%v, want all three untouched",
			got.Core.Value, got.Soc.Value, got.Misc.Value)
	}
}

func TestCPUPowerSmootherLeavesRailsAloneWhenPackageUnavailable(t *testing.T) {
	var s cpuPowerSmoother
	got := s.observe(cpuPowerSample{
		Package: unavailableNumber("W", "bridge failed"),
		Core:    w(30),
	})
	if got.Core.Value != 30 {
		t.Fatalf("core = %v, want 30 (no package to clamp against)", got.Core.Value)
	}
	if got.Package.Available {
		t.Fatalf("package = %+v, want the unavailable reading preserved", got.Package)
	}
}

// An unavailable reading must neither be masked by a stale average nor poison
// the window -- the per-metric degradation invariant.
func TestCPUPowerSmootherPassesThroughUnavailableWithoutPoisoning(t *testing.T) {
	var s cpuPowerSmoother
	s.observe(cpuPowerSample{Package: w(60)})
	s.observe(cpuPowerSample{Package: w(60)})

	got := s.observe(cpuPowerSample{Package: unavailableNumber("W", "bridge timeout")})
	if got.Package.Available || got.Package.Error != "bridge timeout" {
		t.Fatalf("package = %+v, want the unavailable reading passed through verbatim", got.Package)
	}

	// The dropout contributed nothing, so the mean is still over the two 60s.
	got = s.observe(cpuPowerSample{Package: w(60)})
	if got.Package.Value != 60 {
		t.Fatalf("after recovery = %v, want 60 (dropout excluded from the mean)", got.Package.Value)
	}
}

func TestCPUPowerSmootherSmoothsEachRailIndependently(t *testing.T) {
	var s cpuPowerSmoother
	s.observe(cpuPowerSample{Package: w(100), Core: w(20), Soc: w(20), Misc: w(10), PSUOut: w(200)})
	got := s.observe(cpuPowerSample{Package: w(200), Core: w(40), Soc: w(30), Misc: w(20), PSUOut: w(100)})
	for name, want := range map[string]struct {
		got  float64
		want float64
	}{
		"package": {got.Package.Value, 150},
		"core":    {got.Core.Value, 30},
		"soc":     {got.Soc.Value, 25},
		"misc":    {got.Misc.Value, 15},
		"psu":     {got.PSUOut.Value, 150},
	} {
		if want.got != want.want {
			t.Errorf("%s = %v, want %v", name, want.got, want.want)
		}
	}
}
