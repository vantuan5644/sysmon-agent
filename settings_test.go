package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultDashboardSettingsPreferAlwaysOnDeviceDisplay(t *testing.T) {
	got := defaultDashboardSettings()
	if !got.Shift || got.Dim || got.RefreshMS != defaultRefreshMS || got.Panel != defaultPanelMode {
		t.Fatalf("default settings = %+v, want shift on, dim off, default refresh and all panels", got)
	}
	if got.Thresholds != defaultDashboardThresholds() {
		t.Fatalf("default thresholds = %+v, want %+v", got.Thresholds, defaultDashboardThresholds())
	}
	if got.UpdatedAt.IsZero() {
		t.Fatal("default settings UpdatedAt is zero")
	}
}

func TestRuntimeStatePersistsSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")

	state, err := NewRuntimeState(path)
	if err != nil {
		t.Fatal(err)
	}

	refresh := 2000
	shift := true
	panel := "gpu"
	if _, err := state.UpdateSettings(DashboardSettingsUpdate{Shift: &shift, RefreshMS: &refresh, Panel: &panel}); err != nil {
		t.Fatal(err)
	}

	reloaded, err := NewRuntimeState(path)
	if err != nil {
		t.Fatal(err)
	}
	got := reloaded.GetSettings()
	if !got.Shift || got.RefreshMS != 2000 || got.Panel != "gpu" {
		t.Fatalf("settings = %+v, want persisted shift, 2000ms refresh, and gpu panel", got)
	}
}

func TestRuntimeStatePersistsThresholdSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")

	state, err := NewRuntimeState(path)
	if err != nil {
		t.Fatal(err)
	}

	cpuWarn := 80
	gpuWarn := 85
	tempWarn := 75
	if _, err := state.UpdateSettings(DashboardSettingsUpdate{Thresholds: &DashboardThresholdsUpdate{
		CPUWarn:   &cpuWarn,
		GPUWarn:   &gpuWarn,
		TempWarnC: &tempWarn,
	}}); err != nil {
		t.Fatal(err)
	}

	reloaded, err := NewRuntimeState(path)
	if err != nil {
		t.Fatal(err)
	}
	got := reloaded.GetSettings().Thresholds
	if got.CPUWarn != cpuWarn || got.GPUWarn != gpuWarn || got.TempWarnC != tempWarn {
		t.Fatalf("thresholds = %+v, want persisted CPU/GPU/temp thresholds", got)
	}
	if got.MemoryWarn != defaultWarningThreshold || got.DiskWarn != defaultWarningThreshold {
		t.Fatalf("untouched thresholds = %+v, want defaults preserved", got)
	}
}

func TestSaveDashboardSettingsReplacesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	first := defaultDashboardSettings()
	if err := saveDashboardSettings(path, first); err != nil {
		t.Fatal(err)
	}

	second := first
	second.RefreshMS = 2000
	second.Panel = "network"
	if err := saveDashboardSettings(path, second); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadDashboardSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.RefreshMS != 2000 || loaded.Panel != "network" {
		t.Fatalf("settings = %+v, want replacement settings", loaded)
	}
}

