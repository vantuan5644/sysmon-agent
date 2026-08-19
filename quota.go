package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Claude Code quota surface (the fourth dashboard page).
//
// Data source is FILES-FIRST: the agent reads the two local files the
// claude-quota-monitor skill already maintains --
//
//   <config>/quota.json                    fresh 5h/weekly, written by the
//                                          status line on every render
//   <config>/widgets/quota-live-cache.json the full last-successful
//                                          api.anthropic.com/api/oauth/usage
//                                          payload (per-model weeklies like
//                                          Fable, usage credits)
//
// and merges them the way the desktop widget does. That reproduces exactly what
// the widget shows with zero new outbound traffic and zero OAuth token handling
// in Go. The usage endpoint rate-limits hard (once-a-minute reliably earns
// 429s), and the widget already spends the 5-minute slot on this machine; a
// second poller from the same NAT IP would fight it. A host with no widget (a
// Linux box) can opt into live polling with -claude-quota-poll.
//
// Invariant: the number of open dashboards never affects upstream call volume.
// The client polls the agent; the agent polls the filesystem (always) and the
// API (on its own timer, only when opted in). This is the same warm-sample rule
// the resident sampler enforces for metrics.

const (
	// quotaUsageURL is the endpoint Claude Code's own /usage command reads, so
	// it sees every bucket including per-model weeklies (Fable) and usage
	// credits. The token is read per-poll, sent only to this host, and is never
	// logged, persisted, or included in any HTTP response the agent serves.
	quotaUsageURL = "https://api.anthropic.com/api/oauth/usage"

	// quotaPollInterval matches the widget's cadence: quota moves slowly, and
	// polling the usage endpoint more often than 5 minutes earns 429s.
	quotaPollInterval = 5 * time.Minute

	// quotaPollBackoffOn429 pushes the next attempt out after the server asked
	// us to slow down. The endpoint is stingy; a 429 must not trigger an
	// immediate retry.
	quotaPollBackoffOn429 = 15 * time.Minute

	// quotaPollStartupDelay spaces the first poll away from agent start so a
	// boot-time network blip does not immediately flag the live path failed.
	quotaPollStartupDelay = 30 * time.Second

	// quotaRequestTimeout bounds one usage-endpoint call.
	quotaRequestTimeout = 10 * time.Second

	// quotaMaxPayloadBytes caps the usage response. Real payloads are a few
	// KB; 1 MB is a generous ceiling that still rejects a runaway response.
	quotaMaxPayloadBytes = 1 << 20

	// quotaStaleAfter is the age at which the freshest contributor is flagged
	// stale in the dashboard (the status line refreshes quota.json every few
	// seconds; the widget/agent poll every 5 minutes, so 15 minutes means
	// something actually stopped).
	quotaStaleAfter = 15 * time.Minute

	// quotaStatusTTL caches the merged /api/quota body so a dashboard refresh
	// loop does not stat + parse the two files on every request.
	quotaStatusTTL = 5 * time.Second
)

// resolveClaudeConfigDir returns the directory holding quota.json and
// widgets/quota-live-cache.json, or "" when the feature is not configured.
// Precedence:
//  1. explicit -claude-config-dir / SYSMON_CLAUDE_CONFIG_DIR
//  2. CLAUDE_CONFIG_DIR (what Claude Code itself honors)
//  3. $HOME/.claude
//
// A path that does not exist resolves to "" -> feature off, page hidden.
//
// The LocalSystem trap: the Windows service runs as LocalSystem, whose $HOME is
// C:\Windows\System32\config\systemprofile -- step 3 can never find the
// interactive user's config there. On a Windows service install the flag is
// effectively mandatory (install-windows.ps1 passes it).
func resolveClaudeConfigDir(explicit string) string {
	return resolveClaudeConfigDirFrom(explicit, os.Getenv, homeDir)
}

func resolveClaudeConfigDirFrom(explicit string, getenv func(string) string, home func() string) string {
	for _, candidate := range []string{
		strings.TrimSpace(explicit),
		strings.TrimSpace(getenv("SYSMON_CLAUDE_CONFIG_DIR")),
		strings.TrimSpace(getenv("CLAUDE_CONFIG_DIR")),
	} {
		if candidate != "" {
			return existingDir(candidate)
		}
	}
	if h := strings.TrimSpace(home()); h != "" {
		return existingDir(filepath.Join(h, ".claude"))
	}
	return ""
}

