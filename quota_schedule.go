package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Poll scheduling for the two quota pollers (Claude's usage endpoint and the
// Codex app server), and the small state file that carries the schedule and
// the last good reading across restarts.
//
// Every delay here is a floor on top of the normal cadence, never a shortcut
// below it: a failure can only push the next attempt further out. The one
// exception is a CHANGED OAuth token after a 401/403, which is sent at once,
// because the rejection was about the old token and the new one has never
// been tried.

const (
	// quotaFailureBackoffBase/Max: consecutive Claude failures wait 1, 2, 4,
	// 8, then 15 minutes, never less than the normal interval.
	quotaFailureBackoffBase = time.Minute
	quotaFailureBackoffMax  = 15 * time.Minute

	// quotaRetryAfterCap bounds a server-sent Retry-After, so a bogus or hostile
	// value cannot park the poller for days. It also bounds a persisted next
	// attempt read back on start, which guards against a clock jump.
	quotaRetryAfterCap = time.Hour

	// quotaTokenRecheck is how often the credentials file is re-read while the
	// token in it is one the server already rejected. Reading the file costs
	// nothing on the network; the rejected token is not re-sent.
	quotaTokenRecheck = time.Minute

	// quotaRejectedTokenRetry is when the same rejected token is tried once
	// more, in case the rejection was transient on the server's side.
	quotaRejectedTokenRetry = 30 * time.Minute

	// codexFailureBackoffBase/Max: consecutive Codex failures wait 2, 4, 8,
	// 16, then 30 minutes, never less than the normal interval.
	codexFailureBackoffBase = 2 * time.Minute
	codexFailureBackoffMax  = 30 * time.Minute

	// quotaStateFile lives in the agent's private state folder (the pairing
	// folder beside the settings file). It never holds a token.
	quotaStateFile = "quota-state.json"
)

const (
	quotaFiveHourSeconds = int64(5 * 60 * 60)
	quotaWeekSeconds     = int64(7 * 24 * 60 * 60)
)

// quotaWindowSecondsFor is the window length behind a Claude row id. Credits
// and the cinder_cove pot are not rolling windows, so they report 0.
func quotaWindowSecondsFor(id string) int64 {
	switch {
	case id == "five_hour":
		return quotaFiveHourSeconds
	case id == "seven_day", id == "seven_day_opus", id == "seven_day_sonnet":
		return quotaWeekSeconds
	case strings.HasPrefix(id, "weekly_"):
		return quotaWeekSeconds
	}
	return 0
}

// quotaLimitWindowsByKind maps the unscoped limits[] entries onto the named
// windows they describe: kind "session" is five_hour and "weekly_all" is
// seven_day. quotaRowsFromLivePayload only uses them when the named window
// itself is absent, so a payload that carries both yields one row each, and
// a payload that only carries limits[] still fills the page.
func quotaLimitWindowsByKind(limits []quotaLiveLimit) map[string]*quotaLiveWindow {
	out := map[string]*quotaLiveWindow{}
	for _, limit := range limits {
		if limit.Percent == nil || strings.TrimSpace(limit.Scope.ModelDisplayName()) != "" {
			continue
		}
		var id string
		switch strings.ToLower(strings.TrimSpace(limit.Kind)) {
		case "session":
			id = "five_hour"
		case "weekly_all":
			id = "seven_day"
		default:
			continue
		}
		if _, seen := out[id]; seen {
			continue
		}
		out[id] = &quotaLiveWindow{Utilization: limit.Percent, ResetsAt: limit.ResetsAt}
	}
	return out
}

// exponentialBackoff returns base << (failures-1), capped at max, and never
// below floor. failures <= 0 means no failure: the floor alone.
func exponentialBackoff(failures int, base, max, floor time.Duration) time.Duration {
	if failures <= 0 {
		return floor
	}
	wait := max
	if shift := failures - 1; shift < 31 {
		if candidate := base << uint(shift); candidate > 0 && candidate < max {
			wait = candidate
		}
	}
	if wait < floor {
		wait = floor
	}
	return wait
}

// quotaFailureBackoff is the Claude poller's wait after its Nth consecutive
// failure: 1, 2, 4, 8, 15 minutes, floored at the normal interval.
func quotaFailureBackoff(failures int, interval time.Duration) time.Duration {
	return exponentialBackoff(failures, quotaFailureBackoffBase, quotaFailureBackoffMax, interval)
}

// codexFailureBackoff is the Codex poller's wait after its Nth consecutive
// failure: 2, 4, 8, 16, 30 minutes, floored at the normal interval.
func codexFailureBackoff(failures int, interval time.Duration) time.Duration {
	return exponentialBackoff(failures, codexFailureBackoffBase, codexFailureBackoffMax, interval)
}

// parseRetryAfter reads a Retry-After header: delta seconds or an HTTP date.
// It reports false for a missing or unparseable value. A value in the past
// is 0; anything longer than quotaRetryAfterCap is capped.
func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	var wait time.Duration
	if secs, err := strconv.ParseInt(value, 10, 64); err == nil {
		if secs < 0 {
			return 0, false
		}
		if secs > int64(quotaRetryAfterCap/time.Second) {
			return quotaRetryAfterCap, true
		}
		wait = time.Duration(secs) * time.Second
	} else if at, err := http.ParseTime(value); err == nil {
		wait = at.Sub(now)
	} else {
		return 0, false
	}
	if wait < 0 {
		wait = 0
	}
	if wait > quotaRetryAfterCap {
		wait = quotaRetryAfterCap
	}
	return wait, true
}

