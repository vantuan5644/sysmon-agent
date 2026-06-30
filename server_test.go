package main

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

type fakeCollector struct {
	metrics Metrics
	err     error
}

func (f fakeCollector) Collect(ctx context.Context) (Metrics, error) {
	return f.metrics, f.err
}

type countingCollector struct {
	mu    sync.Mutex
	calls int
}

func (c *countingCollector) Collect(ctx context.Context) (Metrics, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	metrics := baseMetrics("test-host")
	metrics.CPU = availableNumber(float64(c.calls), "%")
	metrics.Memory = availableCapacity(25, 100)
	return metrics, nil
}

func (c *countingCollector) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

type blockingCollector struct {
	metrics Metrics
	started chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	calls   int
}

func (b *blockingCollector) Collect(ctx context.Context) (Metrics, error) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	b.once.Do(func() { close(b.started) })

	select {
	case <-b.release:
		return b.metrics, nil
	case <-ctx.Done():
		return Metrics{}, ctx.Err()
	}
}

func (b *blockingCollector) Calls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func testStaticFS() fs.FS {
	return fstest.MapFS{
		"static/index.html": &fstest.MapFile{Data: []byte("<!doctype html><title>test</title>")},
	}
}

func readyTestMetrics(hostname string) Metrics {
	metrics := baseMetrics(hostname)
	metrics.CPU = availableNumber(12.5, "%")
	metrics.Memory = availableCapacity(25, 100)
	metrics.Disks = []DiskMetric{{
		Name:       "root",
		Mountpoint: "/",
		FSType:     "ext4",
		Capacity:   availableCapacity(40, 100),
	}}
	metrics.Network = NetworkSet{Available: false, Error: "network sampler is warming up"}
	metrics.Tailscale = TailscaleStatus{Available: false, Error: "tailscale status unavailable"}
	metrics.Temperatures = TemperatureSet{Available: false, Error: "temperature sensors unavailable"}
	metrics.GPU = GPUSet{Available: false, Error: "GPU telemetry unavailable"}
	return metrics
}

func TestHealthz(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" {
		t.Fatalf("status body = %q, want ok", body["status"])
	}
}

