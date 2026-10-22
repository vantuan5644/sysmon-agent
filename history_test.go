package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func historyMetricsAt(at time.Time, cpu, mem, gpu, rx float64) Metrics {
	m := baseMetrics("history-host")
	m.Timestamp = at
	m.CPU = availableNumber(cpu, "%")
	m.Memory = availableCapacity(uint64(mem), 100)
	m.GPU = GPUSet{Available: true, Devices: []GPUMetric{
		{Name: "idle iGPU", Usage: unavailableNumber("%", "not exposed")},
		{Name: "dGPU", Usage: availableNumber(gpu, "%")},
	}}
	m.Network = NetworkSet{Available: true, Interfaces: []NetworkInterfaceMetric{
		{Name: "eth0", RXBytesPerSecond: availableNumber(rx, "B/s"), TXBytesPerSecond: availableNumber(1, "B/s")},
		{Name: "wlan0", RXBytesPerSecond: availableNumber(rx, "B/s"), TXBytesPerSecond: unavailableNumber("B/s", "warming up")},
	}}
	return m
}

func historyValue(t *testing.T, name string, value *float64) float64 {
	t.Helper()
	if value == nil {
		t.Fatalf("%s is nil, want a value", name)
	}
	return *value
}

// TestMetricsHistoryKeepsTheBucketPeak: a 5 s bucket covers ~25 fast-lane
// snapshots, and the question a glance dashboard asks is whether anything
// spiked. The bucket must hold the highest reading, not the last one.
func TestMetricsHistoryKeepsTheBucketPeak(t *testing.T) {
	h := newMetricsHistory(5*time.Second, 15*time.Minute)
	base := time.UnixMilli(1_760_000_000_000) // a multiple of 5000 ms
	h.observe(historyMetricsAt(base.Add(100*time.Millisecond), 10, 40, 5, 100), true)
	h.observe(historyMetricsAt(base.Add(2*time.Second), 90, 41, 50, 900), true)
	h.observe(historyMetricsAt(base.Add(4900*time.Millisecond), 20, 39, 7, 300), true)
	h.observe(historyMetricsAt(base.Add(5*time.Second), 30, 42, 8, 200), true)

	got := h.snapshot()
	if got.StepSeconds != 5 || got.RetentionSeconds != 900 {
		t.Fatalf("step/retention = %d/%d, want 5/900", got.StepSeconds, got.RetentionSeconds)
	}
	if len(got.Points) != 2 {
		t.Fatalf("points = %d, want 2 buckets", len(got.Points))
	}
	first := got.Points[0]
	if first.T != base.UnixMilli() {
		t.Fatalf("bucket start = %d, want %d", first.T, base.UnixMilli())
	}
	if v := historyValue(t, "cpu", first.CPU); v != 90 {
		t.Fatalf("cpu peak = %v, want 90", v)
	}
	if v := historyValue(t, "mem", first.Memory); v != 41 {
		t.Fatalf("mem peak = %v, want 41", v)
	}
	// The GPU gauge shows the first device reporting usage; history follows it.
	if v := historyValue(t, "gpu", first.GPU); v != 50 {
		t.Fatalf("gpu peak = %v, want 50", v)
	}
	// Interface rates sum like the NET gauge; an unavailable rate adds nothing.
	if v := historyValue(t, "net_rx", first.NetRX); v != 1800 {
		t.Fatalf("net_rx peak = %v, want 1800", v)
	}
	if v := historyValue(t, "net_tx", first.NetTX); v != 1 {
		t.Fatalf("net_tx peak = %v, want 1", v)
	}
	if v := historyValue(t, "cpu", got.Points[1].CPU); v != 30 {
		t.Fatalf("second bucket cpu = %v, want 30", v)
	}
}