func existingDir(path string) string {
	if path == "" {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return ""
	}
	return path
}

func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}

// QuotaRow is one usage window on the dashboard page.
type QuotaRow struct {
	// ID is a stable slug ("five_hour", "seven_day", "weekly_fable", "credits")
	// used to merge rows across sources.
	ID       string  `json:"id"`
	Label    string  `json:"label"`
	Percent  float64 `json:"percent"`
	ResetsAt string  `json:"resets_at,omitempty"` // RFC3339, "" when absent
	// Note carries auxiliary text under the bar: the credits amount
	// ("$0.00 of $50.00 used") and, for rows carried over from a stale live
	// cache, their own sample age ("as of 4m ago") so a stale Fable row is
	// shown as stale rather than dropped.
	Note string `json:"note,omitempty"`
}

// QuotaStatus is the /api/quota body. It is always well-formed, even on a host
// with no Claude config: configured:false is the "feature off" state the
// dashboard latches on to hide the page.
type QuotaStatus struct {
	Configured bool       `json:"configured"`
	Source     string     `json:"source"` // "live" | "cache" | "snapshot" | "none"
	Rows       []QuotaRow `json:"rows"`
	// FetchedAt / AgeSeconds describe the FRESHEST contributor (the snapshot
	// once the status line has written it; otherwise the live cache or the
	// agent's own last successful poll). Per-model and credit rows keep the
	// cache's timestamp in their Note instead, so each row degrades
	// independently -- the same per-field degradation invariant the metrics
	// collectors follow. A pointer (not a bare time.Time) so `omitempty`
	// actually omits the zero value: an unconfigured host serves exactly
	// {"configured":false,"source":"none","rows":[],"stale":false}.
	FetchedAt  *time.Time `json:"fetched_at,omitempty"`
	AgeSeconds int        `json:"age_seconds,omitempty"`
	Stale      bool       `json:"stale"`
	Error      string     `json:"error,omitempty"`
}

// --- snapshot file: <config>/quota.json --------------------------------------
//
// Written by the skill's statusline.js on every render. Windows carry unix
// epoch SECONDS (the live API uses RFC3339 strings); updated_at is epoch MS.

type quotaSnapshotWindow struct {
	UsedPercentage *float64 `json:"used_percentage"`
	ResetsAtSecs   *float64 `json:"resets_at"`
}

type quotaSnapshotScoped struct {
	DisplayName  string   `json:"display_name"`
	Utilization  *float64 `json:"utilization"`
	ResetsAtSecs *float64 `json:"resets_at"`
}

type quotaSnapshotFile struct {
	UpdatedAtMS    *float64              `json:"updated_at"`
	FiveHour       *quotaSnapshotWindow  `json:"five_hour"`
	SevenDay       *quotaSnapshotWindow  `json:"seven_day"`
	SevenDayOpus   *quotaSnapshotWindow  `json:"seven_day_opus"`
	SevenDaySonnet *quotaSnapshotWindow  `json:"seven_day_sonnet"`
	ModelScoped    []quotaSnapshotScoped `json:"model_scoped"`
}

// --- live payload: api.anthropic.com/api/oauth/usage --------------------------
//
// Also the shape cached in widgets/quota-live-cache.json under a "data" key with
// an epoch-MS "fetched_at" beside it.

type quotaLiveWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

type quotaLiveLimitModel struct {
	DisplayName string `json:"display_name"`
}

type quotaLiveLimitScope struct {
	Model *quotaLiveLimitModel `json:"model"`
}

type quotaLiveLimit struct {
	Percent  *float64             `json:"percent"`
	ResetsAt string               `json:"resets_at"`
	Scope    *quotaLiveLimitScope `json:"scope"`
}

