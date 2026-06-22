package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const defaultRefreshMS = 1000
const defaultPanelMode = "all"
const maxClientCheckHistory = 16
const defaultWarningThreshold = 70
const minWarningThreshold = 50
const maxWarningThreshold = 90

type SettingsValidationError struct {
	Message string
}

func (e *SettingsValidationError) Error() string {
	return e.Message
}

var allowedRefreshIntervals = map[int]bool{
	250:  true,
	500:  true,
	1000: true,
	2000: true,
}

var allowedPanelModes = map[string]bool{
	"all":         true,
	"performance": true,
	"storage":     true,
	"network":     true,
	"sensors":     true,
	"gpu":         true,
}

type DashboardSettings struct {
	Dim        bool                `json:"dim"`
	Shift      bool                `json:"shift"`
	RefreshMS  int                 `json:"refresh_ms"`
	Panel      string              `json:"panel"`
	Thresholds DashboardThresholds `json:"thresholds"`
	UpdatedAt  time.Time           `json:"updated_at"`
}

type DashboardSettingsUpdate struct {
	Dim        *bool                      `json:"dim,omitempty"`
	Shift      *bool                      `json:"shift,omitempty"`
	RefreshMS  *int                       `json:"refresh_ms,omitempty"`
	Panel      *string                    `json:"panel,omitempty"`
	Thresholds *DashboardThresholdsUpdate `json:"thresholds,omitempty"`
}

type DashboardThresholds struct {
	CPUWarn    int `json:"cpu_warn"`
	MemoryWarn int `json:"memory_warn"`
	DiskWarn   int `json:"disk_warn"`
	GPUWarn    int `json:"gpu_warn"`
	TempWarnC  int `json:"temp_warn_c"`
}

type DashboardThresholdsUpdate struct {
	CPUWarn    *int `json:"cpu_warn,omitempty"`
	MemoryWarn *int `json:"memory_warn,omitempty"`
	DiskWarn   *int `json:"disk_warn,omitempty"`
	GPUWarn    *int `json:"gpu_warn,omitempty"`
	TempWarnC  *int `json:"temp_warn_c,omitempty"`
}

type ClientCheck struct {
	Seen             bool       `json:"seen"`
	LastSeen         *time.Time `json:"last_seen,omitempty"`
	DashboardBuild   string     `json:"dashboard_build,omitempty"`
	Interaction      string     `json:"interaction,omitempty"`
	UserAgent        string     `json:"user_agent,omitempty"`
	ViewportWidth    int        `json:"viewport_width,omitempty"`
	ViewportHeight   int        `json:"viewport_height,omitempty"`
	ScreenWidth      int        `json:"screen_width,omitempty"`
	ScreenHeight     int        `json:"screen_height,omitempty"`
	DevicePixelRatio float64    `json:"device_pixel_ratio,omitempty"`
	TouchPoints      int        `json:"touch_points,omitempty"`
	DisplayMode      string     `json:"display_mode,omitempty"`
	Standalone       bool       `json:"standalone"`
	Visibility       string     `json:"visibility,omitempty"`
	Orientation      string     `json:"orientation,omitempty"`
}

type ClientCheckUpdate struct {
	DashboardBuild   string  `json:"dashboard_build,omitempty"`
	Interaction      string  `json:"interaction,omitempty"`
	ViewportWidth    int     `json:"viewport_width,omitempty"`
	ViewportHeight   int     `json:"viewport_height,omitempty"`
	ScreenWidth      int     `json:"screen_width,omitempty"`
	ScreenHeight     int     `json:"screen_height,omitempty"`
	DevicePixelRatio float64 `json:"device_pixel_ratio,omitempty"`
	TouchPoints      int     `json:"touch_points,omitempty"`
	DisplayMode      string  `json:"display_mode,omitempty"`
	Standalone       bool    `json:"standalone,omitempty"`
	Visibility       string  `json:"visibility,omitempty"`
	Orientation      string  `json:"orientation,omitempty"`
}

type ClientCheckHistory struct {
	Checks []ClientCheck `json:"checks"`
}

