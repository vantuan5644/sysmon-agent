package main

import "time"

// A failed background update check (GitHub unreachable, rate-limited, DNS not
// up yet) is retried sooner than the 24 h interval, so one failure at start-up
// does not leave the dashboard without an answer for a day: 15 min after the
// first failure, then doubling to at most updateCheckRetryMax, and back to the
// interval after a check succeeds. At the cap that is six asks a day, still
// far under GitHub's anonymous limit.
const (
	updateCheckRetryMin = 15 * time.Minute
	updateCheckRetryMax = 4 * time.Hour
)

// nextUpdateCheckDelay is the wait before the next background check, given the
// steady-state interval and how many checks in a row have failed. A retry is
// never later than the interval itself.
func nextUpdateCheckDelay(interval time.Duration, failures int) time.Duration {
	if failures <= 0 {
		return interval
	}
	delay := updateCheckRetryMin
	for i := 1; i < failures && delay < updateCheckRetryMax; i++ {
		delay *= 2
	}
	if delay > updateCheckRetryMax {
		delay = updateCheckRetryMax
	}
	if interval > 0 && delay > interval {
		delay = interval
	}
	return delay
}