// quotaSpend is the CLI's credits shape:
// {enabled, percent, used:{amount_minor,exponent}, limit:{...}}.
type quotaSpend struct {
	Enabled bool        `json:"enabled"`
	Percent *float64    `json:"percent"`
	Used    *quotaMoney `json:"used"`
	Limit   *quotaMoney `json:"limit"`
}

type quotaMoney struct {
	AmountMinor float64 `json:"amount_minor"`
	Exponent    int     `json:"exponent"`
}

// quotaExtraUsage is the shape the cached payload carries instead:
// {is_enabled, utilization, used_credits, monthly_limit, currency,
// decimal_places}. Both must be handled; spend is preferred when both parse.
type quotaExtraUsage struct {
	IsEnabled     bool     `json:"is_enabled"`
	Utilization   *float64 `json:"utilization"`
	UsedCredits   float64  `json:"used_credits"`
	MonthlyLimit  float64  `json:"monthly_limit"`
	Currency      string   `json:"currency"`
	DecimalPlaces int      `json:"decimal_places"`
}

type quotaLivePayload struct {
	FiveHour       *quotaLiveWindow `json:"five_hour"`
	SevenDay       *quotaLiveWindow `json:"seven_day"`
	SevenDayOpus   *quotaLiveWindow `json:"seven_day_opus"`
	SevenDaySonnet *quotaLiveWindow `json:"seven_day_sonnet"`
	CinderCove     *quotaLiveWindow `json:"cinder_cove"`
	ExtraUsage     *quotaExtraUsage `json:"extra_usage"`
	Limits         []quotaLiveLimit `json:"limits"`
	Spend          *quotaSpend      `json:"spend"`
}

type quotaLiveCacheFile struct {
	FetchedAtMS *float64         `json:"fetched_at"`
	Data        quotaLivePayload `json:"data"`
}

// --- row construction ---------------------------------------------------------

// quotaNamedWindows is the fixed order the CLI, the widget and this page all
// agree on for the named windows.
var quotaNamedWindows = []struct {
	id    string
	label string
}{
	{"five_hour", "Session (5h)"},
	{"seven_day", "Weekly (all)"},
	{"seven_day_opus", "Weekly (Opus)"},
	{"seven_day_sonnet", "Weekly (Sonnet)"},
	{"cinder_cove", "Code + Cowork credit"},
}

// quotaRowsFromLivePayload mirrors quota-cli.js rowsFromLive /
// quota-widget.ps1 Get-RowsFromLive so all four surfaces agree. Every limits[]
// entry with a scope.model.display_name becomes its own row -- this is how the
// per-model weeklies (Fable) arrive, and it means a NEW per-model bucket
// appears on the dashboard with no code change here.
func quotaRowsFromLivePayload(payload quotaLivePayload) []QuotaRow {
	var rows []QuotaRow
	for _, named := range quotaNamedWindows {
		var window *quotaLiveWindow
		switch named.id {
		case "five_hour":
			window = payload.FiveHour
		case "seven_day":
			window = payload.SevenDay
		case "seven_day_opus":
			window = payload.SevenDayOpus
		case "seven_day_sonnet":
			window = payload.SevenDaySonnet
		case "cinder_cove":
			window = payload.CinderCove
		}
		if window == nil || window.Utilization == nil {
			continue
		}
		rows = append(rows, QuotaRow{
			ID:       named.id,
			Label:    named.label,
			Percent:  *window.Utilization,
			ResetsAt: window.ResetsAt,
		})
	}
	for _, limit := range payload.Limits {
		name := strings.TrimSpace(limit.Scope.ModelDisplayName())
		if name == "" || limit.Percent == nil {
			continue
		}
		rows = append(rows, QuotaRow{
			ID:       quotaWeeklyRowID(name),
			Label:    "Weekly (" + name + ")",
			Percent:  *limit.Percent,
			ResetsAt: limit.ResetsAt,
		})
	}
	if row, ok := quotaCreditsRow(payload.Spend, payload.ExtraUsage); ok {
		rows = append(rows, row)
	}
	return rows
}

func (s *quotaLiveLimitScope) ModelDisplayName() string {
	if s == nil || s.Model == nil {
		return ""
	}
	return s.Model.DisplayName
}