func TestReadyzCollectsAndValidatesMetrics(t *testing.T) {
	metrics := readyTestMetrics("test-host")
	metrics.CollectionErrors = []string{
		"temperatures: temperature sensors unavailable",
		"gpu: GPU telemetry unavailable",
	}
	handler, err := newHTTPHandler(fakeCollector{metrics: metrics}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var got ReadinessStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "ok" || !got.Metrics || got.Hostname != "test-host" || got.Timestamp.IsZero() {
		t.Fatalf("readyz = %+v, want ok metrics readiness for test-host", got)
	}
	if len(got.CollectionErrors) != 2 || got.CollectionErrors[0] != "temperatures: temperature sensors unavailable" {
		t.Fatalf("readyz collection errors = %#v, want optional collector summaries", got.CollectionErrors)
	}
}

func TestReadyzFailsWhenCollectorFails(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{err: errors.New("collector offline")}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	var got ReadinessStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "not_ready" || got.Metrics || !strings.Contains(got.Error, "collector offline") {
		t.Fatalf("readyz = %+v, want collector error", got)
	}
}

func TestReadyzFailsInvalidMetricsShape(t *testing.T) {
	metrics := baseMetrics("test-host")
	metrics.CPU = availableNumber(12.5, "%")
	metrics.Memory = availableCapacity(25, 100)
	handler, err := newHTTPHandler(fakeCollector{metrics: metrics}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	var got ReadinessStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "not_ready" || got.Metrics || !strings.Contains(got.Error, "metrics schema") {
		t.Fatalf("readyz = %+v, want metrics schema error", got)
	}
}

func TestMetricsHandler(t *testing.T) {
	metrics := baseMetrics("test-host")
	metrics.CPU = availableNumber(12.5, "%")
	metrics.Memory = availableCapacity(25, 100)
	handler, err := newHTTPHandler(fakeCollector{metrics: metrics}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/metrics", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var got Metrics
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Hostname != "test-host" {
		t.Fatalf("hostname = %q, want test-host", got.Hostname)
	}
	if !got.CPU.Available || got.CPU.Value != 12.5 {
		t.Fatalf("CPU metric = %+v", got.CPU)
	}
}

func TestMetricsHandlerCachesNearDuplicateRequests(t *testing.T) {
	collector := &countingCollector{}
	handler, err := newHTTPHandler(collector, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	first := httptest.NewRecorder()
	firstReq := httptest.NewRequest(http.MethodGet, "/api/metrics", nil)
	handler.ServeHTTP(first, firstReq)
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want %d", first.Code, http.StatusOK)
	}

	second := httptest.NewRecorder()
	secondReq := httptest.NewRequest(http.MethodGet, "/api/metrics", nil)
	handler.ServeHTTP(second, secondReq)
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, want %d", second.Code, http.StatusOK)
	}

	if calls := collector.Calls(); calls != 1 {
		t.Fatalf("collector calls = %d, want one cached collection", calls)
	}

	var got Metrics
	if err := json.Unmarshal(second.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.CPU.Value != 1 {
		t.Fatalf("second CPU value = %v, want cached first value", got.CPU.Value)
	}
}

func TestMetricsHandlerCoalescesConcurrentRequests(t *testing.T) {
	metrics := baseMetrics("test-host")
	metrics.CPU = availableNumber(12.5, "%")
	metrics.Memory = availableCapacity(25, 100)
	collector := &blockingCollector{
		metrics: metrics,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	handler, err := newHTTPHandler(collector, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	first := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/metrics", nil)
		handler.ServeHTTP(rec, req)
		first <- rec
	}()

	select {
	case <-collector.started:
	case <-time.After(time.Second):
		t.Fatal("first metrics collection did not start")
	}

	second := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/metrics", nil)
		handler.ServeHTTP(rec, req)
		second <- rec
	}()

	time.Sleep(20 * time.Millisecond)
	if calls := collector.Calls(); calls != 1 {
		t.Fatalf("collector calls before release = %d, want one shared in-flight collection", calls)
	}
	close(collector.release)

	for name, done := range map[string]chan *httptest.ResponseRecorder{"first": first, "second": second} {
		select {
		case rec := <-done:
			if rec.Code != http.StatusOK {
				t.Fatalf("%s response status = %d, want %d", name, rec.Code, http.StatusOK)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s response did not complete", name)
		}
	}
	if calls := collector.Calls(); calls != 1 {
		t.Fatalf("collector calls after both responses = %d, want one", calls)
	}
}

func TestMetricsHandlerKeepsSharedCollectionAfterCallerCancel(t *testing.T) {
	metrics := baseMetrics("test-host")
	metrics.CPU = availableNumber(12.5, "%")
	metrics.Memory = availableCapacity(25, 100)
	collector := &blockingCollector{
		metrics: metrics,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	handler, err := newHTTPHandler(collector, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/metrics", nil).WithContext(firstCtx)
		handler.ServeHTTP(rec, req)
		first <- rec
	}()

	select {
	case <-collector.started:
	case <-time.After(time.Second):
		t.Fatal("first metrics collection did not start")
	}

	second := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/metrics", nil)
		handler.ServeHTTP(rec, req)
		second <- rec
	}()

	time.Sleep(20 * time.Millisecond)
	cancelFirst()
	time.Sleep(20 * time.Millisecond)
	if calls := collector.Calls(); calls != 1 {
		t.Fatalf("collector calls after caller cancellation = %d, want one shared collection", calls)
	}
	close(collector.release)

	for name, done := range map[string]chan *httptest.ResponseRecorder{"first": first, "second": second} {
		select {
		case rec := <-done:
			if rec.Code != http.StatusOK {
				t.Fatalf("%s response status = %d, want %d", name, rec.Code, http.StatusOK)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s response did not complete", name)
		}
	}
	if calls := collector.Calls(); calls != 1 {
		t.Fatalf("collector calls after both responses = %d, want one", calls)
	}
}

func TestMetricsHandlerError(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{err: errors.New("boom")}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/metrics", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestMetricsHandlerReportsCollectorPanicAsJSONError(t *testing.T) {
	collector := &panicOnceMetricsCollector{metrics: completeCoreMetrics()}
	handler, err := newHTTPHandler(collector, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	first := httptest.NewRecorder()
	firstReq := httptest.NewRequest(http.MethodGet, "/api/metrics", nil)
	handler.ServeHTTP(first, firstReq)
	if first.Code != http.StatusInternalServerError {
		t.Fatalf("first status = %d, want %d", first.Code, http.StatusInternalServerError)
	}
	if got := first.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("first Content-Type = %q, want application/json", got)
	}
	var body map[string]string
	if err := json.Unmarshal(first.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body["error"], "metrics collection panicked: collector boom") {
		t.Fatalf("first error = %q, want collector panic error", body["error"])
	}

	second := httptest.NewRecorder()
	secondReq := httptest.NewRequest(http.MethodGet, "/api/metrics", nil)
	handler.ServeHTTP(second, secondReq)
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, want %d: %s", second.Code, http.StatusOK, second.Body.String())
	}
}

func TestWriteJSONReportsEncodeFailure(t *testing.T) {
	rec := httptest.NewRecorder()

	writeJSON(rec, http.StatusOK, map[string]float64{"bad": math.NaN()})

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "json encode failed" {
		t.Fatalf("error = %q, want json encode failed", body["error"])
	}
}

func TestStatusHandler(t *testing.T) {
	state := NewMemoryRuntimeState()
	state.settingsPath = filepath.Join(t.TempDir(), "settings.json")
	if _, err := state.RecordClientCheck(ClientCheckUpdate{
		DashboardBuild:   dashboardBuild,
		ViewportWidth:    390,
		ViewportHeight:   844,
		ScreenWidth:      390,
		ScreenHeight:     844,
		DevicePixelRatio: 3,
		TouchPoints:      5,
		DisplayMode:      "standalone",
		Standalone:       true,
		Visibility:       "visible",
		Orientation:      "portrait-primary",
	}, "Mozilla/5.0 iPhone Mobile Safari", time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	handler, err := newHTTPHandlerWithState(fakeCollector{}, testStaticFS(), state)
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var got AgentStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "ok" || got.OS == "" || got.Arch == "" || got.StartedAt.IsZero() {
		t.Fatalf("status body = %+v", got)
	}
	if got.DashboardBuild != dashboardBuild {
		t.Fatalf("dashboard_build = %q, want %q", got.DashboardBuild, dashboardBuild)
	}
	if !got.SettingsPersisted {
		t.Fatalf("settings_persisted = false, want true")
	}
	if len(got.RefreshOptionsMS) != 4 || got.RefreshOptionsMS[0] != 250 || got.RefreshOptionsMS[3] != 2000 {
		t.Fatalf("refresh_options_ms = %+v", got.RefreshOptionsMS)
	}
	if !containsString(got.PanelOptions, "gpu") || !containsString(got.PanelOptions, "network") {
		t.Fatalf("panel_options = %+v", got.PanelOptions)
	}
	if got.Settings.RefreshMS != defaultRefreshMS || got.Settings.Panel != defaultPanelMode {
		t.Fatalf("settings = %+v, want active dashboard settings", got.Settings)
	}
	if got.Settings.Thresholds != defaultDashboardThresholds() {
		t.Fatalf("settings thresholds = %+v, want defaults", got.Settings.Thresholds)
	}
	if !got.ClientCheck.Seen || got.ClientCheck.DashboardBuild != dashboardBuild || got.ClientCheck.DisplayMode != "standalone" || !got.ClientCheck.Standalone {
		t.Fatalf("client_check = %+v, want latest Home Screen client evidence", got.ClientCheck)
	}
	if !got.DeviceClientCheck.Seen || got.DeviceClientCheck.DashboardBuild != dashboardBuild || got.DeviceClientCheck.DisplayMode != "standalone" || !got.DeviceClientCheck.Standalone {
		t.Fatalf("device_client_check = %+v, want latest device Home Screen client evidence", got.DeviceClientCheck)
	}
}

func TestStaticDashboardRoute(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "<title>test</title>") {
		t.Fatalf("dashboard body = %q", rec.Body.String())
	}
}

func TestSecurityHeadersApplyToAPIAndDashboard(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/api/status", "/api/client-check", "/api/client-checks", "/"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, path, nil)
			handler.ServeHTTP(rec, req)

			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
			}
			if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
				t.Fatalf("Referrer-Policy = %q, want no-referrer", got)
			}
			if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
				t.Fatalf("X-Frame-Options = %q, want DENY", got)
			}
			csp := rec.Header().Get("Content-Security-Policy")
			for _, want := range []string{`default-src 'self'`, `connect-src 'self'`, `worker-src 'self'`, `frame-ancestors 'none'`} {
				if !strings.Contains(csp, want) {
					t.Fatalf("Content-Security-Policy = %q, want %q", csp, want)
				}
			}
		})
	}
}

