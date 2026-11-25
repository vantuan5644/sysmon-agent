package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestQuotaFailureBackoffSequence(t *testing.T) {
	// With the production cadence the 1, 2 and 4-minute steps are floored at
	// the 5-minute interval: a failure never polls faster than a success.
	want := []time.Duration{5 * time.Minute, 5 * time.Minute, 5 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	for i, expected := range want {
		if got := quotaFailureBackoff(i+1, quotaPollInterval); got != expected {
			t.Errorf("failure %d: backoff = %v, want %v", i+1, got, expected)
		}
	}
	// The raw sequence, visible with a shorter interval.
	raw := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute}
	for i, expected := range raw {
		if got := quotaFailureBackoff(i+1, time.Second); got != expected {
			t.Errorf("raw failure %d: backoff = %v, want %v", i+1, got, expected)
		}
	}
	if got := quotaFailureBackoff(0, quotaPollInterval); got != quotaPollInterval {
		t.Errorf("no failure: backoff = %v, want the interval", got)
	}
	if got := quotaFailureBackoff(1000, quotaPollInterval); got != quotaFailureBackoffMax {
		t.Errorf("huge failure count: backoff = %v, want the %v cap", got, quotaFailureBackoffMax)
	}
}

func TestCodexFailureBackoffSequence(t *testing.T) {
	want := []time.Duration{5 * time.Minute, 5 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i, expected := range want {
		if got := codexFailureBackoff(i+1, quotaPollInterval); got != expected {
			t.Errorf("failure %d: backoff = %v, want %v", i+1, got, expected)
		}
	}
	raw := []time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute}
	for i, expected := range raw {
		if got := codexFailureBackoff(i+1, time.Second); got != expected {
			t.Errorf("raw failure %d: backoff = %v, want %v", i+1, got, expected)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		value string
		want  time.Duration
		ok    bool
	}{
		{"120", 2 * time.Minute, true},
		{" 7 ", 7 * time.Second, true},
		{"0", 0, true},
		{"3600", time.Hour, true},
		{"99999", time.Hour, true},
		{"9223372036854775807", time.Hour, true},
		{now.Add(10 * time.Minute).Format(http.TimeFormat), 10 * time.Minute, true},
		{now.Add(-10 * time.Minute).Format(http.TimeFormat), 0, true},
		{now.Add(48 * time.Hour).Format(http.TimeFormat), time.Hour, true},
		{"", 0, false},
		{"soon", 0, false},
		{"-5", 0, false},
		{"1.5", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseRetryAfter(tc.value, now)
		if got != tc.want || ok != tc.ok {
			t.Errorf("parseRetryAfter(%q) = (%v, %v), want (%v, %v)", tc.value, got, ok, tc.want, tc.ok)
		}
	}
}

func TestQuotaTokenFingerprint(t *testing.T) {
	const token = "sk-ant-oat-SECRET-TOKEN-VALUE"
	fp := quotaTokenFingerprint(token)
	if len(fp) != 16 || strings.Trim(fp, "0123456789abcdef") != "" {
		t.Fatalf("fingerprint = %q, want 16 lowercase hex digits", fp)
	}
	if strings.Contains(token, fp) || strings.Contains(fp, "SECRET") {
		t.Fatalf("fingerprint %q carries the token", fp)
	}
	if fp != quotaTokenFingerprint(token) || fp == quotaTokenFingerprint(token+"x") {
		t.Fatal("fingerprint must be stable for one token and differ between tokens")
	}
}

func TestQuotaPoller429HonorsRetryAfterAndBacksOff(t *testing.T) {
	dir := t.TempDir()
	var retryAfter atomic.Value
	retryAfter.Store("")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if value := retryAfter.Load().(string); value != "" {
			w.Header().Set("Retry-After", value)
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	checker := newQuotaChecker(QuotaCheckerOptions{ConfigDir: dir, Poll: true, UsageURL: server.URL, HTTPClient: server.Client()})
	quotaTestCredentials(t, dir, "tok", time.Now().Add(time.Hour))

	steps := []struct {
		header string
		want   time.Duration
	}{
		{"1800", 30 * time.Minute},    // longer than the backoff: honoured
		{"99999", time.Hour},          // capped at an hour
		{"30", 5 * time.Minute},       // shorter than the cadence: the cadence wins
		{"", 8 * time.Minute},         // 4th consecutive failure
		{"garbage", 15 * time.Minute}, // unparseable: the backoff alone
		{time.Now().Add(-time.Hour).Format(http.TimeFormat), 15 * time.Minute},
	}
	for i, step := range steps {
		retryAfter.Store(step.header)
		wait, err := checker.pollOnce()
		if err == nil || !strings.Contains(err.Error(), "rate limited") {
			t.Fatalf("step %d: err = %v", i, err)
		}
		if wait != step.want {
			t.Fatalf("step %d (Retry-After %q): wait = %v, want %v", i, step.header, wait, step.want)
		}
	}
	if checker.sched.Failures != len(steps) {
		t.Fatalf("failures = %d, want %d", checker.sched.Failures, len(steps))
	}
}

func TestQuotaPollerFailureResetsOnSuccess(t *testing.T) {
	dir := t.TempDir()
	var fail atomic.Bool
	fail.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, sampleLivePayload())
	}))
	defer server.Close()
	checker := newQuotaChecker(QuotaCheckerOptions{ConfigDir: dir, Poll: true, UsageURL: server.URL, HTTPClient: server.Client()})
	quotaTestCredentials(t, dir, "tok", time.Now().Add(time.Hour))
	for i := 0; i < 4; i++ {
		if _, err := checker.pollOnce(); err == nil {
			t.Fatal("502 should fail")
		}
	}
	if wait, _ := checker.pollOnce(); wait != 15*time.Minute {
		t.Fatalf("5th failure wait = %v, want 15m", wait)
	}
	fail.Store(false)
	if wait, err := checker.pollOnce(); err != nil || wait != 0 {
		t.Fatalf("recovery = (%v, %v), want (0, nil)", wait, err)
	}
	if checker.sched.Failures != 0 {
		t.Fatalf("failures after success = %d, want 0", checker.sched.Failures)
	}
}

