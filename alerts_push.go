package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Outbound delivery for host alerts: ntfy, a JSON webhook and Web Push
// (alerts_webpush.go). One worker sends everything, in order, from a short
// queue, so a slow target can delay notices but never the alert loop.
//
// Every target gets the same limits:
//   - a 10 s timeout per request and no redirects (a redirect would carry the
//     token somewhere the config never named);
//   - at most one retry, 2 s later, and only for a network error, a 429 or a
//     5xx; any other answer is final;
//   - a global token bucket (6 notices at once, then one a minute), so a
//     flapping host cannot flood a phone or exhaust an ntfy quota. A notice
//     over the limit is dropped and counted, never queued for later.
//
// Errors are reduced to the target's host and the status or failure kind
// before they are logged or served: an ntfy topic in a URL path is the secret
// that lets anyone read it, and a token never appears anywhere.

const (
	alertQueueSize       = 32
	alertRequestTimeout  = 10 * time.Second
	alertRetryAfter      = 2 * time.Second
	alertRateBurst       = 6
	alertRateRefillEvery = time.Minute
	alertResponseDrain   = 4 << 10
)

// alertNotification is one notice, the same for every target.
type alertNotification struct {
	Kind      string // firing, resolved or test
	Host      string
	Title     string
	Message   string
	Rule      string
	Metric    string
	Instance  string
	Label     string
	Value     float64
	Known     bool
	Threshold float64
	Direction string
	Unit      string
	Priority  string // ntfy priority
	At        time.Time
	FiredAt   time.Time
}

func notificationFromEvent(host string, ev alertEvent) alertNotification {
	n := alertNotification{
		Kind:      ev.Kind,
		Host:      host,
		Rule:      ev.Rule.ID,
		Metric:    ev.Rule.Metric,
		Instance:  ev.Instance,
		Label:     ev.Label,
		Value:     roundAlert(ev.Value),
		Known:     ev.Known,
		Threshold: ev.Rule.Threshold,
		Direction: ev.Rule.direction(),
		Unit:      ev.Rule.spec().Unit,
		At:        ev.At.UTC(),
		FiredAt:   ev.FiredAt.UTC(),
	}
	message := alertMessage(ev.Rule, ev.Label, ev.Value, ev.Known)
	if ev.Kind == "resolved" {
		n.Title = host + ": resolved"
		n.Message = "Resolved: " + alertResolvedMessage(ev.Rule, ev.Label, ev.Value, ev.Known)
		n.Priority = "default"
		return n
	}
	n.Title = host + ": " + alertMetricSpecs[ev.Rule.Metric].Name
	n.Message = message
	n.Priority = firstNonEmpty(ev.Rule.Priority, "high")
	if ev.Rule.Metric == alertMetricUpdate {
		n.Priority = firstNonEmpty(ev.Rule.Priority, "default")
	}
	return n
}

func alertResolvedMessage(rule alertRule, label string, value float64, known bool) string {
	text := alertMessage(rule, label, value, known)
	// "CPU 97% (over 90%)" reads wrong once resolved; drop the bracket.
	if i := strings.LastIndex(text, " ("); i > 0 {
		text = text[:i]
	}
	return text + " now"
}

func testNotification(host string, at time.Time) alertNotification {
	return alertNotification{
		Kind:     "test",
		Host:     host,
		Title:    host + ": test notification",
		Message:  "Host alerts from " + host + " reach this device.",
		Rule:     "test",
		Priority: "default",
		At:       at.UTC(),
	}
}

// alertTarget sends one notice. retry wraps one attempt with the shared retry
// rule; a target with several recipients (Web Push) calls it per recipient.
type alertTarget interface {
	Name() string
	Deliver(ctx context.Context, n alertNotification, retry alertRetryFunc) error
}

type alertRetryFunc func(ctx context.Context, attempt func(context.Context) error) error

// alertHTTPError is a target's answer other than 2xx.
type alertHTTPError struct {
	Status int
}

func (e *alertHTTPError) Error() string { return fmt.Sprintf("HTTP %d", e.Status) }

func alertRetryable(err error) bool {
	var httpErr *alertHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Status == http.StatusTooManyRequests || httpErr.Status >= 500
	}
	var final *alertFinalError
	if errors.As(err, &final) {
		return false
	}
	return err != nil && !errors.Is(err, context.Canceled)
}

// alertFinalError marks a failure no retry can fix (a payload too large, a
// subscription that is gone).
type alertFinalError struct{ msg string }

func (e *alertFinalError) Error() string { return e.msg }

