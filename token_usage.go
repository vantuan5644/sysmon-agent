package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const tokenUsageDays = 7
const tokenUsageRefreshInterval = 30 * time.Second

// TokenUsageDay is one local-calendar-day bucket in the rolling usage chart.
// Date deliberately has no timezone: the server has already assigned every
// event to the host's local day before serializing it.
type TokenUsageDay struct {
	Date   string `json:"date"`
	Tokens int64  `json:"tokens"`
}

func emptyTokenUsageDays(now time.Time) ([]TokenUsageDay, time.Time, time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	localNow := now.In(time.Local)
	today := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, time.Local)
	start := today.AddDate(0, 0, -(tokenUsageDays - 1))
	days := make([]TokenUsageDay, 0, tokenUsageDays)
	for i := 0; i < tokenUsageDays; i++ {
		days = append(days, TokenUsageDay{Date: start.AddDate(0, 0, i).Format("2006-01-02")})
	}
	return days, start, today.AddDate(0, 0, 1)
}

func tokenDayIndex(days []TokenUsageDay) map[string]int {
	index := make(map[string]int, len(days))
	for i := range days {
		index[days[i].Date] = i
	}
	return index
}

// recentJSONLFiles walks only the provider-owned transcript tree and filters
// by mtime before opening a file. A session that started before the chart range
// but is still active is included because its append-only transcript is fresh.
func recentJSONLFiles(root string, since time.Time) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".jsonl") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.ModTime().Before(since) {
			paths = append(paths, path)
		}
		return nil
	})
	sort.Strings(paths)
	return paths, err
}