type RuntimeState struct {
	mu           sync.RWMutex
	settings     DashboardSettings
	clientCheck  ClientCheck
	deviceCheck  ClientCheck
	clientChecks []ClientCheck
	settingsPath string
}

func NewRuntimeState(settingsPath string) (*RuntimeState, error) {
	settings := defaultDashboardSettings()
	if settingsPath != "" {
		loaded, err := loadDashboardSettings(settingsPath)
		if err != nil {
			return nil, err
		}
		settings = loaded
	}
	return &RuntimeState{settings: settings, settingsPath: settingsPath}, nil
}

func NewMemoryRuntimeState() *RuntimeState {
	return &RuntimeState{settings: defaultDashboardSettings()}
}

func defaultDashboardSettings() DashboardSettings {
	return DashboardSettings{
		Shift:      true,
		RefreshMS:  defaultRefreshMS,
		Panel:      defaultPanelMode,
		Thresholds: defaultDashboardThresholds(),
		UpdatedAt:  time.Now().UTC(),
	}
}

func defaultDashboardThresholds() DashboardThresholds {
	return DashboardThresholds{
		CPUWarn:    defaultWarningThreshold,
		MemoryWarn: defaultWarningThreshold,
		DiskWarn:   defaultWarningThreshold,
		GPUWarn:    defaultWarningThreshold,
		TempWarnC:  defaultWarningThreshold,
	}
}

func (s *RuntimeState) GetSettings() DashboardSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.settings
}

func (s *RuntimeState) HasPersistentSettings() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.settingsPath != ""
}

func (s *RuntimeState) GetClientCheck() ClientCheck {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.clientCheck
}

func (s *RuntimeState) GetDeviceClientCheck() ClientCheck {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.deviceCheck
}

func (s *RuntimeState) GetClientCheckHistory() ClientCheckHistory {
	s.mu.RLock()
	defer s.mu.RUnlock()
	checks := make([]ClientCheck, len(s.clientChecks))
	copy(checks, s.clientChecks)
	return ClientCheckHistory{Checks: checks}
}

func (s *RuntimeState) RecordClientCheck(update ClientCheckUpdate, userAgent string, now time.Time) (ClientCheck, error) {
	if err := validateClientCheckUpdate(update); err != nil {
		return ClientCheck{}, err
	}

	seenAt := now.UTC()
	check := ClientCheck{
		Seen:             true,
		LastSeen:         &seenAt,
		DashboardBuild:   clientMetadataString(update.DashboardBuild, 64),
		Interaction:      clientInteractionString(update.Interaction),
		UserAgent:        clientMetadataString(userAgent, 256),
		ViewportWidth:    update.ViewportWidth,
		ViewportHeight:   update.ViewportHeight,
		ScreenWidth:      update.ScreenWidth,
		ScreenHeight:     update.ScreenHeight,
		DevicePixelRatio: update.DevicePixelRatio,
		TouchPoints:      update.TouchPoints,
		DisplayMode:      clientDisplayModeString(update.DisplayMode),
		Standalone:       update.Standalone,
		Visibility:       clientMetadataString(update.Visibility, 32),
		Orientation:      clientMetadataString(update.Orientation, 64),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.clientCheck = check
	if isDeviceClientCheck(check) {
		s.deviceCheck = check
	}
	s.clientChecks = append([]ClientCheck{check}, s.clientChecks...)
	if len(s.clientChecks) > maxClientCheckHistory {
		s.clientChecks = s.clientChecks[:maxClientCheckHistory]
	}
	return s.clientCheck, nil
}

func (s *RuntimeState) UpdateSettings(update DashboardSettingsUpdate) (DashboardSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := s.settings
	if update.Dim != nil {
		next.Dim = *update.Dim
	}
	if update.Shift != nil {
		next.Shift = *update.Shift
	}
	if update.RefreshMS != nil {
		next.RefreshMS = *update.RefreshMS
	}
	if update.Panel != nil {
		next.Panel = strings.ToLower(strings.TrimSpace(*update.Panel))
	}
	if update.Thresholds != nil {
		next.Thresholds = applyDashboardThresholdsUpdate(next.Thresholds, *update.Thresholds)
	}
	next.UpdatedAt = time.Now().UTC()
	if err := validateDashboardSettings(next); err != nil {
		return s.settings, err
	}
	if s.settingsPath != "" {
		if err := saveDashboardSettings(s.settingsPath, next); err != nil {
			return s.settings, err
		}
	}
	s.settings = next
	return s.settings, nil
}

func loadDashboardSettings(path string) (DashboardSettings, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		settings := defaultDashboardSettings()
		if err := saveDashboardSettings(path, settings); err != nil {
			return DashboardSettings{}, err
		}
		return settings, nil
	}
	if err != nil {
		return DashboardSettings{}, err
	}

	settings := defaultDashboardSettings()
	if err := json.Unmarshal(data, &settings); err != nil {
		return recoverDashboardSettings(path)
	}
	if err := validateDashboardSettings(settings); err != nil {
		return recoverDashboardSettings(path)
	}
	return settings, nil
}