func TestClientCheckHandlerRecordsDashboardVisit(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	before := httptest.NewRecorder()
	beforeReq := httptest.NewRequest(http.MethodGet, "/api/client-check", nil)
	handler.ServeHTTP(before, beforeReq)
	if before.Code != http.StatusOK {
		t.Fatalf("initial GET status = %d, want %d", before.Code, http.StatusOK)
	}
	var initial ClientCheck
	if err := json.Unmarshal(before.Body.Bytes(), &initial); err != nil {
		t.Fatal(err)
	}
	if initial.Seen {
		t.Fatalf("initial client check = %+v, want unseen", initial)
	}

	post := httptest.NewRecorder()
	postReq := httptest.NewRequest(http.MethodPost, "https://sysmon.tailnet.example:9443/api/client-check", strings.NewReader(`{"dashboard_build":"sysmon-static-v112","interaction":"status_strip_tap","viewport_width":390,"viewport_height":844,"screen_width":390,"screen_height":844,"device_pixel_ratio":3,"touch_points":5,"display_mode":"standalone","standalone":true,"visibility":"visible","orientation":"portrait-primary"}`))
	postReq.Header.Set("Origin", "https://sysmon.tailnet.example:9443")
	postReq.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) Mobile Safari")
	handler.ServeHTTP(post, postReq)
	if post.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want %d: %s", post.Code, http.StatusOK, post.Body.String())
	}

	get := httptest.NewRecorder()
	getReq := httptest.NewRequest(http.MethodGet, "/api/client-check", nil)
	handler.ServeHTTP(get, getReq)
	if get.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d", get.Code, http.StatusOK)
	}

	var got ClientCheck
	if err := json.Unmarshal(get.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Seen || got.LastSeen == nil || got.LastSeen.IsZero() {
		t.Fatalf("client check seen/last_seen = %+v, want populated", got)
	}
	if !strings.Contains(got.UserAgent, "iPhone") ||
		got.DashboardBuild != dashboardBuild ||
		got.Interaction != "status_strip_tap" ||
		got.ViewportWidth != 390 ||
		got.ViewportHeight != 844 ||
		got.ScreenWidth != 390 ||
		got.ScreenHeight != 844 ||
		got.DevicePixelRatio != 3 ||
		got.TouchPoints != 5 ||
		got.DisplayMode != "standalone" ||
		!got.Standalone ||
		got.Visibility != "visible" ||
		got.Orientation != "portrait-primary" {
		t.Fatalf("client check = %+v, want posted dashboard metadata", got)
	}

	historyRec := httptest.NewRecorder()
	historyReq := httptest.NewRequest(http.MethodGet, "/api/client-checks", nil)
	handler.ServeHTTP(historyRec, historyReq)
	if historyRec.Code != http.StatusOK {
		t.Fatalf("history GET status = %d, want %d", historyRec.Code, http.StatusOK)
	}
	var history ClientCheckHistory
	if err := json.Unmarshal(historyRec.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	if len(history.Checks) != 1 || history.Checks[0].DashboardBuild != dashboardBuild || history.Checks[0].Interaction != "status_strip_tap" || history.Checks[0].ViewportWidth != 390 || history.Checks[0].DisplayMode != "standalone" {
		t.Fatalf("client check history = %+v, want recorded device sample", history)
	}
}

func TestClientCheckHistoryKeepsEarlierDeviceSampleAfterDesktopPost(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	device := httptest.NewRecorder()
	deviceReq := httptest.NewRequest(http.MethodPost, "https://sysmon.tailnet.example:9443/api/client-check", strings.NewReader(`{"viewport_width":390,"viewport_height":844,"screen_width":390,"screen_height":844,"device_pixel_ratio":3,"touch_points":5,"display_mode":"standalone","standalone":true,"visibility":"visible","orientation":"portrait-primary"}`))
	deviceReq.Header.Set("Origin", "https://sysmon.tailnet.example:9443")
	deviceReq.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) Mobile Safari")
	handler.ServeHTTP(device, deviceReq)
	if device.Code != http.StatusOK {
		t.Fatalf("device POST status = %d, want %d: %s", device.Code, http.StatusOK, device.Body.String())
	}

	desktop := httptest.NewRecorder()
	desktopReq := httptest.NewRequest(http.MethodPost, "https://sysmon.tailnet.example:9443/api/client-check", strings.NewReader(`{"viewport_width":1440,"viewport_height":900,"screen_width":1440,"screen_height":900,"device_pixel_ratio":1,"touch_points":0,"display_mode":"browser","standalone":false,"visibility":"visible","orientation":"landscape"}`))
	desktopReq.Header.Set("Origin", "https://sysmon.tailnet.example:9443")
	desktopReq.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) Firefox/128.0")
	handler.ServeHTTP(desktop, desktopReq)
	if desktop.Code != http.StatusOK {
		t.Fatalf("desktop POST status = %d, want %d: %s", desktop.Code, http.StatusOK, desktop.Body.String())
	}

	historyRec := httptest.NewRecorder()
	historyReq := httptest.NewRequest(http.MethodGet, "/api/client-checks", nil)
	handler.ServeHTTP(historyRec, historyReq)
	if historyRec.Code != http.StatusOK {
		t.Fatalf("history GET status = %d, want %d", historyRec.Code, http.StatusOK)
	}
	var history ClientCheckHistory
	if err := json.Unmarshal(historyRec.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	if len(history.Checks) != 2 {
		t.Fatalf("history length = %d, want 2", len(history.Checks))
	}
	if history.Checks[0].ViewportWidth != 1440 || strings.Contains(history.Checks[0].UserAgent, "iPhone") {
		t.Fatalf("history newest = %+v, want desktop sample", history.Checks[0])
	}
	if history.Checks[1].ViewportWidth != 390 || !strings.Contains(history.Checks[1].UserAgent, "iPhone") || !history.Checks[1].Standalone {
		t.Fatalf("history older = %+v, want retained device standalone sample", history.Checks[1])
	}
}

