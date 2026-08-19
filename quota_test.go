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

func writeQuotaFile(t *testing.T, path string, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

// sampleLivePayload is a full usage payload shaped like the one the widget
// caches: named windows plus a per-model limit (Fable) and both credit shapes.
func sampleLivePayload() string {
	return `{
  "five_hour": {"utilization": 9.0, "resets_at": "2026-08-19T06:39:59Z"},
  "seven_day": {"utilization": 37.0, "resets_at": "2026-08-22T16:00:00Z"},
  "seven_day_opus": null,
  "seven_day_sonnet": null,
  "cinder_cove": null,
  "extra_usage": {
    "is_enabled": true, "monthly_limit": 5000, "used_credits": 500,
    "utilization": 10.0, "currency": "USD", "decimal_places": 2
  },
  "limits": [
    {"kind": "session", "group": "session", "percent": 9, "scope": null},
    {"kind": "weekly_all", "group": "weekly", "percent": 37, "scope": null},
    {"kind": "weekly_scoped", "group": "weekly", "percent": 29, "scope": {"model": {"id": null, "display_name": "Fable"}}}
  ],
  "spend": {
    "enabled": true, "percent": 10,
    "used": {"amount_minor": 500, "currency": "USD", "exponent": 2},
    "limit": {"amount_minor": 5000, "currency": "USD", "exponent": 2}
  }
}`
}

func sampleSnapshotJSON(fiveHour, sevenDay float64, updatedAtMS int64) string {
	return fmt.Sprintf(`{
  "updated_at": %d,
  "five_hour": {"used_percentage": %f, "resets_at": 1787121600},
  "seven_day": {"used_percentage": %f, "resets_at": 1787414400},
  "seven_day_opus": null,
  "seven_day_sonnet": null,
  "model_scoped": [],
  "cinder_cove": null
}`, updatedAtMS, fiveHour, sevenDay)
}

func quotaRowByID(status QuotaStatus, id string) (QuotaRow, bool) {
	for _, row := range status.Rows {
		if row.ID == id {
			return row, true
		}
	}
	return QuotaRow{}, false
}

// --- config-dir resolution ------------------------------------------------------

func TestResolveClaudeConfigDirPrecedence(t *testing.T) {
	explicit := t.TempDir()
	envDir := t.TempDir()
	claudeEnvDir := t.TempDir()
	home := t.TempDir()
	homeDotClaude := filepath.Join(home, ".claude")
	if err := os.MkdirAll(homeDotClaude, 0o755); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "plain-file")
	if err := os.WriteFile(missing, nil, 0o644); err != nil {
		t.Fatal(err) // a candidate that exists but is a FILE, not a dir
	}

	getenv := func(name string) string {
		switch name {
		case "SYSMON_CLAUDE_CONFIG_DIR":
			return envDir
		case "CLAUDE_CONFIG_DIR":
			return claudeEnvDir
		default:
			return ""
		}
	}
	homeFn := func() string { return home }

	if got := resolveClaudeConfigDirFrom(explicit, getenv, homeFn); got != explicit {
		t.Fatalf("explicit dir = %q, want %q", got, explicit)
	}
	// SYSMON_CLAUDE_CONFIG_DIR fills the "explicit" slot when the flag is unset.
	if got := resolveClaudeConfigDirFrom("", getenv, homeFn); got != envDir {
		t.Fatalf("SYSMON_CLAUDE_CONFIG_DIR = %q, want %q", got, envDir)
	}
	getenvNoSysmon := func(name string) string {
		if name == "SYSMON_CLAUDE_CONFIG_DIR" {
			return ""
		}
		return getenv(name)
	}
	if got := resolveClaudeConfigDirFrom("", getenvNoSysmon, homeFn); got != claudeEnvDir {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q, want %q", got, claudeEnvDir)
	}
	getenvNone := func(string) string { return "" }
	if got := resolveClaudeConfigDirFrom("", getenvNone, homeFn); got != homeDotClaude {
		t.Fatalf("home fallback = %q, want %q", got, homeDotClaude)
	}
	// A candidate that does not exist resolves to "" -> feature off.
	if got := resolveClaudeConfigDirFrom(filepath.Join(home, "missing"), getenvNone, homeFn); got != "" {
		t.Fatalf("missing explicit dir = %q, want empty", got)
	}
	if got := resolveClaudeConfigDirFrom(missing, getenvNone, homeFn); got != "" {
		t.Fatalf("file (not dir) candidate = %q, want empty", got)
	}
	// An explicitly set CLAUDE_CONFIG_DIR that does not exist turns the
	// feature off rather than silently reading some other config's quota:
	// Claude Code honors that variable, so its quota.json would live THERE.
	getenvMissingClaude := func(name string) string {
		if name == "CLAUDE_CONFIG_DIR" {
			return filepath.Join(home, "nope")
		}
		return ""
	}
	if got := resolveClaudeConfigDirFrom("", getenvMissingClaude, homeFn); got != "" {
		t.Fatalf("missing CLAUDE_CONFIG_DIR = %q, want feature off", got)
	}
}