// quotaWeeklyRowID turns a display name into the stable merge slug:
// "Fable" -> "weekly_fable", "Code + Cowork" -> "weekly_code_cowork".
func quotaWeeklyRowID(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return "weekly_" + strings.Trim(b.String(), "_")
}

// quotaCreditsRow builds the usage-credits row from whichever shape is
// present. The spend shape (what the CLI reads) is preferred when both parse;
// the cached payload on some hosts carries extra_usage instead. A disabled
// credits pot yields no row at all -- mirroring the CLI -- rather than a
// misleading 0%.
func quotaCreditsRow(spend *quotaSpend, extra *quotaExtraUsage) (QuotaRow, bool) {
	if spend != nil && spend.Enabled && spend.Percent != nil {
		row := QuotaRow{ID: "credits", Label: "Usage credits", Percent: *spend.Percent}
		if note, ok := quotaSpendNote(spend); ok {
			row.Note = note
		}
		return row, true
	}
	if extra != nil && extra.IsEnabled && extra.Utilization != nil {
		row := QuotaRow{ID: "credits", Label: "Usage credits", Percent: *extra.Utilization}
		places := extra.DecimalPlaces
		if places < 0 || places > 4 {
			places = 2
		}
		// used_credits/monthly_limit are minor units scaled by 10^decimal_places.
		scale := 1.0
		for i := 0; i < places; i++ {
			scale *= 10
		}
		symbol := quotaCurrencySymbol(extra.Currency)
		row.Note = fmt.Sprintf("%s%.*f of %s%.*f used",
			symbol, places, extra.UsedCredits/scale,
			symbol, places, extra.MonthlyLimit/scale)
		return row, true
	}
	return QuotaRow{}, false
}

func quotaSpendNote(spend *quotaSpend) (string, bool) {
	if spend.Used == nil || spend.Limit == nil {
		return "", false
	}
	places := spend.Used.Exponent
	if places < 0 || places > 4 {
		places = 2
	}
	used := spend.Used.AmountMinor
	limit := spend.Limit.AmountMinor
	if spend.Used.Exponent != spend.Limit.Exponent {
		// Normalize both to the used exponent so the pair shares one unit.
		limit = rescaleMinor(limit, spend.Limit.Exponent, spend.Used.Exponent)
	}
	return fmt.Sprintf("$%.*f of $%.*f used", places, minorToUnits(used, places), places, minorToUnits(limit, places)), true
}

func minorToUnits(amountMinor float64, exponent int) float64 {
	scale := 1.0
	for i := 0; i < exponent; i++ {
		scale *= 10
	}
	return amountMinor / scale
}

func rescaleMinor(amountMinor float64, from, to int) float64 {
	if from == to {
		return amountMinor
	}
	if from > to {
		for i := 0; i < from-to; i++ {
			amountMinor /= 10
		}
		return amountMinor
	}
	for i := 0; i < to-from; i++ {
		amountMinor *= 10
	}
	return amountMinor
}

func quotaCurrencySymbol(currency string) string {
	switch strings.ToUpper(strings.TrimSpace(currency)) {
	case "USD", "":
		return "$"
	case "EUR":
		return "€"
	case "GBP":
		return "£"
	default:
		return strings.ToUpper(strings.TrimSpace(currency)) + " "
	}
}

// quotaRowsFromSnapshot mirrors quota-cli.js rowsFromSnapshot. cinder_cove and
// credits never appear in the snapshot (the status line does not see them), so
// they are not read here.
func quotaRowsFromSnapshot(file quotaSnapshotFile) []QuotaRow {
	var rows []QuotaRow
	for _, named := range quotaNamedWindows {
		var window *quotaSnapshotWindow
		switch named.id {
		case "five_hour":
			window = file.FiveHour
		case "seven_day":
			window = file.SevenDay
		case "seven_day_opus":
			window = file.SevenDayOpus
		case "seven_day_sonnet":
			window = file.SevenDaySonnet
		}
		if window == nil || window.UsedPercentage == nil {
			continue
		}
		rows = append(rows, QuotaRow{
			ID:       named.id,
			Label:    named.label,
			Percent:  *window.UsedPercentage,
			ResetsAt: quotaResetFromEpochSeconds(window.ResetsAtSecs),
		})
	}
	for _, scoped := range file.ModelScoped {
		name := strings.TrimSpace(scoped.DisplayName)
		if name == "" || scoped.Utilization == nil {
			continue
		}
		rows = append(rows, QuotaRow{
			ID:       quotaWeeklyRowID(name),
			Label:    "Weekly (" + name + ")",
			Percent:  *scoped.Utilization,
			ResetsAt: quotaResetFromEpochSeconds(scoped.ResetsAtSecs),
		})
	}
	return rows
}