type claudeTranscriptEvent struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	RequestID string `json:"requestId"`
	UUID      string `json:"uuid"`
	Message   struct {
		ID    string `json:"id"`
		Role  string `json:"role"`
		Usage *struct {
			InputTokens         int64 `json:"input_tokens"`
			CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadTokens     int64 `json:"cache_read_input_tokens"`
			OutputTokens        int64 `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// scanClaudeTokenUsage sums one usage record per Claude API response. Claude's
// transcript repeats the same response as successive thinking/tool/text blocks,
// so requestId (then message id/uuid) is the deduplication boundary. It reads
// every file from the start; the resident sampler keeps a claudeUsageScanner
// instead, which reads only what each pass appended.
func scanClaudeTokenUsage(configDir string, now time.Time) ([]TokenUsageDay, time.Time, error) {
	var scanner claudeUsageScanner
	return scanner.scan(configDir, now)
}

// claudeUsageRecord is one assistant usage line, kept so later passes can
// re-aggregate without re-reading the transcript.
type claudeUsageRecord struct {
	key   string
	at    time.Time
	total int64
}

type claudeTranscriptState struct {
	cursor  transcriptCursor
	records []claudeUsageRecord
	// firstSeen drops a repeat of a key on the same local day while reading.
	// A repeat can never count: the earlier copy is in the same day bucket, so
	// it either counted already or lies outside the window just the same.
	firstSeen map[claudeRecordKey]struct{}
}

type claudeRecordKey struct{ key, date string }

func (s *claudeTranscriptState) reset() {
	s.records = nil
	s.firstSeen = nil
}

// dropBefore forgets records the window has passed. A full scan never let
// them count or mark a request seen, so the result does not change.
func (s *claudeTranscriptState) dropBefore(start time.Time) {
	kept := s.records[:0]
	for _, record := range s.records {
		if !record.at.Before(start) {
			kept = append(kept, record)
		}
	}
	if len(kept) == len(s.records) {
		return
	}
	clear(s.records[len(kept):]) // release the dropped keys
	s.records = kept
	first := start.Format("2006-01-02")
	for id := range s.firstSeen {
		if id.date < first {
			delete(s.firstSeen, id)
		}
	}
}

var claudeUsageMarker = []byte(`"usage"`)

func (s *claudeTranscriptState) add(line []byte) {
	if !bytes.Contains(line, claudeUsageMarker) {
		return
	}
	var event claudeTranscriptEvent
	if err := json.Unmarshal(line, &event); err != nil {
		return
	}
	if event.Type != "assistant" || event.Message.Role != "assistant" || event.Message.Usage == nil {
		return
	}
	at, err := time.Parse(time.RFC3339Nano, event.Timestamp)
	if err != nil {
		return
	}
	at = at.In(time.Local)
	key := strings.TrimSpace(event.RequestID)
	if key == "" {
		key = strings.TrimSpace(event.Message.ID)
	}
	if key == "" {
		key = strings.TrimSpace(event.UUID)
	}
	if key != "" {
		id := claudeRecordKey{key: key, date: at.Format("2006-01-02")}
		if _, dup := s.firstSeen[id]; dup {
			return
		}
		if s.firstSeen == nil {
			s.firstSeen = make(map[claudeRecordKey]struct{})
		}
		s.firstSeen[id] = struct{}{}
	}
	usage := event.Message.Usage
	s.records = append(s.records, claudeUsageRecord{
		key: key,
		at:  at,
		total: sumNonNegativeTokens(
			usage.InputTokens,
			usage.CacheCreationTokens,
			usage.CacheReadTokens,
			usage.OutputTokens,
		),
	})
}

// claudeUsageScanner keeps each transcript's records between passes, so a
// pass reads only the lines appended since the last one. Aggregation runs over
// the kept records every pass in path order, which is what makes the result
// the same as a full rescan: the first copy of a request in that order counts.
type claudeUsageScanner struct {
	mu    sync.Mutex
	files map[string]*claudeTranscriptState
}

func (s *claudeUsageScanner) scan(configDir string, now time.Time) ([]TokenUsageDay, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	days, start, end := emptyTokenUsageDays(now)
	paths, err := recentJSONLFiles(filepath.Join(configDir, "projects"), start)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.files = nil
			return days, time.Time{}, nil
		}
		return days, time.Time{}, fmt.Errorf("Claude transcripts: %w", err)
	}
	if s.files == nil {
		s.files = make(map[string]*claudeTranscriptState)
	}
	pruneTranscriptStates(s.files, paths)
	for _, path := range paths {
		state := s.files[path]
		if state == nil {
			state = &claudeTranscriptState{}
			s.files[path] = state
		}
		if err := readNewTranscriptLines(path, &state.cursor, state.reset, state.add); err != nil {
			return days, time.Time{}, fmt.Errorf("Claude transcript %s: %w", filepath.Base(path), err)
		}
	}
	index := tokenDayIndex(days)
	seen := make(map[string]struct{})
	var newest time.Time
	for _, path := range paths {
		state := s.files[path]
		state.dropBefore(start)
		for _, record := range state.records {
			if !record.at.Before(end) {
				continue
			}
			if record.key != "" {
				if _, ok := seen[record.key]; ok {
					continue
				}
				seen[record.key] = struct{}{}
			}
			if record.total == 0 {
				continue
			}
			if i, ok := index[record.at.Format("2006-01-02")]; ok {
				days[i].Tokens = saturatingTokenAdd(days[i].Tokens, record.total)
			}
			if record.at.After(newest) {
				newest = record.at
			}
		}
	}
	return days, newest, nil
}

// pruneTranscriptStates forgets files that left the window or were deleted.
func pruneTranscriptStates[T any](files map[string]T, current []string) {
	keep := make(map[string]struct{}, len(current))
	for _, path := range current {
		keep[path] = struct{}{}
	}
	for path := range files {
		if _, ok := keep[path]; !ok {
			delete(files, path)
		}
	}
}

type codexRateWindow struct {
	UsedPercent  *float64 `json:"used_percent"`
	WindowMinute int64    `json:"window_minutes"`
	ResetsAt     int64    `json:"resets_at"`
}

type codexRateLimits struct {
	Primary   *codexRateWindow `json:"primary"`
	Secondary *codexRateWindow `json:"secondary"`
}

type codexTranscriptEvent struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Payload   struct {
		Type string `json:"type"`
		Info struct {
			TotalTokenUsage *struct {
				TotalTokens int64 `json:"total_tokens"`
			} `json:"total_token_usage"`
		} `json:"info"`
		RateLimits *codexRateLimits `json:"rate_limits"`
	} `json:"payload"`
}

type codexScanResult struct {
	Days    []TokenUsageDay
	Rows    []QuotaRow
	Newest  time.Time
	QuotaAt time.Time
}

// scanCodexUsage uses cumulative counter deltas, so repeated token_count events
// add zero and every completed model interaction is counted exactly once. It
// reads every file from the start; the resident sampler keeps a
// codexUsageScanner instead, which reads only what each pass appended.
func scanCodexUsage(configDir string, now time.Time) (codexScanResult, error) {
	var scanner codexUsageScanner
	return scanner.scan(configDir, now)
}

// codexUsageRecord is one positive counter step. The step is computed while
// reading, from the session's previous token_count, so it stays correct when
// later passes resume mid-file.
type codexUsageRecord struct {
	at    time.Time
	delta int64
}

type codexTranscriptState struct {
	cursor   transcriptCursor
	previous int64 // the session's last cumulative total_tokens
	records  []codexUsageRecord
	limits   *codexRateLimits // the session's newest rate-limit snapshot
	limitsAt time.Time
}

func (s *codexTranscriptState) reset() {
	s.previous = 0
	s.records = nil
	s.limits = nil
	s.limitsAt = time.Time{}
}

var codexTokenCountMarker = []byte(`"token_count"`)

func (s *codexTranscriptState) add(line []byte) {
	if !bytes.Contains(line, codexTokenCountMarker) {
		return
	}
	var event codexTranscriptEvent
	if err := json.Unmarshal(line, &event); err != nil {
		return
	}
	if event.Type != "event_msg" || event.Payload.Type != "token_count" {
		return
	}
	at, err := time.Parse(time.RFC3339Nano, event.Timestamp)
	if err != nil {
		return
	}
	at = at.In(time.Local)
	if event.Payload.RateLimits != nil && at.After(s.limitsAt) {
		s.limits = event.Payload.RateLimits
		s.limitsAt = at
	}
	usage := event.Payload.Info.TotalTokenUsage
	if usage == nil || usage.TotalTokens < 0 {
		return
	}
	current := usage.TotalTokens
	delta := current - s.previous
	if current < s.previous {
		delta = current
	}
	s.previous = current
	if delta > 0 {
		s.records = append(s.records, codexUsageRecord{at: at, delta: delta})
	}
}

// codexUsageScanner is the Codex counterpart of claudeUsageScanner.
type codexUsageScanner struct {
	mu    sync.Mutex
	files map[string]*codexTranscriptState
}

func (s *codexUsageScanner) scan(configDir string, now time.Time) (codexScanResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	days, start, end := emptyTokenUsageDays(now)
	result := codexScanResult{Days: days, Rows: codexQuotaRows(nil)}
	paths, err := recentJSONLFiles(filepath.Join(configDir, "sessions"), start)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.files = nil
			return result, nil
		}
		return result, fmt.Errorf("Codex sessions: %w", err)
	}
	if s.files == nil {
		s.files = make(map[string]*codexTranscriptState)
	}
	pruneTranscriptStates(s.files, paths)
	for _, path := range paths {
		state := s.files[path]
		if state == nil {
			state = &codexTranscriptState{}
			s.files[path] = state
		}
		if err := readNewTranscriptLines(path, &state.cursor, state.reset, state.add); err != nil {
			return result, fmt.Errorf("Codex session %s: %w", filepath.Base(path), err)
		}
	}
	index := tokenDayIndex(days)
	var latestLimits *codexRateLimits
	for _, path := range paths {
		state := s.files[path]
		// Path order with a strict After keeps the first of equal timestamps,
		// as the single pass over every file did.
		if state.limits != nil && state.limitsAt.After(result.QuotaAt) {
			latestLimits = state.limits
			result.QuotaAt = state.limitsAt
		}
		kept := state.records[:0]
		for _, record := range state.records {
			if record.at.Before(start) {
				continue // the window only moves forward
			}
			kept = append(kept, record)
			if !record.at.Before(end) {
				continue
			}
			if i, ok := index[record.at.Format("2006-01-02")]; ok {
				days[i].Tokens = saturatingTokenAdd(days[i].Tokens, record.delta)
			}
			if record.at.After(result.Newest) {
				result.Newest = record.at
			}
		}
		state.records = kept
	}
	result.Rows = codexQuotaRows(latestLimits)
	if result.QuotaAt.After(result.Newest) {
		result.Newest = result.QuotaAt
	}
	return result, nil
}

func codexQuotaRows(limits *codexRateLimits) []QuotaRow {
	if limits == nil {
		return []QuotaRow{}
	}
	rows := make([]QuotaRow, 0, 2)
	for _, candidate := range []struct {
		id     string
		window *codexRateWindow
	}{
		{id: "primary", window: limits.Primary},
		{id: "secondary", window: limits.Secondary},
	} {
		window := candidate.window
		if window == nil || window.UsedPercent == nil || math.IsNaN(*window.UsedPercent) || math.IsInf(*window.UsedPercent, 0) {
			continue
		}
		row := QuotaRow{
			ID:      candidate.id,
			Label:   codexWindowLabel(window.WindowMinute),
			Percent: math.Max(0, math.Min(100, *window.UsedPercent)),
		}
		if window.ResetsAt > 0 {
			row.ResetsAt = time.Unix(window.ResetsAt, 0).UTC().Format(time.RFC3339)
		}
		rows = append(rows, row)
	}
	return rows
}

func codexWindowLabel(minutes int64) string {
	switch minutes {
	case 300:
		return "Session (5h)"
	case 10080:
		return "Weekly"
	}
	if minutes > 0 && minutes%1440 == 0 {
		return fmt.Sprintf("%d-day", minutes/1440)
	}
	if minutes > 0 && minutes%60 == 0 {
		return fmt.Sprintf("%d-hour", minutes/60)
	}
	if minutes > 0 {
		return fmt.Sprintf("%d-minute", minutes)
	}
	return "Usage"
}

func sumNonNegativeTokens(values ...int64) int64 {
	var total int64
	for _, value := range values {
		if value > 0 {
			total = saturatingTokenAdd(total, value)
		}
	}
	return total
}

func saturatingTokenAdd(left, right int64) int64 {
	if right > 0 && left > math.MaxInt64-right {
		return math.MaxInt64
	}
	return left + right
}

// CodexUsageStatus is the read-only /api/codex-usage response. For session data,
// FetchedAt is the last successful scan and QuotaAt is the quota event time.
// For live quota both timestamps describe the last successful account lookup.
// A newer session event can supersede live quota without affecting token charts.
type CodexUsageStatus struct {
	Configured      bool            `json:"configured"`
	Source          string          `json:"source"`
	Rows            []QuotaRow      `json:"rows"`
	TokenDays       []TokenUsageDay `json:"token_days"`
	FetchedAt       *time.Time      `json:"fetched_at,omitempty"`
	AgeSeconds      int             `json:"age_seconds,omitempty"`
	QuotaAt         *time.Time      `json:"quota_at,omitempty"`
	QuotaAgeSeconds int             `json:"quota_age_seconds,omitempty"`
	Stale           bool            `json:"stale"`
	Error           string          `json:"error,omitempty"`
}

type CodexUsageChecker struct {
	configDir   string
	pollOptions CodexQuotaPollOptions
	pollMu      sync.Mutex
	pollCancel  func()
	pollDone    chan struct{}

	// scanner keeps per-session read positions between sampler passes.
	scanner codexUsageScanner

	mu        sync.RWMutex
	cached    CodexUsageStatus
	liveRows  []QuotaRow
	liveAt    time.Time
	pollError string
}

func newCodexUsageChecker(configDir string) *CodexUsageChecker {
	status := CodexUsageStatus{Configured: false, Source: "none", Rows: []QuotaRow{}, TokenDays: []TokenUsageDay{}}
	if configDir != "" {
		status.Configured = true
		status.TokenDays, _, _ = emptyTokenUsageDays(time.Now())
	}
	return &CodexUsageChecker{configDir: configDir, cached: status}
}

func (c *CodexUsageChecker) Status(now time.Time) CodexUsageStatus {
	if c == nil {
		return CodexUsageStatus{Configured: false, Source: "none", Rows: []QuotaRow{}, TokenDays: []TokenUsageDay{}}
	}
	if now.IsZero() {
		now = time.Now()
	}
	c.mu.RLock()
	status := c.cached
	if !c.liveAt.IsZero() && (status.QuotaAt == nil || !status.QuotaAt.After(c.liveAt)) {
		at := c.liveAt
		status.Source = "live"
		status.Rows = c.liveRows
		status.QuotaAt = &at
		status.FetchedAt = &at
		if status.Error == "no Codex usage data yet; run a Codex session" {
			status.Error = ""
		}
	}
	if c.pollError != "" {
		if status.Error != "" {
			status.Error += "; "
		}
		status.Error += c.pollError
	}
	c.mu.RUnlock()
	status.AgeSeconds = 0
	status.QuotaAgeSeconds = 0
	status.Stale = false
	if status.FetchedAt != nil && !status.FetchedAt.After(now) {
		age := now.Sub(*status.FetchedAt)
		status.AgeSeconds = int(age.Seconds())
		status.Stale = age > quotaStaleAfter
	}
	if status.QuotaAt != nil && !status.QuotaAt.After(now) {
		status.QuotaAgeSeconds = int(now.Sub(*status.QuotaAt).Seconds())
	}
	return status
}

// Refresh performs the provider-owned session scan outside request handling.
// On a transient read error the last complete rows/chart remain available and
// the error is attached to that snapshot.
func (c *CodexUsageChecker) Refresh(now time.Time) {
	if c == nil || c.configDir == "" {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	result, err := c.scanner.scan(c.configDir, now)
	if err != nil {
		c.mu.Lock()
		c.cached.Error = err.Error()
		c.mu.Unlock()
		return
	}
	status := CodexUsageStatus{
		Configured: true,
		Source:     "none",
		Rows:       result.Rows,
		TokenDays:  result.Days,
	}
	if !result.Newest.IsZero() {
		status.Source = "sessions"
		// Freshness describes our view of the local source, not user activity.
		// Keep the previous FetchedAt on scan errors (the early return above),
		// but advance it whenever the complete scan succeeds. This makes an idle
		// weekly window stay current while a broken sampler still ages to stale.
		fetched := now.UTC()
		status.FetchedAt = &fetched
	}
	if !result.QuotaAt.IsZero() {
		quotaAt := result.QuotaAt.UTC()
		status.QuotaAt = &quotaAt
	}
	if status.Source == "none" && status.Error == "" {
		status.Error = "no Codex usage data yet; run a Codex session"
	}
	c.mu.Lock()
	c.cached = status
	c.mu.Unlock()
}

// tokenUsageSampler owns the periodic transcript scans for both providers.
// API requests only read the resulting in-memory snapshots.
type tokenUsageSampler struct {
	claude   *QuotaChecker
	codex    *CodexUsageChecker
	interval time.Duration

	mu      sync.Mutex
	stop    chan struct{}
	stopped chan struct{}
	started bool
}

func newTokenUsageSampler(claude *QuotaChecker, codex *CodexUsageChecker) *tokenUsageSampler {
	return &tokenUsageSampler{claude: claude, codex: codex, interval: tokenUsageRefreshInterval}
}

func (s *tokenUsageSampler) Refresh(now time.Time) {
	if s == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	if s.claude != nil {
		s.claude.refreshTokenUsage(now)
	}
	if s.codex != nil {
		s.codex.Refresh(now)
	}
}

func (s *tokenUsageSampler) Start() {
	if s == nil {
		return
	}
	s.mu.Lock()
	configured := (s.claude != nil && s.claude.configDir != "") || (s.codex != nil && s.codex.configDir != "")
	if s.started || !configured {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.stop = make(chan struct{})
	s.stopped = make(chan struct{})
	stop, stopped, interval := s.stop, s.stopped, s.interval
	if interval <= 0 {
		interval = tokenUsageRefreshInterval
	}
	s.mu.Unlock()

	go func() {
		defer close(stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case at := <-ticker.C:
				s.Refresh(at)
			case <-stop:
				return
			}
		}
	}()
}

func (s *tokenUsageSampler) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return
	}
	stop, stopped := s.stop, s.stopped
	s.started = false
	s.mu.Unlock()
	close(stop)
	<-stopped
}

// resolveCodexConfigDir follows Codex's own CODEX_HOME override, while giving
// the service an explicit flag/env slot for LocalSystem deployments.
func resolveCodexConfigDir(explicit string) string {
	return resolveCodexConfigDirFrom(explicit, os.Getenv, homeDir)
}

func resolveCodexConfigDirFrom(explicit string, getenv func(string) string, home func() string) string {
	for _, candidate := range []string{
		strings.TrimSpace(explicit),
		strings.TrimSpace(getenv("SYSMON_CODEX_CONFIG_DIR")),
		strings.TrimSpace(getenv("CODEX_HOME")),
	} {
		if candidate != "" {
			return existingDir(candidate)
		}
	}
	if base := strings.TrimSpace(home()); base != "" {
		return existingDir(filepath.Join(base, ".codex"))
	}
	return ""
}
