package main

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestBoundedReadsGiveUpOnAStalledRead: a read that outlives its deadline
// leaves the caller with the last finished value, and the next caller joins
// the read still outstanding instead of starting a second one. Measured case:
// a voice recorder took 13-23 s to answer a capacity query while another tool
// was sending it commands.
func TestBoundedReadsGiveUpOnAStalledRead(t *testing.T) {
	var reads boundedReads[uint64]
	var calls atomic.Int32
	answer := func(value uint64) func() (uint64, error) {
		return func() (uint64, error) { calls.Add(1); return value, nil }
	}

	first := reads.start("disk", answer(64))
	if value, ok, err := reads.result("disk", first, time.Now().Add(time.Second)); !ok || err != nil || value != 64 {
		t.Fatalf("prompt read = %d, %t, %v; want 64", value, ok, err)
	}

	release := make(chan struct{})
	stalled := reads.start("disk", func() (uint64, error) {
		calls.Add(1)
		<-release
		return 128, nil
	})
	started := time.Now()
	value, ok, err := reads.result("disk", stalled, time.Now().Add(50*time.Millisecond))
	if waited := time.Since(started); waited > time.Second {
		t.Fatalf("waited %s on a stalled read", waited)
	}
	if !ok || err != nil || value != 64 {
		t.Fatalf("stalled read = %d, %t, %v; want the last finished value 64", value, ok, err)
	}
	if joined := reads.start("disk", answer(999)); joined != stalled {
		t.Fatal("a second caller started another read while one was outstanding")
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("%d reads issued, want 2", n)
	}

	close(release)
	if value, ok, err := reads.result("disk", stalled, time.Now().Add(time.Second)); !ok || err != nil || value != 128 {
		t.Fatalf("finished read = %d, %t, %v; want 128", value, ok, err)
	}
}

// TestBoundedReadsReportNothingBeforeTheFirstAnswer: with no read ever
// finished there is no value to fall back to, which the caller must be able
// to tell apart from a zero. Errors and panics are results like any other.
func TestBoundedReadsReportNothingBeforeTheFirstAnswer(t *testing.T) {
	var reads boundedReads[uint64]
	release := make(chan struct{})
	defer close(release)
	pending := reads.start("new", func() (uint64, error) { <-release; return 1, nil })
	if _, ok, _ := reads.result("new", pending, time.Now()); ok {
		t.Fatal("a read that never finished reported a value")
	}

	failure := errors.New("no media")
	failed := reads.start("empty", func() (uint64, error) { return 0, failure })
	if _, ok, err := reads.result("empty", failed, time.Now().Add(time.Second)); !ok || !errors.Is(err, failure) {
		t.Fatalf("failed read = %t, %v; want the error", ok, err)
	}

	panicked := reads.start("bad", func() (uint64, error) { panic("boom") })
	if _, ok, err := reads.result("bad", panicked, time.Now().Add(time.Second)); !ok || err == nil {
		t.Fatalf("panicking read = %t, %v; want it recovered into an error", ok, err)
	}
}
