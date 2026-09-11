package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
// so requestId (then message id/uuid) is the deduplication boundary.
func scanClaudeTokenUsage(configDir string, now time.Time) ([]TokenUsageDay, time.Time, error) {
	days, start, end := emptyTokenUsageDays(now)
	root := filepath.Join(configDir, "projects")
	paths, err := recentJSONLFiles(root, start)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return days, time.Time{}, nil
		}
		return days, time.Time{}, fmt.Errorf("Claude transcripts: %w", err)
	}
	index := tokenDayIndex(days)
	seen := make(map[string]struct{})
	var newest time.Time
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			return days, newest, fmt.Errorf("Claude transcript %s: %w", filepath.Base(path), err)
		}
		decoder := json.NewDecoder(file)
		for {
			var event claudeTranscriptEvent
			if err := decoder.Decode(&event); err != nil {
				if !errors.Is(err, io.EOF) {
					// An actively written JSONL file can end in a partial record.
					// Keep every complete response decoded before that tail.
				}
				break
			}
			if event.Type != "assistant" || event.Message.Role != "assistant" || event.Message.Usage == nil {
				continue
			}
			at, err := time.Parse(time.RFC3339Nano, event.Timestamp)
			if err != nil {
				continue
			}
			at = at.In(time.Local)
			if at.Before(start) || !at.Before(end) {
				continue
			}
			key := strings.TrimSpace(event.RequestID)
			if key == "" {
				key = strings.TrimSpace(event.Message.ID)
			}
			if key == "" {
				key = strings.TrimSpace(event.UUID)
			}
			if key != "" {
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
			}
			usage := event.Message.Usage
			total := sumNonNegativeTokens(
				usage.InputTokens,
				usage.CacheCreationTokens,
				usage.CacheReadTokens,
				usage.OutputTokens,
			)
			if total == 0 {
				continue
			}
			if i, ok := index[at.Format("2006-01-02")]; ok {
				days[i].Tokens = saturatingTokenAdd(days[i].Tokens, total)
			}
			if at.After(newest) {
				newest = at
			}
		}
		_ = file.Close()
	}
	return days, newest, nil
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
// add zero and every completed model interaction is counted exactly once.
func scanCodexUsage(configDir string, now time.Time) (codexScanResult, error) {
	days, start, end := emptyTokenUsageDays(now)
	result := codexScanResult{Days: days}
	paths, err := recentJSONLFiles(filepath.Join(configDir, "sessions"), start)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return result, nil
		}
		return result, fmt.Errorf("Codex sessions: %w", err)
	}
	index := tokenDayIndex(days)
	var latestLimits *codexRateLimits
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			return result, fmt.Errorf("Codex session %s: %w", filepath.Base(path), err)
		}
		decoder := json.NewDecoder(file)
		var previous int64
		for {
			var event codexTranscriptEvent
			if err := decoder.Decode(&event); err != nil {
				break
			}
			if event.Type != "event_msg" || event.Payload.Type != "token_count" {
				continue
			}
			at, err := time.Parse(time.RFC3339Nano, event.Timestamp)
			if err != nil {
				continue
			}
			at = at.In(time.Local)
			if event.Payload.RateLimits != nil && at.After(result.QuotaAt) {
				limits := *event.Payload.RateLimits
				latestLimits = &limits
				result.QuotaAt = at
			}
			usage := event.Payload.Info.TotalTokenUsage
			if usage == nil || usage.TotalTokens < 0 {
				continue
			}
			current := usage.TotalTokens
			delta := current - previous
			if current < previous {
				delta = current
			}
			previous = current
			if at.Before(start) || !at.Before(end) || delta <= 0 {
				continue
			}
			if i, ok := index[at.Format("2006-01-02")]; ok {
				days[i].Tokens = saturatingTokenAdd(days[i].Tokens, delta)
			}
			if at.After(result.Newest) {
				result.Newest = at
			}
		}
		_ = file.Close()
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

// CodexUsageStatus is the read-only /api/codex-usage response. FetchedAt is
// the last successful session-tree scan, not the timestamp of the newest Codex
// interaction. An idle account has not changed, so tying freshness to activity
// would make a healthy 30-second sampler report stale after 15 minutes. QuotaAt
// separately preserves the age of the quota-bearing event for transparency.
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
	configDir string

	mu     sync.RWMutex
	cached CodexUsageStatus
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
	result, err := scanCodexUsage(c.configDir, now)
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
