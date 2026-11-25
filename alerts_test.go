package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sysmon-agent/internal/gpuguard"
)

var alertT0 = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

func at(seconds float64) time.Time {
	return alertT0.Add(time.Duration(seconds * float64(time.Second)))
}

func cpuRule() alertRule {
	return alertRule{ID: "cpu", Metric: alertMetricCPU, Threshold: 90, Hysteresis: 3, Hold: 15 * time.Second, Release: 4 * time.Second, Cooldown: 10 * time.Minute, Resolved: true}
}

func cpuObs(value float64) []alertObservation {
	return []alertObservation{{RuleID: "cpu", Label: "CPU", Value: value, Known: true}}
}

func phaseOf(e *alertEngine, rule, instance string) alertPhase {
	inst := e.instances[alertKey(rule, instance)]
	if inst == nil {
		return alertIdle
	}
	return inst.Phase
}

func wantEvents(t *testing.T, step string, got []alertEvent, want ...string) {
	t.Helper()
	var kinds []string
	for _, ev := range got {
		kind := ev.Kind
		if !ev.Notify {
			kind += "(quiet)"
		}
		kinds = append(kinds, kind)
	}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("%s: events %v, want %v", step, kinds, want)
	}
}

func TestAlertEngineHoldReleaseAndHysteresis(t *testing.T) {
	e := newAlertEngine([]alertRule{cpuRule()})
	wantEvents(t, "first breach", e.Observe(at(0), cpuObs(95)))
	if got := phaseOf(e, "cpu", ""); got != alertPending {
		t.Fatalf("phase after first breach = %v, want pending", got)
	}
	wantEvents(t, "14s in", e.Observe(at(14), cpuObs(95)))
	events := e.Observe(at(15), cpuObs(96))
	wantEvents(t, "15s in", events, "firing")
	if events[0].Value != 96 || !events[0].FiredAt.Equal(at(15)) {
		t.Fatalf("firing event = %+v", events[0])
	}
	// 88 is under 90 but inside the 3-point hysteresis: still firing.
	wantEvents(t, "inside hysteresis", e.Observe(at(16), cpuObs(88)))
	if got := phaseOf(e, "cpu", ""); got != alertFiring {
		t.Fatalf("phase at 88 = %v, want firing", got)
	}
	wantEvents(t, "clear starts release", e.Observe(at(17), cpuObs(86)))
	if got := phaseOf(e, "cpu", ""); got != alertReleasing {
		t.Fatalf("phase at 86 = %v, want releasing", got)
	}
	wantEvents(t, "3s clear", e.Observe(at(20), cpuObs(80)))
	events = e.Observe(at(21), cpuObs(80))
	wantEvents(t, "4s clear", events, "resolved")
	if len(e.instances) != 0 {
		t.Fatalf("resolved instance kept: %+v", e.instances)
	}
	if !strings.Contains(alertResolvedMessage(events[0].Rule, events[0].Label, events[0].Value, true), "CPU 80% now") {
		t.Fatalf("resolved message for %+v", events[0])
	}
}

func TestAlertEnginePendingResetsAndReleaseInterrupted(t *testing.T) {
	e := newAlertEngine([]alertRule{cpuRule()})
	e.Observe(at(0), cpuObs(95))
	wantEvents(t, "dip", e.Observe(at(5), cpuObs(80)))
	if len(e.instances) != 0 {
		t.Fatal("a dip during hold should drop the pending instance")
	}
	e.Observe(at(6), cpuObs(95))
	wantEvents(t, "14s after restart", e.Observe(at(20), cpuObs(95)))
	wantEvents(t, "15s after restart", e.Observe(at(21), cpuObs(95)), "firing")

	e.Observe(at(22), cpuObs(50))
	wantEvents(t, "back over during release", e.Observe(at(24), cpuObs(91)))
	if got := phaseOf(e, "cpu", ""); got != alertFiring {
		t.Fatalf("phase = %v, want firing again", got)
	}
	e.Observe(at(25), cpuObs(50))
	wantEvents(t, "release restarts", e.Observe(at(28), cpuObs(50)))
	wantEvents(t, "release done", e.Observe(at(29), cpuObs(50)), "resolved")
}

