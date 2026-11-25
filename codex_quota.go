package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

const codexQuotaRequestTimeout = 20 * time.Second

// Codex handles its own authentication; the agent never parses credentials.
type CodexQuotaPollOptions struct {
	Enabled bool
	Binary  string
	// Fetch is supplied by tests; production uses the installed Codex CLI.
	Fetch    func(context.Context) ([]QuotaRow, error)
	Interval time.Duration
	// State persists the poll schedule and the last good reading across
	// restarts. nil keeps both in memory.
	State *quotaStateStore
}

func newCodexUsageCheckerWithOptions(configDir string, options CodexQuotaPollOptions) *CodexUsageChecker {
	c := newCodexUsageChecker(configDir)
	c.pollOptions = options
	if configDir != "" && options.Enabled && options.State != nil {
		saved := options.State.Codex()
		c.pollFailures = saved.Failures
		c.pollNextAt = saved.NextAt
		if len(saved.Rows) > 0 && !saved.FetchedAt.IsZero() {
			c.liveRows, c.liveAt = saved.Rows, saved.FetchedAt.UTC()
		}
	}
	return c
}

func (c *CodexUsageChecker) pollInterval() time.Duration {
	if c.pollOptions.Interval > 0 {
		return c.pollOptions.Interval
	}
	return quotaPollInterval
}

// Start owns a separate slow lane, so a quota lookup cannot block transcript
// scans, metrics publication, or HTTP handlers. Stop cancels an active lookup.
func (c *CodexUsageChecker) Start() {
	if c == nil || c.configDir == "" || !c.pollOptions.Enabled {
		return
	}
	c.pollMu.Lock()
	defer c.pollMu.Unlock()
	if c.pollCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.pollCancel = cancel
	c.pollDone = make(chan struct{})
	done := c.pollDone
	c.mu.RLock()
	// A persisted next attempt (a backoff, or simply the last poll's cadence)
	// survives a restart, so restarting the agent does not re-poll at once.
	first := quotaInitialWait(0, c.pollNextAt, time.Now())
	c.mu.RUnlock()
	go func() {
		defer close(done)
		wait := first
		for {
			if wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
			wait = c.pollQuota(ctx)
			if ctx.Err() != nil {
				return
			}
		}
	}()
}

// pollQuota runs one lookup and returns the wait before the next: the normal
// interval after a success, and 2, 4, 8, 16, then 30 minutes (never less than
// the interval) after consecutive failures.
func (c *CodexUsageChecker) pollQuota(parent context.Context) time.Duration {
	interval := c.pollInterval()
	ctx, cancel := context.WithTimeout(parent, codexQuotaRequestTimeout)
	defer cancel()
	fetch := c.pollOptions.Fetch
	if fetch == nil {
		fetch = func(ctx context.Context) ([]QuotaRow, error) {
			return readCodexQuota(ctx, c.pollOptions.Binary, c.configDir)
		}
	}
	rows, err := fetch(ctx)
	if parent.Err() != nil {
		return interval
	}
	now := time.Now().UTC()
	c.mu.Lock()
	var wait time.Duration
	if err != nil {
		c.pollError = "Codex quota poll: " + err.Error()
		c.pollFailures++
		wait = codexFailureBackoff(c.pollFailures, interval)
	} else {
		c.liveRows = rows
		c.liveAt = now
		c.pollError = ""
		c.pollFailures = 0
		wait = interval
	}
	c.pollNextAt = now.Add(wait)
	state := quotaProviderState{NextAt: c.pollNextAt, Failures: c.pollFailures, Rows: c.liveRows, FetchedAt: c.liveAt}
	c.mu.Unlock()
	if store := c.pollOptions.State; store != nil {
		// A failed save is not surfaced: the poller keeps working from
		// memory, and the state file only matters at the next start.
		_ = store.SetCodex(state)
	}
	return wait
}

func (c *CodexUsageChecker) Stop() {
	if c == nil {
		return
	}
	c.pollMu.Lock()
	defer c.pollMu.Unlock()
	if c.pollCancel == nil {
		return
	}
	c.pollCancel()
	<-c.pollDone
	c.pollCancel = nil
}

type codexAppWindow struct {
	UsedPercent   *float64 `json:"usedPercent"`
	WindowMinutes int64    `json:"windowDurationMins"`
	ResetsAt      int64    `json:"resetsAt"`
}