func TestStatusKeepsDeviceClientCheckAfterHistoryRotates(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	device := httptest.NewRecorder()
	deviceReq := httptest.NewRequest(http.MethodPost, "https://sysmon.tailnet.example:9443/api/client-check", strings.NewReader(`{"dashboard_build":"sysmon-static-v112","interaction":"status_strip_tap","viewport_width":390,"viewport_height":844,"screen_width":390,"screen_height":844,"device_pixel_ratio":3,"touch_points":5,"display_mode":"standalone","standalone":true,"visibility":"visible","orientation":"portrait-primary"}`))
	deviceReq.Header.Set("Origin", "https://sysmon.tailnet.example:9443")
	deviceReq.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) Mobile Safari")
	handler.ServeHTTP(device, deviceReq)
	if device.Code != http.StatusOK {
		t.Fatalf("device POST status = %d, want %d: %s", device.Code, http.StatusOK, device.Body.String())
	}

	for i := 0; i < maxClientCheckHistory+1; i++ {
		rec := httptest.NewRecorder()
		body := strings.NewReader(`{"viewport_width":1440,"viewport_height":900,"screen_width":1440,"screen_height":900,"device_pixel_ratio":1,"touch_points":0,"display_mode":"browser","standalone":false,"visibility":"visible","orientation":"landscape"}`)
		req := httptest.NewRequest(http.MethodPost, "https://sysmon.tailnet.example:9443/api/client-check", body)
		req.Header.Set("Origin", "https://sysmon.tailnet.example:9443")
		req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) Firefox/128.0")
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("desktop POST %d status = %d, want %d: %s", i, rec.Code, http.StatusOK, rec.Body.String())
		}
	}

	historyRec := httptest.NewRecorder()
	historyReq := httptest.NewRequest(http.MethodGet, "/api/client-checks", nil)
	handler.ServeHTTP(historyRec, historyReq)
	if historyRec.Code != http.StatusOK {
		t.Fatalf("history GET status = %d, want %d", historyRec.Code, http.StatusOK)
	}
	var history ClientCheckHistory
	if err := json.Unmarshal(historyRec.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	for _, check := range history.Checks {
		if strings.Contains(check.UserAgent, "iPhone") {
			t.Fatalf("history still contains iPhone sample; test did not rotate it out: %+v", history.Checks)
		}
	}

	statusRec := httptest.NewRecorder()
	statusReq := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	handler.ServeHTTP(statusRec, statusReq)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("status GET status = %d, want %d", statusRec.Code, http.StatusOK)
	}
	var status AgentStatus
	if err := json.Unmarshal(statusRec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	got := status.DeviceClientCheck
	if !got.Seen ||
		got.DashboardBuild != dashboardBuild ||
		got.Interaction != "status_strip_tap" ||
		got.ViewportWidth != 390 ||
		got.DisplayMode != "standalone" ||
		!got.Standalone ||
		!strings.Contains(got.UserAgent, "iPhone") {
		t.Fatalf("status device_client_check = %+v, want preserved device Home Screen proof after history rotation", got)
	}
	if status.ClientCheck.ViewportWidth != 1440 || strings.Contains(status.ClientCheck.UserAgent, "iPhone") {
		t.Fatalf("status client_check = %+v, want latest desktop sample", status.ClientCheck)
	}
}