func TestAlertEngineCooldownSuppressesNotices(t *testing.T) {
	rule := cpuRule()
	rule.Hold, rule.Release = 0, 0
	e := newAlertEngine([]alertRule{rule})
	wantEvents(t, "first", e.Observe(at(0), cpuObs(95)), "firing")
	wantEvents(t, "resolve", e.Observe(at(10), cpuObs(50)), "resolved")
	// Inside the ten-minute cooldown: shown but not sent, and its resolve
	// is not sent either.
	wantEvents(t, "second", e.Observe(at(20), cpuObs(95)), "firing(quiet)")
	if inst := e.Instances(); len(inst) != 1 || inst[0].Notified {
		t.Fatalf("instances = %+v, want one unnotified", inst)
	}
	wantEvents(t, "second resolve", e.Observe(at(30), cpuObs(50)), "resolved(quiet)")
	wantEvents(t, "after cooldown", e.Observe(at(601), cpuObs(95)), "firing")
}

func TestAlertEngineResolvedNoticeOptional(t *testing.T) {
	rule := cpuRule()
	rule.Hold, rule.Release, rule.Resolved = 0, 0, false
	e := newAlertEngine([]alertRule{rule})
	wantEvents(t, "fire", e.Observe(at(0), cpuObs(95)), "firing")
	wantEvents(t, "resolve", e.Observe(at(1), cpuObs(10)), "resolved(quiet)")
}

func TestAlertEngineUnknownReadingsHoldState(t *testing.T) {
	e := newAlertEngine([]alertRule{cpuRule()})
	e.Observe(at(0), cpuObs(95))
	unknown := []alertObservation{{RuleID: "cpu", Label: "CPU", Known: false}}
	wantEvents(t, "unknown while pending", e.Observe(at(16), unknown))
	if len(e.instances) != 0 {
		t.Fatal("an unknown reading must not carry a pending alert to firing")
	}
	e.Observe(at(20), cpuObs(95))
	wantEvents(t, "fire", e.Observe(at(35), cpuObs(95)), "firing")
	// A firing alert neither resolves on unknown readings nor on missing
	// ones (a parked or broken collector), however long.
	wantEvents(t, "unknown while firing", e.Observe(at(40), unknown))
	wantEvents(t, "missing while firing", e.Observe(at(400), nil))
	if got := phaseOf(e, "cpu", ""); got != alertFiring {
		t.Fatalf("phase = %v, want firing", got)
	}
	e.Observe(at(401), cpuObs(50))
	wantEvents(t, "missing while releasing", e.Observe(at(410), nil))
	if got := phaseOf(e, "cpu", ""); got != alertFiring {
		t.Fatalf("phase = %v, want firing after a gap in release", got)
	}
	// An unknown reading never creates an instance.
	e2 := newAlertEngine([]alertRule{cpuRule()})
	e2.Observe(at(0), unknown)
	if len(e2.instances) != 0 {
		t.Fatal("unknown reading created an instance")
	}
}