// TestMetricsHistoryRecordsAParkedSlowLaneAsAGap: with nobody watching, the
// slow lane stops and its last GPU/network values sit in every fast-lane
// snapshot. Recording them would draw a flat line through a period nobody
// measured, so those buckets carry nil while CPU/RAM stay complete.
func TestMetricsHistoryRecordsAParkedSlowLaneAsAGap(t *testing.T) {
	h := newMetricsHistory(5*time.Second, 15*time.Minute)
	base := time.UnixMilli(1_760_000_000_000)
	h.observe(historyMetricsAt(base, 12, 40, 66, 500), false)

	point := h.snapshot().Points[0]
	if point.CPU == nil || point.Memory == nil {
		t.Fatalf("fast-lane fields missing from a parked bucket: %+v", point)
	}
	if point.GPU != nil || point.NetRX != nil || point.NetTX != nil {
		t.Fatalf("parked slow lane recorded stale readings: gpu=%v rx=%v tx=%v", point.GPU, point.NetRX, point.NetTX)
	}

	encoded, err := json.Marshal(point)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if value, present := wire["gpu"]; !present || value != nil {
		t.Fatalf("gap must encode as an explicit null, got %s", encoded)
	}
}

// TestMetricsHistoryTrimsToRetentionAndSurvivesAClockStep covers the two ways
// the ring changes shape: age, and the host clock moving backwards (points
// "from the future" would otherwise pin the end of every chart).
func TestMetricsHistoryTrimsToRetentionAndSurvivesAClockStep(t *testing.T) {
	h := newMetricsHistory(5*time.Second, time.Minute)
	base := time.UnixMilli(1_760_000_000_000)
	for i := 0; i < 30; i++ {
		h.observe(historyMetricsAt(base.Add(time.Duration(i)*5*time.Second), float64(i), 1, 1, 1), true)
	}
	points := h.snapshot().Points
	if len(points) != 12 {
		t.Fatalf("points after a minute of retention = %d, want 12", len(points))
	}
	if newest := points[len(points)-1].T; newest != base.Add(29*5*time.Second).UnixMilli() {
		t.Fatalf("newest bucket = %d, want the last observed", newest)
	}
	if oldest := points[0].T; oldest <= points[len(points)-1].T-time.Minute.Milliseconds() {
		t.Fatalf("bucket %d is older than the retention window", oldest)
	}

	h.observe(historyMetricsAt(base, 99, 1, 1, 1), true)
	points = h.snapshot().Points
	if len(points) != 1 || points[0].T != base.UnixMilli() {
		t.Fatalf("after a backwards clock step points = %+v, want one bucket at %d", points, base.UnixMilli())
	}
}

// TestSamplerServesHistoryWithoutRegisteringDemand drives the real sampler:
// published snapshots land in /api/history, and reading it must not count as
// demand, or every history fetch would wake the expensive slow lane.
func TestSamplerServesHistoryWithoutRegisteringDemand(t *testing.T) {
	s := newSampler(&fakeLaneCollector{hostname: "history-host"}, 0, 0)
	s.fastEvery = 20 * time.Millisecond
	s.Start()
	defer s.Stop()
	waitForCond(t, 2*time.Second, func() bool {
		return len(s.History().Points) > 0 && s.History().Points[0].CPU != nil
	})

	handler, err := newHTTPHandlerWithController(s, testStaticFS(), NewMemoryRuntimeState(), &fakeController{})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.lastDemandAt = time.Time{}
	s.mu.Unlock()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/history", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/history = %d, body %s", rec.Code, rec.Body.String())
	}
	var body HistoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.StepSeconds != int(historyStep/time.Second) || len(body.Points) == 0 {
		t.Fatalf("history body = %+v", body)
	}
	if s.hasDemand() {
		t.Fatal("reading /api/history registered demand")
	}
}

// TestHistoryRouteIsAbsentWithoutASampler: a plain collector keeps no history,
// so the route must 404, which the dashboard reads as "older agent".
func TestHistoryRouteIsAbsentWithoutASampler(t *testing.T) {
	handler, err := newHTTPHandlerWithController(fakeCollector{metrics: baseMetrics("plain")}, testStaticFS(), NewMemoryRuntimeState(), &fakeController{})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/history", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /api/history without a sampler = %d, want 404", rec.Code)
	}
}
