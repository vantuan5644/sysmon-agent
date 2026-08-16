package main

import (
	"runtime"
	"time"
)

var agentStartedAt = time.Now().UTC()

const dashboardBuild = "sysmon-static-v125"

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
	// Version is the running agent's version tag (the same value reported by
	// `sysmon-agent.exe -version`). The dashboard shows it in the status strip;
	// it is also the comparison baseline for the update channel. "dev" is the
	// default for an untagged `go build` (see main.go's `version` variable).
	Version string `json:"version"`
	// Update is the cached update-channel result surfaced to the dashboard's
	// update banner. Available=false covers all "no update" states: the check
	// has not run yet, the channel is offline, or the host is on the latest.
	Update UpdateStatus `json:"update"`
	// UpdateCheckSupported reports whether the in-dashboard self-update flow is
	// available on this host. It is false outside the Windows service (Linux,
	// console runs) so the dashboard can hide the self-update affordance and
	// direct the user to the installer / package manager path.
	UpdateCheckSupported bool `json:"update_check_supported"`
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
	checker := state.UpdateChecker()
	status := UpdateStatus{Available: false}
	if checker != nil {
		status = checker.Status()
	}
	return AgentStatus{
		Status:               "ok",
		DashboardBuild:       dashboardBuild,
		StartedAt:            agentStartedAt,
		UptimeSeconds:        int64(uptime.Seconds()),
		OS:                   runtime.GOOS,
		Arch:                 runtime.GOARCH,
		SettingsPersisted:    state.HasPersistentSettings(),
		RefreshOptionsMS:     refreshIntervalOptions(),
		PanelOptions:         panelModeOptions(),
		Controls:             controls,
		Settings:             state.GetSettings(),
		ClientCheck:          state.GetClientCheck(),
		DeviceClientCheck:    state.GetDeviceClientCheck(),
		Version:              version,
		Update:               status,
		UpdateCheckSupported: selfUpdateSupported(),
	}
}