func TestAlertEngineBelowRuleAndInstances(t *testing.T) {
	rule := alertRule{ID: "disk_free", Metric: alertMetricDiskFree, Below: true, Threshold: 10, Hysteresis: 3, Resolved: true}
	e := newAlertEngine([]alertRule{rule})
	obs := func(root, data float64) []alertObservation {
		return []alertObservation{
			{RuleID: "disk_free", Instance: "/", Label: "/", Value: root, Known: true},
			{RuleID: "disk_free", Instance: "/data", Label: "/data", Value: data, Known: true},
		}
	}
	events := e.Observe(at(0), obs(8, 50))
	wantEvents(t, "root low", events, "firing")
	if events[0].Instance != "/" {
		t.Fatalf("instance = %q", events[0].Instance)
	}
	if msg := alertMessage(rule, "/", 8, true); msg != "Free space on / 8% (under 10%)" {
		t.Fatalf("message = %q", msg)
	}
	wantEvents(t, "inside hysteresis", e.Observe(at(1), obs(12, 9)), "firing")
	wantEvents(t, "root clear", e.Observe(at(2), obs(14, 9)), "resolved")
	if inst := e.Instances(); len(inst) != 1 || inst[0].Instance != "/data" {
		t.Fatalf("instances = %+v", inst)
	}
	// A disk that disappears holds its alert, it does not resolve.
	wantEvents(t, "data unmounted", e.Observe(at(3), obs(50, 0)[:1]))
	if got := phaseOf(e, "disk_free", "/data"); got != alertFiring {
		t.Fatalf("unmounted disk phase = %v", got)
	}
}

func TestAlertEngineUpdateRule(t *testing.T) {
	rule, _, err := buildAlertRule(alertRuleFile{Metric: "update"}, alertRule{Hold: 15 * time.Second, Release: 4 * time.Second, Cooldown: 10 * time.Minute, Resolved: true})
	if err != nil {
		t.Fatal(err)
	}
	if rule.Hold != 0 || rule.Resolved || rule.Threshold != 1 {
		t.Fatalf("update rule = %+v", rule)
	}
	e := newAlertEngine([]alertRule{rule})
	update := func(version string) []alertObservation {
		return alertObservations([]alertRule{rule}, alertInputs{Update: &UpdateStatus{Available: version != "", LatestVersion: version}})
	}
	events := e.Observe(at(0), update("0.3.0"))
	wantEvents(t, "available", events, "firing")
	if msg := alertMessage(rule, events[0].Label, 1, true); msg != "sysmon-agent 0.3.0 is available" {
		t.Fatalf("message = %q", msg)
	}
	wantEvents(t, "still available", e.Observe(at(60), update("0.3.0")))
	wantEvents(t, "newer one", e.Observe(at(120), update("0.3.1")), "resolved(quiet)", "firing")
	wantEvents(t, "updated", e.Observe(at(180), update("")), "resolved(quiet)")
	if len(e.instances) != 0 {
		t.Fatal("update instance kept after it went away")
	}
}