// --- parsing + merging -----------------------------------------------------------

func TestQuotaStatusUnconfiguredWithoutDir(t *testing.T) {
	checker := newQuotaChecker(QuotaCheckerOptions{ConfigDir: ""})
	status := checker.Status(time.Now().UTC())
	if status.Configured {
		t.Fatal("empty config dir must report unconfigured")
	}
	if status.Source != "none" {
		t.Fatalf("source = %q, want none", status.Source)
	}
	if len(status.Rows) != 0 {
		t.Fatalf("rows = %+v, want empty", status.Rows)
	}
	if status.Error != "" {
		t.Fatalf("unconfigured error = %q, want empty", status.Error)
	}
}

func TestQuotaStatusSnapshotOnly(t *testing.T) {
	dir := t.TempDir()
	writeQuotaFile(t, filepath.Join(dir, "quota.json"), sampleSnapshotJSON(7, 36, 1_787_104_665_319))
	checker := newQuotaChecker(QuotaCheckerOptions{ConfigDir: dir})

	now := time.UnixMilli(1_787_104_665_319).Add(2 * time.Minute)
	status := checker.Status(now)
	if !status.Configured || status.Source != "snapshot" {
		t.Fatalf("configured/source = %v/%q, want true/snapshot", status.Configured, status.Source)
	}
	if row, ok := quotaRowByID(status, "five_hour"); !ok || row.Percent != 7 {
		t.Fatalf("five_hour row = %+v ok=%v", row, ok)
	}
	if row, ok := quotaRowByID(status, "seven_day"); !ok || row.Percent != 36 {
		t.Fatalf("seven_day row = %+v ok=%v", row, ok)
	}
	if row, ok := quotaRowByID(status, "five_hour"); !ok || row.ResetsAt != time.Unix(1787121600, 0).UTC().Format(time.RFC3339) {
		t.Fatalf("snapshot reset not normalized to RFC3339: %+v", row)
	}
	if status.AgeSeconds != 120 {
		t.Fatalf("age_seconds = %d, want 120", status.AgeSeconds)
	}
	if status.Stale {
		t.Fatal("2-minute-old snapshot must not be stale")
	}
}

func TestQuotaStatusCacheOnly(t *testing.T) {
	dir := t.TempDir()
	writeQuotaFile(t, filepath.Join(dir, "widgets", "quota-live-cache.json"),
		fmt.Sprintf(`{"fetched_at": 1787104907065, "data": %s}`, sampleLivePayload()))
	checker := newQuotaChecker(QuotaCheckerOptions{ConfigDir: dir})

	now := time.UnixMilli(1787104907065).Add(3 * time.Minute)
	status := checker.Status(now)
	if !status.Configured || status.Source != "cache" {
		t.Fatalf("configured/source = %v/%q, want true/cache", status.Configured, status.Source)
	}
	for _, id := range []string{"five_hour", "seven_day", "weekly_fable", "credits"} {
		if _, ok := quotaRowByID(status, id); !ok {
			t.Fatalf("cache-only status missing row %q: %+v", id, status.Rows)
		}
	}
	if row, _ := quotaRowByID(status, "weekly_fable"); row.Percent != 29 || row.Label != "Weekly (Fable)" {
		t.Fatalf("fable row = %+v", row)
	}
	if row, _ := quotaRowByID(status, "credits"); row.Percent != 10 || row.Note != "$5.00 of $50.00 used" {
		t.Fatalf("credits row = %+v", row)
	}
	if status.AgeSeconds != 180 {
		t.Fatalf("age_seconds = %d, want 180", status.AgeSeconds)
	}
}