func TestLoadDashboardSettingsBackfillsLegacyThresholds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"compact":true,"refresh_ms":2000,"panel":"gpu","updated_at":"2026-06-21T12:00:00Z"}`), 0644); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadDashboardSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	// The legacy "compact" key is no longer a field; loading must ignore it and
	// preserve the remaining values.
	if loaded.RefreshMS != 2000 || loaded.Panel != "gpu" {
		t.Fatalf("settings = %+v, want legacy settings values preserved", loaded)
	}
	if loaded.Thresholds != defaultDashboardThresholds() {
		t.Fatalf("thresholds = %+v, want legacy load to backfill defaults", loaded.Thresholds)
	}
}

func TestRuntimeStateKeepsRecentClientCheckHistory(t *testing.T) {
	state := NewMemoryRuntimeState()
	start := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)

	for i := 0; i < maxClientCheckHistory+3; i++ {
		width := 300 + i
		if _, err := state.RecordClientCheck(ClientCheckUpdate{
			Interaction:      "auto",
			ViewportWidth:    width,
			ViewportHeight:   800,
			DevicePixelRatio: 2,
			DisplayMode:      "browser",
			Visibility:       "visible",
		}, "test-agent", start.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}

	history := state.GetClientCheckHistory()
	if len(history.Checks) != maxClientCheckHistory {
		t.Fatalf("history length = %d, want %d", len(history.Checks), maxClientCheckHistory)
	}
	if got := history.Checks[0].ViewportWidth; got != 300+maxClientCheckHistory+2 {
		t.Fatalf("latest history viewport = %d, want newest sample", got)
	}
	if got := history.Checks[len(history.Checks)-1].ViewportWidth; got != 300+3 {
		t.Fatalf("oldest history viewport = %d, want capped recent sample", got)
	}
	if latest := state.GetClientCheck(); latest.ViewportWidth != history.Checks[0].ViewportWidth {
		t.Fatalf("latest check = %+v, want newest history sample %+v", latest, history.Checks[0])
	}
	if latest := state.GetClientCheck(); latest.Interaction != "auto" {
		t.Fatalf("latest check interaction = %q, want auto", latest.Interaction)
	}
}

func TestRuntimeStateKeepsLatestDeviceClientCheckSeparateFromLatestClientCheck(t *testing.T) {
	state := NewMemoryRuntimeState()
	start := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)

	if _, err := state.RecordClientCheck(ClientCheckUpdate{
		ViewportWidth:    390,
		ViewportHeight:   844,
		DevicePixelRatio: 3,
		DisplayMode:      "standalone",
		Standalone:       true,
		Visibility:       "visible",
	}, "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) Mobile Safari/604.1", start); err != nil {
		t.Fatal(err)
	}
	if _, err := state.RecordClientCheck(ClientCheckUpdate{
		ViewportWidth:    1440,
		ViewportHeight:   900,
		DevicePixelRatio: 1,
		DisplayMode:      "browser",
		Visibility:       "visible",
	}, "Mozilla/5.0 (X11; Linux x86_64) Firefox/128.0", start.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	if latest := state.GetClientCheck(); latest.ViewportWidth != 1440 {
		t.Fatalf("latest check viewport = %d, want desktop sample", latest.ViewportWidth)
	}
	device := state.GetDeviceClientCheck()
	if !device.Seen || device.ViewportWidth != 390 || device.DisplayMode != "standalone" || !device.Standalone {
		t.Fatalf("device check = %+v, want latest device Home Screen sample", device)
	}
}

func TestRuntimeStateKeepsLatestDeviceClientCheckAfterHistoryRotates(t *testing.T) {
	state := NewMemoryRuntimeState()
	start := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)

	if _, err := state.RecordClientCheck(ClientCheckUpdate{
		DashboardBuild: dashboardBuild,
		Interaction:    "status_strip_tap",
		ViewportWidth:  390,
		ViewportHeight: 844,
		DisplayMode:    "standalone",
		Standalone:     true,
		Visibility:     "visible",
	}, "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) Mobile Safari/604.1", start); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < maxClientCheckHistory+1; i++ {
		if _, err := state.RecordClientCheck(ClientCheckUpdate{
			ViewportWidth:  1440 + i,
			ViewportHeight: 900,
			DisplayMode:    "browser",
			Visibility:     "visible",
		}, "Mozilla/5.0 (X11; Linux x86_64) Firefox/128.0", start.Add(time.Duration(i+1)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}

	history := state.GetClientCheckHistory()
	for _, check := range history.Checks {
		if strings.Contains(check.UserAgent, "iPhone") {
			t.Fatalf("history still contains iPhone sample; test did not rotate it out: %+v", history.Checks)
		}
	}
	device := state.GetDeviceClientCheck()
	if !device.Seen ||
		device.DashboardBuild != dashboardBuild ||
		device.Interaction != "status_strip_tap" ||
		device.ViewportWidth != 390 ||
		device.DisplayMode != "standalone" ||
		!device.Standalone {
		t.Fatalf("device check = %+v, want preserved latest device proof after history rotation", device)
	}
}

func TestRuntimeStateNormalizesClientCheckMetadataStrings(t *testing.T) {
	state := NewMemoryRuntimeState()

	check, err := state.RecordClientCheck(ClientCheckUpdate{
		DashboardBuild:   " sysmon-static-v97\n",
		Interaction:      " Status_Strip_Tap\n",
		ViewportWidth:    390,
		ViewportHeight:   844,
		DevicePixelRatio: 3,
		DisplayMode:      " Standalone\n",
		Visibility:       "visible\t",
		Orientation:      "portrait\x00primary",
	}, "Mozilla/5.0\r\nMobile\tSafari", time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}

	if check.UserAgent != "Mozilla/5.0 Mobile Safari" {
		t.Fatalf("user agent = %q, want normalized single-line value", check.UserAgent)
	}
	if check.DashboardBuild != "sysmon-static-v97" {
		t.Fatalf("dashboard build = %q, want normalized build token", check.DashboardBuild)
	}
	if check.Interaction != "status_strip_tap" {
		t.Fatalf("interaction = %q, want normalized status_strip_tap", check.Interaction)
	}
	if check.DisplayMode != "standalone" {
		t.Fatalf("display mode = %q, want standalone", check.DisplayMode)
	}
	if check.Visibility != "visible" {
		t.Fatalf("visibility = %q, want visible", check.Visibility)
	}
	if check.Orientation != "portrait primary" {
		t.Fatalf("orientation = %q, want control character replaced", check.Orientation)
	}
}

func TestRuntimeStateRejectsUnknownClientDisplayMode(t *testing.T) {
	state := NewMemoryRuntimeState()
	_, err := state.RecordClientCheck(ClientCheckUpdate{
		ViewportWidth: 390,
		DisplayMode:   "home-screen",
	}, "test-agent", time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC))
	if err == nil || !strings.Contains(err.Error(), "display_mode") {
		t.Fatalf("RecordClientCheck error = %v, want display_mode validation error", err)
	}
}

func TestRuntimeStateRejectsOversizedClientInteraction(t *testing.T) {
	state := NewMemoryRuntimeState()
	_, err := state.RecordClientCheck(ClientCheckUpdate{
		Interaction: strings.Repeat("x", 65),
	}, "test-agent", time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC))
	if err == nil || !strings.Contains(err.Error(), "interaction") {
		t.Fatalf("RecordClientCheck error = %v, want interaction validation error", err)
	}
}

func TestWindowsSettingsReplacementUsesReplaceExistingMoveFile(t *testing.T) {
	data, err := os.ReadFile("settings_replace_windows.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	for _, needle := range []string{
		`//go:build windows`,
		`kernel32.dll`,
		`MoveFileExW`,
		`moveFileReplaceExisting`,
		`moveFileWriteThrough`,
		`os.LinkError`,
	} {
		if !strings.Contains(source, needle) {
			t.Fatalf("settings_replace_windows.go missing %q", needle)
		}
	}
}