func TestQuotaPollerHoldsBackRejectedToken(t *testing.T) {
	dir := t.TempDir()
	const rejected = "sk-ant-oat-REJECTED-TOKEN"
	const fresh = "sk-ant-oat-FRESH-TOKEN"
	var calls atomic.Int32
	var sent []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		sent = append(sent, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") == "Bearer "+rejected {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		fmt.Fprint(w, sampleLivePayload())
	}))
	defer server.Close()
	var logLines []string
	checker := newQuotaChecker(QuotaCheckerOptions{
		ConfigDir: dir, Poll: true, UsageURL: server.URL, HTTPClient: server.Client(),
		Logf: func(format string, args ...any) { logLines = append(logLines, fmt.Sprintf(format, args...)) },
	})
	quotaTestCredentials(t, dir, rejected, time.Now().Add(time.Hour))

	// 403 is treated like 401.
	if _, err := checker.pollOnce(); !errors.Is(err, errQuotaTokenRejected) {
		t.Fatalf("403 err = %v", err)
	}
	if checker.sched.RejectedFP != quotaTokenFingerprint(rejected) {
		t.Fatalf("rejected fingerprint = %q", checker.sched.RejectedFP)
	}
	// The same token is not sent again; the file is re-read within a minute.
	for i := 0; i < 3; i++ {
		wait, err := checker.pollOnce()
		if !errors.Is(err, errQuotaTokenRejected) || wait <= 0 || wait > quotaTokenRecheck {
			t.Fatalf("held-back poll %d = (%v, %v)", i, wait, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("rejected token re-sent: %d calls", calls.Load())
	}
	// After 30 minutes the same token is tried once more.
	checker.sched.RejectedAt = time.Now().Add(-quotaRejectedTokenRetry - time.Second)
	if _, err := checker.pollOnce(); !errors.Is(err, errQuotaTokenRejected) || calls.Load() != 2 {
		t.Fatalf("30-minute retry: err %v, calls %d", err, calls.Load())
	}
	// A new token in the file is sent at once.
	quotaTestCredentials(t, dir, fresh, time.Now().Add(time.Hour))
	if wait, err := checker.pollOnce(); err != nil || wait != 0 || calls.Load() != 3 {
		t.Fatalf("new token: (%v, %v), calls %d", wait, err, calls.Load())
	}
	if sent[2] != "Bearer "+fresh {
		t.Fatalf("third request sent %q", sent[2])
	}
	if checker.sched.RejectedFP != "" || checker.Status(time.Now()).Source != "live" {
		t.Fatalf("success must clear the rejection: %+v", checker.sched)
	}
	for _, line := range logLines {
		if strings.Contains(line, rejected) || strings.Contains(line, fresh) {
			t.Fatalf("token leaked into a log line: %s", line)
		}
	}
}

func TestQuotaStatePersistsAcrossRestart(t *testing.T) {
	configDir := t.TempDir()
	stateDir := filepath.Join(t.TempDir(), "pairing")
	const token = "sk-ant-oat-SECRET-TOKEN-VALUE"
	var calls atomic.Int32
	var status atomic.Int32
	status.Store(http.StatusOK)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if code := int(status.Load()); code != http.StatusOK {
			w.WriteHeader(code)
			return
		}
		fmt.Fprint(w, sampleLivePayload())
	}))
	defer server.Close()
	quotaTestCredentials(t, configDir, token, time.Now().Add(time.Hour))

	start := func() *QuotaChecker {
		t.Helper()
		store, err := newQuotaStateStore(stateDir)
		if err != nil {
			t.Fatalf("load state: %v", err)
		}
		return newQuotaChecker(QuotaCheckerOptions{ConfigDir: configDir, Poll: true, UsageURL: server.URL, HTTPClient: server.Client(), State: store})
	}

	first := start()
	if _, err := first.pollOnce(); err != nil {
		t.Fatalf("poll: %v", err)
	}
	path := filepath.Join(stateDir, quotaStateFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("state file not written: %v", err)
	}
	if strings.Contains(string(data), token) || strings.Contains(string(data), "SECRET") {
		t.Fatalf("token persisted: %s", data)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("state file mode = %v, want 0600", perm)
		}
	}
	var doc quotaStateDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("state file is not JSON: %v", err)
	}
	if len(doc.Claude.Rows) == 0 || doc.Claude.FetchedAt.IsZero() || doc.Claude.NextAt.IsZero() {
		t.Fatalf("state file missing the reading or schedule: %+v", doc.Claude)
	}

	// A restart shows the last reading at once, without calling out, and
	// keeps the schedule: the first poll waits for the saved next attempt.
	second := start()
	restored := second.Status(time.Now())
	if restored.Source != "live" || len(restored.Rows) == 0 || restored.Stale {
		t.Fatalf("restored status = %+v", restored)
	}
	if row, ok := quotaRowByID(restored, "five_hour"); !ok || row.WindowSeconds != quotaFiveHourSeconds {
		t.Fatalf("restored five_hour row = %+v", row)
	}
	if wait := quotaInitialWait(quotaPollStartupDelay, second.sched.NextAt, time.Now()); wait < quotaPollInterval-time.Minute {
		t.Fatalf("restart would poll after %v, want about the %v cadence", wait, quotaPollInterval)
	}
	if calls.Load() != 1 {
		t.Fatalf("restart called out: %d calls", calls.Load())
	}
	// The restored reading ages like any other.
	if !second.computeStatus(time.Now().Add(quotaStaleAfter + time.Minute)).Stale {
		t.Fatal("a restored reading must go stale on the usual clock")
	}

	// A rejection survives a restart too: the same token is not re-sent.
	status.Store(http.StatusUnauthorized)
	if _, err := second.pollOnce(); !errors.Is(err, errQuotaTokenRejected) {
		t.Fatalf("401: %v", err)
	}
	data, _ = os.ReadFile(path)
	if strings.Contains(string(data), token) {
		t.Fatalf("token persisted after a rejection: %s", data)
	}
	third := start()
	if !strings.Contains(third.Status(time.Now()).Error, "token rejected") {
		t.Fatalf("restored rejection not surfaced: %q", third.Status(time.Now()).Error)
	}
	before := calls.Load()
	if _, err := third.pollOnce(); !errors.Is(err, errQuotaTokenRejected) || calls.Load() != before {
		t.Fatalf("restart re-sent the rejected token: err %v, calls %d -> %d", err, before, calls.Load())
	}
}