func TestQuotaStatusMergeSnapshotNewerOverridesWindowsNotFable(t *testing.T) {
	dir := t.TempDir()
	// Cache is 5 minutes old; the snapshot was written 10 seconds ago.
	cacheAt := time.Now().Add(-5 * time.Minute).Truncate(time.Millisecond)
	snapAt := time.Now().Add(-10 * time.Second).Truncate(time.Millisecond)
	writeQuotaFile(t, filepath.Join(dir, "widgets", "quota-live-cache.json"),
		fmt.Sprintf(`{"fetched_at": %d, "data": %s}`, cacheAt.UnixMilli(), sampleLivePayload()))
	writeQuotaFile(t, filepath.Join(dir, "quota.json"), sampleSnapshotJSON(12, 41, snapAt.UnixMilli()))
	checker := newQuotaChecker(QuotaCheckerOptions{ConfigDir: dir})

	now := time.Now()
	status := checker.Status(now)
	if status.Source != "snapshot" {
		t.Fatalf("source = %q, want snapshot (freshest contributor)", status.Source)
	}
	// 5h + weekly come from the FRESHER snapshot.
	if row, _ := quotaRowByID(status, "five_hour"); row.Percent != 12 {
		t.Fatalf("five_hour = %+v, want snapshot's 12%%", row)
	}
	if row, _ := quotaRowByID(status, "seven_day"); row.Percent != 41 {
		t.Fatalf("seven_day = %+v, want snapshot's 41%%", row)
	}
	// Fable + credits only exist in the cache payload: they survive, annotated
	// with their own sample age so staleness is visible per row.
	row, ok := quotaRowByID(status, "weekly_fable")
	if !ok || row.Percent != 29 {
		t.Fatalf("fable row = %+v ok=%v, want the cache's 29%%", row, ok)
	}
	if !strings.Contains(row.Note, "as of 5m ago") {
		t.Fatalf("carried-over fable note = %q, want an 'as of 5m ago' annotation", row.Note)
	}
	if row, _ := quotaRowByID(status, "credits"); row.Note == "" || !strings.Contains(row.Note, "$5.00 of $50.00 used") {
		t.Fatalf("carried-over credits note = %q, want the amounts preserved", row.Note)
	}
	if !strings.Contains(row.Note, "as of 5m ago") {
		t.Fatalf("carried-over credits note = %q, want sample-age annotation", row.Note)
	}
	if status.AgeSeconds != 10 {
		t.Fatalf("age_seconds = %d, want 10 (from the snapshot)", status.AgeSeconds)
	}
}

func TestQuotaStatusMergeCacheNewerKeepsCacheRows(t *testing.T) {
	dir := t.TempDir()
	cacheAt := time.Now().Add(-30 * time.Second).Truncate(time.Millisecond)
	snapAt := time.Now().Add(-5 * time.Minute).Truncate(time.Millisecond)
	writeQuotaFile(t, filepath.Join(dir, "widgets", "quota-live-cache.json"),
		fmt.Sprintf(`{"fetched_at": %d, "data": %s}`, cacheAt.UnixMilli(), sampleLivePayload()))
	writeQuotaFile(t, filepath.Join(dir, "quota.json"), sampleSnapshotJSON(12, 41, snapAt.UnixMilli()))
	checker := newQuotaChecker(QuotaCheckerOptions{ConfigDir: dir})

	status := checker.Status(time.Now())
	if status.Source != "cache" {
		t.Fatalf("source = %q, want cache (freshest contributor)", status.Source)
	}
	if row, _ := quotaRowByID(status, "five_hour"); row.Percent != 9 {
		t.Fatalf("five_hour = %+v, want the cache's 9%% (snapshot is older)", row)
	}
	if row, _ := quotaRowByID(status, "weekly_fable"); row.Percent != 29 || row.Note != "" {
		t.Fatalf("fable = %+v, want un-annotated cache row", row)
	}
}

