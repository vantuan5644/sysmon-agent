package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The internet latency prober times a TCP connect to one target (default
// 1.1.1.1:443, -latency-target / SYSMON_LATENCY_TARGET, "off" disables it):
// the handshake's SYN to SYN-ACK is one round trip, needs no privilege and no
// exec, and works where ICMP is filtered. The connection is closed as soon as
// it opens; nothing is sent over it. A target given by name is resolved first
// and separately, so the figure is the network's round trip, not DNS's.
//
// It probes every latencyProbeEvery (5 s) while there is demand (a dashboard
// open or /api/metrics fetched recently), and is parked otherwise, like the
// slow lane: an idle agent sends no traffic at all. The window it reports is
// the probes of the last latencyWindow (5 min), so it says "while someone was
// watching", and right after a dashboard opens it holds only the probes since.
//
// Any failed probe (refused, timed out, unresolvable) is loss. Loss is a
// measurement, not a collection failure, so it never reaches
// collection_errors.

const (
	latencyProbeEvery   = 5 * time.Second
	latencyProbeTimeout = 2 * time.Second
	latencyWindow       = 5 * time.Minute
	latencyDefaultPort  = "443"
	// DefaultLatencyTarget is the -latency-target default.
	DefaultLatencyTarget = "1.1.1.1:443"
	latencyWaitingError  = "waiting for the first probe"
)

// NetworkLatency is metrics.network.latency: the newest probe and the window
// behind it. Times are milliseconds. Last is the newest probe (unavailable,
// with its error, when that probe failed); Average, Min and Max cover the
// window's successful probes; Loss is the share of the window's probes that
// failed. Available is false only before the first probe of a window.
type NetworkLatency struct {
	Available       bool         `json:"available"`
	Target          string       `json:"target"`
	Method          string       `json:"method"`
	IntervalSeconds int          `json:"interval_seconds"`
	WindowSeconds   int          `json:"window_seconds"`
	Last            NumberMetric `json:"last_ms"`
	Average         NumberMetric `json:"avg_ms"`
	Min             NumberMetric `json:"min_ms"`
	Max             NumberMetric `json:"max_ms"`
	Loss            NumberMetric `json:"loss_percent"`
	Probes          int          `json:"probes"`
	Failures        int          `json:"failures"`
	UpdatedAt       *time.Time   `json:"updated_at,omitempty"`
	Error           string       `json:"error,omitempty"`
}

// parseLatencyTarget reads -latency-target. It returns enabled=false for "off"
// (or an empty value), and adds port 443 to a target that has none.
func parseLatencyTarget(value string) (target string, enabled bool, err error) {
	value = strings.TrimSpace(value)
	switch strings.ToLower(value) {
	case "", "off", "none", "false", "0", "disabled":
		return "", false, nil
	}
	host, port, splitErr := net.SplitHostPort(value)
	if splitErr != nil {
		// No port: a bare name, IPv4 or IPv6 address (with or without
		// brackets).
		bare := strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
		if strings.ContainsAny(bare, "[]") || (strings.Contains(bare, ":") && net.ParseIP(bare) == nil) {
			return "", false, fmt.Errorf("latency target %q must be host or host:port", value)
		}
		host, port = bare, latencyDefaultPort
	}
	if host == "" || strings.ContainsAny(host, " /\t") {
		return "", false, fmt.Errorf("latency target %q has no usable host", value)
	}
	n, convErr := strconv.Atoi(port)
	if convErr != nil || n < 1 || n > 65535 {
		return "", false, fmt.Errorf("latency target %q has an invalid port", value)
	}
	return net.JoinHostPort(host, port), true, nil
}

type latencySample struct {
	at  time.Time
	rtt time.Duration
	err string // empty for a successful probe
}

type latencyProber struct {
	every   time.Duration
	timeout time.Duration
	window  time.Duration
	now     func() time.Time
	dial    func(ctx context.Context, network, address string) (net.Conn, error)
	resolve func(ctx context.Context, host string) ([]net.IPAddr, error)

	mu         sync.Mutex
	target     string
	generation uint64
	samples    []latencySample
	summary    *NetworkLatency // rebuilt on every probe, never mutated after
}

func newLatencyProber(target string) (*latencyProber, error) {
	normalized, enabled, err := parseLatencyTarget(target)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, nil
	}
	var dialer net.Dialer
	p := &latencyProber{
		every:   latencyProbeEvery,
		timeout: latencyProbeTimeout,
		window:  latencyWindow,
		now:     time.Now,
		dial:    dialer.DialContext,
		resolve: net.DefaultResolver.LookupIPAddr,
		target:  normalized,
	}
	p.summary = summarizeLatency(normalized, nil, p.every, p.window)
	return p, nil
}