func recoverDashboardSettings(path string) (DashboardSettings, error) {
	backupPath := fmt.Sprintf("%s.bad-%s", path, time.Now().UTC().Format("20060102T150405.000000000Z"))
	if err := os.Rename(path, backupPath); err != nil {
		return DashboardSettings{}, fmt.Errorf("backup invalid settings file %s: %w", path, err)
	}
	settings := defaultDashboardSettings()
	if err := saveDashboardSettings(path, settings); err != nil {
		return DashboardSettings{}, err
	}
	return settings, nil
}

func saveDashboardSettings(path string, settings DashboardSettings) error {
	if err := validateDashboardSettings(settings); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create settings directory %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, ".sysmon-settings-*.json")
	if err != nil {
		return fmt.Errorf("create temporary settings file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temporary settings file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary settings file: %w", err)
	}
	if err := replaceFile(tmpName, path); err != nil {
		return fmt.Errorf("replace settings file %s: %w", path, err)
	}
	return nil
}

func validateClientCheckUpdate(update ClientCheckUpdate) error {
	if len(update.DashboardBuild) > 64 {
		return &SettingsValidationError{Message: "dashboard_build is too long"}
	}
	if len(update.Interaction) > 64 {
		return &SettingsValidationError{Message: "interaction is too long"}
	}
	if update.ViewportWidth < 0 || update.ViewportWidth > 10000 {
		return &SettingsValidationError{Message: "viewport_width must be between 0 and 10000"}
	}
	if update.ViewportHeight < 0 || update.ViewportHeight > 10000 {
		return &SettingsValidationError{Message: "viewport_height must be between 0 and 10000"}
	}
	if update.ScreenWidth < 0 || update.ScreenWidth > 10000 {
		return &SettingsValidationError{Message: "screen_width must be between 0 and 10000"}
	}
	if update.ScreenHeight < 0 || update.ScreenHeight > 10000 {
		return &SettingsValidationError{Message: "screen_height must be between 0 and 10000"}
	}
	if math.IsNaN(update.DevicePixelRatio) || math.IsInf(update.DevicePixelRatio, 0) || update.DevicePixelRatio < 0 || update.DevicePixelRatio > 16 {
		return &SettingsValidationError{Message: "device_pixel_ratio must be between 0 and 16"}
	}
	if update.TouchPoints < 0 || update.TouchPoints > 20 {
		return &SettingsValidationError{Message: "touch_points must be between 0 and 20"}
	}
	displayMode := strings.ToLower(clientMetadataCleanString(update.DisplayMode))
	if len(displayMode) > 32 {
		return &SettingsValidationError{Message: "display_mode is too long"}
	}
	if displayMode != "" && !allowedClientDisplayModes[displayMode] {
		return &SettingsValidationError{Message: "display_mode must be one of standalone, fullscreen, minimal-ui, browser, unknown"}
	}
	if len(update.Visibility) > 32 {
		return &SettingsValidationError{Message: "visibility is too long"}
	}
	if len(update.Orientation) > 64 {
		return &SettingsValidationError{Message: "orientation is too long"}
	}
	return nil
}

