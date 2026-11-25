package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type capturedRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   string
}

type captureServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []capturedRequest
	statuses []int // answered in turn; 200 once they run out
}

func newCaptureServer(t *testing.T, statuses ...int) *captureServer {
	t.Helper()
	c := &captureServer{statuses: statuses}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.requests = append(c.requests, capturedRequest{Method: r.Method, Path: r.URL.RequestURI(), Header: r.Header.Clone(), Body: string(body)})
		status := http.StatusOK
		if len(c.statuses) > 0 {
			status, c.statuses = c.statuses[0], c.statuses[1:]
		}
		c.mu.Unlock()
		// The answer echoes the request: it must never reach an error.
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(c.Close)
	return c
}

func (c *captureServer) got() []capturedRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]capturedRequest(nil), c.requests...)
}

// testDispatcher delivers synchronously with a fake clock and no real sleep.
func testDispatcher(targets ...alertTarget) (*alertDispatcher, *[]time.Duration) {
	d := newAlertDispatcher(targets, nil)
	d.now = func() time.Time { return alertT0 }
	var slept []time.Duration
	d.sleep = func(ctx context.Context, wait time.Duration) bool {
		slept = append(slept, wait)
		return ctx.Err() != nil
	}
	return d, &slept
}

func firingNotification() alertNotification {
	rule := alertRule{ID: "disk_free", Metric: alertMetricDiskFree, Below: true, Threshold: 10}
	return notificationFromEvent("test-host", alertEvent{Kind: "firing", Rule: rule, Instance: "/srv", Label: "/srv", Value: 4.04, Known: true, FiredAt: alertT0, At: alertT0, Notify: true})
}

func TestNtfyRequestShape(t *testing.T) {
	srv := newCaptureServer(t)
	target := &ntfyTarget{cfg: ntfyConfig{URL: srv.URL + "/alerts-topic", Token: "tk_abcdef", Tags: []string{"computer"}}, client: newAlertHTTPClient(), userAgent: "sysmon-agent/test"}
	d, _ := testDispatcher(target)
	d.deliver(context.Background(), firingNotification())

	reqs := srv.got()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d", len(reqs))
	}
	r := reqs[0]
	if r.Method != http.MethodPost || r.Path != "/alerts-topic" {
		t.Fatalf("request line = %s %s", r.Method, r.Path)
	}
	want := map[string]string{
		"Title":         "test-host: Free space",
		"Priority":      "high",
		"Tags":          "warning,computer",
		"Authorization": "Bearer tk_abcdef",
		"User-Agent":    "sysmon-agent/test",
		"Content-Type":  "text/plain; charset=utf-8",
	}
	for name, value := range want {
		if got := r.Header.Get(name); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
	if r.Body != "Free space on /srv 4% (under 10%)" {
		t.Fatalf("body = %q", r.Body)
	}
	if s := d.Status(); s.Sent != 1 || s.Failed != 0 || s.LastSentAt == nil {
		t.Fatalf("status = %+v", s)
	}

	// Resolved and test notices, a configured priority, and header-safe titles.
	target.cfg.Priority = "urgent"
	target.cfg.Token = ""
	resolved := firingNotification()
	resolved.Kind, resolved.Title, resolved.Priority = "resolved", "test-host: resolved", "default"
	d.deliver(context.Background(), resolved)
	odd := testNotification("hôst\r\nX-Injected: 1", alertT0)
	d.deliver(context.Background(), odd)
	fired := firingNotification()
	d.deliver(context.Background(), fired)
	reqs = srv.got()
	if got := reqs[1].Header.Get("Tags"); got != "white_check_mark,computer" || reqs[1].Header.Get("Priority") != "default" || reqs[1].Header.Get("Authorization") != "" {
		t.Fatalf("resolved headers = %v", reqs[1].Header)
	}
	if got := reqs[2].Header.Get("Title"); got != "h?st  X-Injected: 1: test notification" || reqs[2].Header.Get("X-Injected") != "" {
		t.Fatalf("test title = %q", got)
	}
	if got := reqs[3].Header.Get("Priority"); got != "urgent" {
		t.Fatalf("configured priority = %q", got)
	}
}

