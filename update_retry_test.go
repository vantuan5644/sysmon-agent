package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestNextUpdateCheckDelayBacksOffAfterFailures(t *testing.T) {
	day := updateCheckInterval
	for _, tc := range []struct {
		interval time.Duration
		failures int
		want     time.Duration
	}{
		{day, 0, day},
		{day, 1, 15 * time.Minute},
		{day, 2, 30 * time.Minute},
		{day, 3, time.Hour},
		{day, 4, 2 * time.Hour},
		{day, 5, 4 * time.Hour},
		{day, 6, 4 * time.Hour},
		{day, 1000, 4 * time.Hour},
		// A retry never waits longer than the interval itself.
		{time.Hour, 0, time.Hour},
		{time.Hour, 3, time.Hour},
		{time.Hour, 9, time.Hour},
		{10 * time.Minute, 1, 10 * time.Minute},
	} {
		if got := nextUpdateCheckDelay(tc.interval, tc.failures); got != tc.want {
			t.Errorf("nextUpdateCheckDelay(%v, %d) = %v, want %v", tc.interval, tc.failures, got, tc.want)
		}
	}
}

func TestUpdateCheckerLoopKeepsCheckingAfterAFailure(t *testing.T) {
	checker := newUpdateChecker(UpdateCheckerOptions{
		CurrentVersion: "v1.0.0",
		Enabled:        true,
		StartupDelay:   time.Millisecond,
		Interval:       5 * time.Millisecond,
	})
	var calls atomic.Int32
	checker.fetchRelease = func(ctx context.Context, repo string) (ReleaseInfo, error) {
		if calls.Add(1) < 3 {
			return ReleaseInfo{}, errors.New("network down")
		}
		return ReleaseInfo{Tag: "v1.1.0"}, nil
	}
	checker.Start()
	defer checker.Stop()
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 4 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if calls.Load() < 4 {
		t.Fatalf("loop made %d checks, want it to go on past failures and a success", calls.Load())
	}
	if status := checker.Status(); status.LatestVersion != "v1.1.0" || !status.Available {
		t.Fatalf("status after recovery = %+v, want v1.1.0 available", status)
	}
}