// SetTarget points the prober somewhere else and starts a new window, since
// the old probes measured a different path.
func (p *latencyProber) SetTarget(target string) error {
	normalized, enabled, err := parseLatencyTarget(target)
	if err != nil {
		return err
	}
	if !enabled {
		return errors.New("the latency prober cannot be turned off once running")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if normalized == p.target {
		return nil
	}
	p.target = normalized
	p.generation++
	p.samples = nil
	p.summary = summarizeLatency(normalized, nil, p.every, p.window)
	return nil
}

// Run probes every p.every while there is demand and parks otherwise.
func (p *latencyProber) Run(ctx context.Context, demand func() bool) {
	var last time.Time
	for {
		if serviceDue(demand, last, p.every, p.now()) {
			last = p.now()
			p.probe(ctx)
			if ctx.Err() != nil {
				return
			}
		}
		if sleepCanceled(ctx, serviceWait(last, p.every, p.now())) {
			return
		}
	}
}

// probe times one TCP connect and adds it to the window.
func (p *latencyProber) probe(ctx context.Context) {
	p.mu.Lock()
	target, generation := p.target, p.generation
	p.mu.Unlock()

	rtt, err := p.connect(ctx, target)
	if ctx.Err() != nil {
		return
	}
	sample := latencySample{at: p.now(), rtt: rtt}
	if err != nil {
		sample.err = err.Error()
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if generation != p.generation {
		return // the target changed while this probe ran
	}
	p.samples = append(pruneLatencySamples(p.samples, sample.at, p.window), sample)
	p.summary = summarizeLatency(target, p.samples, p.every, p.window)
}

func (p *latencyProber) connect(ctx context.Context, target string) (time.Duration, error) {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return 0, err
	}
	address := target
	if net.ParseIP(host) == nil {
		resolveCtx, cancel := context.WithTimeout(ctx, p.timeout)
		addrs, err := p.resolve(resolveCtx, host)
		cancel()
		if err != nil {
			return 0, fmt.Errorf("cannot resolve %s: %v", host, describeLatencyError(err, p.timeout))
		}
		if len(addrs) == 0 {
			return 0, fmt.Errorf("cannot resolve %s: no addresses", host)
		}
		address = net.JoinHostPort(addrs[0].IP.String(), port)
	}
	dialCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	started := time.Now()
	conn, err := p.dial(dialCtx, "tcp", address)
	rtt := time.Since(started)
	if err != nil {
		return 0, describeLatencyError(err, p.timeout)
	}
	_ = conn.Close()
	return rtt, nil
}

func describeLatencyError(err error, timeout time.Duration) error {
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
		return fmt.Errorf("no answer within %s", timeout)
	case errors.Is(err, syscall.ECONNREFUSED):
		return errors.New("connection refused")
	case errors.Is(err, syscall.ENETUNREACH):
		return errors.New("network unreachable")
	case errors.Is(err, syscall.EHOSTUNREACH):
		return errors.New("host unreachable")
	}
	return err
}

// pruneLatencySamples drops the samples older than window, reusing the slice.
func pruneLatencySamples(samples []latencySample, now time.Time, window time.Duration) []latencySample {
	cutoff := now.Add(-window)
	keep := 0
	for keep < len(samples) && !samples[keep].at.After(cutoff) {
		keep++
	}
	if keep == 0 {
		return samples
	}
	return append(samples[:0], samples[keep:]...)
}

func summarizeLatency(target string, samples []latencySample, every, window time.Duration) *NetworkLatency {
	out := &NetworkLatency{
		Available:       len(samples) > 0,
		Target:          target,
		Method:          "tcp-connect",
		IntervalSeconds: int(every / time.Second),
		WindowSeconds:   int(window / time.Second),
		Probes:          len(samples),
	}
	if len(samples) == 0 {
		out.Error = latencyWaitingError
		out.Last = unavailableNumber("ms", latencyWaitingError)
		out.Average = out.Last
		out.Min = out.Last
		out.Max = out.Last
		out.Loss = unavailableNumber("%", latencyWaitingError)
		return out
	}
	newest := samples[len(samples)-1]
	at := newest.at.UTC()
	out.UpdatedAt = &at
	if newest.err != "" {
		out.Last = unavailableNumber("ms", newest.err)
	} else {
		out.Last = availableNumber(latencyMS(newest.rtt), "ms")
	}
	var sum, lo, hi float64
	ok := 0
	for _, s := range samples {
		if s.err != "" {
			out.Failures++
			continue
		}
		ms := latencyMS(s.rtt)
		if ok == 0 || ms < lo {
			lo = ms
		}
		if ok == 0 || ms > hi {
			hi = ms
		}
		sum += ms
		ok++
	}
	if ok == 0 {
		message := "no successful probe in the window"
		out.Average = unavailableNumber("ms", message)
		out.Min = out.Average
		out.Max = out.Average
	} else {
		out.Average = availableNumber(math.Round(sum/float64(ok)*100)/100, "ms")
		out.Min = availableNumber(lo, "ms")
		out.Max = availableNumber(hi, "ms")
	}
	out.Loss = availableNumber(math.Round(float64(out.Failures)/float64(len(samples))*1000)/10, "%")
	return out
}

func latencyMS(d time.Duration) float64 {
	return math.Round(float64(d)/float64(time.Millisecond)*100) / 100
}

// Attach publishes the window as metrics.network.latency.
func (p *latencyProber) Attach(metrics *Metrics) {
	p.mu.Lock()
	summary := p.summary
	p.mu.Unlock()
	metrics.Network.Latency = summary
}