// quotaResetFromEpochSeconds converts the snapshot's unix-seconds reset
// timestamp into the RFC3339 form the live payload (and QuotaRow) uses.
func quotaResetFromEpochSeconds(secs *float64) string {
	if secs == nil || *secs <= 0 {
		return ""
	}
	seconds := int64(*secs)
	nanos := int64((*secs - float64(seconds)) * 1e9)
	if nanos < 0 || nanos >= int64(time.Second) {
		nanos = 0
	}
	return time.Unix(seconds, nanos).UTC().Format(time.RFC3339)
}

// --- file reading --------------------------------------------------------------

func readQuotaSnapshot(configDir string) ([]QuotaRow, time.Time, error) {
	data, err := os.ReadFile(filepath.Join(configDir, "quota.json"))
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("quota.json: %w", err)
	}
	rows, at, err := parseQuotaSnapshot(data)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("quota.json: %w", err)
	}
	return rows, at, nil
}

func parseQuotaSnapshot(data []byte) ([]QuotaRow, time.Time, error) {
	var file quotaSnapshotFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, time.Time{}, err
	}
	rows := quotaRowsFromSnapshot(file)
	at := quotaEpochMSTime(file.UpdatedAtMS)
	if at.IsZero() && len(rows) > 0 {
		return nil, time.Time{}, errors.New("snapshot has rows but no updated_at")
	}
	return rows, at, nil
}

func readQuotaLiveCache(configDir string) ([]QuotaRow, time.Time, error) {
	data, err := os.ReadFile(filepath.Join(configDir, "widgets", "quota-live-cache.json"))
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("quota-live-cache.json: %w", err)
	}
	rows, at, err := parseQuotaLiveCache(data)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("quota-live-cache.json: %w", err)
	}
	return rows, at, nil
}

func parseQuotaLiveCache(data []byte) ([]QuotaRow, time.Time, error) {
	var file quotaLiveCacheFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, time.Time{}, err
	}
	rows := quotaRowsFromLivePayload(file.Data)
	at := quotaEpochMSTime(file.FetchedAtMS)
	if at.IsZero() && len(rows) > 0 {
		return nil, time.Time{}, errors.New("cache has rows but no fetched_at")
	}
	return rows, at, nil
}

func quotaEpochMSTime(ms *float64) time.Time {
	if ms == nil || *ms <= 0 {
		return time.Time{}
	}
	whole := int64(*ms)
	nanos := int64((*ms - float64(whole)) * 1e6)
	if nanos < 0 || nanos >= int64(time.Millisecond) {
		nanos = 0
	}
	return time.UnixMilli(whole).Add(time.Duration(nanos) * time.Nanosecond)
}

// --- the checker ----------------------------------------------------------------

// QuotaChecker resolves the merged quota status from the local files and,
// when -claude-quota-poll is set, its own live polls. It is modelled directly
// on UpdateChecker: same Start/Stop idempotent shape, injectable httpClient and
// logf, cached last result under an RWMutex, serve-path-only lifecycle.
type QuotaChecker struct {
	configDir    string
	poll         bool
	interval     time.Duration
	startupDelay time.Duration
	usageURL     string
	httpClient   *http.Client
	logf         func(format string, args ...any)

	mu sync.RWMutex
	// liveRows/liveAt cache the last SUCCESSFUL poll (source "live").
	liveRows []QuotaRow
	liveAt   time.Time
	// liveErr is the last poll failure reason, surfaced in Error while the
	// file sources keep serving rows ("token rejected -- run any Claude Code
	// session" etc.).
	liveErr string
	// statusCache/statusCacheAt implement the short /api/quota TTL so a
	// refresh loop does not re-read the files per request.
	statusCache   QuotaStatus
	statusCacheAt time.Time

	stop    chan struct{}
	stopped chan struct{}
	started bool
}

