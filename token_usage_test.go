package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTranscript(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEmptyTokenUsageDaysUsesSevenLocalCalendarDates(t *testing.T) {
	now := time.Date(2026, time.September, 8, 23, 59, 0, 0, time.Local)
	days, start, end := emptyTokenUsageDays(now)
	if len(days) != 7 || days[0].Date != "2026-09-02" || days[6].Date != "2026-09-08" {
		t.Fatalf("days = %+v", days)
	}
	if start.Hour() != 0 || end.Sub(start) < 6*24*time.Hour || end.Format("2006-01-02") != "2026-09-09" {
		t.Fatalf("range = %s to %s", start, end)
	}
}

func TestResolveCodexConfigDirPrecedence(t *testing.T) {
	explicit := t.TempDir()
	sysmon := t.TempDir()
	codexHome := t.TempDir()
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	getenv := func(name string) string {
		switch name {
		case "SYSMON_CODEX_CONFIG_DIR":
			return sysmon
		case "CODEX_HOME":
			return codexHome
		default:
			return ""
		}
	}
	if got := resolveCodexConfigDirFrom(explicit, getenv, func() string { return home }); got != explicit {
		t.Fatalf("explicit = %q, want %q", got, explicit)
	}
	if got := resolveCodexConfigDirFrom("", getenv, func() string { return home }); got != sysmon {
		t.Fatalf("sysmon env = %q, want %q", got, sysmon)
	}
	withoutSysmon := func(name string) string {
		if name == "SYSMON_CODEX_CONFIG_DIR" {
			return ""
		}
		return getenv(name)
	}
	if got := resolveCodexConfigDirFrom("", withoutSysmon, func() string { return home }); got != codexHome {
		t.Fatalf("CODEX_HOME = %q, want %q", got, codexHome)
	}
	if got := resolveCodexConfigDirFrom("", func(string) string { return "" }, func() string { return home }); got != filepath.Join(home, ".codex") {
		t.Fatalf("home fallback = %q", got)
	}
	if got := resolveCodexConfigDirFrom(filepath.Join(home, "missing"), func(string) string { return "" }, func() string { return home }); got != "" {
		t.Fatalf("missing explicit = %q, want empty", got)
	}
}

func TestScanClaudeTokenUsageDeduplicatesResponses(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().Truncate(time.Second)
	outside := now.AddDate(0, 0, -7).UTC().Format(time.RFC3339Nano)
	first := now.Add(-24 * time.Hour).UTC().Format(time.RFC3339Nano)
	second := now.Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)
	line := func(timestamp, request, message string, in, create, cached, out int64) string {
		return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"requestId":%q,"uuid":%q,"message":{"id":%q,"role":"assistant","usage":{"input_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,"output_tokens":%d}}}`+"\n", timestamp, request, message+"-uuid", message, in, create, cached, out)
	}
	contents := line(outside, "req-old", "msg-old", 1000, 0, 0, 0) +
		line(first, "req-one", "msg-one", 10, 20, 30, 40) +
		line(first, "req-one", "msg-one", 10, 20, 30, 40) + // repeated content block
		line(second, "req-two", "msg-two", 2, 3, 5, 7) +
		`{"type":"assistant"` // active transcript's incomplete tail
	writeTranscript(t, filepath.Join(dir, "projects", "project", "session.jsonl"), contents)

	days, newest, err := scanClaudeTokenUsage(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 7 {
		t.Fatalf("days = %d, want 7", len(days))
	}
	var total int64
	for _, day := range days {
		total += day.Tokens
	}
	if total != 117 {
		t.Fatalf("total = %d, want 117", total)
	}
	if newest.IsZero() || newest.UTC().Format(time.RFC3339Nano) != second {
		t.Fatalf("newest = %s, want %s", newest, second)
	}
}

func TestScanCodexUsageUsesCumulativeDeltasAndLatestLimits(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().Truncate(time.Second)
	outside := now.AddDate(0, 0, -7)
	dayOne := now.Add(-24 * time.Hour)
	dayTwo := now.Add(-2 * time.Hour)
	event := func(at time.Time, total int64, limits string) string {
		if limits == "" {
			limits = "null"
		}
		return fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":%d}},"rate_limits":%s}}`+"\n", at.UTC().Format(time.RFC3339Nano), total, limits)
	}
	latestLimits := `{"primary":{"used_percent":18.5,"window_minutes":10080,"resets_at":1893456000},"secondary":{"used_percent":4,"window_minutes":300,"resets_at":1893024000}}`
	contents := event(outside, 1000, "") +
		event(dayOne, 100, "") + // reset after the out-of-range event
		event(dayOne.Add(time.Minute), 100, "") +
		event(dayTwo, 250, latestLimits) +
		event(dayTwo.Add(time.Minute), 50, latestLimits) // counter reset
	writeTranscript(t, filepath.Join(dir, "sessions", "2026", "09", "08", "rollout.jsonl"), contents)

	result, err := scanCodexUsage(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, day := range result.Days {
		total += day.Tokens
	}
	if total != 300 {
		t.Fatalf("total = %d, want 300", total)
	}
	if len(result.Rows) != 2 || result.Rows[0].Label != "Weekly" || result.Rows[0].Percent != 18.5 || result.Rows[1].Label != "Session (5h)" {
		t.Fatalf("rows = %+v", result.Rows)
	}
	if result.Rows[0].ResetsAt != time.Unix(1893456000, 0).UTC().Format(time.RFC3339) {
		t.Fatalf("reset = %q", result.Rows[0].ResetsAt)
	}
}

func TestCodexUsageStatusAndHandler(t *testing.T) {
	unconfigured := newCodexUsageChecker("").Status(time.Now())
	if unconfigured.Configured || unconfigured.Source != "none" || len(unconfigured.Rows) != 0 || len(unconfigured.TokenDays) != 0 {
		t.Fatalf("unconfigured = %+v", unconfigured)
	}

	state := NewMemoryRuntimeState()
	state.SetCodexUsageChecker(newCodexUsageChecker(""))
	handler, err := newHTTPHandlerWithState(fakeCollector{}, testStaticFS(), state)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/codex-usage", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var got CodexUsageStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Configured || got.Source != "none" || got.TokenDays == nil {
		t.Fatalf("body = %+v", got)
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("content type = %q", rec.Header().Get("Content-Type"))
	}
}

func TestCodexUsageStatusUsesRefreshedSnapshot(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().Truncate(time.Second)
	path := filepath.Join(dir, "sessions", "rollout.jsonl")
	write := func(total int64) {
		t.Helper()
		writeTranscript(t, path, fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":%d}},"rate_limits":null}}`+"\n", now.UTC().Format(time.RFC3339Nano), total))
	}
	write(100)
	checker := newCodexUsageChecker(dir)
	checker.Refresh(now)
	first := checker.Status(now)
	write(900)
	cached := checker.Status(now.Add(tokenUsageRefreshInterval / 2))
	checker.Refresh(now.Add(tokenUsageRefreshInterval))
	refreshed := checker.Status(now.Add(tokenUsageRefreshInterval))
	if tokenDaysTotal(first.TokenDays) != 100 || tokenDaysTotal(cached.TokenDays) != 100 || tokenDaysTotal(refreshed.TokenDays) != 900 {
		t.Fatalf("totals first/cached/refreshed = %d/%d/%d", tokenDaysTotal(first.TokenDays), tokenDaysTotal(cached.TokenDays), tokenDaysTotal(refreshed.TokenDays))
	}
}

func TestCodexUsageFreshnessTracksSuccessfulScanNotLastActivity(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().Truncate(time.Second)
	lastActivity := now.Add(-2 * time.Hour)
	limits := `{"primary":{"used_percent":18.5,"window_minutes":10080,"resets_at":1893456000}}`
	writeTranscript(t, filepath.Join(dir, "sessions", "rollout.jsonl"), fmt.Sprintf(
		`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":100}},"rate_limits":%s}}`+"\n",
		lastActivity.UTC().Format(time.RFC3339Nano),
		limits,
	))

	checker := newCodexUsageChecker(dir)
	checker.Refresh(now)
	status := checker.Status(now)
	if status.Source != "sessions" || status.FetchedAt == nil || !status.FetchedAt.Equal(now.UTC()) {
		t.Fatalf("fresh status = %+v, want successful scan time %s", status, now.UTC())
	}
	if status.QuotaAt == nil || !status.QuotaAt.Equal(lastActivity.UTC()) || status.QuotaAgeSeconds != int((2*time.Hour).Seconds()) {
		t.Fatalf("quota sample = %+v, want source event time %s", status, lastActivity.UTC())
	}
	if status.Stale || status.AgeSeconds != 0 {
		t.Fatalf("fresh status = %+v, want non-stale despite old activity", status)
	}

	stale := checker.Status(now.Add(quotaStaleAfter + time.Second))
	if !stale.Stale || stale.AgeSeconds != int((quotaStaleAfter+time.Second).Seconds()) {
		t.Fatalf("unrefreshed status = %+v, want stale after sampler stops", stale)
	}
}

func TestClaudeTokenUsageUsesRefreshedSnapshot(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().Truncate(time.Second)
	path := filepath.Join(dir, "projects", "project", "session.jsonl")
	write := func(tokens int64) {
		t.Helper()
		writeTranscript(t, path, fmt.Sprintf(`{"type":"assistant","timestamp":%q,"requestId":"req","message":{"id":"msg","role":"assistant","usage":{"input_tokens":%d,"output_tokens":0}}}`+"\n", now.UTC().Format(time.RFC3339Nano), tokens))
	}
	write(100)
	checker := newQuotaChecker(QuotaCheckerOptions{ConfigDir: dir})
	checker.refreshTokenUsage(now)
	first := checker.Status(now)
	write(900)
	cached := checker.Status(now.Add(quotaStatusTTL + time.Second))
	checker.refreshTokenUsage(now.Add(tokenUsageRefreshInterval))
	refreshed := checker.Status(now.Add(tokenUsageRefreshInterval))
	if tokenDaysTotal(first.TokenDays) != 100 || tokenDaysTotal(cached.TokenDays) != 100 || tokenDaysTotal(refreshed.TokenDays) != 900 {
		t.Fatalf("totals first/cached/refreshed = %d/%d/%d", tokenDaysTotal(first.TokenDays), tokenDaysTotal(cached.TokenDays), tokenDaysTotal(refreshed.TokenDays))
	}
}

func TestTokenUsageSamplerStartStopIdempotent(t *testing.T) {
	claude := newQuotaChecker(QuotaCheckerOptions{ConfigDir: t.TempDir()})
	codex := newCodexUsageChecker(t.TempDir())
	sampler := newTokenUsageSampler(claude, codex)
	sampler.interval = time.Millisecond
	sampler.Start()
	sampler.Start()
	sampler.Stop()
	sampler.Stop()
}

func tokenDaysTotal(days []TokenUsageDay) int64 {
	var total int64
	for _, day := range days {
		total += day.Tokens
	}
	return total
}

func TestCodexWindowLabelFallbacks(t *testing.T) {
	for minutes, want := range map[int64]string{300: "Session (5h)", 10080: "Weekly", 2880: "2-day", 120: "2-hour", 45: "45-minute", 0: "Usage"} {
		if got := codexWindowLabel(minutes); got != want {
			t.Fatalf("codexWindowLabel(%d) = %q, want %q", minutes, got, want)
		}
	}
}