func TestAlertObservationsFreshnessAndSources(t *testing.T) {
	m := Metrics{
		CPU:            NumberMetric{Available: true, Value: 97},
		CPUTemperature: NumberMetric{Available: true, Value: 81},
		Memory:         CapacityMetric{Available: true, Percent: 42},
		Disks: []DiskMetric{
			{Mountpoint: "/", Capacity: CapacityMetric{Available: true, Percent: 93, TotalBytes: 100}},
			{Mountpoint: "/mnt/x", Capacity: CapacityMetric{Available: false}},
		},
	}
	m.GPU.Devices = []GPUMetric{{Name: "Test GPU", Temperature: NumberMetric{Available: true, Value: 70}, HotspotTemperature: NumberMetric{Available: true, Value: 84}}}
	m.Temperatures.Sensors = []TemperatureMetric{{Name: "acpitz", Celsius: NumberMetric{Available: true, Value: 50}}}
	m.Storage.Devices = []StorageDevice{{Name: "nvme0n1", Model: "Fast SSD", Temperature: NumberMetric{Available: true, Value: 66}}}

	rules := []alertRule{
		{ID: "cpu", Metric: alertMetricCPU},
		{ID: "disk", Metric: alertMetricDiskFree, Below: true},
		{ID: "gpu", Metric: alertMetricGPUTemp},
		{ID: "ssd", Metric: alertMetricSensorTemp, Match: "fast"},
		{ID: "cputemp", Metric: alertMetricCPUTemp},
	}
	byRule := func(obs []alertObservation) map[string][]alertObservation {
		out := map[string][]alertObservation{}
		for _, o := range obs {
			out[o.RuleID] = append(out[o.RuleID], o)
		}
		return out
	}

	got := byRule(alertObservations(rules, alertInputs{Metrics: m, Have: true, FastFresh: true, SlowFresh: true, Now: alertT0}))
	if o := got["cpu"]; len(o) != 1 || o[0].Value != 97 || !o[0].Known {
		t.Fatalf("cpu = %+v", o)
	}
	if o := got["disk"]; len(o) != 2 || o[0].Value != 7 || !o[0].Known || o[1].Known {
		t.Fatalf("disk = %+v", o)
	}
	if o := got["gpu"]; len(o) != 1 || o[0].Value != 84 || o[0].Instance != "Test GPU" {
		t.Fatalf("gpu = %+v", o)
	}
	if o := got["ssd"]; len(o) != 1 || o[0].Label != "Fast SSD" || o[0].Value != 66 {
		t.Fatalf("sensor match = %+v", o)
	}
	if o := got["cputemp"]; len(o) != 1 || o[0].Value != 81 {
		t.Fatalf("cpu temp = %+v", o)
	}

	// A stale slow lane gives no slow readings; a stale fast lane no CPU.
	got = byRule(alertObservations(rules, alertInputs{Metrics: m, Have: true, FastFresh: false, SlowFresh: false, Now: alertT0}))
	if len(got) != 0 {
		t.Fatalf("stale lanes still gave %+v", got)
	}

	// A fresh GPU guard stands in for the slow lane's reading of its GPU.
	guard := &gpuguard.State{Source: "nvml", GPU: "Test GPU", UpdatedAt: alertT0.Add(-time.Second),
		Hotspot: gpuguard.Reading{Available: true, Value: 99}, Core: gpuguard.Reading{Available: true, Value: 80}}
	got = byRule(alertObservations(rules, alertInputs{Metrics: m, Have: true, SlowFresh: false, Guard: guard, Now: alertT0}))
	if o := got["gpu"]; len(o) != 1 || o[0].Value != 99 || o[0].Instance != "Test GPU" {
		t.Fatalf("guard gpu = %+v", o)
	}
	got = byRule(alertObservations(rules, alertInputs{Metrics: m, Have: true, SlowFresh: true, Guard: guard, Now: alertT0}))
	if o := got["gpu"]; len(o) != 1 || o[0].Value != 99 {
		t.Fatalf("guard plus lane gpu = %+v, want the guard's reading only", o)
	}
	guard.UpdatedAt = alertT0.Add(-time.Minute)
	got = byRule(alertObservations(rules, alertInputs{Metrics: m, Have: true, SlowFresh: true, Guard: guard, Now: alertT0}))
	if o := got["gpu"]; len(o) != 1 || o[0].Value != 84 {
		t.Fatalf("stale guard gpu = %+v, want the lane's", o)
	}
}

func TestHostAlertsAttachToSnapshotJSON(t *testing.T) {
	var m Metrics
	attachHostAlerts(&m, nil)
	if m.HostAlerts != nil {
		t.Fatal("no source should leave host_alerts out")
	}
	rule := cpuRule()
	rule.Hold = 0
	service := &alertService{cfg: alertsConfig{Rules: []alertRule{rule}}, engine: newAlertEngine([]alertRule{rule}), logf: t.Logf, dispatcher: newAlertDispatcher(nil, nil)}
	service.source = fakeAlertSource{snap: alertSnapshot{Have: true, FastFresh: true, Metrics: Metrics{CPU: NumberMetric{Available: true, Value: 99}}}}
	service.tick(alertT0)
	attachHostAlerts(&m, service)
	if m.HostAlerts == nil || len(m.HostAlerts.Active) != 1 || m.HostAlerts.Active[0].State != "firing" || m.HostAlerts.Active[0].Message != "CPU 99% (over 90%)" {
		t.Fatalf("host_alerts = %+v", m.HostAlerts)
	}
}

type fakeAlertSource struct {
	snap  alertSnapshot
	guard *gpuguard.State
}