func TestQuotaRowsFromLiveUnknownModelStillYieldsRow(t *testing.T) {
	// A limits[] entry whose display_name this build has never heard of must
	// still produce a row: new per-model buckets appear with no code change.
	payload := `{"limits": [{"kind": "weekly_scoped", "percent": 55, "scope": {"model": {"display_name": "Zephyr Prime"}}}]}`
	rows, at, err := parseQuotaLiveCache([]byte(fmt.Sprintf(`{"fetched_at": 1787104907065, "data": %s}`, payload)))
	if err != nil {
		t.Fatal(err)
	}
	if at.IsZero() || len(rows) != 1 {
		t.Fatalf("rows = %+v at=%v, want one row", rows, at)
	}
	if rows[0].ID != "weekly_zephyr_prime" || rows[0].Label != "Weekly (Zephyr Prime)" || rows[0].Percent != 55 {
		t.Fatalf("unknown-model row = %+v", rows[0])
	}
}

func TestQuotaCreditsBothShapes(t *testing.T) {
	spendOnly := `{"spend": {"enabled": true, "percent": 42, "used": {"amount_minor": 2100, "exponent": 2}, "limit": {"amount_minor": 5000, "exponent": 2}}}`
	rows, _, err := parseQuotaLiveCache([]byte(fmt.Sprintf(`{"fetched_at": 1, "data": %s}`, spendOnly)))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "credits" || rows[0].Percent != 42 || rows[0].Note != "$21.00 of $50.00 used" {
		t.Fatalf("spend-shaped credits row = %+v", rows)
	}

	extraOnly := `{"extra_usage": {"is_enabled": true, "utilization": 42, "used_credits": 2100, "monthly_limit": 5000, "currency": "USD", "decimal_places": 2}}`
	rows, _, err = parseQuotaLiveCache([]byte(fmt.Sprintf(`{"fetched_at": 1, "data": %s}`, extraOnly)))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "credits" || rows[0].Percent != 42 || rows[0].Note != "$21.00 of $50.00 used" {
		t.Fatalf("extra_usage-shaped credits row = %+v", rows)
	}

	// Both present: spend wins (this host's live payload carries both shapes).
	both := `{"spend": {"enabled": true, "percent": 7, "used": {"amount_minor": 350, "exponent": 2}, "limit": {"amount_minor": 5000, "exponent": 2}},
	         "extra_usage": {"is_enabled": true, "utilization": 42, "used_credits": 2100, "monthly_limit": 5000, "currency": "USD", "decimal_places": 2}}`
	rows, _, err = parseQuotaLiveCache([]byte(fmt.Sprintf(`{"fetched_at": 1, "data": %s}`, both)))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Percent != 7 || rows[0].Note != "$3.50 of $50.00 used" {
		t.Fatalf("spend must be preferred when both parse: %+v", rows)
	}

	// A disabled pot yields no row (mirrors the CLI), not a misleading 0%.
	disabled := `{"spend": {"enabled": false, "percent": 0}, "extra_usage": {"is_enabled": false, "utilization": 0}}`
	rows, _, err = parseQuotaLiveCache([]byte(fmt.Sprintf(`{"fetched_at": 1, "data": %s}`, disabled)))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("disabled credits produced rows: %+v", rows)
	}
}

