package main

import (
	"bytes"
	"encoding/json"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPWAInstallMetadata(t *testing.T) {
	index, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	indexHTML := string(index)
	for _, needle := range []string{
		`<link rel="manifest" href="/manifest.json">`,
		`<link rel="icon" href="/icon.svg" type="image/svg+xml">`,
		`<link rel="apple-touch-icon" sizes="180x180" href="/icon-180.png">`,
		`apple-mobile-web-app-capable`,
		`apple-mobile-web-app-title`,
		`id="agentMeta"`,
		`id="statusStrip" class="status-strip" role="button" tabindex="0" aria-label="Refresh metrics now"`,
		`id="alertsPanel" class="panel alerts-panel" role="button" tabindex="0" aria-label="Alert details" aria-expanded="false" aria-live="polite" hidden`,
		`id="alertsSummary"`,
		`id="alertsList"`,
		`id="issuesPanel" class="panel issues-panel" role="button" tabindex="0" aria-label="Issue details" aria-expanded="false" aria-live="polite" hidden`,
		`id="issuesSummary"`,
		`id="issuesList"`,
		`id="cpuTrend" aria-hidden="true"`,
		`id="memTrend" aria-hidden="true"`,
		`id="gpuTrend" aria-hidden="true"`,
		`id="netTrend" aria-hidden="true"`,
	} {
		if !strings.Contains(indexHTML, needle) {
			t.Fatalf("index.html missing %q", needle)
		}
	}

	manifestBytes, err := staticFS.ReadFile("static/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Name            string `json:"name"`
		ShortName       string `json:"short_name"`
		Description     string `json:"description"`
		ID              string `json:"id"`
		StartURL        string `json:"start_url"`
		Scope           string `json:"scope"`
		Display         string `json:"display"`
		Orientation     string `json:"orientation"`
		BackgroundColor string `json:"background_color"`
		ThemeColor      string `json:"theme_color"`
		Icons           []struct {
			Src   string `json:"src"`
			Sizes string `json:"sizes"`
			Type  string `json:"type"`
		} `json:"icons"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Name == "" || manifest.ShortName == "" || manifest.Description == "" || manifest.Display != "standalone" {
		t.Fatalf("manifest has weak install metadata: %+v", manifest)
	}
	if manifest.ID != "/" || manifest.StartURL != "/" || manifest.Scope != "/" || manifest.Orientation != "any" {
		t.Fatalf("manifest navigation scope = id:%q start:%q scope:%q orientation:%q", manifest.ID, manifest.StartURL, manifest.Scope, manifest.Orientation)
	}
	if manifest.BackgroundColor != "#080b10" || manifest.ThemeColor != "#080b10" {
		t.Fatalf("manifest colors = %s/%s", manifest.BackgroundColor, manifest.ThemeColor)
	}

	requiredIcons := map[string]bool{
		"/icon.svg":     false,
		"/icon-180.png": false,
		"/icon-512.png": false,
	}
	for _, icon := range manifest.Icons {
		if _, ok := requiredIcons[icon.Src]; ok && icon.Sizes != "" && icon.Type != "" {
			requiredIcons[icon.Src] = true
		}
	}
	for src, ok := range requiredIcons {
		if !ok {
			t.Fatalf("manifest missing usable icon %s", src)
		}
	}
}

func TestPWAIconAssets(t *testing.T) {
	svg, err := staticFS.ReadFile("static/icon.svg")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(svg), "<svg") {
		t.Fatal("icon.svg is not an SVG")
	}

	for path, expectedSize := range map[string]int{
		"static/icon-180.png": 180,
		"static/icon-512.png": 512,
	} {
		data, err := staticFS.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s is not a decodable PNG: %v", path, err)
		}
		if cfg.Width != expectedSize || cfg.Height != expectedSize {
			t.Fatalf("%s dimensions = %dx%d, want %dx%d", path, cfg.Width, cfg.Height, expectedSize, expectedSize)
		}
	}
}

func TestServiceWorkerCachingPolicy(t *testing.T) {
	data, err := staticFS.ReadFile("static/sw.js")
	if err != nil {
		t.Fatal(err)
	}
	sw := string(data)
	for _, needle := range []string{
		`const STATIC_CACHE = "sysmon-static-v107"`,
		`const STATIC_ASSET_SET = new Set(STATIC_ASSETS);`,
		`self.skipWaiting()`,
		`self.clients.claim()`,
		`if (isLiveEndpoint(url)) {`,
		`event.respondWith(fetch(event.request))`,
		`function isLiveEndpoint(url) {`,
		`return url.pathname.startsWith("/api/") || url.pathname === "/healthz" || url.pathname === "/readyz";`,
		`function shouldCacheStaticRequest(request, url, response) {`,
		`return request.method === "GET" && response.ok && STATIC_ASSET_SET.has(url.pathname);`,
		`caches.match(event.request)`,
		`event.request.mode === "navigate"`,
		`return caches.match("/")`,
		`return Response.error()`,
	} {
		if !strings.Contains(sw, needle) {
			t.Fatalf("service worker missing %q", needle)
		}
	}
}

func TestDashboardBuildTokenMatchesStaticAssets(t *testing.T) {
	status := newAgentStatus(NewMemoryRuntimeState(), nil, time.Time{})
	if status.DashboardBuild != dashboardBuild {
		t.Fatalf("status dashboard build = %q, want %q", status.DashboardBuild, dashboardBuild)
	}

	app, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	sw, err := staticFS.ReadFile("static/sw.js")
	if err != nil {
		t.Fatal(err)
	}
	quotedBuild := `"` + dashboardBuild + `"`
	for path, body := range map[string]string{
		"static/app.js": string(app),
		"static/sw.js":  string(sw),
	} {
		if !strings.Contains(body, quotedBuild) {
			t.Fatalf("%s missing current dashboard build token %s", path, quotedBuild)
		}
	}
	if !strings.Contains(string(app), `const dashboardBuild = `+quotedBuild+`;`) {
		t.Fatalf("app.js dashboardBuild constant does not match %s", dashboardBuild)
	}
	if !strings.Contains(string(sw), `const STATIC_CACHE = `+quotedBuild+`;`) {
		t.Fatalf("service worker cache name does not match dashboard build %s", dashboardBuild)
	}
}

func TestDashboardReloadsWhenServiceWorkerUpdates(t *testing.T) {
	app, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(app)
	for _, needle := range []string{
		`registerServiceWorker();`,
		`function registerServiceWorker() {`,
		`typeof navigator.serviceWorker.register !== "function"`,
		`typeof navigator.serviceWorker.addEventListener === "function"`,
		`navigator.serviceWorker.addEventListener("controllerchange", () => {`,
		`if (reloading) {`,
		`window.location.reload();`,
		`navigator.serviceWorker.register("/sw.js").catch(() => {});`,
	} {
		if !strings.Contains(appJS, needle) {
			t.Fatalf("app.js missing service-worker update behavior %q", needle)
		}
	}
}

func TestDashboardCanRecoverStaleStaticAssetsFromStatusStrip(t *testing.T) {
	app, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(app)
	for _, needle := range []string{
		`staleDashboardBuild: "",`,
		`staticRefreshInFlight: false,`,
		`if (state.staleDashboardBuild) {`,
		"showTransientStatus(`Refreshing app ${state.staleDashboardBuild}`);",
		`refreshStaticAssets();`,
		`state.staleDashboardBuild = serverBuild && serverBuild !== dashboardBuild ? serverBuild : "";`,
		`tap status strip to refresh app or re-add Home Screen app`,
		`async function refreshStaticAssets() {`,
		`stopVisibleTimers();`,
		`await unregisterSysmonServiceWorkers();`,
		`await deleteSysmonStaticCaches();`,
		`window.location.reload();`,
		`async function unregisterSysmonServiceWorkers() {`,
		`typeof serviceWorker.getRegistrations !== "function"`,
		`registration.unregister()`,
		`async function deleteSysmonStaticCaches() {`,
		`String(key).startsWith("sysmon-static-")`,
		`window.caches.delete(key)`,
	} {
		if !strings.Contains(appJS, needle) {
			t.Fatalf("app.js missing stale static asset recovery behavior %q", needle)
		}
	}
}

func TestDashboardResumeShowsUpdatingUntilRefreshCompletes(t *testing.T) {
	app, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(app)
	for _, needle := range []string{
		`if (state.paused) {`,
		`setConnectionState("paused", "Paused");`,
		`setConnectionState("loading", "Updating");`,
		`fetchMetrics();`,
	} {
		if !strings.Contains(appJS, needle) {
			t.Fatalf("app.js missing pause/resume updating state behavior %q", needle)
		}
	}
}

func TestDashboardShowsMetricSampleAge(t *testing.T) {
	app, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(app)
	for _, needle := range []string{
		`lastMetricTimestampMS: 0,`,
		`lastCollectionDurationMS: null,`,
		`renderMetricTimestamp(metrics.timestamp, metrics.collection_duration_ms);`,
		`function renderMetricTimestamp(timestamp, durationMS) {`,
		`state.lastMetricTimestampMS = parsed;`,
		`state.lastCollectionDurationMS = normalizedDurationMS(durationMS);`,
		`function refreshMetricAge() {`,
		"const durationLabel = state.lastCollectionDurationMS === null ? \"\" : ` / ${formatDurationMS(state.lastCollectionDurationMS)}`;",
		"formatTime(state.lastMetricTimestampMS)} / ${formatSampleAge(state.lastMetricTimestampMS)}${durationLabel}",
		`function normalizedDurationMS(value) {`,
		`function formatDurationMS(milliseconds) {`,
		`function formatSampleAge(timestampMS) {`,
		`const ageSeconds = Math.max(0, Math.floor((nowMS() - timestampMS) / 1000));`,
		"return `${ageSeconds}s`;",
		"return `${ageMinutes}m`;",
	} {
		if !strings.Contains(appJS, needle) {
			t.Fatalf("app.js missing metric sample age behavior %q", needle)
		}
	}

	css, err := staticFS.ReadFile("static/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(css), `#updatedAt`) || !strings.Contains(string(css), `white-space: nowrap;`) {
		t.Fatal("styles.css missing nowrap rule for metric sample age")
	}
}

func TestDashboardStatusAndSettingsUseTimeouts(t *testing.T) {
	app, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(app)
	for _, needle := range []string{
		`const metricsTimeoutMS = 4500;`,
		`const auxiliaryTimeoutMS = 3000;`,
		`const dashboardBuild = "sysmon-static-v107";`,
		`const clientCheckIntervalMS = 30000;`,
		`const clientCheckStaleAfterMS = clientCheckIntervalMS * 3;`,
		`const clientCheckDebounceMS = 500;`,
		`fetchWithTimeout("/api/metrics", { cache: "no-store" }, metricsTimeoutMS)`,
		`fetchWithTimeout("/api/status", { cache: "no-store" }, auxiliaryTimeoutMS)`,
		`fetchWithTimeout("/api/settings", { cache: "no-store" }, auxiliaryTimeoutMS)`,
		`settingsInFlight: false,`,
		`if (status?.settings && !state.settingsInFlight) {`,
		`applySettings(status.settings);`,
		`clientCheckAgeLabel(statusProofClientCheck(status))`,
		`function clientCheckAgeLabel(clientCheck) {`,
		`function statusProofClientCheck(status) {`,
		`return status?.device_client_check?.seen ? status.device_client_check : status?.client_check;`,
		`function clientCheckLastSeenTimeMS(clientCheck) {`,
		`const requestOptions = { ...options };`,
		`typeof window.AbortController === "function"`,
		`requestOptions.signal = controller.signal;`,
		`return await Promise.race([`,
		`controller.abort();`,
		`new Promise((_, reject) => {`,
		`reject(new Error("Request timed out"))`,
		`document.addEventListener("visibilitychange", handleVisibilityChange);`,
		`window.addEventListener("online", refreshVisibleDashboard);`,
		`window.addEventListener("offline", markDashboardOffline);`,
		`window.addEventListener("pagehide", handlePageHide);`,
		`window.addEventListener("pageshow", (event) => {`,
		`event?.persisted`,
		`window.addEventListener("resize", scheduleClientCheck);`,
		`window.addEventListener("orientationchange", scheduleClientCheck);`,
		`function refreshVisibleDashboard() {`,
		`function handleVisibilityChange() {`,
		`sendClientCheckBeacon();`,
		`function markDashboardOffline() {`,
		`setConnectionState("bad", "Offline");`,
		`function scheduleClientCheckPolling() {`,
		`function scheduleClientCheck() {`,
		`function shouldSendClientCheck() {`,
		`return document.visibilityState === "visible";`,
		`function refreshNow() {`,
		`const clientCheck = sendClientCheck({ interaction: "status_strip_tap" });`,
		`showTransientStatus("Refreshing");`,
		`Promise.allSettled([clientCheck, metrics]).then(([clientCheckResult]) => {`,
		`clientCheckResult.value`,
		`function sendClientCheck(options = {}) {`,
		`clientCheckPayload(options)`,
		`function sendClientCheckBeacon() {`,
		`navigator.sendBeacon("/api/client-check", blob)`,
		`fetchWithTimeout("/api/client-check", {`,
		`if (!response.ok) {`,
		`throw new Error(` + "`HTTP ${response.status}`" + `);`,
		`accepted = await response.json();`,
		`function acceptedClientCheckModeLabel(clientCheck) {`,
		`const displayMode = String(clientCheck?.display_mode || "").trim();`,
		`return clientModeLabel(displayMode);`,
		`return false;`,
		"showTransientStatus(`Client check sent (${acceptedClientCheckModeLabel(clientCheckResult.value)})`);",
		`function clientCheckPayload(options = {}) {`,
		`dashboard_build: dashboardBuild,`,
		`const interaction = String(options.interaction || "").trim();`,
		`payload.interaction = interaction;`,
		`viewport_width: positiveInteger(window.innerWidth),`,
		`screen_width: positiveInteger(window.screen?.width),`,
		`screen_height: positiveInteger(window.screen?.height),`,
		`touch_points: positiveInteger(navigator.maxTouchPoints),`,
		`display_mode: displayMode,`,
		`standalone: displayMode === "standalone",`,
		`orientation: currentOrientation(),`,
		`function currentDisplayMode() {`,
		`function clientModeLabel(displayMode) {`,
		`return "app";`,
		`return "web";`,
		`function currentOrientation() {`,
		`const response = await fetchWithTimeout("/api/settings", {`,
		`}, auxiliaryTimeoutMS);`,
	} {
		if !strings.Contains(appJS, needle) {
			t.Fatalf("app.js missing timeout-backed dashboard request %q", needle)
		}
	}
}

func TestDashboardPersistsWakePreferenceLocally(t *testing.T) {
	app, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(app)
	for _, needle := range []string{
		`const wakePreferenceKey = "sysmon:wake-wanted";`,
		`restoreWakePreference();`,
		`function restoreWakePreference() {`,
		`readStoredBoolean(wakePreferenceKey)`,
		`function readStoredBoolean(key) {`,
		`window.localStorage?.getItem(key) === "1"`,
		`function writeStoredBoolean(key, value) {`,
		`window.localStorage?.setItem(key, value ? "1" : "0")`,
		`writeStoredBoolean(wakePreferenceKey, state.wakeWanted);`,
		`writeStoredBoolean(wakePreferenceKey, false);`,
	} {
		if !strings.Contains(appJS, needle) {
			t.Fatalf("app.js missing local wake preference behavior %q", needle)
		}
	}
}

func TestReadmeDocumentsLocalWakePreference(t *testing.T) {
	data, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `const wakePreferenceKey = "sysmon:wake-wanted";`) {
		t.Fatal("app.js no longer has the local Wake preference key")
	}

	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{
		`Wake preference is remembered`,
		`locally on the device`,
		`Wake is not stored on the host because it represents per-device browser state`,
		`device dashboard's local storage`,
	} {
		if !strings.Contains(string(readme), needle) {
			t.Fatalf("README.md missing local Wake preference note %q", needle)
		}
	}
}

func TestReadmeDocumentsStrictHomeScreenGate(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{
		`polls the ` + "`/api/client-checks`" + ` history, falls back to ` + "`/api/client-check`",
		`also considers the ` + "`/api/status`" + ` ` + "`device_client_check`",
		`status-carried device evidence is not missed`,
		`requires both ` + "`standalone=true`" + ` and ` + "`display_mode=standalone`",
		`gate requiring ` + "`standalone=true`",
		"`interaction=status_strip_tap`",
		`Client checks include the dashboard build token from ` + "`/api/status`",
		`passive page-open beacon cannot satisfy`,
		`history, latest endpoint, or ` + "`/api/status`" + ` client-check fields have`,
		"`standalone=true`" + `, ` + "`display_mode=standalone`",
		"`interaction=status_strip_tap`" + `, and a ` + "`dashboard_build`" + ` matching ` + "`/api/status`",
		`plus a ` + "`last_seen`" + ` timestamp from the hold window`,
		`Tap the status strip while that issue is`,
		`visible to unregister the Sysmon service worker`,
		`clear only ` + "`sysmon-static-*`",
		`caches, and reload the Home Screen dashboard once`,
		`Successful device control changes also send lightweight ` + "`settings_*`",
		`without relaxing the final deployed verifier's ` + "`status_strip_tap`" + ` requirement`,
	} {
		if !strings.Contains(string(readme), needle) {
			t.Fatalf("README.md missing strict Home Screen gate note %q", needle)
		}
	}
}

func TestReadmeDocumentsTrustedProxyHeadersForDeviceControls(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{
		`Settings updates require an ` + "`Origin`" + ` header matching the public request host`,
		`including script-driven checks`,
		`X-Forwarded-Host`,
		`standard ` + "`Forwarded`" + ` header with ` + "`host=...`",
		`installed device dashboard`,
		`ignores spoofed`,
		`non-loopback clients`,
	} {
		if !strings.Contains(string(readme), needle) {
			t.Fatalf("README.md missing trusted proxy header note %q", needle)
		}
	}
}

func TestDashboardSkipsHiddenIntervalPolling(t *testing.T) {
	app, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(app)
	for _, needle := range []string{
		`syncVisibleTimers();`,
		`window.addEventListener("pagehide", handlePageHide);`,
		`if (event?.persisted) {`,
		`function syncVisibleTimers() {`,
		`function stopVisibleTimers() {`,
		`clearDashboardInterval("timer");`,
		`clearDashboardInterval("statusTimer");`,
		`clearDashboardInterval("staleTimer");`,
		`clearDashboardInterval("clientCheckTimer");`,
		`clearDashboardTimeout("clientCheckDebounceTimer");`,
		`function handlePageHide() {`,
		`if (shouldPollMetrics()) {`,
		`function shouldPollMetrics() {`,
		`return !state.paused && document.visibilityState === "visible";`,
		`if (shouldPollStatus()) {`,
		`function shouldPollStatus() {`,
		`return document.visibilityState === "visible";`,
	} {
		if !strings.Contains(appJS, needle) {
			t.Fatalf("app.js missing hidden interval polling guard %q", needle)
		}
	}
}

func TestDashboardKeepsStaleStateDuringRefresh(t *testing.T) {
	app, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(app)
	for _, needle := range []string{
		`if (state.lastMetricsAtMS === 0 || state.connectionKind === "bad") {`,
		`setConnectionState("loading", "Updating");`,
		`setConnectionState("warn", "Stale");`,
	} {
		if !strings.Contains(appJS, needle) {
			t.Fatalf("app.js missing stale refresh state behavior %q", needle)
		}
	}
}

func TestDashboardUnavailableGaugesUseMutedRing(t *testing.T) {
	app, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(app)
	for _, needle := range []string{
		`if (metric.available) {`,
		`gauge.style.setProperty("--p", gaugeValue);`,
		`gauge.style.setProperty("--c", colorFor(gaugeValue, warnThreshold));`,
		`} else {`,
		`gauge.style.setProperty("--p", 100);`,
		`gauge.style.setProperty("--c", "#394656");`,
	} {
		if !strings.Contains(appJS, needle) {
			t.Fatalf("app.js missing unavailable gauge muted ring behavior %q", needle)
		}
	}
}

func TestDashboardRendersCollectorIssuesPanel(t *testing.T) {
	app, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(app)
	for _, needle := range []string{
		`issuesExpanded: false,`,
		`issueMessages: [],`,
		`clientIssueMessages: [],`,
		`displayIssueMessages: [],`,
		`metricIssueMessages: [],`,
		`settingsIssueMessages: [],`,
		`statusIssueMessages: [],`,
		`const collapsedIssueLimit = 5;`,
		`$("issuesPanel").addEventListener("click", toggleIssuesPanel);`,
		`$("issuesPanel").addEventListener("keydown", handleIssuesPanelKeydown);`,
		`renderStatusIssues(status);`,
		`function renderStatusIssues(status) {`,
		`dashboard build stale: app ${dashboardBuild}, server ${serverBuild}; tap status strip to refresh app or re-add Home Screen app`,
		`latest client check stale: client ${clientBuild}, app ${dashboardBuild}; reload or re-add Home Screen app`,
		`latest device check stale: client ${deviceBuild}, app ${dashboardBuild}; reload or re-add Home Screen app`,
		`latest ${proofLabel} check stale: seen ${formatSampleAge(clientCheckLastSeenMS)} ago; tap status strip to refresh client check`,
		`renderDisplayModeIssues();`,
		`function renderDisplayModeIssues() {`,
		`function shouldWarnAboutDisplayMode(displayMode) {`,
		`function isMobileClient() {`,
		"`Display is in ${clientModeLabel(displayMode)} mode; open the installed Home Screen app for final monitor verification`",
		`renderStatusError(error);`,
		`function renderStatusError(error) {`,
		"`status unavailable: ${message || \"request failed\"}`",
		`renderCollectionErrors(metrics.collection_errors);`,
		`function renderCollectionErrors(errors) {`,
		`state.metricIssueMessages = Array.isArray(errors)`,
		`renderMetricError(error);`,
		`function renderMetricError(error) {`,
		"`metrics unavailable: ${message || \"request failed\"}`",
		`clearClientCheckIssue();`,
		`function renderClientCheckError(error) {`,
		"`client check unavailable: ${message || \"request failed\"}`",
		`function renderIssuesPanel() {`,
		`...state.displayIssueMessages,`,
		`...state.settingsIssueMessages,`,
		`...state.clientIssueMessages,`,
		`...state.metricIssueMessages,`,
		`state.issueMessages = messages;`,
		`if (messages.length <= collapsedIssueLimit) {`,
		`panel.hidden = messages.length === 0;`,
		`panel.classList.toggle("expanded", state.issuesExpanded);`,
		`panel.setAttribute("aria-expanded", state.issuesExpanded ? "true" : "false");`,
		`const visibleMessages = state.issuesExpanded ? messages : messages.slice(0, collapsedIssueLimit);`,
		"summary.textContent = `${messages.length} issue${messages.length === 1 ? \"\" : \"s\"}`;",
		"list.append(issueRow(`${messages.length - collapsedIssueLimit} more`));",
		`function toggleIssuesPanel() {`,
		`state.issuesExpanded = !state.issuesExpanded;`,
		`function handleIssuesPanelKeydown(event) {`,
		`event.preventDefault();`,
		`function issueRow(message) {`,
		`row.className = "row issue-row";`,
	} {
		if !strings.Contains(appJS, needle) {
			t.Fatalf("app.js missing collector issues renderer %q", needle)
		}
	}
	if strings.Contains(appJS, `renderCollectionErrors(state.issueMessages);`) {
		t.Fatal("issues panel toggle must not feed combined status and metric issues back into metric issue state")
	}

	cssData, err := staticFS.ReadFile("static/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(cssData)
	for _, needle := range []string{
		`.issues-panel {`,
		`border-color: #7a5c22;`,
		`.issues-panel[role="button"] {`,
		`touch-action: manipulation;`,
		`.issues-panel:focus-visible {`,
		`.issue-row {`,
		`color: var(--warn);`,
		`display: grid;`,
	} {
		if !strings.Contains(css, needle) {
			t.Fatalf("styles.css missing collector issues style %q", needle)
		}
	}
}

func TestDashboardRendersMetricAlertsPanel(t *testing.T) {
	app, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(app)
	for _, needle := range []string{
		`alertsExpanded: false,`,
		`alertMessages: [],`,
		`$("alertsPanel").addEventListener("click", toggleAlertsPanel);`,
		`$("alertsPanel").addEventListener("keydown", handleAlertsPanelKeydown);`,
		`renderMetricAlerts(metrics);`,
		`function renderMetricAlerts(metrics) {`,
		`state.alertMessages = messages;`,
		"summary.textContent = `${messages.length} alert${messages.length === 1 ? \"\" : \"s\"}`;",
		`function metricAlertMessages(metrics) {`,
		`addPercentAlert(messages, ` + "`Disk ${label}`" + `, capacityPercent(disk.capacity), thresholdValue("disk_warn"));`,
		`if (isPrimaryCardTemperatureSensor(sensor.name)) {`,
		`addTemperatureAlert(messages, sensor.name || "sensor", numberMetric(sensor.celsius), thresholdValue("temp_warn_c"));`,
		`function isPrimaryCardTemperatureSensor(name) {`,
		`function addPercentAlert(messages, label, metric, threshold) {`,
		"messages.push(`${label} ${Math.round(metric.value)}% over ${threshold}%`);",
		`function addTemperatureAlert(messages, label, metric, threshold) {`,
		"messages.push(`${label} ${formatTemp(metric.value)} over ${formatTemp(threshold)}`);",
		`function toggleAlertsPanel() {`,
		`state.alertsExpanded = !state.alertsExpanded;`,
		`function renderMetricAlertsFromMessages(messages) {`,
		`function handleAlertsPanelKeydown(event) {`,
		`function alertRow(message) {`,
		`row.className = "row alert-row";`,
	} {
		if !strings.Contains(appJS, needle) {
			t.Fatalf("app.js missing metric alerts renderer %q", needle)
		}
	}

	cssData, err := staticFS.ReadFile("static/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(cssData)
	for _, needle := range []string{
		`.alerts-panel,`,
		`.alerts-panel {`,
		`border-color: #7a3030;`,
		`.alerts-panel[role="button"],`,
		`touch-action: manipulation;`,
		`.alerts-panel:focus-visible,`,
		`.alert-row,`,
		`.alert-row {`,
		`color: var(--bad);`,
		`display: grid;`,
	} {
		if !strings.Contains(css, needle) {
			t.Fatalf("styles.css missing metric alerts style %q", needle)
		}
	}
}

func TestDashboardSettingsFailuresShowTransientAndIssue(t *testing.T) {
	app, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(app)
	for _, needle := range []string{
		`transientStatusTimer: null,`,
		`renderSettingsError("settings update failed", error);`,
		`function renderSettingsError(prefix, error) {`,
		"state.settingsIssueMessages = [`${prefix}: ${message || \"request failed\"}`];",
		`function clearSettingsIssue() {`,
		`showTransientStatus(error.message ? ` + "`Settings failed: ${error.message}`" + ` : "Settings failed");`,
		`function showTransientStatus(text) {`,
		`$("statusText").textContent = state.connectionText || "Updating";`,
		`function clearTransientStatus() {`,
		`clearTransientStatus();`,
		`state.connectionKind = kind;`,
		`state.connectionText = text;`,
	} {
		if !strings.Contains(appJS, needle) {
			t.Fatalf("app.js missing transient settings failure behavior %q", needle)
		}
	}
}

func TestDashboardSettingsUpdatesAreOptimistic(t *testing.T) {
	app, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(app)
	for _, needle := range []string{
		`async function updateSettings(update, interaction = "") {`,
		`state.settingsInFlight = true;`,
		`const previousSettings = state.settings;`,
		`const optimisticSettings = mergeSettings(state.settings, update);`,
		`function mergeSettings(base, update) {`,
		`applySettings(optimisticSettings);`,
		`sendSettingsInteraction(interaction);`,
		`function sendSettingsInteraction(interaction) {`,
		`sendClientCheck({ interaction });`,
		`applySettings(previousSettings);`,
		`fetchSettings({ clearIssueOnSuccess: false });`,
		`} finally {`,
		`state.settingsInFlight = false;`,
	} {
		if !strings.Contains(appJS, needle) {
			t.Fatalf("app.js missing optimistic settings update behavior %q", needle)
		}
	}
}

func TestDashboardWakeLockUnavailableClearsWantedState(t *testing.T) {
	app, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(app)
	for _, needle := range []string{
		`if (!("wakeLock" in navigator)) {`,
		`state.wakeWanted = false;`,
		`const lock = state.wakeLock;`,
		`state.wakeLock = null;`,
		`setWakeButtonIdle();`,
		`await lock.release();`,
		`const lock = await navigator.wakeLock.request("screen");`,
		`lock.addEventListener("release", () => handleWakeLockRelease(lock));`,
		`function handleWakeLockRelease(lock) {`,
		`if (state.wakeLock !== lock) {`,
		`if (!state.wakeWanted) {`,
		`setIconButton(button, "☀", "Wake lock retrying");`,
		`if (document.visibilityState === "visible") {`,
		`requestWakeLock();`,
		`setIconButton(button, "×", "Wake lock unavailable");`,
		`setIconButton(button, "×", "Wake lock denied");`,
		`setPressed(button, false);`,
	} {
		if !strings.Contains(appJS, needle) {
			t.Fatalf("app.js missing wake lock unavailable handling %q", needle)
		}
	}
}

func TestDashboardRowsWrapLongDegradedDetails(t *testing.T) {
	data, err := staticFS.ReadFile("static/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(data)
	for _, needle := range []string{
		`.row > :first-child {`,
		`min-width: 0;`,
		`.alert-row,`,
		`.issue-row {`,
		`grid-template-columns: minmax(0, 1fr);`,
		`line-height: 1.25;`,
		`overflow-wrap: anywhere;`,
	} {
		if !strings.Contains(css, needle) {
			t.Fatalf("styles.css missing long degraded detail wrapping rule %q", needle)
		}
	}
}

func TestDashboardSuppressesAccidentalDeviceGestures(t *testing.T) {
	data, err := staticFS.ReadFile("static/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(data)
	for _, needle := range []string{
		`overscroll-behavior: none;`,
		`-webkit-tap-highlight-color: transparent;`,
		`-webkit-user-select: none;`,
		`user-select: none;`,
	} {
		if !strings.Contains(css, needle) {
			t.Fatalf("styles.css missing device gesture guard %q", needle)
		}
	}
}

func TestDashboardUsesInstalledAppViewport(t *testing.T) {
	index, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	indexHTML := string(index)
	for _, needle := range []string{
		`viewport-fit=cover`,
		`apple-mobile-web-app-status-bar-style`,
		`apple-mobile-web-app-capable`,
	} {
		if !strings.Contains(indexHTML, needle) {
			t.Fatalf("index.html missing installed-app viewport metadata %q", needle)
		}
	}

	data, err := staticFS.ReadFile("static/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(data)
	for _, needle := range []string{
		`--pad-x: 12px;`,
		`min-height: 100svh;`,
		`env(safe-area-inset-right)`,
		`env(safe-area-inset-left)`,
		`@supports (min-height: 100dvh)`,
		`min-height: 100dvh;`,
	} {
		if !strings.Contains(css, needle) {
			t.Fatalf("styles.css missing installed-app viewport rule %q", needle)
		}
	}
}

func TestDashboardControlsExposePressedState(t *testing.T) {
	index, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	indexHTML := string(index)
	for _, needle := range []string{
		`id="dimBtn" class="button" type="button" aria-label="Dim mode" aria-pressed="false"`,
		`id="shiftBtn" class="button" type="button" aria-label="Screen shift mode" aria-pressed="false"`,
		`id="wakeBtn" class="button" type="button" aria-label="Keep screen awake" aria-pressed="false"`,
		`id="pauseBtn" class="button" type="button" aria-label="Pause updates" aria-pressed="false"`,
		`id="micCtl" class="control-btn" type="button" data-control="mic_mute" aria-pressed="false"`,
		`id="volumeCtl" class="control-btn" type="button" data-control="volume_mute" aria-pressed="false"`,
	} {
		if !strings.Contains(indexHTML, needle) {
			t.Fatalf("index.html missing pressed-state control markup %q", needle)
		}
	}

	app, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(app)
	for _, needle := range []string{
		`function setPressed(button, pressed) {`,
		`button.classList.toggle("active", pressed);`,
		`button.setAttribute("aria-pressed", pressed ? "true" : "false");`,
		`setPressed(button, muted);`,
		`setPressed(button, state.paused);`,
	} {
		if !strings.Contains(appJS, needle) {
			t.Fatalf("app.js missing pressed-state sync %q", needle)
		}
	}
}

// Warn thresholds moved off the touch UI onto host config (CLI flags / env), but
// the threshold *values* still flow through settings into gauge/sparkline/alert
// coloring. This guards that plumbing without the removed stepper UI.
func TestDashboardThresholdValuesDriveColoring(t *testing.T) {
	app, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(app)
	for _, needle := range []string{
		`const defaultThresholds = {`,
		`const thresholdTargets = [`,
		`function mergeSettings(base, update) {`,
		`next.thresholds = { ...(base.thresholds || defaultThresholds), ...update.thresholds };`,
		`thresholds: normalizeThresholds(settings?.thresholds),`,
		`function normalizeThresholds(thresholds) {`,
		`function thresholdValue(key) {`,
		`function colorFor(percent, warnThreshold = defaultThresholds.cpu_warn) {`,
	} {
		if !strings.Contains(appJS, needle) {
			t.Fatalf("app.js missing threshold value plumbing %q", needle)
		}
	}

	// The removed stepper UI must be fully gone.
	for _, absent := range []string{
		`thresholdTargetIndex`,
		`thresholdTargetBtn`,
		`thresholdDownBtn`,
		`thresholdUpBtn`,
		`function syncThresholdControls(`,
		`function stepActiveThreshold(`,
		`"settings_threshold"`,
	} {
		if strings.Contains(appJS, absent) {
			t.Fatalf("app.js still references removed threshold UI %q", absent)
		}
	}
}

// The bottom toolbar exposes four host-control buttons (mic mute, media
// play/pause, speaker mute, lock screen) wired by fixed id to /api/control.
func TestDashboardHostControlButtons(t *testing.T) {
	index, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	indexHTML := string(index)
	for _, needle := range []string{
		`id="micCtl" class="control-btn" type="button" data-control="mic_mute"`,
		`id="mediaCtl" class="control-btn" type="button" data-control="media_toggle"`,
		`id="volumeCtl" class="control-btn" type="button" data-control="volume_mute"`,
		`id="lockCtl" class="control-btn" type="button" data-control="lock_screen"`,
		`<span class="control-label" id="micCtlLabel">Mic</span>`,
		`<span class="control-glyph" id="volumeCtlGlyph" aria-hidden="true">🔊</span>`,
		`<span class="control-label" id="volumeCtlLabel">Speaker</span>`,
	} {
		if !strings.Contains(indexHTML, needle) {
			t.Fatalf("index.html missing host control markup %q", needle)
		}
	}
	// The removed refresh + threshold rows must be gone.
	for _, absent := range []string{`data-interval=`, `threshold-row`, `class="segment`} {
		if strings.Contains(indexHTML, absent) {
			t.Fatalf("index.html still references removed toolbar markup %q", absent)
		}
	}

	app, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(app)
	for _, needle := range []string{
		`const controlButtonIDs = {`,
		`mic_mute: "micCtl",`,
		`media_toggle: "mediaCtl",`,
		`volume_mute: "volumeCtl",`,
		`lock_screen: "lockCtl",`,
		`button.addEventListener("click", () => sendControl(action, button));`,
		`async function sendControl(action, button) {`,
		`fetchWithTimeout(`,
		`"/api/control",`,
		`body: JSON.stringify({ action }),`,
		`function applyControlCapabilities(controls) {`,
		`button.disabled = !available.has(action);`,
		`applyControlCapabilities(status?.controls);`,
		`$("volumeCtlGlyph").textContent = muted ? "🔇" : "🔊";`,
	} {
		if !strings.Contains(appJS, needle) {
			t.Fatalf("app.js missing host control behavior %q", needle)
		}
	}

	css, err := staticFS.ReadFile("static/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	cssText := string(css)
	for _, needle := range []string{
		`.control-btn {`,
		`.control-glyph {`,
		`.control-label {`,
		`.control-btn:disabled {`,
		`.bottom-controls {`,
	} {
		if !strings.Contains(cssText, needle) {
			t.Fatalf("styles.css missing host control style %q", needle)
		}
	}
}

func TestCoarsePointerControlsHaveUsableTouchTargets(t *testing.T) {
	data, err := staticFS.ReadFile("static/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(data)
	for _, needle := range []string{
		`@media (pointer: coarse) {`,
		`grid-template-columns: repeat(4, 40px);`,
		`width: 40px;`,
		`min-width: 40px;`,
		`min-height: 40px;`,
		`.status-strip {`,
		`.metric-card {`,
		`min-height: 118px;`,
	} {
		if !strings.Contains(css, needle) {
			t.Fatalf("styles.css missing coarse pointer touch rule %q", needle)
		}
	}
}

func TestNarrowDeviceHeaderKeepsHostnameUsable(t *testing.T) {
	data, err := staticFS.ReadFile("static/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(data)
	for _, needle := range []string{
		`@media (max-width: 390px) {`,
		`.topbar {`,
		`align-items: stretch;`,
		`flex-direction: column;`,
		`.top-actions {`,
		`width: 100%;`,
		`grid-template-columns: repeat(4, minmax(0, 1fr));`,
		`.button {`,
		`width: 100%;`,
		`min-width: 0;`,
	} {
		if !strings.Contains(css, needle) {
			t.Fatalf("styles.css missing narrow device header rule %q", needle)
		}
	}

	const (
		viewportWidthPX = 320
		shellPaddingPX  = 10 * 2
		buttonGapPX     = 6 * 3
		buttonCount     = 4
		minTouchPX      = 40
	)
	contentWidth := viewportWidthPX - shellPaddingPX
	buttonWidth := (contentWidth - buttonGapPX) / buttonCount
	if buttonWidth < minTouchPX {
		t.Fatalf("narrow header action button width = %dpx, want at least %dpx", buttonWidth, minTouchPX)
	}
}

func TestDashboardFontSizesAvoidViewportUnits(t *testing.T) {
	data, err := staticFS.ReadFile("static/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "font-size:") {
			continue
		}
		for _, unit := range []string{"vw", "vh", "vmin", "vmax"} {
			if strings.Contains(trimmed, unit) {
				t.Fatalf("font-size uses viewport unit %q: %s", unit, trimmed)
			}
		}
	}
}

func TestNarrowDeviceMetricCardsFitFourAcross(t *testing.T) {
	data, err := staticFS.ReadFile("static/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(data)
	for _, needle := range []string{
		`@media (max-width: 360px) {`,
		`.metric-card {`,
		`padding: 8px 4px 7px;`,
		`.gauge {`,
		`min-width: 54px;`,
		`.control-btn {`,
		`padding: 0 6px;`,
	} {
		if !strings.Contains(css, needle) {
			t.Fatalf("styles.css missing narrow device layout rule %q", needle)
		}
	}

	const (
		viewportWidthPX     = 320
		shellPaddingPX      = 10 * 2
		gridGapPX           = 7 * 3
		cardHorizontalPadPX = 4 * 2
		gaugeMinWidthPX     = 54
		columnCount         = 4
	)
	contentWidth := viewportWidthPX - shellPaddingPX
	cardWidth := (contentWidth - gridGapPX) / columnCount
	requiredWidth := gaugeMinWidthPX + cardHorizontalPadPX
	if requiredWidth > cardWidth {
		t.Fatalf("narrow metric card needs %dpx but only has %dpx", requiredWidth, cardWidth)
	}
}

func TestLandscapeDeviceFillsViewportWithGauges(t *testing.T) {
	data, err := staticFS.ReadFile("static/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(data)
	// In landscape the dashboard becomes a flex column whose gauge row grows to
	// fill the viewport height, the circles scale off the short side, and the
	// bottom controls are tucked out of the always-on glance.
	for _, needle := range []string{
		`@media (orientation: landscape) and (max-height: 500px) {`,
		`flex-direction: column;`,
		`flex: 1 1 auto;`,
		`width: min(42vh, 21vw);`,
		`min-width: 92px;`,
		`.bottom-controls {`,
		`display: none;`,
	} {
		if !strings.Contains(css, needle) {
			t.Fatalf("styles.css missing landscape device layout rule %q", needle)
		}
	}
}

func TestDashboardPreventsDeviceTextInflation(t *testing.T) {
	data, err := staticFS.ReadFile("static/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(data)
	for _, needle := range []string{
		`-webkit-text-size-adjust: 100%;`,
		`text-size-adjust: 100%;`,
	} {
		if !strings.Contains(css, needle) {
			t.Fatalf("styles.css missing device text sizing guard %q", needle)
		}
	}
}

func TestMobileStatusMetadataRemainsVisible(t *testing.T) {
	data, err := staticFS.ReadFile("static/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(data)
	if strings.Contains(css, "#agentMeta {\n    display: none;") {
		t.Fatal("mobile CSS hides agent status metadata")
	}
	for _, needle := range []string{
		`grid-template-areas:`,
		`"meta meta"`,
		`#agentMeta {`,
		`grid-area: meta;`,
		`.status-dot.warn {`,
		`.status-dot.paused {`,
		`.status-strip:focus-visible {`,
		`touch-action: manipulation;`,
	} {
		if !strings.Contains(css, needle) {
			t.Fatalf("styles.css missing mobile status metadata rule %q", needle)
		}
	}
}

func TestMobileHeaderControlsStayCompact(t *testing.T) {
	index, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	indexHTML := string(index)
	for _, needle := range []string{
		`id="dimBtn" class="button" type="button" aria-label="Dim mode"`,
		`id="shiftBtn" class="button" type="button" aria-label="Screen shift mode"`,
		`id="wakeBtn" class="button" type="button" aria-label="Keep screen awake"`,
		`id="pauseBtn" class="button" type="button" aria-label="Pause updates"`,
	} {
		if !strings.Contains(indexHTML, needle) {
			t.Fatalf("index.html missing compact mobile control %q", needle)
		}
	}

	data, err := staticFS.ReadFile("static/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(data)
	for _, needle := range []string{
		`grid-template-columns: repeat(4, 34px);`,
		`.topbar > :first-child {`,
		`min-width: 0;`,
		`width: 34px;`,
		`touch-action: manipulation;`,
	} {
		if !strings.Contains(css, needle) {
			t.Fatalf("styles.css missing compact header rule %q", needle)
		}
	}
}

func TestScreenShiftModeMovesDashboardSlightly(t *testing.T) {
	data, err := staticFS.ReadFile("static/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(data)
	for _, needle := range []string{
		`body.shift .shell {`,
		`animation: screen-shift 120s steps(1, end) infinite;`,
		`@keyframes screen-shift {`,
		`translate3d(2px, 1px, 0)`,
		`translate3d(-2px, -1px, 0)`,
	} {
		if !strings.Contains(css, needle) {
			t.Fatalf("styles.css missing screen shift rule %q", needle)
		}
	}

	app, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(app), `shift: true,`) {
		t.Fatal("app.js default state does not enable screen shift for always-on display fallback")
	}
}

func TestDashboardRendersPrimaryMetricSparklines(t *testing.T) {
	index, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	indexHTML := string(index)
	for _, needle := range []string{
		`<div class="sparkline" id="cpuTrend" aria-hidden="true"></div>`,
		`<div class="sparkline" id="memTrend" aria-hidden="true"></div>`,
		`<div class="sparkline" id="gpuTrend" aria-hidden="true"></div>`,
		`<div class="sparkline" id="netTrend" aria-hidden="true"></div>`,
	} {
		if !strings.Contains(indexHTML, needle) {
			t.Fatalf("index.html missing primary metric sparkline markup %q", needle)
		}
	}

	app, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(app)
	for _, needle := range []string{
		`history: {`,
		`const sparklineSampleLimit = 24;`,
		`appendPrimaryMetricHistory({`,
		`net: net.available ? { available: true, value: netRingPercent(net.rx) } : unavailable(),`,
		`function appendPrimaryMetricHistory(metrics) {`,
		`renderSparkline("cpuTrend", state.history.cpu, thresholdValue("cpu_warn"));`,
		`renderSparkline("netTrend", state.history.net, netRingWarnPercent);`,
		`function appendMetricHistory(key, metric) {`,
		`metric?.available ? clamp(metric.value, 0, 100) : null`,
		`function renderSparkline(id, samples, warnThreshold) {`,
		`Array(Math.max(0, sparklineSampleLimit - recentSamples.length)).fill(null)`,
		`function sparklineBar(sample, warnThreshold) {`,
		`bar.className = "sparkline-bar";`,
		`bar.classList.add("unavailable");`,
		`Math.max(6, Math.round(value))`,
		`colorFor(value, warnThreshold)`,
	} {
		if !strings.Contains(appJS, needle) {
			t.Fatalf("app.js missing primary metric sparkline behavior %q", needle)
		}
	}

	data, err := staticFS.ReadFile("static/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(data)
	for _, needle := range []string{
		`.sparkline {`,
		`grid-template-columns: repeat(24, minmax(0, 1fr));`,
		`height: 18px;`,
		`.sparkline-bar {`,
		`height: var(--h, 3px);`,
		`.sparkline-bar.unavailable {`,
		`background: #394656;`,
	} {
		if !strings.Contains(css, needle) {
			t.Fatalf("styles.css missing primary metric sparkline style %q", needle)
		}
	}
}