func (f fakeAlertSource) AlertSnapshot(time.Time) alertSnapshot { return f.snap }
func (f fakeAlertSource) GPUGuardState() *gpuguard.State        { return f.guard }

// ---- config ----

// fakeFiles keys its map by slash paths. The loader builds the default
// alerts.json path with filepath.Join, which uses backslashes on Windows.
func fakeFiles(files map[string]string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		if data, ok := files[filepath.ToSlash(path)]; ok {
			return []byte(data), nil
		}
		return nil, os.ErrNotExist
	}
}

func TestLoadAlertsConfigOffByDefault(t *testing.T) {
	cfg, err := loadAlertsConfig(alertFlags{SettingsPath: "/etc/sysmon/settings.json"}, fakeFiles(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.enabled() || cfg.File != "" {
		t.Fatalf("no flags and no file should be off: %+v", cfg)
	}
	if _, err := loadAlertsConfig(alertFlags{ConfigFile: "/nope/alerts.json"}, fakeFiles(nil)); err == nil {
		t.Fatal("a named config file that is missing should fail")
	}
}

func TestLoadAlertsConfigFlags(t *testing.T) {
	cfg, err := loadAlertsConfig(alertFlags{CPU: 90, DiskFree: 10, GPUTemp: 88, Update: true, Hold: 30 * time.Second, Cooldown: time.Hour, NtfyURL: "https://ntfy.example/topic-abc", NtfyToken: "tk_secret"}, fakeFiles(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rules) != 4 {
		t.Fatalf("rules = %+v", cfg.Rules)
	}
	byID := map[string]alertRule{}
	for _, r := range cfg.Rules {
		byID[r.ID] = r
	}
	if r := byID["cpu"]; r.Threshold != 90 || r.Below || r.Hold != 30*time.Second || r.Release != defaultAlertRelease || r.Cooldown != time.Hour || r.Hysteresis != defaultAlertHysteresis || !r.Resolved {
		t.Fatalf("cpu rule = %+v", r)
	}
	if r := byID["disk_free"]; !r.Below || r.Threshold != 10 {
		t.Fatalf("disk rule = %+v", r)
	}
	if r := byID["update"]; r.Hold != 0 || r.Resolved {
		t.Fatalf("update rule = %+v", r)
	}
	if cfg.Ntfy == nil || cfg.Ntfy.Token != "tk_secret" || cfg.Webhook != nil || cfg.WebPush != nil {
		t.Fatalf("targets = %+v %+v %+v", cfg.Ntfy, cfg.Webhook, cfg.WebPush)
	}
	if !cfg.needsSlowSampling(true) || cfg.SampleEvery != defaultAlertSampleEvery {
		t.Fatal("disk and GPU rules need background slow sampling")
	}
	cpuOnly, _ := loadAlertsConfig(alertFlags{CPU: 90, Update: true}, fakeFiles(nil))
	if cpuOnly.needsSlowSampling(true) || !cpuOnly.needsSlowSampling(false) {
		t.Fatal("a CPU rule needs the slow loop only without lanes")
	}
}

func TestLoadAlertsConfigFile(t *testing.T) {
	file := `{
	  "defaults": {"hold": "20s", "release": "6s", "cooldown": "30m", "resolved": false},
	  "rules": [
	    {"metric": "cpu", "above": 95},
	    {"id": "nvme", "metric": "sensor_temp", "match": "nvme", "above": 70, "hysteresis": 5, "resolved": true, "priority": "urgent"},
	    {"metric": "disk_free", "below": 5, "match": "/srv"},
	    {"id": "off", "metric": "memory", "above": 90, "enabled": false}
	  ],
	  "ntfy": {"url": "https://ntfy.example/alerts-xyz", "token_file": "/run/secrets/ntfy", "priority": "4", "tags": ["computer"]},
	  "webhook": {"url": "http://127.0.0.1:8080/hook", "token_file": "/run/secrets/hook"},
	  "web_push": {"subject": "mailto:admin@example.com", "allow_hosts": ["push.example.org"]},
	  "sample_every": "30s"
	}`
	read := fakeFiles(map[string]string{
		"/etc/sysmon/alerts.json": file,
		"/run/secrets/ntfy":       "tk_file_secret\n",
		"/run/secrets/hook":       "hook-secret",
	})
	cfg, err := loadAlertsConfig(alertFlags{SettingsPath: "/etc/sysmon/settings.json", CPU: 80}, read)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.ToSlash(cfg.File) != "/etc/sysmon/alerts.json" {
		t.Fatalf("file = %q", cfg.File)
	}
	if len(cfg.Rules) != 3 {
		t.Fatalf("rules = %+v", cfg.Rules)
	}
	if r := cfg.Rules[0]; r.ID != "cpu" || r.Threshold != 80 || r.Hold != 20*time.Second || r.Resolved {
		t.Fatalf("the -alert-cpu flag should replace the file's cpu rule on file defaults: %+v", r)
	}
	if r := cfg.Rules[1]; r.ID != "nvme" || r.Hysteresis != 5 || !r.Resolved || r.Priority != "urgent" || r.Release != 6*time.Second {
		t.Fatalf("nvme rule = %+v", r)
	}
	if r := cfg.Rules[2]; r.ID != "disk_free:/srv" || r.Match != "/srv" || r.Cooldown != 30*time.Minute {
		t.Fatalf("disk rule = %+v", r)
	}
	if cfg.Ntfy.Token != "tk_file_secret" || cfg.Ntfy.Priority != "high" || len(cfg.Ntfy.Tags) != 1 {
		t.Fatalf("ntfy = %+v", cfg.Ntfy)
	}
	if cfg.Webhook.Token != "hook-secret" {
		t.Fatalf("webhook = %+v", cfg.Webhook)
	}
	if cfg.WebPush == nil || cfg.WebPush.Subject != "mailto:admin@example.com" || !webPushHostAllowed(cfg.WebPush.AllowHosts, "push.example.org") || !webPushHostAllowed(cfg.WebPush.AllowHosts, "fcm.googleapis.com") {
		t.Fatalf("web push = %+v", cfg.WebPush)
	}
	if cfg.SampleEvery != 30*time.Second {
		t.Fatalf("sample every = %v", cfg.SampleEvery)
	}
	// The environment's token wins over the file's.
	cfg, err = loadAlertsConfig(alertFlags{SettingsPath: "/etc/sysmon/settings.json", NtfyToken: "tk_env"}, read)
	if err != nil || cfg.Ntfy.Token != "tk_env" {
		t.Fatalf("env token: %v %+v", err, cfg.Ntfy)
	}
}

func TestLoadAlertsConfigRejects(t *testing.T) {
	cases := map[string]struct {
		flags alertFlags
		file  string
		want  string
	}{
		"unknown field":       {file: `{"rules":[{"metric":"cpu","above":90,"threshold":1}]}`, want: "unknown field"},
		"two objects":         {file: `{} {}`, want: "single JSON object"},
		"bad metric":          {file: `{"rules":[{"metric":"fan","above":90}]}`, want: "is not one of"},
		"wrong direction":     {file: `{"rules":[{"metric":"disk_free","above":90}]}`, want: "below"},
		"no threshold":        {file: `{"rules":[{"metric":"cpu"}]}`, want: "needs a threshold"},
		"out of range":        {file: `{"rules":[{"metric":"cpu","above":150}]}`, want: "outside"},
		"update threshold":    {file: `{"rules":[{"metric":"update","above":1}]}`, want: "takes no threshold"},
		"duplicate id":        {file: `{"rules":[{"metric":"cpu","above":90},{"metric":"cpu","above":80,"enabled":false}]}`, want: "used twice"},
		"bad id":              {file: `{"rules":[{"id":"a b","metric":"cpu","above":90}]}`, want: "id"},
		"bad duration":        {file: `{"rules":[{"metric":"cpu","above":90,"hold":"soon"}]}`, want: "not a duration"},
		"bad priority":        {file: `{"rules":[{"metric":"cpu","above":90,"priority":"loud"}]}`, want: "priority"},
		"ntfy no topic":       {flags: alertFlags{NtfyURL: "https://ntfy.example/"}, want: "needs a topic"},
		"ntfy userinfo":       {flags: alertFlags{NtfyURL: "https://u:p@ntfy.example/t"}, want: "token file"},
		"token over http":     {flags: alertFlags{NtfyURL: "http://ntfy.example/t", NtfyToken: "tk_x"}, want: "only over https"},
		"token without url":   {flags: alertFlags{NtfyToken: "tk_x"}, want: "without -ntfy-url"},
		"webhook token alone": {flags: alertFlags{WebhookTokenFile: "/x"}, want: "without -alert-webhook-url"},
		"webhook scheme":      {flags: alertFlags{WebhookURL: "ftp://hooks.example/x"}, want: "http:// or https://"},
		"sample too fast":     {flags: alertFlags{CPU: 90, SampleEvery: time.Second}, want: "between 5s and 5m"},
		"push subject":        {flags: alertFlags{WebPush: true, WebPushSubject: "admin@example.com"}, want: "mailto:"},
		"push host":           {file: `{"web_push":{"allow_hosts":["192.0.2.9"]}}`, want: "allow_hosts"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			files := map[string]string{}
			if tc.file != "" {
				files["/cfg/alerts.json"] = tc.file
				tc.flags.ConfigFile = "/cfg/alerts.json"
			}
			_, err := loadAlertsConfig(tc.flags, fakeFiles(files))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
	var many []string
	for i := 0; i <= maxAlertRules; i++ {
		many = append(many, `{"id":"r`+strings.Repeat("x", i%3)+string(rune('a'+i%26))+string(rune('a'+i/26))+`","metric":"cpu","above":90}`)
	}
	_, err := loadAlertsConfig(alertFlags{ConfigFile: "/cfg/a.json"}, fakeFiles(map[string]string{"/cfg/a.json": `{"rules":[` + strings.Join(many, ",") + `]}`}))
	if err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("65 rules: %v", err)
	}
}

func TestAlertTokenErrorsNeverHoldTheToken(t *testing.T) {
	secret := "tk_super secret value"
	_, err := alertToken("ntfy", "", "/run/t", fakeFiles(map[string]string{"/run/t": secret}))
	if err == nil {
		t.Fatal("a token with a space should be refused")
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "/run/t") {
		t.Fatalf("error leaks the token or its path: %v", err)
	}
	_, err = alertToken("ntfy", "", "/run/missing", fakeFiles(nil))
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing token file: %v", err)
	}
	_, err = alertToken("ntfy", "", "/run/big", fakeFiles(map[string]string{"/run/big": strings.Repeat("a", maxAlertTokenSize+1)}))
	if err == nil {
		t.Fatal("an oversized token file should be refused")
	}
	_, err = alertToken("ntfy", "", "/run/denied", func(string) ([]byte, error) { return nil, errors.New("open /run/denied: tk_in_error") })
	if err == nil || strings.Contains(err.Error(), "tk_in_error") {
		t.Fatalf("read error passed through: %v", err)
	}
}

func TestAlertTargetHostKeepsOnlyTheHost(t *testing.T) {
	if got := alertTargetHost("https://ntfy.example:8443/secret-topic?x=1"); got != "ntfy.example:8443" {
		t.Fatalf("host = %q", got)
	}
	names := alertTargetNames(alertsConfig{Ntfy: &ntfyConfig{URL: "https://ntfy.example/secret-topic"}, Webhook: &webhookConfig{URL: "https://hooks.example/path/secret"}})
	if strings.Contains(names, "secret") || !strings.Contains(names, "ntfy.example") {
		t.Fatalf("target names = %q", names)
	}
}