func TestWebhookRequestShape(t *testing.T) {
	srv := newCaptureServer(t)
	target := &webhookTarget{cfg: webhookConfig{URL: srv.URL + "/hook?key=abc", Token: "hook-token"}, client: newAlertHTTPClient(), userAgent: "sysmon-agent/1.2.3", version: "1.2.3"}
	d, _ := testDispatcher(target)
	d.deliver(context.Background(), firingNotification())
	d.deliver(context.Background(), testNotification("test-host", alertT0))

	reqs := srv.got()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d", len(reqs))
	}
	r := reqs[0]
	if r.Path != "/hook?key=abc" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Authorization") != "Bearer hook-token" {
		t.Fatalf("request = %s %v", r.Path, r.Header)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(r.Body), &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"source": "sysmon-agent", "version": "1.2.3", "host": "test-host", "kind": "firing",
		"rule": "disk_free", "metric": "disk_free", "instance": "/srv", "label": "/srv",
		"value": 4.0, "threshold": 10.0, "direction": "below", "unit": "%",
		"title": "test-host: Free space", "message": "Free space on /srv 4% (under 10%)",
		"at": "2026-11-20T12:00:00Z", "fired_at": "2026-11-20T12:00:00Z",
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("%s = %#v, want %#v", k, body[k], v)
		}
	}
	if len(body) != len(want) {
		t.Errorf("fields = %v", body)
	}
	var test map[string]any
	if err := json.Unmarshal([]byte(reqs[1].Body), &test); err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"value", "threshold", "fired_at", "metric"} {
		if _, ok := test[absent]; ok {
			t.Errorf("test notice carries %s: %v", absent, test)
		}
	}
	if test["kind"] != "test" {
		t.Fatalf("test kind = %v", test["kind"])
	}
}

func TestAlertDeliveryRetriesOnceOnServerErrors(t *testing.T) {
	srv := newCaptureServer(t, http.StatusServiceUnavailable)
	target := &webhookTarget{cfg: webhookConfig{URL: srv.URL + "/h"}, client: newAlertHTTPClient()}
	d, slept := testDispatcher(target)
	d.deliver(context.Background(), firingNotification())
	if n := len(srv.got()); n != 2 || len(*slept) != 1 || (*slept)[0] != alertRetryAfter {
		t.Fatalf("requests = %d, sleeps = %v", n, *slept)
	}
	if s := d.Status(); s.Sent != 1 || s.Failed != 0 {
		t.Fatalf("status = %+v", s)
	}

	// Two server errors: one retry, then a failure.
	srv2 := newCaptureServer(t, http.StatusBadGateway, http.StatusBadGateway, http.StatusOK)
	target2 := &webhookTarget{cfg: webhookConfig{URL: srv2.URL + "/h"}, client: newAlertHTTPClient()}
	d2, _ := testDispatcher(target2)
	d2.deliver(context.Background(), firingNotification())
	if n := len(srv2.got()); n != 2 {
		t.Fatalf("requests = %d, want 2", n)
	}
	if s := d2.Status(); s.Failed != 1 || !strings.Contains(s.LastError, "HTTP 502") {
		t.Fatalf("status = %+v", s)
	}

	// A client error is final.
	srv3 := newCaptureServer(t, http.StatusBadRequest)
	target3 := &ntfyTarget{cfg: ntfyConfig{URL: srv3.URL + "/t"}, client: newAlertHTTPClient()}
	d3, slept3 := testDispatcher(target3)
	d3.deliver(context.Background(), firingNotification())
	if n := len(srv3.got()); n != 1 || len(*slept3) != 0 {
		t.Fatalf("400: requests = %d, sleeps = %v", n, *slept3)
	}

	// 429 is retried; a redirect is not followed.
	srv4 := newCaptureServer(t, http.StatusTooManyRequests)
	d4, _ := testDispatcher(&ntfyTarget{cfg: ntfyConfig{URL: srv4.URL + "/t"}, client: newAlertHTTPClient()})
	d4.deliver(context.Background(), firingNotification())
	if n := len(srv4.got()); n != 2 {
		t.Fatalf("429: requests = %d", n)
	}
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	before := len(srv.got())
	d5, _ := testDispatcher(&webhookTarget{cfg: webhookConfig{URL: redirect.URL + "/h", Token: "tok-123"}, client: newAlertHTTPClient()})
	d5.deliver(context.Background(), firingNotification())
	if len(srv.got()) != before || d5.Status().Failed != 1 {
		t.Fatalf("redirect was followed: %+v", d5.Status())
	}
}