func TestClientCheckHandlerRejectsCrossOriginBrowserPost(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "https://sysmon.tailnet.example:9443/api/client-check", strings.NewReader(`{"viewport_width":390}`))
	req.Header.Set("Origin", "https://example.invalid")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if !strings.Contains(rec.Body.String(), "same-origin") {
		t.Fatalf("body = %q, want same-origin error", rec.Body.String())
	}
}

func TestClientCheckHandlerRejectsMissingOriginPost(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "https://sysmon.tailnet.example:9443/api/client-check", strings.NewReader(`{"viewport_width":390,"viewport_height":844,"screen_width":390,"screen_height":844,"device_pixel_ratio":3,"touch_points":5,"display_mode":"standalone","standalone":true,"visibility":"visible","orientation":"portrait-primary"}`))
	req.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) Mobile Safari")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if !strings.Contains(rec.Body.String(), "same-origin browser") {
		t.Fatalf("body = %q, want same-origin browser error", rec.Body.String())
	}
}

func TestClientCheckHandlerAllowsForwardedSameOriginBrowserPost(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9099/api/client-check", strings.NewReader(`{"viewport_width":390,"viewport_height":844,"screen_width":390,"screen_height":844,"device_pixel_ratio":3,"touch_points":5,"display_mode":"standalone","standalone":true,"visibility":"visible","orientation":"portrait-primary"}`))
	req.Host = "127.0.0.1:9099"
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("Origin", "https://sysmon.tailnet.example:9443")
	req.Header.Set("X-Forwarded-Host", "sysmon.tailnet.example:9443")
	req.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) Mobile Safari")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var got ClientCheck
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Seen || !strings.Contains(got.UserAgent, "iPhone") || got.ViewportWidth != 390 || got.ScreenWidth != 390 || got.TouchPoints != 5 || got.DisplayMode != "standalone" || !got.Standalone {
		t.Fatalf("client check = %+v, want forwarded mobile dashboard metadata", got)
	}
}

func TestClientCheckHandlerAllowsForwardedDefaultHTTPSPortBrowserPost(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9099/api/client-check", strings.NewReader(`{"viewport_width":390,"viewport_height":844,"screen_width":390,"screen_height":844,"device_pixel_ratio":3,"touch_points":5,"display_mode":"standalone","standalone":true,"visibility":"visible","orientation":"portrait-primary"}`))
	req.Host = "127.0.0.1:9099"
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("Origin", "https://sysmon.tailnet.example")
	req.Header.Set("X-Forwarded-Host", "sysmon.tailnet.example:443")
	req.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) Mobile Safari")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var got ClientCheck
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Seen || !strings.Contains(got.UserAgent, "iPhone") || got.ViewportWidth != 390 || got.DisplayMode != "standalone" || !got.Standalone {
		t.Fatalf("client check = %+v, want default-port forwarded mobile dashboard metadata", got)
	}
}

func TestClientCheckHandlerAllowsStandardForwardedHeaderBrowserPost(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9099/api/client-check", strings.NewReader(`{"viewport_width":390,"viewport_height":844,"screen_width":390,"screen_height":844,"device_pixel_ratio":3,"touch_points":5,"display_mode":"standalone","standalone":true,"visibility":"visible","orientation":"portrait-primary"}`))
	req.Host = "127.0.0.1:9099"
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("Origin", "https://sysmon.tailnet.example:9443")
	req.Header.Set("Forwarded", `for=100.80.1.2;proto=https;host="sysmon.tailnet.example:9443"`)
	req.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) Mobile Safari")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var got ClientCheck
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Seen || !strings.Contains(got.UserAgent, "iPhone") || got.ViewportWidth != 390 || got.DisplayMode != "standalone" || !got.Standalone {
		t.Fatalf("client check = %+v, want standard Forwarded mobile dashboard metadata", got)
	}
}