// sanitizeAlertError reduces a send error to something safe to log and
// serve. An *url.Error carries the full URL, ntfy topic included, so only
// its cause is kept, and any secret given is cut out of what remains.
func sanitizeAlertError(target, host string, err error, secrets ...string) string {
	if err == nil {
		return ""
	}
	var urlErr *url.Error
	cause := err
	if errors.As(err, &urlErr) {
		cause = urlErr.Err
	}
	text := cause.Error()
	switch {
	case errors.Is(cause, context.DeadlineExceeded) || strings.Contains(text, "Client.Timeout"):
		text = "timed out"
	case errors.Is(cause, context.Canceled):
		text = "canceled"
	}
	for _, secret := range secrets {
		// Very short strings are skipped: cutting "a" out of every word
		// hides nothing and wrecks the message.
		if len(secret) >= 4 {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	if len(text) > 200 {
		text = text[:200]
	}
	if host != "" {
		return target + " (" + host + "): " + text
	}
	return target + ": " + text
}

// alertDeliveryStatus is what /api/alerts shows about delivery.
type alertDeliveryStatus struct {
	Sent       int        `json:"sent"`
	Failed     int        `json:"failed"`
	Dropped    int        `json:"dropped"`
	LastError  string     `json:"last_error,omitempty"`
	LastErrAt  *time.Time `json:"last_error_at,omitempty"`
	LastSentAt *time.Time `json:"last_sent_at,omitempty"`
}

type alertDispatcher struct {
	targets []alertTarget
	queue   chan alertNotification
	now     func() time.Time
	sleep   func(context.Context, time.Duration) bool // true when canceled
	logf    func(string, ...any)

	mu       sync.Mutex
	tokens   float64
	refillAt time.Time
	status   alertDeliveryStatus
}

func newAlertDispatcher(targets []alertTarget, logf func(string, ...any)) *alertDispatcher {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &alertDispatcher{
		targets: targets,
		queue:   make(chan alertNotification, alertQueueSize),
		now:     time.Now,
		sleep:   sleepCanceled,
		logf:    logf,
		tokens:  alertRateBurst,
	}
}

// Enqueue takes a notice if the rate limit and the queue allow it. It never
// blocks.
func (d *alertDispatcher) Enqueue(n alertNotification) bool {
	if d == nil || len(d.targets) == 0 {
		return false
	}
	d.mu.Lock()
	now := d.now()
	if d.refillAt.IsZero() {
		d.refillAt = now
	}
	if elapsed := now.Sub(d.refillAt); elapsed > 0 {
		d.tokens += elapsed.Seconds() / alertRateRefillEvery.Seconds()
		if d.tokens > alertRateBurst {
			d.tokens = alertRateBurst
		}
		d.refillAt = now
	}
	if d.tokens < 1 {
		d.status.Dropped++
		d.mu.Unlock()
		d.logf("alerts: rate limit reached, %s notice for %s not sent", n.Kind, n.Rule)
		return false
	}
	d.tokens--
	d.mu.Unlock()
	select {
	case d.queue <- n:
		return true
	default:
		d.mu.Lock()
		d.status.Dropped++
		d.mu.Unlock()
		d.logf("alerts: send queue full, %s notice for %s not sent", n.Kind, n.Rule)
		return false
	}
}

// Run sends queued notices until ctx ends.
func (d *alertDispatcher) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case n := <-d.queue:
			d.deliver(ctx, n)
		}
	}
}

func (d *alertDispatcher) deliver(ctx context.Context, n alertNotification) {
	for _, target := range d.targets {
		err := target.Deliver(ctx, n, d.retry)
		d.mu.Lock()
		at := d.now().UTC()
		if err != nil {
			d.status.Failed++
			d.status.LastError = err.Error()
			d.status.LastErrAt = &at
		} else {
			d.status.Sent++
			d.status.LastSentAt = &at
		}
		d.mu.Unlock()
		if err != nil {
			// Targets return sanitized errors only.
			d.logf("alerts: %s notice for %s not delivered: %v", n.Kind, n.Rule, err)
		}
	}
}

func (d *alertDispatcher) retry(ctx context.Context, attempt func(context.Context) error) error {
	err := d.attempt(ctx, attempt)
	if err == nil || !alertRetryable(err) {
		return err
	}
	if d.sleep(ctx, alertRetryAfter) {
		return err
	}
	return d.attempt(ctx, attempt)
}

func (d *alertDispatcher) attempt(ctx context.Context, attempt func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, alertRequestTimeout)
	defer cancel()
	return attempt(ctx)
}

func (d *alertDispatcher) Status() alertDeliveryStatus {
	if d == nil {
		return alertDeliveryStatus{}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.status
}

// newAlertHTTPClient follows no redirects: a target answering with one is
// misconfigured, and following it would hand the token to another URL.
func newAlertHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 4
	transport.IdleConnTimeout = 90 * time.Second
	return &http.Client{
		Timeout:   alertRequestTimeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// doAlertRequest sends req and reads a little of the answer so the
// connection can be reused. The body is never put in an error: a target may
// echo what it was sent.
func doAlertRequest(client *http.Client, req *http.Request) (int, error) {
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, alertResponseDrain))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, &alertHTTPError{Status: resp.StatusCode}
	}
	return resp.StatusCode, nil
}