// QuotaCheckerOptions captures the inputs main.go resolves once.
type QuotaCheckerOptions struct {
	// ConfigDir is the resolved Claude config directory ("" = feature off).
	ConfigDir string
	// Poll enables the optional live api.anthropic.com usage poller.
	Poll bool
	// UsageURL overrides quotaUsageURL (tests).
	UsageURL string
	// Interval overrides quotaPollInterval (tests).
	Interval time.Duration
	// StartupDelay overrides quotaPollStartupDelay (tests).
	StartupDelay time.Duration
	HTTPClient   *http.Client
	Logf         func(format string, args ...any)
}

func newQuotaChecker(opts QuotaCheckerOptions) *QuotaChecker {
	if opts.UsageURL == "" {
		opts.UsageURL = quotaUsageURL
	}
	if opts.Interval <= 0 {
		opts.Interval = quotaPollInterval
	}
	if opts.StartupDelay < 0 {
		opts.StartupDelay = quotaPollStartupDelay
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: quotaRequestTimeout}
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	return &QuotaChecker{
		configDir:    opts.ConfigDir,
		poll:         opts.Poll,
		interval:     opts.Interval,
		startupDelay: opts.StartupDelay,
		usageURL:     opts.UsageURL,
		httpClient:   opts.HTTPClient,
		logf:         opts.Logf,
	}
}

// Start launches the optional live poll loop. It is safe to call when polling
// is disabled or the feature is unconfigured: the loop parks without making
// any network calls. Start is idempotent. It must never run under -self-check.
func (c *QuotaChecker) Start() {
	c.mu.Lock()
	if c.started || !c.poll || c.configDir == "" {
		c.mu.Unlock()
		return
	}
	c.started = true
	c.stop = make(chan struct{})
	c.stopped = make(chan struct{})
	c.mu.Unlock()

	go c.loop()
}

// Stop signals the loop to exit and blocks until it has.
func (c *QuotaChecker) Stop() {
	c.mu.Lock()
	if !c.started {
		c.mu.Unlock()
		return
	}
	stop := c.stop
	stopped := c.stopped
	c.started = false
	c.mu.Unlock()

	close(stop)
	<-stopped
}

// Status returns the merged quota status, cached for quotaStatusTTL so a
// dashboard refresh loop does not stat the files on every request.
func (c *QuotaChecker) Status(now time.Time) QuotaStatus {
	if now.IsZero() {
		now = time.Now()
	}
	c.mu.RLock()
	cached, cachedAt := c.statusCache, c.statusCacheAt
	c.mu.RUnlock()
	if !cachedAt.IsZero() && now.Sub(cachedAt) < quotaStatusTTL {
		return cached
	}
	status := c.computeStatus(now)
	c.mu.Lock()
	c.statusCache, c.statusCacheAt = status, now
	c.mu.Unlock()
	return status
}

