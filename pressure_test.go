package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParsePSIReadsAvg10OfBothLines(t *testing.T) {
	some, full, err := parsePSI("some avg10=12.50 avg60=3.10 avg300=0.80 total=123456\nfull avg10=4.25 avg60=1.00 avg300=0.20 total=7890\n")
	if err != nil {
		t.Fatal(err)
	}
	if !some.Available || some.Value != 12.5 || some.Unit != "%" {
		t.Fatalf("some = %+v, want 12.5%%", some)
	}
	if !full.Available || full.Value != 4.25 {
		t.Fatalf("full = %+v, want 4.25%%", full)
	}
}

// TestParsePSIDegradesAMissingFullLine: system-wide CPU pressure had no "full"
// line before kernel 5.13; that degrades one field, not the file.
func TestParsePSIDegradesAMissingFullLine(t *testing.T) {
	some, full, err := parsePSI("some avg10=0.30 avg60=0.10 avg300=0.00 total=42\n")
	if err != nil {
		t.Fatal(err)
	}
	if !some.Available || some.Value != 0.3 || full.Available {
		t.Fatalf("some=%+v full=%+v", some, full)
	}
	if _, _, err := parsePSI("not a pressure file"); err == nil {
		t.Fatal("garbage parsed without an error")
	}
}

func TestCollectPressureFromReadsEachResource(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"cpu":    "some avg10=1.00 avg60=0 avg300=0 total=1\nfull avg10=0.00 avg60=0 avg300=0 total=0\n",
		"memory": "some avg10=7.50 avg60=0 avg300=0 total=1\nfull avg10=2.00 avg60=0 avg300=0 total=1\n",
		"io":     "some avg10=30.00 avg60=0 avg300=0 total=1\nfull avg10=12.25 avg60=0 avg300=0 total=1\n",
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	set := collectPressureFrom(root)
	if !set.Available || set.CPUSome.Value != 1 || set.MemorySome.Value != 7.5 || set.MemoryFull.Value != 2 || set.IOSome.Value != 30 || set.IOFull.Value != 12.25 {
		t.Fatalf("pressure = %+v", set)
	}
}

// TestCollectPressureFromExplainsAKernelWithoutPSI: a kernel built without
// CONFIG_PSI has no /proc/pressure at all.
func TestCollectPressureFromExplainsAKernelWithoutPSI(t *testing.T) {
	set := collectPressureFrom(filepath.Join(t.TempDir(), "missing"))
	if set.Available || !strings.Contains(set.Error, "CONFIG_PSI") {
		t.Fatalf("missing PSI = %+v", set)
	}
	if set.MemorySome.Available || set.MemorySome.Error == "" {
		t.Fatalf("missing PSI left a field without a reason: %+v", set.MemorySome)
	}
}

// TestPressureAndCommitStayOutOfCollectionErrors: each is unavailable on one
// platform for good, so neither may pin an issue.
func TestPressureAndCommitStayOutOfCollectionErrors(t *testing.T) {
	metrics := baseMetrics("host")
	metrics.CPU = availableNumber(1, "%")
	metrics.Memory = availableCapacity(1, 2)
	metrics.MemoryCommit = unavailableCapacity(linuxNoCommitMessage)
	metrics.Pressure = unavailablePressure(windowsNoPressureMessage)
	for _, message := range summarizeCollectionErrors(metrics) {
		if strings.Contains(message, "commit") || strings.Contains(message, "pressure") {
			t.Fatalf("collection_errors carried %q", message)
		}
	}
}
