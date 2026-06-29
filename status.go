package main

import (
	"runtime"
	"time"
)

var agentStartedAt = time.Now().UTC()

const dashboardBuild = "sysmon-static-v111"

type AgentStatus struct {
	Status            string              `json:"status"`
	DashboardBuild    string              `json:"dashboard_build"`
	StartedAt         time.Time           `json:"started_at"`
	UptimeSeconds     int64               `json:"uptime_seconds"`
	OS                string              `json:"os"`
	Arch              string              `json:"arch"`
	SettingsPersisted bool                `json:"settings_persisted"`
	RefreshOptionsMS  []int               `json:"refresh_options_ms"`
	PanelOptions      []string            `json:"panel_options"`
	Controls          []ControlCapability `json:"controls"`
	Settings          DashboardSettings   `json:"settings"`
	ClientCheck       ClientCheck         `json:"client_check"`
	DeviceClientCheck ClientCheck         `json:"device_client_check"`
}

type ReadinessStatus struct {
	Status           string    `json:"status"`
	Metrics          bool      `json:"metrics"`
	Hostname         string    `json:"hostname,omitempty"`
	Timestamp        time.Time `json:"timestamp,omitempty"`
	CollectionErrors []string  `json:"collection_errors,omitempty"`
	Error            string    `json:"error,omitempty"`
}

func newAgentStatus(state *RuntimeState, controls []ControlCapability, now time.Time) AgentStatus {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	uptime := now.Sub(agentStartedAt)
	if uptime < 0 {
		uptime = 0
	}
	return AgentStatus{
		Status:            "ok",
		DashboardBuild:    dashboardBuild,
		StartedAt:         agentStartedAt,
		UptimeSeconds:     int64(uptime.Seconds()),
		OS:                runtime.GOOS,
		Arch:              runtime.GOARCH,
		SettingsPersisted: state.HasPersistentSettings(),
		RefreshOptionsMS:  refreshIntervalOptions(),
		PanelOptions:      panelModeOptions(),
		Controls:          controls,
		Settings:          state.GetSettings(),
		ClientCheck:       state.GetClientCheck(),
		DeviceClientCheck: state.GetDeviceClientCheck(),
	}
}