func TestClientCheckHandlerRejectsForwardedNonDefaultPortMismatch(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9099/api/client-check", strings.NewReader(`{"viewport_width":390}`))
	req.Host = "127.0.0.1:9099"
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("Origin", "https://sysmon.tailnet.example:9443")
	req.Header.Set("X-Forwarded-Host", "sysmon.tailnet.example")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if !strings.Contains(rec.Body.String(), "same-origin") {
		t.Fatalf("body = %q, want same-origin error", rec.Body.String())
	}
}

func TestClientCheckHandlerRejectsSpoofedForwardedHostFromRemoteClient(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://192.0.2.10:9099/api/client-check", strings.NewReader(`{"viewport_width":390}`))
	req.Host = "192.0.2.10:9099"
	req.RemoteAddr = "198.51.100.25:5555"
	req.Header.Set("Origin", "https://sysmon.tailnet.example:9443")
	req.Header.Set("X-Forwarded-Host", "sysmon.tailnet.example:9443")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if !strings.Contains(rec.Body.String(), "same-origin") {
		t.Fatalf("body = %q, want same-origin error", rec.Body.String())
	}
}

func TestClientCheckHandlerRejectsSpoofedStandardForwardedHeaderFromRemoteClient(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://192.0.2.10:9099/api/client-check", strings.NewReader(`{"viewport_width":390}`))
	req.Host = "192.0.2.10:9099"
	req.RemoteAddr = "198.51.100.25:5555"
	req.Header.Set("Origin", "https://sysmon.tailnet.example:9443")
	req.Header.Set("Forwarded", `for=100.80.1.2;proto=https;host="sysmon.tailnet.example:9443"`)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if !strings.Contains(rec.Body.String(), "same-origin") {
		t.Fatalf("body = %q, want same-origin error", rec.Body.String())
	}
}

func TestClientCheckHandlerRejectsInvalidPayload(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{name: "viewport", body: `{"viewport_width":10001}`, want: "viewport_width"},
		{name: "dashboard build", body: `{"dashboard_build":"` + strings.Repeat("x", 65) + `"}`, want: "dashboard_build"},
		{name: "screen", body: `{"screen_height":10001}`, want: "screen_height"},
		{name: "touch points", body: `{"touch_points":21}`, want: "touch_points"},
		{name: "display mode", body: `{"display_mode":"` + strings.Repeat("x", 33) + `"}`, want: "display_mode"},
		{name: "unknown display mode", body: `{"display_mode":"home-screen"}`, want: "display_mode"},
		{name: "orientation", body: `{"orientation":"` + strings.Repeat("x", 65) + `"}`, want: "orientation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/client-check", strings.NewReader(tc.body))
			req.Header.Set("Origin", "http://example.com")
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("body = %q, want %s validation error", rec.Body.String(), tc.want)
			}
		})
	}
}

func TestStaticAssetsRevalidateThroughBrowserCache(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", got)
	}
}

func TestEmbeddedServiceWorkerRoute(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, staticFS)
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sw.js", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", got)
	}
	if body := rec.Body.String(); !strings.Contains(body, "const STATIC_CACHE") || !strings.Contains(body, `url.pathname.startsWith("/api/")`) {
		t.Fatalf("service worker body missing expected cache/API policy: %q", body)
	}
}

func TestSettingsHandler(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	post := httptest.NewRecorder()
	postReq := newSameOriginSettingsPostRequest(`{"dim":true,"shift":true,"refresh_ms":2000,"panel":"network","thresholds":{"cpu_warn":80,"gpu_warn":85,"temp_warn_c":75}}`)
	handler.ServeHTTP(post, postReq)
	if post.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want %d: %s", post.Code, http.StatusOK, post.Body.String())
	}

	get := httptest.NewRecorder()
	getReq := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	handler.ServeHTTP(get, getReq)
	if get.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d", get.Code, http.StatusOK)
	}

	var got DashboardSettings
	if err := json.Unmarshal(get.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Dim || !got.Shift || got.RefreshMS != 2000 || got.Panel != "network" {
		t.Fatalf("settings = %+v, want dim, shift, 2000ms refresh, and network panel", got)
	}
	if got.Thresholds.CPUWarn != 80 || got.Thresholds.GPUWarn != 85 || got.Thresholds.TempWarnC != 75 {
		t.Fatalf("thresholds = %+v, want updated CPU/GPU/temp warning thresholds", got.Thresholds)
	}
	if got.Thresholds.MemoryWarn != defaultWarningThreshold || got.Thresholds.DiskWarn != defaultWarningThreshold {
		t.Fatalf("untouched thresholds = %+v, want defaults preserved", got.Thresholds)
	}
}

func newSameOriginSettingsPostRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "https://sysmon.tailnet.example:9443/api/settings", strings.NewReader(body))
	req.Header.Set("Origin", "https://sysmon.tailnet.example:9443")
	return req
}

func TestSettingsHandlerReturnsAlwaysOnDisplayDefaults(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d", rec.Code, http.StatusOK)
	}
	var got DashboardSettings
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Shift || got.Dim || got.RefreshMS != defaultRefreshMS || got.Panel != defaultPanelMode {
		t.Fatalf("default settings = %+v, want shift on, dim off, default refresh and all panels", got)
	}
	if got.Thresholds != defaultDashboardThresholds() {
		t.Fatalf("default thresholds = %+v, want %+v", got.Thresholds, defaultDashboardThresholds())
	}
}

func TestSettingsHandlerAllowsSameOriginBrowserPost(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "https://sysmon.tailnet.example:9443/api/settings", strings.NewReader(`{"panel":"gpu"}`))
	req.Header.Set("Origin", "https://sysmon.tailnet.example:9443")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestSettingsHandlerAllowsForwardedSameOriginBrowserPost(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9099/api/settings", strings.NewReader(`{"panel":"gpu"}`))
	req.Host = "127.0.0.1:9099"
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("Origin", "https://sysmon.tailnet.example:9443")
	req.Header.Set("X-Forwarded-Host", "sysmon.tailnet.example:9443")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestSettingsHandlerAllowsForwardedDefaultHTTPSPortBrowserPost(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9099/api/settings", strings.NewReader(`{"panel":"gpu"}`))
	req.Host = "127.0.0.1:9099"
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("Origin", "https://sysmon.tailnet.example")
	req.Header.Set("X-Forwarded-Host", "sysmon.tailnet.example:443")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestSettingsHandlerAllowsStandardForwardedHeaderBrowserPost(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9099/api/settings", strings.NewReader(`{"panel":"gpu"}`))
	req.Host = "127.0.0.1:9099"
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("Origin", "https://sysmon.tailnet.example:9443")
	req.Header.Set("Forwarded", `for=100.80.1.2;proto=https;host="sysmon.tailnet.example:9443"`)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestSettingsHandlerRejectsForwardedNonDefaultPortMismatch(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9099/api/settings", strings.NewReader(`{"panel":"gpu"}`))
	req.Host = "127.0.0.1:9099"
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("Origin", "https://sysmon.tailnet.example:9443")
	req.Header.Set("X-Forwarded-Host", "sysmon.tailnet.example")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if !strings.Contains(rec.Body.String(), "same-origin") {
		t.Fatalf("body = %q, want same-origin error", rec.Body.String())
	}
}

func TestSettingsHandlerRejectsSpoofedForwardedHostFromRemoteClient(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://192.0.2.10:9099/api/settings", strings.NewReader(`{"panel":"gpu"}`))
	req.Host = "192.0.2.10:9099"
	req.RemoteAddr = "198.51.100.25:5555"
	req.Header.Set("Origin", "https://sysmon.tailnet.example:9443")
	req.Header.Set("X-Forwarded-Host", "sysmon.tailnet.example:9443")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if !strings.Contains(rec.Body.String(), "same-origin") {
		t.Fatalf("body = %q, want same-origin error", rec.Body.String())
	}
}

func TestSettingsHandlerRejectsSpoofedStandardForwardedHeaderFromRemoteClient(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://192.0.2.10:9099/api/settings", strings.NewReader(`{"panel":"gpu"}`))
	req.Host = "192.0.2.10:9099"
	req.RemoteAddr = "198.51.100.25:5555"
	req.Header.Set("Origin", "https://sysmon.tailnet.example:9443")
	req.Header.Set("Forwarded", `for=100.80.1.2;proto=https;host="sysmon.tailnet.example:9443"`)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if !strings.Contains(rec.Body.String(), "same-origin") {
		t.Fatalf("body = %q, want same-origin error", rec.Body.String())
	}
}

func TestSettingsHandlerRejectsCrossOriginBrowserPost(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "https://sysmon.tailnet.example:9443/api/settings", strings.NewReader(`{"panel":"gpu"}`))
	req.Header.Set("Origin", "https://example.invalid")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if !strings.Contains(rec.Body.String(), "same-origin") {
		t.Fatalf("body = %q, want same-origin error", rec.Body.String())
	}
}

func TestSettingsHandlerRejectsMissingOriginPost(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "https://sysmon.tailnet.example:9443/api/settings", strings.NewReader(`{"panel":"gpu"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if !strings.Contains(rec.Body.String(), "same-origin browser") {
		t.Fatalf("body = %q, want same-origin browser error", rec.Body.String())
	}
}

func TestSettingsHandlerRejectsInvalidRefresh(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := newSameOriginSettingsPostRequest(`{"refresh_ms":1500}`)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestSettingsHandlerRejectsInvalidPanel(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := newSameOriginSettingsPostRequest(`{"panel":"shell"}`)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestSettingsHandlerRejectsInvalidThreshold(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := newSameOriginSettingsPostRequest(`{"thresholds":{"cpu_warn":95}}`)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rec.Body.String(), "thresholds.cpu_warn") {
		t.Fatalf("body = %q, want threshold validation error", rec.Body.String())
	}
}

func TestSettingsHandlerReportsPersistenceFailure(t *testing.T) {
	state := NewMemoryRuntimeState()
	state.settingsPath = filepath.Join(t.TempDir(), "settings-dir")
	if err := os.Mkdir(state.settingsPath, 0755); err != nil {
		t.Fatal(err)
	}
	handler, err := newHTTPHandlerWithState(fakeCollector{}, testStaticFS(), state)
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := newSameOriginSettingsPostRequest(`{"refresh_ms":2000}`)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestSettingsHandlerRejectsUnknownFields(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := newSameOriginSettingsPostRequest(`{"command":"shutdown"}`)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestSettingsHandlerRejectsTrailingJSON(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := newSameOriginSettingsPostRequest(`{"dim":true}{"shift":true}`)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rec.Body.String(), "single JSON object") {
		t.Fatalf("body = %q, want single JSON object error", rec.Body.String())
	}
}

// fakeController records the action it was asked to apply and returns a canned
// result, so the HTTP layer can be tested without touching real audio endpoints.
type fakeController struct {
	capabilities []ControlCapability
	result       ControlResult
	calls        int
	gotAction    ControlAction
}

func (c *fakeController) Capabilities() []ControlCapability {
	return c.capabilities
}

func (c *fakeController) Apply(ctx context.Context, action ControlAction) ControlResult {
	c.calls++
	c.gotAction = action
	res := c.result
	res.Action = action
	return res
}

func newControlTestHandler(t *testing.T, controller SystemController) http.Handler {
	t.Helper()
	handler, err := newHTTPHandlerWithController(fakeCollector{}, testStaticFS(), NewMemoryRuntimeState(), controller)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func newSameOriginControlPostRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "https://sysmon.tailnet.example:9443/api/control", strings.NewReader(body))
	req.Header.Set("Origin", "https://sysmon.tailnet.example:9443")
	return req
}

func TestStatusHandlerReportsControlCapabilities(t *testing.T) {
	controller := &fakeController{capabilities: capabilitiesFor(func(ControlAction) bool { return true })}
	handler := newControlTestHandler(t, controller)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var status AgentStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Controls) != len(controlActionOrder) {
		t.Fatalf("controls = %d, want %d", len(status.Controls), len(controlActionOrder))
	}
	for i, want := range controlActionOrder {
		if status.Controls[i].Action != want {
			t.Fatalf("control %d action = %q, want %q", i, status.Controls[i].Action, want)
		}
		if status.Controls[i].Label == "" {
			t.Fatalf("control %q has empty label", status.Controls[i].Action)
		}
		if !status.Controls[i].Available {
			t.Fatalf("control %q should be available", status.Controls[i].Action)
		}
	}
}

func TestControlHandlerAppliesKnownAction(t *testing.T) {
	controller := &fakeController{result: ControlResult{Available: true, Applied: true, State: "muted"}}
	handler := newControlTestHandler(t, controller)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newSameOriginControlPostRequest(`{"action":"mic_mute"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if controller.calls != 1 || controller.gotAction != ControlMicMute {
		t.Fatalf("controller calls = %d, action = %q, want 1 mic_mute", controller.calls, controller.gotAction)
	}
	var result ControlResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Action != ControlMicMute || !result.Applied || result.State != "muted" {
		t.Fatalf("result = %+v, want applied mic_mute muted", result)
	}
}

func TestControlHandlerRejectsUnknownAction(t *testing.T) {
	controller := &fakeController{}
	handler := newControlTestHandler(t, controller)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newSameOriginControlPostRequest(`{"action":"reboot"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if controller.calls != 0 {
		t.Fatalf("controller called %d times for an unknown action, want 0", controller.calls)
	}
}

func TestControlHandlerRejectsCrossOriginPost(t *testing.T) {
	controller := &fakeController{}
	handler := newControlTestHandler(t, controller)

	req := httptest.NewRequest(http.MethodPost, "https://sysmon.tailnet.example:9443/api/control", strings.NewReader(`{"action":"mic_mute"}`))
	req.Header.Set("Origin", "https://example.invalid")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if controller.calls != 0 {
		t.Fatalf("controller called %d times for a cross-origin post, want 0", controller.calls)
	}
}

func TestControlHandlerRejectsUnknownFields(t *testing.T) {
	controller := &fakeController{}
	handler := newControlTestHandler(t, controller)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newSameOriginControlPostRequest(`{"action":"mic_mute","extra":true}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if controller.calls != 0 {
		t.Fatalf("controller called %d times for an unknown field, want 0", controller.calls)
	}
}

func TestControlHandlerRejectsTrailingJSON(t *testing.T) {
	controller := &fakeController{}
	handler := newControlTestHandler(t, controller)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newSameOriginControlPostRequest(`{"action":"mic_mute"}{"action":"lock_screen"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rec.Body.String(), "single JSON object") {
		t.Fatalf("body = %q, want single JSON object error", rec.Body.String())
	}
	if controller.calls != 0 {
		t.Fatalf("controller called %d times for trailing JSON, want 0", controller.calls)
	}
}