func validateDashboardSettings(settings DashboardSettings) error {
	if settings.RefreshMS == 0 {
		return &SettingsValidationError{Message: "refresh_ms is required"}
	}
	if !allowedRefreshIntervals[settings.RefreshMS] {
		return &SettingsValidationError{Message: "refresh_ms must be one of 250, 500, 1000, 2000"}
	}
	if settings.Panel == "" {
		return &SettingsValidationError{Message: "panel is required"}
	}
	if !allowedPanelModes[settings.Panel] {
		return &SettingsValidationError{Message: "panel must be one of all, performance, storage, network, sensors, gpu"}
	}
	if err := validateDashboardThresholds(settings.Thresholds); err != nil {
		return err
	}
	return nil
}

func applyDashboardThresholdsUpdate(thresholds DashboardThresholds, update DashboardThresholdsUpdate) DashboardThresholds {
	if update.CPUWarn != nil {
		thresholds.CPUWarn = *update.CPUWarn
	}
	if update.MemoryWarn != nil {
		thresholds.MemoryWarn = *update.MemoryWarn
	}
	if update.DiskWarn != nil {
		thresholds.DiskWarn = *update.DiskWarn
	}
	if update.GPUWarn != nil {
		thresholds.GPUWarn = *update.GPUWarn
	}
	if update.TempWarnC != nil {
		thresholds.TempWarnC = *update.TempWarnC
	}
	return thresholds
}

func validateDashboardThresholds(thresholds DashboardThresholds) error {
	for _, item := range []struct {
		name  string
		value int
	}{
		{name: "thresholds.cpu_warn", value: thresholds.CPUWarn},
		{name: "thresholds.memory_warn", value: thresholds.MemoryWarn},
		{name: "thresholds.disk_warn", value: thresholds.DiskWarn},
		{name: "thresholds.gpu_warn", value: thresholds.GPUWarn},
		{name: "thresholds.temp_warn_c", value: thresholds.TempWarnC},
	} {
		if item.value < minWarningThreshold || item.value > maxWarningThreshold {
			return &SettingsValidationError{Message: fmt.Sprintf("%s must be between %d and %d", item.name, minWarningThreshold, maxWarningThreshold)}
		}
	}
	return nil
}

func refreshIntervalOptions() []int {
	values := make([]int, 0, len(allowedRefreshIntervals))
	for value := range allowedRefreshIntervals {
		values = append(values, value)
	}
	sort.Ints(values)
	return values
}

func panelModeOptions() []string {
	values := make([]string, 0, len(allowedPanelModes))
	for value := range allowedPanelModes {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

func trimMax(value string, maxLen int) string {
	value = strings.TrimSpace(value)
	if len(value) <= maxLen {
		return value
	}
	return value[:maxLen]
}

func clientMetadataString(value string, maxLen int) string {
	return trimMax(clientMetadataCleanString(value), maxLen)
}

func clientMetadataCleanString(value string) string {
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, value)
	return strings.Join(strings.Fields(value), " ")
}

var allowedClientDisplayModes = map[string]bool{
	"standalone": true,
	"fullscreen": true,
	"minimal-ui": true,
	"browser":    true,
	"unknown":    true,
}

func clientDisplayModeString(value string) string {
	return strings.ToLower(clientMetadataString(value, 32))
}

func clientInteractionString(value string) string {
	return strings.ToLower(clientMetadataString(value, 64))
}

// isDeviceClientCheck reports whether a client check came from a mobile/handheld
// device (any OS) rather than a desktop browser. It is OS-agnostic: any client
// whose User-Agent advertises a mobile build (the "Mobile" token covers iPhone
// and Android phones) or a known handheld platform (iPhone/iPad/iPod/Android)
// is promoted to the dedicated "device" proof slot surfaced at
// /api/status -> device_client_check.
func isDeviceClientCheck(check ClientCheck) bool {
	userAgent := check.UserAgent
	return check.Seen &&
		check.ViewportWidth > 0 &&
		check.ViewportHeight > 0 &&
		(strings.Contains(userAgent, "Mobile") ||
			strings.Contains(userAgent, "Android") ||
			strings.Contains(userAgent, "iPhone") ||
			strings.Contains(userAgent, "iPad") ||
			strings.Contains(userAgent, "iPod"))
}