type codexAppLimits struct {
	LimitID   string          `json:"limitId"`
	Primary   *codexAppWindow `json:"primary"`
	Secondary *codexAppWindow `json:"secondary"`
}

type codexQuotaResult struct {
	RateLimits *codexAppLimits            `json:"rateLimits"`
	ByID       map[string]*codexAppLimits `json:"rateLimitsByLimitId"`
}

func codexAppQuotaRows(result codexQuotaResult) ([]QuotaRow, error) {
	limits := result.ByID["codex"]
	if limits == nil {
		limits = result.RateLimits
	}
	if limits == nil || (limits.LimitID != "" && limits.LimitID != "codex") {
		return nil, errors.New("no Codex quota returned; check Codex ChatGPT login")
	}
	convert := func(w *codexAppWindow) *codexRateWindow {
		if w == nil {
			return nil
		}
		return &codexRateWindow{UsedPercent: w.UsedPercent, WindowMinute: w.WindowMinutes, ResetsAt: w.ResetsAt}
	}
	rows := codexQuotaRows(&codexRateLimits{Primary: convert(limits.Primary), Secondary: convert(limits.Secondary)})
	if len(rows) == 0 {
		return nil, errors.New("no Codex quota windows returned; check Codex ChatGPT login")
	}
	return rows, nil
}

// readCodexQuota launches only the stdio app server and account lookup. It
// starts no conversation and bounds both process lifetime and protocol output.
// Server diagnostics and error messages may contain private account data, so
// only locally authored errors and numeric protocol codes reach the dashboard.
func readCodexQuota(parent context.Context, binary, configDir string) ([]QuotaRow, error) {
	ctx, cancel := context.WithTimeout(parent, codexQuotaRequestTimeout)
	defer cancel()
	if binary == "" {
		binary = "codex"
	}
	cmd := exec.CommandContext(ctx, binary, "app-server", "--stdio")
	hideChildWindow(cmd)
	for _, value := range os.Environ() {
		if !strings.EqualFold(strings.SplitN(value, "=", 2)[0], "CODEX_HOME") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "CODEX_HOME="+configDir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, errors.New("cannot open Codex input")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, errors.New("cannot open Codex output")
	}
	// A failed executable lookup is actionable without exposing inherited env.
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, errors.New("cannot start Codex; set -codex-binary to the installed CLI")
	}
	defer func() {
		cancel()
		_ = stdin.Close()
		_ = cmd.Wait()
		noteChildExit(cmd)
	}()
	return readCodexQuotaProtocol(ctx, stdout, stdin)
}

func readCodexQuotaProtocol(ctx context.Context, input io.Reader, output io.Writer) ([]QuotaRow, error) {
	encoder := json.NewEncoder(output)
	decoder := json.NewDecoder(io.LimitReader(input, quotaMaxPayloadBytes))
	var responseID int64
	request := func(method string, params any) (json.RawMessage, error) {
		responseID++
		id := responseID
		if err := encoder.Encode(map[string]any{"id": id, "method": method, "params": params}); err != nil {
			return nil, errors.New("cannot write Codex quota request")
		}
		for {
			var response struct {
				ID     int64           `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Code int `json:"code"`
				} `json:"error"`
			}
			if err := decoder.Decode(&response); err != nil {
				if ctx.Err() != nil {
					return nil, errors.New("Codex quota request timed out or was cancelled")
				}
				return nil, errors.New("invalid or incomplete Codex quota response")
			}
			if response.ID != id {
				continue
			}
			if response.Error != nil {
				return nil, fmt.Errorf("Codex %s failed (code %d); check Codex login", method, response.Error.Code)
			}
			return response.Result, nil
		}
	}
	if _, err := request("initialize", map[string]any{"clientInfo": map[string]string{"name": "sysmon_agent", "title": "Sysmon Agent", "version": version}}); err != nil {
		return nil, err
	}
	if err := encoder.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return nil, errors.New("cannot initialize Codex quota connection")
	}
	body, err := request("account/rateLimits/read", nil)
	if err != nil {
		return nil, err
	}
	var result codexQuotaResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, errors.New("invalid Codex quota payload")
	}
	return codexAppQuotaRows(result)
}
