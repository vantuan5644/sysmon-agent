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
}

func newCodexUsageCheckerWithOptions(configDir string, options CodexQuotaPollOptions) *CodexUsageChecker {
	c := newCodexUsageChecker(configDir)
	c.pollOptions = options
	return c
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
	interval := c.pollOptions.Interval
	if interval <= 0 {
		interval = quotaPollInterval
	}
	go func() {
		defer close(done)
		for {
			c.pollQuota(ctx)
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
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

func (c *CodexUsageChecker) pollQuota(parent context.Context) {
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
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.pollError = "Codex quota poll: " + err.Error()
		return
	}
	c.liveRows = rows
	c.liveAt = time.Now().UTC()
	c.pollError = ""
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
	hideCodexQuotaWindow(cmd)
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