func TestQuotaStatusMissingEmptyCorruptFilesNeverError(t *testing.T) {
	// Missing files: configured (the dir exists) but no data yet, and a helpful
	// error rather than a failure.
	dir := t.TempDir()
	checker := newQuotaChecker(QuotaCheckerOptions{ConfigDir: dir})
	status := checker.Status(time.Now())
	if !status.Configured || status.Source != "none" || len(status.Rows) != 0 {
		t.Fatalf("missing files: %+v", status)
	}
	if status.Error == "" || !strings.Contains(status.Error, "no quota data") {
		t.Fatalf("missing files error = %q, want the 'run a Claude Code session' hint", status.Error)
	}

	// Corrupt cache beside a good snapshot: snapshot rows still served, cache
	// parse failure surfaced in Error. (Fresh checker: the status TTL cache
	// from the missing-files half would otherwise mask the re-read.)
	dirCache := t.TempDir()
	writeQuotaFile(t, filepath.Join(dirCache, "widgets", "quota-live-cache.json"), `{"fetched_at": 12, "data":`)
	writeQuotaFile(t, filepath.Join(dirCache, "quota.json"), sampleSnapshotJSON(5, 30, time.Now().UnixMilli()))
	checkerCache := newQuotaChecker(QuotaCheckerOptions{ConfigDir: dirCache})
	cacheStatus := checkerCache.Status(time.Now())
	if row, ok := quotaRowByID(cacheStatus, "seven_day"); !ok || row.Percent != 30 {
		t.Fatalf("corrupt cache dropped snapshot rows: %+v", cacheStatus.Rows)
	}
	if !strings.Contains(cacheStatus.Error, "quota-live-cache.json") {
		t.Fatalf("corrupt cache not reported: %q", cacheStatus.Error)
	}

	// Corrupt snapshot beside a good cache: cache rows still served.
	dir2 := t.TempDir()
	writeQuotaFile(t, filepath.Join(dir2, "quota.json"), "")
	writeQuotaFile(t, filepath.Join(dir2, "widgets", "quota-live-cache.json"),
		fmt.Sprintf(`{"fetched_at": %d, "data": %s}`, time.Now().UnixMilli(), sampleLivePayload()))
	checker2 := newQuotaChecker(QuotaCheckerOptions{ConfigDir: dir2})
	status = checker2.Status(time.Now())
	if _, ok := quotaRowByID(status, "weekly_fable"); !ok {
		t.Fatalf("corrupt snapshot dropped cache rows: %+v", status.Rows)
	}
	if !strings.Contains(status.Error, "quota.json") {
		t.Fatalf("corrupt snapshot not reported: %q", status.Error)
	}
}

func TestQuotaStatusStalenessAgainstInjectedClock(t *testing.T) {
	dir := t.TempDir()
	at := time.Now().Add(-20 * time.Minute).Truncate(time.Millisecond)
	writeQuotaFile(t, filepath.Join(dir, "quota.json"), sampleSnapshotJSON(7, 36, at.UnixMilli()))
	checker := newQuotaChecker(QuotaCheckerOptions{ConfigDir: dir})

	status := checker.Status(at.Add(14 * time.Minute))
	if status.Stale {
		t.Fatal("14-minute-old snapshot must not be stale")
	}
	// Advance the clock monotonically (the status TTL cache assumes it) past
	// the staleness threshold.
	status = checker.Status(at.Add(20 * time.Minute))
	if !status.Stale {
		t.Fatal("20-minute-old snapshot must be stale (limit is 15m)")
	}
	if status.AgeSeconds != 20*60 {
		t.Fatalf("age_seconds = %d, want 1200", status.AgeSeconds)
	}
}

func TestQuotaStatusCacheTTL(t *testing.T) {
	dir := t.TempDir()
	writeQuotaFile(t, filepath.Join(dir, "quota.json"), sampleSnapshotJSON(7, 36, time.Now().UnixMilli()))
	checker := newQuotaChecker(QuotaCheckerOptions{ConfigDir: dir})

	first := checker.Status(time.Now())
	writeQuotaFile(t, filepath.Join(dir, "quota.json"), sampleSnapshotJSON(99, 99, time.Now().UnixMilli()))

	// Inside the TTL the cached body is served unchanged.
	second := checker.Status(time.Now())
	if row, _ := quotaRowByID(second, "five_hour"); row.Percent != 7 {
		t.Fatalf("TTL window re-read the files: %+v (first was %+v)", second, first)
	}
	// ...and the status cache does not wedge the timestamp into the far future.
	if row, _ := quotaRowByID(checker.Status(time.Now().Add(quotaStatusTTL+time.Second)), "five_hour"); row.Percent != 99 {
		t.Fatalf("cache did not expire after the TTL: %+v", row)
	}
}