// ---- ntfy ----

type ntfyTarget struct {
	cfg       ntfyConfig
	client    *http.Client
	userAgent string
}

func (t *ntfyTarget) Name() string { return "ntfy" }

func (t *ntfyTarget) Deliver(ctx context.Context, n alertNotification, retry alertRetryFunc) error {
	err := retry(ctx, func(ctx context.Context) error {
		req, err := t.request(ctx, n)
		if err != nil {
			return err
		}
		_, err = doAlertRequest(t.client, req)
		return err
	})
	if err != nil {
		return errors.New(sanitizeAlertError("ntfy", alertTargetHost(t.cfg.URL), err, t.cfg.Token, ntfyTopic(t.cfg.URL)))
	}
	return nil
}

// request is ntfy's plain publish: the message is the body and the rest are
// headers. Header values are kept to printable ASCII, since sensor and disk
// names can be anything.
func (t *ntfyTarget) request(ctx context.Context, n alertNotification) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.cfg.URL, strings.NewReader(n.Message))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	req.Header.Set("User-Agent", t.userAgent)
	req.Header.Set("Title", asciiHeaderValue(n.Title, 200))
	priority := n.Priority
	if n.Kind == "firing" && t.cfg.Priority != "" && n.Priority == "high" {
		priority = t.cfg.Priority
	}
	req.Header.Set("Priority", firstNonEmpty(priority, "default"))
	tags := []string{map[string]string{"firing": "warning", "resolved": "white_check_mark", "test": "bell"}[n.Kind]}
	tags = append(tags, t.cfg.Tags...)
	req.Header.Set("Tags", strings.Join(nonEmptyStrings(tags), ","))
	if t.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+t.cfg.Token)
	}
	return req, nil
}

func ntfyTopic(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.Trim(u.Path, "/")
}

func asciiHeaderValue(s string, max int) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 0x20 && r <= 0x7e:
			b.WriteRune(r)
		case r == '\t' || r == '\r' || r == '\n':
			b.WriteByte(' ')
		default:
			b.WriteByte('?')
		}
		if b.Len() >= max {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

func nonEmptyStrings(values []string) []string {
	out := values[:0:0]
	for _, v := range values {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// ---- webhook ----

type webhookTarget struct {
	cfg       webhookConfig
	client    *http.Client
	userAgent string
	version   string
}

func (t *webhookTarget) Name() string { return "webhook" }

// webhookPayload is the JSON a webhook receives. It is documented in the
// README; add fields, never rename them.
type webhookPayload struct {
	Source    string     `json:"source"`
	Version   string     `json:"version"`
	Host      string     `json:"host"`
	Kind      string     `json:"kind"`
	Rule      string     `json:"rule"`
	Metric    string     `json:"metric,omitempty"`
	Instance  string     `json:"instance,omitempty"`
	Label     string     `json:"label,omitempty"`
	Value     *float64   `json:"value,omitempty"`
	Threshold *float64   `json:"threshold,omitempty"`
	Direction string     `json:"direction,omitempty"`
	Unit      string     `json:"unit,omitempty"`
	Title     string     `json:"title"`
	Message   string     `json:"message"`
	At        time.Time  `json:"at"`
	FiredAt   *time.Time `json:"fired_at,omitempty"`
}

func (t *webhookTarget) payload(n alertNotification) webhookPayload {
	p := webhookPayload{
		Source:    "sysmon-agent",
		Version:   t.version,
		Host:      n.Host,
		Kind:      n.Kind,
		Rule:      n.Rule,
		Metric:    n.Metric,
		Instance:  n.Instance,
		Label:     n.Label,
		Direction: n.Direction,
		Unit:      n.Unit,
		Title:     n.Title,
		Message:   n.Message,
		At:        n.At,
	}
	if !n.FiredAt.IsZero() {
		firedAt := n.FiredAt
		p.FiredAt = &firedAt
	}
	if n.Kind != "test" {
		if n.Known {
			value := n.Value
			p.Value = &value
		}
		threshold := n.Threshold
		p.Threshold = &threshold
	}
	return p
}

func (t *webhookTarget) Deliver(ctx context.Context, n alertNotification, retry alertRetryFunc) error {
	body, err := json.Marshal(t.payload(n))
	if err != nil {
		return err
	}
	err = retry(ctx, func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.cfg.URL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", t.userAgent)
		if t.cfg.Token != "" {
			req.Header.Set("Authorization", "Bearer "+t.cfg.Token)
		}
		_, err = doAlertRequest(t.client, req)
		return err
	})
	if err != nil {
		u, _ := url.Parse(t.cfg.URL)
		secrets := []string{t.cfg.Token}
		if u != nil {
			secrets = append(secrets, strings.Trim(u.Path, "/"), u.RawQuery)
		}
		return errors.New(sanitizeAlertError("webhook", alertTargetHost(t.cfg.URL), err, secrets...))
	}
	return nil
}
