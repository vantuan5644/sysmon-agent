package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"
)

func runSelfCheck(handler http.Handler) error {
	if err := checkHealthz(handler); err != nil {
		return err
	}
	if err := checkReadyz(handler); err != nil {
		return err
	}
	if err := checkMetrics(handler); err != nil {
		return err
	}
	if err := checkStatus(handler); err != nil {
		return err
	}
	if err := checkSecurityHeaders(handler); err != nil {
		return err
	}
	if err := checkDashboard(handler); err != nil {
		return err
	}
	if err := checkStaticAssets(handler); err != nil {
		return err
	}
	if err := checkSettings(handler); err != nil {
		return err
	}
	if err := checkClientCheck(handler); err != nil {
		return err
	}
	return nil
}

func checkHealthz(handler http.Handler) error {
	rec := serveSelfCheckRequest(handler, http.MethodGet, "/healthz", "")
	if rec.Code != http.StatusOK {
		return fmt.Errorf("GET /healthz returned %d", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		return fmt.Errorf("GET /healthz JSON: %w", err)
	}
	if body["status"] != "ok" {
		return fmt.Errorf("GET /healthz status = %q", body["status"])
	}
	return nil
}

func checkReadyz(handler http.Handler) error {
	rec := serveSelfCheckRequest(handler, http.MethodGet, "/readyz", "")
	if rec.Code != http.StatusOK {
		return fmt.Errorf("GET /readyz returned %d: %s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	var body ReadinessStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		return fmt.Errorf("GET /readyz JSON: %w", err)
	}
	if body.Status != "ok" || !body.Metrics {
		return fmt.Errorf("GET /readyz status = %q metrics=%t", body.Status, body.Metrics)
	}
	if strings.TrimSpace(body.Hostname) == "" {
		return fmt.Errorf("GET /readyz returned empty hostname")
	}
	if err := validateMetricsTimestampFreshness(body.Timestamp, time.Now()); err != nil {
		return fmt.Errorf("GET /readyz timestamp: %w", err)
	}
	if err := validateCollectionErrors(body.CollectionErrors); err != nil {
		return fmt.Errorf("GET /readyz schema: %w", err)
	}
	return nil
}

func checkStatus(handler http.Handler) error {
	rec := serveSelfCheckRequest(handler, http.MethodGet, "/api/status", "")
	if rec.Code != http.StatusOK {
		return fmt.Errorf("GET /api/status returned %d", rec.Code)
	}
	var status AgentStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		return fmt.Errorf("GET /api/status JSON: %w", err)
	}
	if status.Status != "ok" {
		return fmt.Errorf("GET /api/status status = %q", status.Status)
	}
	if status.DashboardBuild != dashboardBuild {
		return fmt.Errorf("GET /api/status dashboard_build = %q", status.DashboardBuild)
	}
	if status.StartedAt.IsZero() {
		return fmt.Errorf("GET /api/status returned empty started_at")
	}
	if status.OS == "" || status.Arch == "" {
		return fmt.Errorf("GET /api/status returned empty runtime metadata")
	}
	if len(status.RefreshOptionsMS) == 0 || len(status.PanelOptions) == 0 {
		return fmt.Errorf("GET /api/status returned empty dashboard option metadata")
	}
	if len(status.Controls) != len(controlActionOrder) {
		return fmt.Errorf("GET /api/status returned %d controls, want %d", len(status.Controls), len(controlActionOrder))
	}
	for i, capability := range status.Controls {
		if capability.Action != controlActionOrder[i] {
			return fmt.Errorf("GET /api/status control %d action = %q, want %q", i, capability.Action, controlActionOrder[i])
		}
		if capability.Label == "" {
			return fmt.Errorf("GET /api/status control %q returned empty label", capability.Action)
		}
	}
	if err := validateDashboardSettings(status.Settings); err != nil {
		return fmt.Errorf("GET /api/status settings: %w", err)
	}
	return nil
}

func checkMetrics(handler http.Handler) error {
	rec := serveSelfCheckRequest(handler, http.MethodGet, "/api/metrics", "")
	if rec.Code != http.StatusOK {
		return fmt.Errorf("GET /api/metrics returned %d: %s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	var metrics Metrics
	if err := json.Unmarshal(rec.Body.Bytes(), &metrics); err != nil {
		return fmt.Errorf("GET /api/metrics JSON: %w", err)
	}
	if metrics.Hostname == "" {
		return fmt.Errorf("GET /api/metrics returned empty hostname")
	}
	if metrics.OS == "" {
		return fmt.Errorf("GET /api/metrics returned empty OS")
	}
	if err := validateMetricsTimestampFreshness(metrics.Timestamp, time.Now()); err != nil {
		return fmt.Errorf("GET /api/metrics timestamp: %w", err)
	}
	if err := validateMetricsShape(metrics); err != nil {
		return fmt.Errorf("GET /api/metrics schema: %w", err)
	}
	return nil
}

func validateMetricsTimestampFreshness(timestamp, now time.Time) error {
	if timestamp.IsZero() {
		return fmt.Errorf("is empty")
	}
	if timestamp.After(now.Add(5 * time.Second)) {
		return fmt.Errorf("is in the future: %s", timestamp.Format(time.RFC3339))
	}
	if now.Sub(timestamp) > time.Minute {
		return fmt.Errorf("is stale: %s", timestamp.Format(time.RFC3339))
	}
	return nil
}

func validateMetricsShape(metrics Metrics) error {
	if strings.TrimSpace(metrics.Hostname) == "" {
		return fmt.Errorf("hostname is required")
	}
	if strings.TrimSpace(metrics.OS) == "" {
		return fmt.Errorf("os is required")
	}
	if strings.TrimSpace(metrics.Arch) == "" {
		return fmt.Errorf("arch is required")
	}
	if metrics.Timestamp.IsZero() {
		return fmt.Errorf("timestamp is required")
	}
	if metrics.CollectionDurationMS < 0 {
		return fmt.Errorf("collection_duration_ms must be non-negative")
	}
	if err := validateNumberMetric("cpu_percent", metrics.CPU, false, "%"); err != nil {
		return err
	}
	if err := validateCapacityMetric("memory", metrics.Memory, false); err != nil {
		return err
	}
	// memory_swap is optional -- many hosts run without swap or use zram, and the
	// warming snapshot leaves it unset -- so an unavailable value (even with an
	// empty error) is always accepted. Only validate its shape when present.
	if metrics.MemorySwap.Available {
		if err := validateCapacityMetric("memory_swap", metrics.MemorySwap, false); err != nil {
			return err
		}
	}
	if len(metrics.Disks) == 0 {
		return fmt.Errorf("disks must include at least one local filesystem or unavailable placeholder")
	}
	for i, disk := range metrics.Disks {
		if disk.Name == "" && disk.Mountpoint == "" {
			return fmt.Errorf("disks[%d] must include name or mountpoint", i)
		}
		if err := validateCapacityMetric(fmt.Sprintf("disks[%d].capacity", i), disk.Capacity, true); err != nil {
			return err
		}
	}
	if err := validateNetworkSet(metrics.Network); err != nil {
		return err
	}
	if err := validateTemperatureSet(metrics.Temperatures); err != nil {
		return err
	}
	if err := validateGPUSet(metrics.GPU); err != nil {
		return err
	}
	if err := validateTailscaleStatus(metrics.Tailscale); err != nil {
		return err
	}
	if err := validateCollectionErrors(metrics.CollectionErrors); err != nil {
		return err
	}
	return nil
}

func validateCollectionErrors(errors []string) error {
	for i, message := range errors {
		if strings.TrimSpace(message) == "" {
			return fmt.Errorf("collection_errors[%d] is empty", i)
		}
	}
	return nil
}

func validateNetworkSet(network NetworkSet) error {
	if !network.Available {
		if strings.TrimSpace(network.Error) == "" {
			return fmt.Errorf("network unavailable without an error")
		}
		return nil
	}
	if len(network.Interfaces) == 0 {
		return fmt.Errorf("network available without interfaces")
	}
	for i, iface := range network.Interfaces {
		if strings.TrimSpace(iface.Name) == "" {
			return fmt.Errorf("network.interfaces[%d] missing name", i)
		}
		if err := validateNumberMetric(fmt.Sprintf("network.interfaces[%d].rx_bytes_per_second", i), iface.RXBytesPerSecond, true, "B/s"); err != nil {
			return err
		}
		if err := validateNumberMetric(fmt.Sprintf("network.interfaces[%d].tx_bytes_per_second", i), iface.TXBytesPerSecond, true, "B/s"); err != nil {
			return err
		}
	}
	return nil
}

func validateTemperatureSet(temperatures TemperatureSet) error {
	if !temperatures.Available {
		if strings.TrimSpace(temperatures.Error) == "" {
			return fmt.Errorf("temperatures unavailable without an error")
		}
		return nil
	}
	if len(temperatures.Sensors) == 0 {
		return fmt.Errorf("temperatures available without sensors")
	}
	for i, sensor := range temperatures.Sensors {
		if strings.TrimSpace(sensor.Name) == "" {
			return fmt.Errorf("temperatures.sensors[%d] missing name", i)
		}
		if err := validateNumberMetric(fmt.Sprintf("temperatures.sensors[%d].celsius", i), sensor.Celsius, true, "C"); err != nil {
			return err
		}
	}
	return nil
}

func validateGPUSet(gpu GPUSet) error {
	if !gpu.Available {
		if strings.TrimSpace(gpu.Error) == "" {
			return fmt.Errorf("gpu unavailable without an error")
		}
		return nil
	}
	if len(gpu.Devices) == 0 {
		return fmt.Errorf("gpu available without devices")
	}
	for i, device := range gpu.Devices {
		if strings.TrimSpace(device.Name) == "" {
			return fmt.Errorf("gpu.devices[%d] missing name", i)
		}
		if err := validateNumberMetric(fmt.Sprintf("gpu.devices[%d].usage_percent", i), device.Usage, true, "%"); err != nil {
			return err
		}
		if err := validateCapacityMetric(fmt.Sprintf("gpu.devices[%d].memory", i), device.Memory, true); err != nil {
			return err
		}
		if err := validateNumberMetric(fmt.Sprintf("gpu.devices[%d].temperature_celsius", i), device.Temperature, true, "C"); err != nil {
			return err
		}
	}
	return nil
}

// validateTailscaleStatus mirrors the other set validators: an unavailable status
// must carry a non-empty error so the dashboard can explain why. When
// available, no further shape is required (booleans have no invalid state).
func validateTailscaleStatus(tailscale TailscaleStatus) error {
	if !tailscale.Available {
		if strings.TrimSpace(tailscale.Error) == "" {
			return fmt.Errorf("tailscale unavailable without an error")
		}
	}
	return nil
}

func validateNumberMetric(name string, metric NumberMetric, allowUnavailable bool, unit string) error {
	if strings.TrimSpace(metric.Unit) != unit {
		return fmt.Errorf("%s unit = %q, want %q", name, metric.Unit, unit)
	}
	if !metric.Available {
		if !allowUnavailable {
			return fmt.Errorf("%s is unavailable: %s", name, metric.Error)
		}
		if strings.TrimSpace(metric.Error) == "" {
			return fmt.Errorf("%s unavailable without an error", name)
		}
		return nil
	}
	if math.IsNaN(metric.Value) || math.IsInf(metric.Value, 0) {
		return fmt.Errorf("%s has invalid value %v", name, metric.Value)
	}
	if unit == "%" && (metric.Value < 0 || metric.Value > 100) {
		return fmt.Errorf("%s percent out of range: %v", name, metric.Value)
	}
	return nil
}

func validateCapacityMetric(name string, metric CapacityMetric, allowUnavailable bool) error {
	if !metric.Available {
		if !allowUnavailable {
			return fmt.Errorf("%s is unavailable: %s", name, metric.Error)
		}
		if strings.TrimSpace(metric.Error) == "" {
			return fmt.Errorf("%s unavailable without an error", name)
		}
		return nil
	}
	if metric.TotalBytes == 0 {
		return fmt.Errorf("%s total_bytes must be greater than zero", name)
	}
	if metric.UsedBytes > metric.TotalBytes {
		return fmt.Errorf("%s used_bytes exceeds total_bytes", name)
	}
	if math.IsNaN(metric.Percent) || math.IsInf(metric.Percent, 0) || metric.Percent < 0 || metric.Percent > 100 {
		return fmt.Errorf("%s percent out of range: %v", name, metric.Percent)
	}
	return nil
}

func checkSecurityHeaders(handler http.Handler) error {
	for _, path := range []string{"/readyz", "/api/status", "/api/client-check", "/api/client-checks", "/"} {
		rec := serveSelfCheckRequest(handler, http.MethodGet, path, "")
		if rec.Code != http.StatusOK {
			return fmt.Errorf("GET %s returned %d while checking security headers", path, rec.Code)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			return fmt.Errorf("GET %s X-Content-Type-Options = %q", path, got)
		}
		if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
			return fmt.Errorf("GET %s Referrer-Policy = %q", path, got)
		}
		if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
			return fmt.Errorf("GET %s X-Frame-Options = %q", path, got)
		}
		csp := rec.Header().Get("Content-Security-Policy")
		for _, want := range []string{`default-src 'self'`, `connect-src 'self'`, `worker-src 'self'`, `frame-ancestors 'none'`} {
			if !strings.Contains(csp, want) {
				return fmt.Errorf("GET %s Content-Security-Policy = %q, missing %q", path, csp, want)
			}
		}
	}
	return nil
}

func checkDashboard(handler http.Handler) error {
	rec := serveSelfCheckRequest(handler, http.MethodGet, "/", "")
	if rec.Code != http.StatusOK {
		return fmt.Errorf("GET / returned %d", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		return fmt.Errorf("GET / Cache-Control = %q", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<title>Sysmon</title>") ||
		!strings.Contains(body, "/app.js") ||
		!strings.Contains(body, "/icon-180.png") {
		return fmt.Errorf("GET / did not return the embedded dashboard")
	}
	return nil
}

func checkStaticAssets(handler http.Handler) error {
	for _, path := range []string{"/app.js", "/styles.css", "/sw.js", "/manifest.json", "/icon.svg", "/icon-180.png", "/icon-512.png"} {
		rec := serveSelfCheckRequest(handler, http.MethodGet, path, "")
		if rec.Code != http.StatusOK {
			return fmt.Errorf("GET %s returned %d", path, rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			return fmt.Errorf("GET %s Cache-Control = %q", path, got)
		}
		if rec.Body.Len() == 0 {
			return fmt.Errorf("GET %s returned an empty body", path)
		}
	}
	return nil
}

func checkSettings(handler http.Handler) error {
	post := serveSelfCheckRequest(handler, http.MethodPost, "/api/settings", `{"dim":true,"refresh_ms":2000,"panel":"gpu","thresholds":{"cpu_warn":80,"memory_warn":75,"disk_warn":85,"gpu_warn":80,"temp_warn_c":75}}`)
	if post.Code != http.StatusOK {
		return fmt.Errorf("POST /api/settings returned %d: %s", post.Code, strings.TrimSpace(post.Body.String()))
	}
	get := serveSelfCheckRequest(handler, http.MethodGet, "/api/settings", "")
	if get.Code != http.StatusOK {
		return fmt.Errorf("GET /api/settings returned %d", get.Code)
	}
	var settings DashboardSettings
	if err := json.Unmarshal(get.Body.Bytes(), &settings); err != nil {
		return fmt.Errorf("GET /api/settings JSON: %w", err)
	}
	if !settings.Dim || settings.RefreshMS != 2000 || settings.Panel != "gpu" {
		return fmt.Errorf("GET /api/settings did not persist dim/refresh/panel state")
	}
	if settings.Thresholds.CPUWarn != 80 ||
		settings.Thresholds.MemoryWarn != 75 ||
		settings.Thresholds.DiskWarn != 85 ||
		settings.Thresholds.GPUWarn != 80 ||
		settings.Thresholds.TempWarnC != 75 {
		return fmt.Errorf("GET /api/settings did not persist threshold state")
	}
	return nil
}

func checkClientCheck(handler http.Handler) error {
	getInitial := serveSelfCheckRequest(handler, http.MethodGet, "/api/client-check", "")
	if getInitial.Code != http.StatusOK {
		return fmt.Errorf("GET /api/client-check returned %d", getInitial.Code)
	}
	var initial ClientCheck
	if err := json.Unmarshal(getInitial.Body.Bytes(), &initial); err != nil {
		return fmt.Errorf("GET /api/client-check JSON: %w", err)
	}
	if initial.Seen {
		return fmt.Errorf("GET /api/client-check was already seen before dashboard POST")
	}
	getInitialHistory := serveSelfCheckRequest(handler, http.MethodGet, "/api/client-checks", "")
	if getInitialHistory.Code != http.StatusOK {
		return fmt.Errorf("GET /api/client-checks returned %d", getInitialHistory.Code)
	}
	var initialHistory ClientCheckHistory
	if err := json.Unmarshal(getInitialHistory.Body.Bytes(), &initialHistory); err != nil {
		return fmt.Errorf("GET /api/client-checks JSON: %w", err)
	}
	if len(initialHistory.Checks) != 0 {
		return fmt.Errorf("GET /api/client-checks was not empty before dashboard POST")
	}

	post := serveSelfCheckRequestWithUserAgent(handler, http.MethodPost, "/api/client-check", `{"dashboard_build":"sysmon-static-v107","interaction":"status_strip_tap","viewport_width":390,"viewport_height":844,"screen_width":390,"screen_height":844,"device_pixel_ratio":3,"touch_points":5,"display_mode":"standalone","standalone":true,"visibility":"visible","orientation":"portrait-primary"}`, selfCheckDeviceUserAgent)
	if post.Code != http.StatusOK {
		return fmt.Errorf("POST /api/client-check returned %d: %s", post.Code, strings.TrimSpace(post.Body.String()))
	}
	get := serveSelfCheckRequest(handler, http.MethodGet, "/api/client-check", "")
	if get.Code != http.StatusOK {
		return fmt.Errorf("GET /api/client-check after POST returned %d", get.Code)
	}
	var check ClientCheck
	if err := json.Unmarshal(get.Body.Bytes(), &check); err != nil {
		return fmt.Errorf("GET /api/client-check after POST JSON: %w", err)
	}
	if !selfCheckClientMetadataMatches(check) {
		return fmt.Errorf("GET /api/client-check did not report the dashboard client metadata")
	}

	getHistory := serveSelfCheckRequest(handler, http.MethodGet, "/api/client-checks", "")
	if getHistory.Code != http.StatusOK {
		return fmt.Errorf("GET /api/client-checks after POST returned %d", getHistory.Code)
	}
	var history ClientCheckHistory
	if err := json.Unmarshal(getHistory.Body.Bytes(), &history); err != nil {
		return fmt.Errorf("GET /api/client-checks after POST JSON: %w", err)
	}
	if len(history.Checks) != 1 || !selfCheckClientMetadataMatches(history.Checks[0]) {
		return fmt.Errorf("GET /api/client-checks did not report the dashboard client metadata")
	}

	statusRec := serveSelfCheckRequest(handler, http.MethodGet, "/api/status", "")
	if statusRec.Code != http.StatusOK {
		return fmt.Errorf("GET /api/status after client-check POST returned %d", statusRec.Code)
	}
	var status AgentStatus
	if err := json.Unmarshal(statusRec.Body.Bytes(), &status); err != nil {
		return fmt.Errorf("GET /api/status after client-check POST JSON: %w", err)
	}
	if !selfCheckClientMetadataMatches(status.ClientCheck) {
		return fmt.Errorf("GET /api/status client_check did not report the dashboard client metadata")
	}
	if !selfCheckClientMetadataMatches(status.DeviceClientCheck) {
		return fmt.Errorf("GET /api/status device_client_check did not report the dashboard client metadata")
	}
	return nil
}

func selfCheckClientMetadataMatches(check ClientCheck) bool {
	return check.Seen &&
		check.LastSeen != nil &&
		check.DashboardBuild == dashboardBuild &&
		check.Interaction == "status_strip_tap" &&
		strings.Contains(check.UserAgent, "iPhone") &&
		strings.Contains(check.UserAgent, "Mobile") &&
		check.ViewportWidth == 390 &&
		check.ViewportHeight == 844 &&
		check.ScreenWidth == 390 &&
		check.ScreenHeight == 844 &&
		check.DevicePixelRatio == 3 &&
		check.TouchPoints == 5 &&
		check.DisplayMode == "standalone" &&
		check.Standalone &&
		check.Visibility == "visible" &&
		check.Orientation == "portrait-primary"
}

// selfCheckDeviceUserAgent simulates a real handheld client (an iOS Safari UA)
// so the self-check exercises the device_client_check promotion path.
const selfCheckDeviceUserAgent = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1"

func serveSelfCheckRequest(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	return serveSelfCheckRequestWithUserAgent(handler, method, path, body, "")
}

func serveSelfCheckRequestWithUserAgent(handler http.Handler, method, path, body, userAgent string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://example.com")
	}
	handler.ServeHTTP(rec, req)
	return rec
}