// computeStatus merges every contributor. Missing, unreadable or corrupt files
// are never a Go error: they yield rows from whichever source did parse, with a
// populated Error explaining the rest.
func (c *QuotaChecker) computeStatus(now time.Time) QuotaStatus {
	if c.configDir == "" {
		return QuotaStatus{Configured: false, Source: "none", Rows: []QuotaRow{}}
	}
	status := QuotaStatus{Configured: true, Source: "none", Rows: []QuotaRow{}}

	c.mu.RLock()
	liveRows, liveAt, liveErr := c.liveRows, c.liveAt, c.liveErr
	c.mu.RUnlock()
	if liveErr != "" {
		status.Error = "live: " + liveErr
	}

	cacheRows, cacheAt, cacheErr := readQuotaLiveCache(c.configDir)
	var cacheErrText string
	if cacheErr != nil && !errors.Is(cacheErr, os.ErrNotExist) {
		cacheErrText = cacheErr.Error()
	}
	snapRows, snapAt, snapErr := readQuotaSnapshot(c.configDir)
	var snapErrText string
	if snapErr != nil && !errors.Is(snapErr, os.ErrNotExist) {
		snapErrText = snapErr.Error()
	}

	// The base is the freshest API-shaped contributor (the agent's own live
	// poll or the widget's cache); the snapshot then overlays its fresher
	// 5h/weekly windows on top.
	baseRows, baseAt, baseSource := liveRows, liveAt, "live"
	if len(cacheRows) > 0 && (baseAt.IsZero() || cacheAt.After(baseAt)) {
		baseRows, baseAt, baseSource = cacheRows, cacheAt, "cache"
	}

	rowMap := make(map[string]QuotaRow, len(baseRows))
	baseIDs := make(map[string]bool, len(baseRows))
	for _, row := range baseRows {
		rowMap[row.ID] = row
		baseIDs[row.ID] = true
	}
	fetchedAt := baseAt
	source := baseSource
	if len(baseRows) > 0 {
		status.Source = source
	}

	overlaid := make(map[string]bool)
	if len(snapRows) > 0 {
		if baseAt.IsZero() || snapAt.After(baseAt) {
			// The snapshot is the freshest contributor: its 5h/weekly rows
			// override the base's, and it owns the reported timestamp.
			for _, row := range snapRows {
				rowMap[row.ID] = row
				overlaid[row.ID] = true
			}
			fetchedAt = snapAt
			status.Source = "snapshot"
			// Rows only the API shape knows (per-model weeklies, credits)
			// survive from the base -- annotated with their own sample age so
			// a stale Fable row is shown as stale rather than dropped.
			for id := range baseIDs {
				if overlaid[id] {
					continue
				}
				row := rowMap[id]
				row.Note = quotaAppendNote(row.Note, "as of "+formatQuotaAge(now.Sub(baseAt)))
				rowMap[id] = row
			}
		} else if len(baseRows) == 0 {
			for _, row := range snapRows {
				rowMap[row.ID] = row
			}
			fetchedAt = snapAt
			status.Source = "snapshot"
		}
	}

	status.Rows = orderQuotaRows(rowMap)
	if !fetchedAt.IsZero() && !fetchedAt.After(now) {
		fetched := fetchedAt.UTC()
		status.FetchedAt = &fetched
		status.AgeSeconds = int(now.Sub(fetchedAt).Seconds())
		status.Stale = now.Sub(fetchedAt) > quotaStaleAfter
	}

	var problems []string
	if status.Error != "" {
		problems = append(problems, status.Error)
	}
	if cacheErrText != "" {
		problems = append(problems, cacheErrText)
	}
	if snapErrText != "" {
		problems = append(problems, snapErrText)
	}
	if len(status.Rows) == 0 && len(problems) == 0 {
		problems = append(problems, "no quota data yet; run a Claude Code session so the status line writes quota.json")
	}
	status.Error = strings.Join(problems, "; ")
	return status
}

// orderQuotaRows renders the map in the canonical order all four surfaces use:
// the fixed named windows, then per-model weeklies alphabetically, credits last.
func orderQuotaRows(rowMap map[string]QuotaRow) []QuotaRow {
	rows := make([]QuotaRow, 0, len(rowMap))
	for _, named := range quotaNamedWindows {
		if row, ok := rowMap[named.id]; ok {
			rows = append(rows, row)
		}
	}
	var weekly []QuotaRow
	for id, row := range rowMap {
		if strings.HasPrefix(id, "weekly_") {
			weekly = append(weekly, row)
		}
	}
	sort.Slice(weekly, func(i, j int) bool {
		if weekly[i].Label == weekly[j].Label {
			return weekly[i].ID < weekly[j].ID
		}
		return weekly[i].Label < weekly[j].Label
	})
	rows = append(rows, weekly...)
	if row, ok := rowMap["credits"]; ok {
		rows = append(rows, row)
	}
	return rows
}

func quotaAppendNote(note, extra string) string {
	if strings.TrimSpace(note) == "" {
		return extra
	}
	return note + " · " + extra
}

