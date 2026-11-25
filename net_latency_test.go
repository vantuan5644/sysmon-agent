package main

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestLatencyProber(t *testing.T, target string) *latencyProber {
	t.Helper()
	p, err := newLatencyProber(target)
	if err != nil || p == nil {
		t.Fatalf("newLatencyProber(%q) = %v, %v", target, p, err)
	}
	p.timeout = 500 * time.Millisecond
	return p
}

func TestLatencyProbeSucceedsAgainstALocalListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	p := newTestLatencyProber(t, ln.Addr().String())

	var before Metrics
	p.Attach(&before)
	if l := before.Network.Latency; l == nil || l.Available || l.Error != latencyWaitingError || l.Target != ln.Addr().String() {
		t.Fatalf("before the first probe: %+v", l)
	}

	p.probe(context.Background())
	p.probe(context.Background())
	var metrics Metrics
	p.Attach(&metrics)
	l := metrics.Network.Latency
	if l == nil || !l.Available || l.Method != "tcp-connect" || l.Probes != 2 || l.Failures != 0 {
		t.Fatalf("latency = %+v", l)
	}
	if !l.Last.Available || l.Last.Unit != "ms" || l.Last.Value < 0 || l.Last.Value > 500 {
		t.Fatalf("last = %+v", l.Last)
	}
	if !l.Average.Available || !l.Min.Available || !l.Max.Available || l.Min.Value > l.Max.Value {
		t.Fatalf("avg/min/max = %+v %+v %+v", l.Average, l.Min, l.Max)
	}
	if !l.Loss.Available || l.Loss.Value != 0 {
		t.Fatalf("loss = %+v", l.Loss)
	}
	if l.IntervalSeconds != 5 || l.WindowSeconds != 300 || l.UpdatedAt == nil {
		t.Fatalf("interval/window/updated = %d %d %v", l.IntervalSeconds, l.WindowSeconds, l.UpdatedAt)
	}
	if len(metrics.CollectionErrors) != 0 {
		t.Fatalf("errors = %v", metrics.CollectionErrors)
	}
}

func TestLatencyClosedPortCountsAsLoss(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().String()
	ln.Close()
	p := newTestLatencyProber(t, closed)
	p.probe(context.Background())
	var metrics Metrics
	p.Attach(&metrics)
	l := metrics.Network.Latency
	if !l.Available || l.Last.Available || l.Last.Error == "" {
		t.Fatalf("latency = %+v", l)
	}
	if !l.Loss.Available || l.Loss.Value != 100 || l.Failures != 1 || l.Average.Available {
		t.Fatalf("loss = %+v failures=%d avg=%+v", l.Loss, l.Failures, l.Average)
	}
	if len(metrics.CollectionErrors) != 0 {
		t.Fatalf("loss must not be a collection error: %v", metrics.CollectionErrors)
	}
}

func TestLatencyTimeoutCountsAsLoss(t *testing.T) {
	p := newTestLatencyProber(t, "192.0.2.1:443")
	p.timeout = 50 * time.Millisecond
	// A dialer that never connects: the probe's own deadline must end it.
	p.dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	started := time.Now()
	p.probe(context.Background())
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("probe took %s, want about the 50ms timeout", elapsed)
	}
	var metrics Metrics
	p.Attach(&metrics)
	l := metrics.Network.Latency
	if l.Last.Available || !strings.Contains(l.Last.Error, "no answer within") || l.Loss.Value != 100 {
		t.Fatalf("latency = %+v", l)
	}
}

func TestLatencyResolvesNamesOutsideTheTimedConnect(t *testing.T) {
	p := newTestLatencyProber(t, "probe.example")
	var dialed string
	p.resolve = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("192.0.2.7")}}, nil
	}
	p.dial = func(_ context.Context, _, address string) (net.Conn, error) {
		dialed = address
		client, server := net.Pipe()
		server.Close()
		return client, nil
	}
	p.probe(context.Background())
	if dialed != "192.0.2.7:443" {
		t.Fatalf("dialed %q, want the resolved address with the default port", dialed)
	}

	p.resolve = func(context.Context, string) ([]net.IPAddr, error) { return nil, errors.New("no such host") }
	p.probe(context.Background())
	var metrics Metrics
	p.Attach(&metrics)
	l := metrics.Network.Latency
	if l.Probes != 2 || l.Failures != 1 || l.Loss.Value != 50 || !strings.Contains(l.Last.Error, "cannot resolve") {
		t.Fatalf("latency = %+v", l)
	}
}