// --- the optional live poller ------------------------------------------------------

func quotaTestCredentials(t *testing.T, dir, token string, expiresAt time.Time) {
	t.Helper()
	creds := fmt.Sprintf(`{"claudeAiOauth": {"accessToken": %q, "expiresAt": %d}}`, token, expiresAt.UnixMilli())
	writeQuotaFile(t, filepath.Join(dir, ".credentials.json"), creds)
}

func TestQuotaPollerSuccessAndSecrets(t *testing.T) {
	dir := t.TempDir()
	const token = "sk-ant-oat-SECRET-TOKEN-VALUE"
	var sawAuth, sawBeta string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		sawBeta = r.Header.Get("anthropic-beta")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, sampleLivePayload())
	}))
	defer server.Close()

	var logLines []string
	checker := newQuotaChecker(QuotaCheckerOptions{
		ConfigDir:  dir,
		Poll:       true,
		UsageURL:   server.URL,
		HTTPClient: server.Client(),
		Logf: func(format string, args ...any) {
			logLines = append(logLines, fmt.Sprintf(format, args...))
		},
	})
	quotaTestCredentials(t, dir, token, time.Now().Add(time.Hour))

	if _, err := checker.pollOnce(); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	if sawAuth != "Bearer "+token || sawBeta != "oauth-2025-04-20" {
		t.Fatalf("usage request headers: auth=%q beta=%q", sawAuth, sawBeta)
	}

	status := checker.Status(time.Now())
	if status.Source != "live" {
		t.Fatalf("source = %q, want live", status.Source)
	}
	if _, ok := quotaRowByID(status, "weekly_fable"); !ok {
		t.Fatalf("live poll rows missing fable: %+v", status.Rows)
	}

	// The access token must never leak into the serialized status or a log line.
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), token) {
		t.Fatalf("access token leaked into serialized QuotaStatus: %s", encoded)
	}
	for _, line := range logLines {
		if strings.Contains(line, token) {
			t.Fatalf("access token leaked into a log line: %s", line)
		}
	}
}

func TestQuotaPoller429BacksOff(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":"rate limited"}`)
	}))
	defer server.Close()

	checker := newQuotaChecker(QuotaCheckerOptions{
		ConfigDir:  dir,
		Poll:       true,
		UsageURL:   server.URL,
		HTTPClient: server.Client(),
	})
	quotaTestCredentials(t, dir, "tok", time.Now().Add(time.Hour))

	backoff, err := checker.pollOnce()
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("429 err = %v, want rate limited", err)
	}
	if backoff != quotaPollBackoffOn429 {
		t.Fatalf("429 backoff = %v, want %v", backoff, quotaPollBackoffOn429)
	}
	// The failure is surfaced (the files keep serving) but no live rows exist.
	status := checker.Status(time.Now())
	if !strings.Contains(status.Error, "rate limited") {
		t.Fatalf("429 not surfaced in status: %+v", status)
	}
	if len(status.Rows) != 0 {
		t.Fatalf("429 host should have no rows: %+v", status.Rows)
	}
}