func TestAlertDeliveryErrorsAreRedacted(t *testing.T) {
	// A refused connection: the transport's error names the whole URL,
	// topic included.
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close()
	ntfy := &ntfyTarget{cfg: ntfyConfig{URL: base + "/very-secret-topic", Token: "tk_token_value"}, client: newAlertHTTPClient()}
	hook := &webhookTarget{cfg: webhookConfig{URL: base + "/hooks/secret-path?sig=secret-query", Token: "hook_token_value"}, client: newAlertHTTPClient()}
	var logs []string
	d, _ := testDispatcher(ntfy, hook)
	d.logf = func(format string, args ...any) {
		logs = append(logs, strings.TrimSpace(strings.ReplaceAll(format, "%v", "")))
		for _, a := range args {
			if err, ok := a.(error); ok {
				logs = append(logs, err.Error())
			}
		}
	}
	d.deliver(context.Background(), firingNotification())
	status := d.Status()
	if status.Failed != 2 {
		t.Fatalf("status = %+v", status)
	}
	everything := strings.Join(append(logs, status.LastError), "\n")
	for _, secret := range []string{"very-secret-topic", "tk_token_value", "secret-path", "secret-query", "hook_token_value"} {
		if strings.Contains(everything, secret) {
			t.Fatalf("%q leaked into %q", secret, everything)
		}
	}
	if !strings.Contains(status.LastError, "webhook (127.0.0.1:") {
		t.Fatalf("last error = %q, want the host named", status.LastError)
	}

	got := sanitizeAlertError("ntfy", "h", errors.New("bad token tk_abc in tk_abc"), "tk_abc", "")
	if got != "ntfy (h): bad token [redacted] in [redacted]" {
		t.Fatalf("sanitize = %q", got)
	}
	if got := sanitizeAlertError("ntfy", "h", context.DeadlineExceeded); got != "ntfy (h): timed out" {
		t.Fatalf("timeout = %q", got)
	}
}

func TestAlertDispatcherRateLimitAndQueue(t *testing.T) {
	d, _ := testDispatcher(&webhookTarget{cfg: webhookConfig{URL: "http://127.0.0.1:1/h"}, client: newAlertHTTPClient()})
	now := alertT0
	d.now = func() time.Time { return now }
	for i := 0; i < alertRateBurst; i++ {
		if !d.Enqueue(firingNotification()) {
			t.Fatalf("notice %d refused inside the burst", i)
		}
	}
	if d.Enqueue(firingNotification()) {
		t.Fatal("the burst should be spent")
	}
	now = now.Add(alertRateRefillEvery)
	if !d.Enqueue(firingNotification()) {
		t.Fatal("a token should come back after a minute")
	}
	if d.Enqueue(firingNotification()) {
		t.Fatal("only one token should come back")
	}
	if s := d.Status(); s.Dropped != 2 {
		t.Fatalf("dropped = %d", s.Dropped)
	}
	if newAlertDispatcher(nil, nil).Enqueue(firingNotification()) {
		t.Fatal("no targets should queue nothing")
	}
}

func TestAlertServiceSendsOnTransitions(t *testing.T) {
	srv := newCaptureServer(t)
	rule := cpuRule()
	rule.Hold, rule.Release = 4*time.Second, 0
	source := &fakeAlertSource{snap: alertSnapshot{Have: true, FastFresh: true, Metrics: Metrics{CPU: NumberMetric{Available: true, Value: 99}}}}
	service, err := newAlertService(alertServiceOptions{
		Config:  alertsConfig{Rules: []alertRule{rule}, Webhook: &webhookConfig{URL: srv.URL + "/h"}},
		Host:    "test-host",
		Version: "1.0.0",
		Source:  source,
		Logf:    t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); service.dispatcher.Run(ctx) }()
	defer func() { cancel(); <-done }()

	service.tick(at(0))
	if s := service.Status(); len(s.Alerts) != 1 || s.Alerts[0].State != "pending" || len(service.Summary().Active) != 0 {
		t.Fatalf("pending: %+v / %+v", s.Alerts, service.Summary())
	}
	service.tick(at(4))
	if active := service.Summary().Active; len(active) != 1 || active[0].State != "firing" {
		t.Fatalf("firing summary = %+v", active)
	}
	source.snap.Metrics.CPU.Value = 10
	service.tick(at(6))
	waitFor(t, func() bool { return len(srv.got()) == 2 })
	reqs := srv.got()
	if !strings.Contains(reqs[0].Body, `"kind":"firing"`) || !strings.Contains(reqs[1].Body, `"kind":"resolved"`) {
		t.Fatalf("bodies = %q / %q", reqs[0].Body, reqs[1].Body)
	}
	if err := service.SendTest(); err != nil {
		t.Fatal(err)
	}
	if err := service.SendTest(); !errors.Is(err, errAlertTestSoon) {
		t.Fatalf("second test = %v", err)
	}
	waitFor(t, func() bool { return len(srv.got()) == 3 })
	status := service.Status()
	if !status.Enabled || status.Targets.Webhook == nil || status.Targets.Webhook.Host == "" || status.Delivery.Sent != 3 {
		t.Fatalf("status = %+v", status)
	}
	data, _ := json.Marshal(status)
	if strings.Contains(string(data), "/h\"") || strings.Contains(string(data), srv.URL) {
		t.Fatalf("status leaks the target URL: %s", data)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
