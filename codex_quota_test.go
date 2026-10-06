package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestCodexQuotaProtocol(t *testing.T) {
	// Pin the handshake order and verify no thread or turn is ever started.
	serverInput, clientOutput := io.Pipe()
	clientInput, serverOutput := io.Pipe()
	defer serverInput.Close()
	defer clientOutput.Close()
	defer clientInput.Close()
	defer serverOutput.Close()
	serverDone := make(chan error, 1)
	go func() {
		defer serverOutput.Close()
		decoder := json.NewDecoder(serverInput)
		encoder := json.NewEncoder(serverOutput)
		for index, method := range []string{"initialize", "initialized", "account/rateLimits/read"} {
			var message struct {
				Method string `json:"method"`
				ID     int    `json:"id"`
			}
			if err := decoder.Decode(&message); err != nil {
				serverDone <- err
				return
			}
			if message.Method != method {
				serverDone <- errors.New("unexpected method: " + message.Method)
				return
			}
			if index == 0 {
				if err := encoder.Encode(map[string]any{"id": message.ID, "result": map[string]any{}}); err != nil {
					serverDone <- err
					return
				}
			}
			if index == 2 {
				// Notifications are allowed before the correlated response. Prefer
				// the Codex bucket even when the compatibility bucket differs.
				if err := encoder.Encode(map[string]any{"method": "account/updated"}); err != nil {
					serverDone <- err
					return
				}
				_, err := io.WriteString(serverOutput, `{"id":2,"result":{"rateLimits":{"limitId":"other","primary":{"usedPercent":99,"windowDurationMins":300}},"rateLimitsByLimitId":{"codex":{"limitId":"codex","primary":{"usedPercent":26,"windowDurationMins":10080,"resetsAt":1789829361}}}}}`+"\n")
				serverDone <- err
			}
		}
	}()
	rows, err := readCodexQuotaProtocol(context.Background(), clientInput, clientOutput)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Percent != 26 || rows[0].Label != "Weekly" || rows[0].ResetsAt != "2026-09-19T14:49:21Z" {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestCodexAppQuotaCompatibilityWindows(t *testing.T) {
	var result codexQuotaResult
	if err := json.Unmarshal([]byte(`{"rateLimits":{"primary":{"usedPercent":12.5,"windowDurationMins":300},"secondary":{"usedPercent":31,"windowDurationMins":10080}}}`), &result); err != nil {
		t.Fatal(err)
	}
	rows, err := codexAppQuotaRows(result)
	if err != nil || len(rows) != 2 || rows[0].Label != "Session (5h)" || rows[0].Percent != 12.5 || rows[1].Label != "Weekly" || rows[1].Percent != 31 {
		t.Fatalf("compatibility windows = %+v, error = %v", rows, err)
	}
}

func TestCodexQuotaProtocolFailures(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"private error", `{"id":1,"error":{"code":401,"message":"SECRET account credential"}}`, "code 401"},
		{"truncated", `{"id":1,"result":`, "incomplete"},
		{"empty quota", `{"id":1,"result":{}}` + "\n" + `{"id":2,"result":{"rateLimits":null}}`, "no Codex quota"},
		{"oversized", `{"id":1,"result":"` + strings.Repeat("x", quotaMaxPayloadBytes) + `"}`, "incomplete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			_, err := readCodexQuotaProtocol(context.Background(), strings.NewReader(tc.input), &output)
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestCodexQuotaPollPreservesSnapshotAndRecovers(t *testing.T) {
	checker := newCodexUsageCheckerWithOptions(t.TempDir(), CodexQuotaPollOptions{Enabled: true})
	checker.Refresh(time.Now())
	checker.pollOptions.Fetch = func(context.Context) ([]QuotaRow, error) {
		return []QuotaRow{{ID: "primary", Label: "Weekly", Percent: 26}}, nil
	}
	checker.pollQuota(context.Background())
	first := checker.Status(time.Now())
	if first.Source != "live" || first.Error != "" || first.QuotaAt == nil || len(first.Rows) != 1 {
		t.Fatalf("first = %+v", first)
	}
	checker.pollOptions.Fetch = func(context.Context) ([]QuotaRow, error) { return nil, errors.New("lookup unavailable") }
	checker.pollQuota(context.Background())
	// A healthy transcript scan must neither erase live quota nor its error.
	checker.Refresh(time.Now())
	failed := checker.Status(first.QuotaAt.Add(quotaStaleAfter + time.Second))
	if failed.Source != "live" || !failed.QuotaAt.Equal(*first.QuotaAt) || !failed.Stale || failed.Rows[0].Percent != 26 || !strings.Contains(failed.Error, "lookup unavailable") {
		t.Fatalf("failed = %+v", failed)
	}
	checker.pollOptions.Fetch = func(context.Context) ([]QuotaRow, error) { return []QuotaRow{{ID: "primary", Percent: 28}}, nil }
	checker.pollQuota(context.Background())
	recovered := checker.Status(time.Now())
	if recovered.Error != "" || recovered.Rows[0].Percent != 28 || recovered.QuotaAt.Before(*first.QuotaAt) {
		t.Fatalf("recovered = %+v", recovered)
	}
	// Account activity after the lookup takes precedence without another call.
	checker.mu.Lock()
	at := time.Now().Add(time.Second)
	checker.cached.QuotaAt = &at
	checker.cached.Rows = []QuotaRow{{Percent: 29}}
	checker.cached.Source = "sessions"
	checker.mu.Unlock()
	if got := checker.Status(time.Now()); got.Source != "sessions" || got.Rows[0].Percent != 29 {
		t.Fatalf("newer transcript = %+v", got)
	}
}

func TestCodexQuotaPollStopCancelsLookup(t *testing.T) {
	started := make(chan struct{})
	checker := newCodexUsageCheckerWithOptions(t.TempDir(), CodexQuotaPollOptions{Enabled: true, Fetch: func(ctx context.Context) ([]QuotaRow, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	checker.Start()
	checker.Start()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("poll did not start")
	}
	done := make(chan struct{})
	go func() { checker.Stop(); checker.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel lookup")
	}
	if got := checker.Status(time.Now()); got.Error != "" {
		t.Fatalf("shutdown published error: %+v", got)
	}
}

func TestCodexQuotaPollDisabled(t *testing.T) {
	for _, configured := range []bool{false, true} {
		dir := ""
		if configured {
			dir = t.TempDir()
		}
		checker := newCodexUsageCheckerWithOptions(dir, CodexQuotaPollOptions{Enabled: !configured, Fetch: func(context.Context) ([]QuotaRow, error) { t.Error("disabled poll was called"); return nil, nil }})
		checker.Start()
		checker.Stop()
	}
}