func TestLatencyWindowPrunesAndSummarizes(t *testing.T) {
	p := newTestLatencyProber(t, "192.0.2.1:443")
	clock := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return clock }
	results := []error{nil, nil, errors.New("refused"), nil}
	i := 0
	p.dial = func(context.Context, string, string) (net.Conn, error) {
		err := results[i%len(results)]
		i++
		if err != nil {
			return nil, err
		}
		client, server := net.Pipe()
		server.Close()
		return client, nil
	}
	for n := 0; n < 4; n++ {
		p.probe(context.Background())
		clock = clock.Add(time.Minute)
	}
	var metrics Metrics
	p.Attach(&metrics)
	if l := metrics.Network.Latency; l.Probes != 4 || l.Failures != 1 || l.Loss.Value != 25 {
		t.Fatalf("window = %+v", l)
	}
	// Four minutes later the first three probes have left the window.
	clock = clock.Add(4*time.Minute - time.Second)
	p.probe(context.Background()) // results[0]: success
	p.Attach(&metrics)
	if l := metrics.Network.Latency; l.Probes != 2 || l.Failures != 0 || l.Loss.Value != 0 {
		t.Fatalf("pruned window = %+v", l)
	}

	samples := []latencySample{
		{rtt: 10 * time.Millisecond},
		{rtt: 30 * time.Millisecond},
		{err: "refused"},
		{rtt: 20 * time.Millisecond},
	}
	s := summarizeLatency("x:1", samples, 5*time.Second, 5*time.Minute)
	if s.Average.Value != 20 || s.Min.Value != 10 || s.Max.Value != 30 || s.Last.Value != 20 || s.Loss.Value != 25 {
		t.Fatalf("summary = %+v", s)
	}
}

func TestLatencySetTargetResetsTheWindow(t *testing.T) {
	p := newTestLatencyProber(t, "192.0.2.1:443")
	p.dial = func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("refused") }
	p.probe(context.Background())
	if err := p.SetTarget("192.0.2.1:443"); err != nil {
		t.Fatal(err)
	}
	var metrics Metrics
	p.Attach(&metrics)
	if metrics.Network.Latency.Probes != 1 {
		t.Fatal("setting the same target must keep the window")
	}
	if err := p.SetTarget("192.0.2.2"); err != nil {
		t.Fatal(err)
	}
	p.Attach(&metrics)
	if l := metrics.Network.Latency; l.Probes != 0 || l.Available || l.Target != "192.0.2.2:443" {
		t.Fatalf("after a new target: %+v", l)
	}
	if err := p.SetTarget("not a target:99999"); err == nil {
		t.Fatal("invalid target accepted")
	}
}

func TestLatencyRunIsDemandGated(t *testing.T) {
	p := newTestLatencyProber(t, "192.0.2.1:443")
	var dials atomic.Int32
	p.dial = func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("refused")
	}
	var demand atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(ctx, demand.Load)
	}()
	time.Sleep(1500 * time.Millisecond)
	if n := dials.Load(); n != 0 {
		cancel()
		<-done
		t.Fatalf("parked prober probed %d times", n)
	}
	demand.Store(true)
	deadline := time.Now().Add(3 * time.Second)
	for dials.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if dials.Load() == 0 {
		t.Fatal("prober never probed once there was demand")
	}
}

func TestParseLatencyTarget(t *testing.T) {
	cases := []struct {
		in, want string
		enabled  bool
		bad      bool
	}{
		{"1.1.1.1:443", "1.1.1.1:443", true, false},
		{"1.1.1.1", "1.1.1.1:443", true, false},
		{"one.one.one.one:80", "one.one.one.one:80", true, false},
		{"2606:4700:4700::1111", "[2606:4700:4700::1111]:443", true, false},
		{"[2606:4700:4700::1111]:53", "[2606:4700:4700::1111]:53", true, false},
		{"off", "", false, false},
		{"OFF", "", false, false},
		{"", "", false, false},
		{"host:0", "", false, true},
		{"host:http", "", false, true},
		{"a b:443", "", false, true},
		{":443", "", false, true},
	}
	for _, tc := range cases {
		got, enabled, err := parseLatencyTarget(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("parseLatencyTarget(%q) accepted", tc.in)
			}
			continue
		}
		if err != nil || got != tc.want || enabled != tc.enabled {
			t.Errorf("parseLatencyTarget(%q) = %q, %v, %v; want %q, %v", tc.in, got, enabled, err, tc.want, tc.enabled)
		}
	}
	if p, err := newLatencyProber("off"); p != nil || err != nil {
		t.Fatalf("off = %v, %v", p, err)
	}
}