func TestRuntimeStateRecoversInvalidPersistedSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"refresh_ms":1500}`), 0644); err != nil {
		t.Fatal(err)
	}

	state, err := NewRuntimeState(path)
	if err != nil {
		t.Fatal(err)
	}
	got := state.GetSettings()
	if got.RefreshMS != defaultRefreshMS || got.Panel != defaultPanelMode {
		t.Fatalf("settings = %+v, want defaults after invalid persisted settings", got)
	}
	assertInvalidSettingsBackup(t, path)

	reloaded, err := NewRuntimeState(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.GetSettings(); got.RefreshMS != defaultRefreshMS || got.Panel != defaultPanelMode {
		t.Fatalf("reloaded settings = %+v, want recovered defaults", got)
	}
}

func TestRuntimeStateRecoversInvalidPersistedPanel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"refresh_ms":1500,"panel":"shell"}`), 0644); err != nil {
		t.Fatal(err)
	}

	state, err := NewRuntimeState(path)
	if err != nil {
		t.Fatal(err)
	}
	got := state.GetSettings()
	if got.RefreshMS != defaultRefreshMS || got.Panel != defaultPanelMode {
		t.Fatalf("settings = %+v, want defaults after invalid persisted panel", got)
	}
	assertInvalidSettingsBackup(t, path)
}

func TestRuntimeStateRecoversInvalidPersistedThreshold(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"refresh_ms":1500,"panel":"all","thresholds":{"cpu_warn":10,"memory_warn":70,"disk_warn":70,"gpu_warn":70,"temp_warn_c":70}}`), 0644); err != nil {
		t.Fatal(err)
	}

	state, err := NewRuntimeState(path)
	if err != nil {
		t.Fatal(err)
	}
	got := state.GetSettings()
	if got.Thresholds != defaultDashboardThresholds() {
		t.Fatalf("thresholds = %+v, want defaults after invalid persisted threshold", got.Thresholds)
	}
	assertInvalidSettingsBackup(t, path)
}

func TestRuntimeStateRecoversMalformedPersistedSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"refresh_ms":1500`), 0644); err != nil {
		t.Fatal(err)
	}

	state, err := NewRuntimeState(path)
	if err != nil {
		t.Fatal(err)
	}
	got := state.GetSettings()
	if got.RefreshMS != defaultRefreshMS || got.Panel != defaultPanelMode {
		t.Fatalf("settings = %+v, want defaults after malformed persisted settings", got)
	}
	assertInvalidSettingsBackup(t, path)
}

func assertInvalidSettingsBackup(t *testing.T, path string) {
	t.Helper()

	backups, err := filepath.Glob(path + ".bad-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 {
		t.Fatalf("invalid settings backups = %v, want one backup", backups)
	}
	if _, err := os.Stat(backups[0]); err != nil {
		t.Fatal(err)
	}
}