func TestQuotaStateStoreMemoryOnlyAndCorrupt(t *testing.T) {
	memory, err := newQuotaStateStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := memory.SetClaude(quotaProviderState{Failures: 2}); err != nil {
		t.Fatalf("memory-only save: %v", err)
	}
	if got := memory.Claude(); got.Failures != 2 {
		t.Fatalf("memory-only state = %+v", got)
	}
	var nilStore *quotaStateStore
	if err := nilStore.SetCodex(quotaProviderState{Failures: 1}); err != nil || nilStore.Codex().Failures != 0 {
		t.Fatal("a nil store must be a no-op")
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, quotaStateFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := newQuotaStateStore(dir)
	if err == nil || store == nil {
		t.Fatalf("corrupt file: store %v err %v, want a usable store and an error", store, err)
	}
	if err := store.SetCodex(quotaProviderState{Failures: 1}); err != nil {
		t.Fatalf("save over a corrupt file: %v", err)
	}
	reloaded, err := newQuotaStateStore(dir)
	if err != nil || reloaded.Codex().Failures != 1 {
		t.Fatalf("reload = %+v, %v", reloaded.Codex(), err)
	}

	// A damaged fingerprint or negative count is dropped, not trusted.
	bad := `{"version":1,"claude":{"failures":-3,"rejected_token_fingerprint":"not-hex","rejected_at":"2026-08-19T00:00:00Z","rows":[{"id":"","label":"x","percent":1}]}}`
	if err := os.WriteFile(filepath.Join(dir, quotaStateFile), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	sanitized, err := newQuotaStateStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := sanitized.Claude(); got.Failures != 0 || got.RejectedFP != "" || !got.RejectedAt.IsZero() || len(got.Rows) != 0 {
		t.Fatalf("sanitized = %+v", got)
	}
}

func TestQuotaInitialWait(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	if got := quotaInitialWait(30*time.Second, time.Time{}, now); got != 30*time.Second {
		t.Errorf("no schedule: %v", got)
	}
	if got := quotaInitialWait(30*time.Second, now.Add(-time.Hour), now); got != 30*time.Second {
		t.Errorf("past schedule: %v", got)
	}
	if got := quotaInitialWait(30*time.Second, now.Add(4*time.Minute), now); got != 4*time.Minute {
		t.Errorf("pending schedule: %v", got)
	}
	if got := quotaInitialWait(0, now.Add(72*time.Hour), now); got != quotaRetryAfterCap {
		t.Errorf("far-future schedule: %v, want the %v cap", got, quotaRetryAfterCap)
	}
}

func TestCodexQuotaPollBacksOffAndPersists(t *testing.T) {
	configDir := t.TempDir()
	stateDir := t.TempDir()
	store, _ := newQuotaStateStore(stateDir)
	checker := newCodexUsageCheckerWithOptions(configDir, CodexQuotaPollOptions{
		Enabled:  true,
		Interval: time.Minute,
		State:    store,
		Fetch:    func(context.Context) ([]QuotaRow, error) { return nil, errors.New("lookup unavailable") },
	})
	want := []time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i, expected := range want {
		if got := checker.pollQuota(context.Background()); got != expected {
			t.Fatalf("failure %d: wait = %v, want %v", i+1, got, expected)
		}
	}
	if saved := store.Codex(); saved.Failures != len(want) || saved.NextAt.IsZero() {
		t.Fatalf("saved schedule = %+v", saved)
	}
	checker.pollOptions.Fetch = func(context.Context) ([]QuotaRow, error) {
		return []QuotaRow{{ID: "primary", Label: "Weekly", Percent: 26, WindowSeconds: quotaWeekSeconds}}, nil
	}
	if got := checker.pollQuota(context.Background()); got != time.Minute {
		t.Fatalf("success wait = %v, want the interval", got)
	}
	if checker.pollFailures != 0 {
		t.Fatalf("failures after success = %d", checker.pollFailures)
	}

	// A restart shows the saved reading and waits for the saved schedule.
	reloaded, err := newQuotaStateStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	restarted := newCodexUsageCheckerWithOptions(configDir, CodexQuotaPollOptions{Enabled: true, State: reloaded})
	got := restarted.Status(time.Now())
	if got.Source != "live" || len(got.Rows) != 1 || got.Rows[0].Percent != 26 || got.Rows[0].WindowSeconds != quotaWeekSeconds {
		t.Fatalf("restored = %+v", got)
	}
	if restarted.pollNextAt.IsZero() {
		t.Fatal("restored schedule lost")
	}
	// Disabled polling ignores the saved reading.
	off := newCodexUsageCheckerWithOptions(configDir, CodexQuotaPollOptions{Enabled: false, State: reloaded})
	if off.Status(time.Now()).Source == "live" {
		t.Fatal("polling off must not show saved live rows")
	}
}

func TestQuotaRowsCarryWindowSeconds(t *testing.T) {
	var payload quotaLivePayload
	if err := json.Unmarshal([]byte(sampleLivePayload()), &payload); err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"five_hour": quotaFiveHourSeconds, "seven_day": quotaWeekSeconds, "weekly_fable": quotaWeekSeconds, "credits": 0}
	rows := quotaRowsFromLivePayload(payload)
	seen := map[string]bool{}
	for _, row := range rows {
		if seen[row.ID] {
			t.Fatalf("duplicate row %q: limits[] must not repeat a named window", row.ID)
		}
		seen[row.ID] = true
		if expected, ok := want[row.ID]; ok && row.WindowSeconds != expected {
			t.Errorf("live %s window = %d, want %d", row.ID, row.WindowSeconds, expected)
		}
	}
	snapRows, _, err := parseQuotaSnapshot([]byte(sampleSnapshotJSON(10, 20, 1)))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range snapRows {
		if row.WindowSeconds != want[row.ID] {
			t.Errorf("snapshot %s window = %d, want %d", row.ID, row.WindowSeconds, want[row.ID])
		}
	}
	used := 40.0
	codexRows := codexQuotaRows(&codexRateLimits{
		Primary:   &codexRateWindow{UsedPercent: &used, WindowMinute: 300},
		Secondary: &codexRateWindow{UsedPercent: &used, WindowMinute: 10080},
	})
	if len(codexRows) != 2 || codexRows[0].WindowSeconds != quotaFiveHourSeconds || codexRows[1].WindowSeconds != quotaWeekSeconds {
		t.Fatalf("codex rows = %+v", codexRows)
	}
	encoded, _ := json.Marshal(QuotaRow{ID: "credits", Label: "Usage credits", Percent: 1})
	if strings.Contains(string(encoded), "window_seconds") {
		t.Fatalf("window_seconds must be omitted when unknown: %s", encoded)
	}
}

func TestQuotaLiveLimitsKindBackfillsNamedWindows(t *testing.T) {
	payload := `{"limits": [
	  {"kind": "session", "percent": 9, "resets_at": "2026-08-19T06:39:59Z", "scope": null},
	  {"kind": "weekly_all", "percent": 37, "resets_at": "2026-08-22T16:00:00Z", "scope": null},
	  {"kind": "weekly_scoped", "percent": 29, "scope": {"model": {"display_name": "Fable"}}},
	  {"kind": "something_new", "percent": 50, "scope": null}
	]}`
	var parsed quotaLivePayload
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		t.Fatal(err)
	}
	rows := quotaRowsFromLivePayload(parsed)
	got := map[string]QuotaRow{}
	for _, row := range rows {
		got[row.ID] = row
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %+v, want five_hour, seven_day and weekly_fable", rows)
	}
	if row := got["five_hour"]; row.Percent != 9 || row.Label != "Session (5h)" || row.ResetsAt != "2026-08-19T06:39:59Z" || row.WindowSeconds != quotaFiveHourSeconds {
		t.Errorf("five_hour = %+v", row)
	}
	if row := got["seven_day"]; row.Percent != 37 || row.Label != "Weekly (all)" {
		t.Errorf("seven_day = %+v", row)
	}
	if row := got["weekly_fable"]; row.Percent != 29 || row.Label != "Weekly (Fable)" {
		t.Errorf("weekly_fable = %+v", row)
	}
	// The named window wins when both are present.
	both := `{"five_hour": {"utilization": 12, "resets_at": "x"}, "limits": [{"kind": "session", "percent": 9, "scope": null}]}`
	var parsedBoth quotaLivePayload
	if err := json.Unmarshal([]byte(both), &parsedBoth); err != nil {
		t.Fatal(err)
	}
	rows = quotaRowsFromLivePayload(parsedBoth)
	if len(rows) != 1 || rows[0].Percent != 12 {
		t.Fatalf("named window must win over limits[]: %+v", rows)
	}
}