// formatQuotaAge renders a duration the way the widget's Format-Age does:
// "just now", "5m ago", "3h ago", "2d ago".
func formatQuotaAge(age time.Duration) string {
	if age < 0 {
		age = 0
	}
	minutes := int(age / time.Minute)
	switch {
	case minutes < 1:
		return "just now"
	case minutes < 60:
		return fmt.Sprintf("%dm ago", minutes)
	case minutes < 24*60:
		return fmt.Sprintf("%dh ago", minutes/60)
	default:
		return fmt.Sprintf("%dd ago", minutes/(24*60))
	}
}

// --- the optional live poller ----------------------------------------------------

func (c *QuotaChecker) loop() {
	defer close(c.stopped)
	select {
	case <-c.stop:
		return
	case <-time.After(c.startupDelay):
	}
	for {
		backoff, err := c.pollOnce()
		if err != nil {
			c.logf("claude quota poll failed: %v", err)
		}
		wait := c.interval
		if backoff > 0 {
			wait = backoff
		}
		select {
		case <-c.stop:
			return
		case <-time.After(wait):
		}
	}
}

// pollOnce performs one usage-endpoint poll. It returns a backoff duration to
// honour (non-zero only after a 429) and the failure, if any. The access token
// is read per-poll, sent only to the usage host, and never logged, persisted,
// or included in any HTTP response the agent itself serves.
func (c *QuotaChecker) pollOnce() (time.Duration, error) {
	token, err := readQuotaAccessToken(c.configDir)
	if err != nil {
		c.cacheLiveError(err.Error())
		return 0, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), quotaRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.usageURL, nil)
	if err != nil {
		c.cacheLiveError(err.Error())
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.cacheLiveError(err.Error())
		return 0, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		err := errors.New("rate limited")
		c.cacheLiveError(err.Error())
		return quotaPollBackoffOn429, err
	case resp.StatusCode == http.StatusUnauthorized:
		err := errors.New("token rejected — run any Claude Code session")
		c.cacheLiveError(err.Error())
		return 0, err
	case resp.StatusCode != http.StatusOK:
		err := fmt.Errorf("usage endpoint returned HTTP %d", resp.StatusCode)
		c.cacheLiveError(err.Error())
		return 0, err
	}

	var payload quotaLivePayload
	if err := json.NewDecoder(io.LimitReader(resp.Body, quotaMaxPayloadBytes)).Decode(&payload); err != nil {
		err = fmt.Errorf("decode usage payload: %w", err)
		c.cacheLiveError(err.Error())
		return 0, err
	}
	rows := quotaRowsFromLivePayload(payload)
	if len(rows) == 0 {
		err := errors.New("usage payload carried no windows")
		c.cacheLiveError(err.Error())
		return 0, err
	}
	c.mu.Lock()
	c.liveRows, c.liveAt, c.liveErr = rows, time.Now().UTC(), ""
	c.mu.Unlock()
	return 0, nil
}

func (c *QuotaChecker) cacheLiveError(reason string) {
	c.mu.Lock()
	c.liveErr = reason
	c.mu.Unlock()
}

// readQuotaAccessToken reads the OAuth access token Claude Code itself stores.
// It is re-read on every poll so a CLI refresh is picked up. expiresAt is
// epoch milliseconds (the same field the widget checks).
func readQuotaAccessToken(configDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(configDir, ".credentials.json"))
	if err != nil {
		return "", fmt.Errorf("cannot read credentials: %w", err)
	}
	var credentials struct {
		ClaudeAiOauth *struct {
			AccessToken string   `json:"accessToken"`
			ExpiresAtMS *float64 `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &credentials); err != nil {
		return "", fmt.Errorf("cannot parse credentials: %w", err)
	}
	if credentials.ClaudeAiOauth == nil || strings.TrimSpace(credentials.ClaudeAiOauth.AccessToken) == "" {
		return "", errors.New("no OAuth token (API key or 3P provider?)")
	}
	if expires := quotaEpochMSTime(credentials.ClaudeAiOauth.ExpiresAtMS); !expires.IsZero() && time.Now().After(expires) {
		return "", errors.New("token expired — run any Claude Code session")
	}
	return credentials.ClaudeAiOauth.AccessToken, nil
}