func TestQuotaPoller401ReportsTokenRejected(t *testing.T) {
	dir := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"unauthorized"}`)
	}))
	defer server.Close()

	checker := newQuotaChecker(QuotaCheckerOptions{
		ConfigDir:  dir,
		Poll:       true,
		UsageURL:   server.URL,
		HTTPClient: server.Client(),
	})
	quotaTestCredentials(t, dir, "tok", time.Now().Add(time.Hour))

	backoff, err := checker.pollOnce()
	if err == nil || !strings.Contains(err.Error(), "token rejected") {
		t.Fatalf("401 err = %v, want the token-rejected message", err)
	}
	if backoff != 0 {
		t.Fatalf("401 backoff = %v, want 0 (retry at the normal cadence)", backoff)
	}
	status := checker.Status(time.Now())
	if !strings.Contains(status.Error, "token rejected") || !strings.Contains(status.Error, "run any Claude Code session") {
		t.Fatalf("401 message = %q", status.Error)
	}
}

func TestQuotaPollerExpiredTokenNeverCallsOut(t *testing.T) {
	dir := t.TempDir()
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		fmt.Fprint(w, sampleLivePayload())
	}))
	defer server.Close()

	checker := newQuotaChecker(QuotaCheckerOptions{
		ConfigDir:  dir,
		Poll:       true,
		UsageURL:   server.URL,
		HTTPClient: server.Client(),
	})
	quotaTestCredentials(t, dir, "tok", time.Now().Add(-time.Hour))
	if _, err := checker.pollOnce(); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired token err = %v", err)
	}
	if called {
		t.Fatal("expired token must not reach the network")
	}

	quotaTestCredentials(t, dir, "", time.Now().Add(time.Hour))
	if _, err := checker.pollOnce(); err == nil || !strings.Contains(err.Error(), "no OAuth token") {
		t.Fatalf("missing token err = %v", err)
	}
}

func TestQuotaCheckerStartStopIdempotent(t *testing.T) {
	dir := t.TempDir()
	quotaTestCredentials(t, dir, "tok", time.Now().Add(time.Hour))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, sampleLivePayload())
	}))
	defer server.Close()

	checker := newQuotaChecker(QuotaCheckerOptions{
		ConfigDir:    dir,
		Poll:         true,
		UsageURL:     server.URL,
		HTTPClient:   server.Client(),
		Interval:     time.Hour, // parked after the startup delay; loop lifecycle is what is under test
		StartupDelay: time.Hour,
	})
	checker.Start()
	checker.Start() // idempotent
	checker.Stop()
	checker.Stop() // idempotent

	// A disabled or unconfigured checker never starts a loop.
	off := newQuotaChecker(QuotaCheckerOptions{ConfigDir: dir, Poll: false})
	off.Start()
	off.Stop()
	unconfigured := newQuotaChecker(QuotaCheckerOptions{ConfigDir: "", Poll: true})
	unconfigured.Start()
	unconfigured.Stop()
}

// --- HTTP route --------------------------------------------------------------------

func TestQuotaHandlerUnconfiguredReturnsWellFormedBody(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/quota", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/quota (unwired checker) = %d, want 200", rec.Code)
	}
	var status QuotaStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode unconfigured quota body: %v (%s)", err, rec.Body.String())
	}
	if status.Configured || status.Source != "none" || len(status.Rows) != 0 || status.Stale {
		t.Fatalf("unconfigured body = %+v", status)
	}
	// The pinned shape: the exact unconfigured body, with no zero-time noise
	// (the pointer field makes omitempty actually omit).
	if got := strings.TrimSpace(rec.Body.String()); got != `{"configured":false,"source":"none","rows":[],"stale":false}` {
		t.Fatalf("unconfigured body = %s", got)
	}
}

func TestQuotaHandlerServesWiredChecker(t *testing.T) {
	dir := t.TempDir()
	writeQuotaFile(t, filepath.Join(dir, "quota.json"), sampleSnapshotJSON(7, 36, time.Now().UnixMilli()))
	checker := newQuotaChecker(QuotaCheckerOptions{ConfigDir: dir})
	state := NewMemoryRuntimeState()
	state.SetQuotaChecker(checker)
	handler, err := newHTTPHandlerWithState(fakeCollector{}, testStaticFS(), state)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/quota", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/quota = %d: %s", rec.Code, rec.Body.String())
	}
	var status QuotaStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.Configured || status.Source != "snapshot" {
		t.Fatalf("wired quota body = %+v", status)
	}
}