// quotaTokenFingerprint is the first 16 hex digits of the token's SHA-256.
// It is enough to tell "the same token again" from "a new token" and is the
// only trace of a token the agent keeps, in memory or on disk.
func quotaTokenFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:16]
}

// --- persisted state ----------------------------------------------------------

// quotaProviderState is one poller's schedule and last good reading. Rows are
// the dashboard rows exactly as served (percentages, reset times, window
// lengths); FetchedAt is when they were read, so a reading loaded after a
// restart goes stale on the usual quotaStaleAfter clock.
type quotaProviderState struct {
	NextAt     time.Time  `json:"next_at,omitempty"`
	Failures   int        `json:"failures,omitempty"`
	RejectedFP string     `json:"rejected_token_fingerprint,omitempty"`
	RejectedAt time.Time  `json:"rejected_at,omitempty"`
	FetchedAt  time.Time  `json:"fetched_at,omitempty"`
	Rows       []QuotaRow `json:"rows,omitempty"`
}

type quotaStateDoc struct {
	Version int                `json:"version"`
	Claude  quotaProviderState `json:"claude"`
	Codex   quotaProviderState `json:"codex"`
}

// quotaStateStore holds both pollers' state and writes it to one file. A nil
// store, or one with no folder, keeps everything in memory.
type quotaStateStore struct {
	dir string
	mu  sync.Mutex
	doc quotaStateDoc
}

// newQuotaStateStore loads dir/quota-state.json. A missing file is a clean
// start; an unreadable or corrupt one is reported and replaced on the next
// save, never fatal.
func newQuotaStateStore(dir string) (*quotaStateStore, error) {
	store := &quotaStateStore{dir: dir, doc: quotaStateDoc{Version: 1}}
	if dir == "" {
		return store, nil
	}
	data, err := os.ReadFile(filepath.Join(dir, quotaStateFile))
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return store, fmt.Errorf("read %s: %w", quotaStateFile, err)
	}
	var doc quotaStateDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return store, fmt.Errorf("parse %s: %w", quotaStateFile, err)
	}
	doc.Version = 1
	doc.Claude = sanitizeQuotaProviderState(doc.Claude)
	doc.Codex = sanitizeQuotaProviderState(doc.Codex)
	store.doc = doc
	return store, nil
}

// sanitizeQuotaProviderState drops anything a hand-edited or damaged file
// could use to misbehave: a malformed fingerprint, negative failures, rows
// without an id.
func sanitizeQuotaProviderState(state quotaProviderState) quotaProviderState {
	if state.Failures < 0 {
		state.Failures = 0
	}
	if fp := state.RejectedFP; len(fp) != 16 || strings.Trim(fp, "0123456789abcdef") != "" {
		state.RejectedFP = ""
		state.RejectedAt = time.Time{}
	}
	rows := state.Rows[:0:0]
	for _, row := range state.Rows {
		if row.ID != "" {
			rows = append(rows, row)
		}
	}
	state.Rows = rows
	if len(state.Rows) == 0 {
		state.Rows = nil
		state.FetchedAt = time.Time{}
	}
	return state
}

func (s *quotaStateStore) Claude() quotaProviderState {
	if s == nil {
		return quotaProviderState{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyQuotaProviderState(s.doc.Claude)
}

func (s *quotaStateStore) Codex() quotaProviderState {
	if s == nil {
		return quotaProviderState{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyQuotaProviderState(s.doc.Codex)
}

func (s *quotaStateStore) SetClaude(state quotaProviderState) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.doc.Claude = copyQuotaProviderState(state)
	return s.saveLocked()
}

func (s *quotaStateStore) SetCodex(state quotaProviderState) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.doc.Codex = copyQuotaProviderState(state)
	return s.saveLocked()
}

func (s *quotaStateStore) saveLocked() error {
	if s.dir == "" {
		return nil
	}
	data, err := json.MarshalIndent(s.doc, "", "  ")
	if err != nil {
		return err
	}
	if err := ensurePrivateDir(s.dir); err != nil {
		return err
	}
	return writePrivateFile(s.dir, quotaStateFile, append(data, '\n'))
}

func copyQuotaProviderState(state quotaProviderState) quotaProviderState {
	if state.Rows != nil {
		state.Rows = append([]QuotaRow(nil), state.Rows...)
	}
	return state
}

// quotaInitialWait is the first wait of a poll loop: the usual startup delay,
// or longer when the persisted schedule says the next attempt is further out.
// The persisted time is capped at quotaRetryAfterCap from now.
func quotaInitialWait(startupDelay time.Duration, nextAt, now time.Time) time.Duration {
	wait := startupDelay
	if nextAt.IsZero() {
		return wait
	}
	remaining := nextAt.Sub(now)
	if remaining > quotaRetryAfterCap {
		remaining = quotaRetryAfterCap
	}
	if remaining > wait {
		wait = remaining
	}
	return wait
}
