package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"sysmon-agent/internal/gpuguard"
)

// Host alerts (docs: README "Host alerts and push notifications").
//
// The agent judges a handful of rules against its own warm snapshot every
// two seconds, dashboard or not, and sends a notification when one has held
// for its hold time: to ntfy, to a webhook, and to browsers that subscribed
// with Web Push (alerts_push.go, alerts_webpush.go). This file holds the
// configuration and the rule state machine, which is pure: it takes the time
// as an argument, so tests drive it with a fake clock.
//
// Every rule's instance walks idle -> pending -> firing -> releasing -> idle:
//
//   - pending: the value is over the line; it must stay there for the hold
//     time (default 15 s) before the alert fires. Dipping back, or the reading
//     going missing, returns it to idle.
//   - firing: the alert is active. It notifies unless the same instance
//     notified within the cooldown (default 10 min), in which case it is
//     still shown as active but sends nothing.
//   - releasing: the value is clear of the line by the hysteresis margin; it
//     must stay clear for the release time (default 4 s) to resolve. A reading
//     that goes missing never resolves an alert, it holds it where it is.
//
// The resolved notice goes out only when the rule asks for one and the
// episode's firing notice was sent, so a suppressed firing never produces a
// lone "resolved".

const (
	alertMetricCPU        = "cpu"
	alertMetricMemory     = "memory"
	alertMetricGPUTemp    = "gpu_temp"
	alertMetricCPUTemp    = "cpu_temp"
	alertMetricSensorTemp = "sensor_temp"
	alertMetricDiskFree   = "disk_free"
	alertMetricUpdate     = "update"

	defaultAlertHold        = 15 * time.Second
	defaultAlertRelease     = 4 * time.Second
	defaultAlertCooldown    = 10 * time.Minute
	defaultAlertSampleEvery = 15 * time.Second
	defaultAlertHysteresis  = 3

	// alertTick is how often the alert loop judges the snapshot, the same
	// cadence as the GPU guard's loop.
	alertTick = 2 * time.Second
	// alertFastFreshFor is how old a fast-lane reading (CPU, memory) may be
	// and still be judged. The fast lane never parks, so anything older means
	// the sampler is stuck, and a stuck reading must not fire or resolve.
	alertFastFreshFor = 10 * time.Second

	maxAlertRules       = 64
	maxAlertsConfigSize = 256 << 10
	maxAlertTokenSize   = 4 << 10
	maxAlertInstances   = 256
)

type alertMetricSpec struct {
	Unit  string
	Below bool // the rule fires when the value falls to the threshold or under
	Min   float64
	Max   float64
	// Slow metrics come from the sampler's slow lane, which parks when no one
	// watches; a rule on one keeps the lane warm at the background cadence.
	Slow bool
	Name string
}

var alertMetricSpecs = map[string]alertMetricSpec{
	alertMetricCPU:        {Unit: "%", Min: 1, Max: 100, Name: "CPU"},
	alertMetricMemory:     {Unit: "%", Min: 1, Max: 100, Name: "Memory"},
	alertMetricGPUTemp:    {Unit: "C", Min: 20, Max: 150, Slow: true, Name: "GPU temperature"},
	alertMetricCPUTemp:    {Unit: "C", Min: 20, Max: 150, Slow: true, Name: "CPU temperature"},
	alertMetricSensorTemp: {Unit: "C", Min: 20, Max: 150, Slow: true, Name: "Temperature"},
	alertMetricDiskFree:   {Unit: "%", Below: true, Min: 1, Max: 99, Slow: true, Name: "Free space"},
	alertMetricUpdate:     {Name: "Agent update"},
}

func alertMetricNames() []string {
	names := make([]string, 0, len(alertMetricSpecs))
	for name := range alertMetricSpecs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// alertRule is one validated rule.
type alertRule struct {
	ID         string
	Metric     string
	Match      string
	Below      bool
	Threshold  float64
	Hysteresis float64
	Hold       time.Duration
	Release    time.Duration
	Cooldown   time.Duration
	Resolved   bool
	// Priority is the ntfy priority of a firing notice (min, low, default,
	// high, urgent); empty means high.
	Priority string
}

func (r alertRule) spec() alertMetricSpec { return alertMetricSpecs[r.Metric] }

func (r alertRule) direction() string {
	if r.Below {
		return "below"
	}
	return "above"
}

// breached and clear are the two lines: firing needs the value at or past the
// threshold, resolving needs it back by the hysteresis margin.
func (r alertRule) breached(v float64) bool {
	if r.Below {
		return v <= r.Threshold
	}
	return v >= r.Threshold
}

func (r alertRule) clear(v float64) bool {
	if r.Below {
		return v > r.Threshold+r.Hysteresis
	}
	return v < r.Threshold-r.Hysteresis
}

// alertsConfig is the whole validated configuration.
type alertsConfig struct {
	Rules       []alertRule
	Ntfy        *ntfyConfig
	Webhook     *webhookConfig
	WebPush     *webPushConfig
	SampleEvery time.Duration
	// File is the config file that was read, empty when none was.
	File string
}

type ntfyConfig struct {
	URL      string
	Token    string
	Priority string
	Tags     []string
}

type webhookConfig struct {
	URL   string
	Token string
}

type webPushConfig struct {
	Subject    string
	AllowHosts []string
}

// enabled is true when there is anything to do: a rule to judge or a target
// to send a test notification to.
func (c alertsConfig) enabled() bool {
	return len(c.Rules) > 0 || c.hasTargets()
}

func (c alertsConfig) hasTargets() bool {
	return c.Ntfy != nil || c.Webhook != nil || c.WebPush != nil
}

// needsSlowSampling reports whether a rule reads the slow lane, or, on a
// collector without lanes, anything from the sampler at all.
func (c alertsConfig) needsSlowSampling(hasLanes bool) bool {
	for _, rule := range c.Rules {
		if rule.Metric == alertMetricUpdate {
			continue
		}
		if !hasLanes || rule.spec().Slow {
			return true
		}
	}
	return false
}

// alertsFile is the JSON config file. Durations are Go duration strings
// ("15s", "10m"). Tokens are never in it, only the files that hold them.
type alertsFile struct {
	Defaults    *alertDefaultsFile `json:"defaults,omitempty"`
	Rules       []alertRuleFile    `json:"rules,omitempty"`
	Ntfy        *ntfyFile          `json:"ntfy,omitempty"`
	Webhook     *webhookFile       `json:"webhook,omitempty"`
	WebPush     *webPushFile       `json:"web_push,omitempty"`
	SampleEvery string             `json:"sample_every,omitempty"`
}

type alertDefaultsFile struct {
	Hold     string `json:"hold,omitempty"`
	Release  string `json:"release,omitempty"`
	Cooldown string `json:"cooldown,omitempty"`
	Resolved *bool  `json:"resolved,omitempty"`
}

type alertRuleFile struct {
	ID         string   `json:"id,omitempty"`
	Metric     string   `json:"metric"`
	Above      *float64 `json:"above,omitempty"`
	Below      *float64 `json:"below,omitempty"`
	Match      string   `json:"match,omitempty"`
	Hysteresis *float64 `json:"hysteresis,omitempty"`
	Hold       string   `json:"hold,omitempty"`
	Release    string   `json:"release,omitempty"`
	Cooldown   string   `json:"cooldown,omitempty"`
	Resolved   *bool    `json:"resolved,omitempty"`
	Priority   string   `json:"priority,omitempty"`
	Enabled    *bool    `json:"enabled,omitempty"`
}

type ntfyFile struct {
	URL       string   `json:"url"`
	TokenFile string   `json:"token_file,omitempty"`
	Priority  string   `json:"priority,omitempty"`
	Tags      []string `json:"tags,omitempty"`
}

type webhookFile struct {
	URL       string `json:"url"`
	TokenFile string `json:"token_file,omitempty"`
}

type webPushFile struct {
	Enabled    *bool    `json:"enabled,omitempty"`
	Subject    string   `json:"subject,omitempty"`
	AllowHosts []string `json:"allow_hosts,omitempty"`
}

// alertFlags is what main.go's flags and their SYSMON_* variables give.
// Zero values mean unset.
type alertFlags struct {
	ConfigFile   string
	SettingsPath string

	CPU      int
	Memory   int
	GPUTemp  int
	CPUTemp  int
	DiskFree int
	Update   bool

	Hold     time.Duration
	Release  time.Duration
	Cooldown time.Duration

	NtfyURL          string
	NtfyTokenFile    string
	NtfyToken        string
	WebhookURL       string
	WebhookTokenFile string
	WebhookToken     string
	WebPush          bool
	WebPushSubject   string
	SampleEvery      time.Duration
}

// defaultAlertsConfigPath is alerts.json beside the settings file.
func defaultAlertsConfigPath(settingsPath string) string {
	if strings.TrimSpace(settingsPath) == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(settingsPath), "alerts.json")
}

// loadAlertsConfig merges the config file and the flags. A file named with
// -alerts-config must exist; the default alerts.json beside the settings is
// read only when it is there. A flag rule replaces a file rule of the same id,
// and flag targets replace the file's.
func loadAlertsConfig(in alertFlags, readFile func(string) ([]byte, error)) (alertsConfig, error) {
	if readFile == nil {
		readFile = os.ReadFile
	}
	var file alertsFile
	cfg := alertsConfig{SampleEvery: defaultAlertSampleEvery}
	path := strings.TrimSpace(in.ConfigFile)
	explicit := path != ""
	if !explicit {
		path = defaultAlertsConfigPath(in.SettingsPath)
	}
	if path != "" {
		data, err := readFile(path)
		switch {
		case err == nil:
			parsed, err := parseAlertsFile(data)
			if err != nil {
				return alertsConfig{}, fmt.Errorf("%s: %w", filepath.Base(path), err)
			}
			file = parsed
			cfg.File = path
		case errors.Is(err, os.ErrNotExist) && !explicit:
		default:
			return alertsConfig{}, fmt.Errorf("read alerts config: %w", err)
		}
	}

	defaults := alertRule{Hold: defaultAlertHold, Release: defaultAlertRelease, Cooldown: defaultAlertCooldown, Resolved: true}
	if d := file.Defaults; d != nil {
		var err error
		if defaults.Hold, err = parseAlertDuration("defaults.hold", d.Hold, defaults.Hold, 0, time.Hour); err != nil {
			return alertsConfig{}, err
		}
		if defaults.Release, err = parseAlertDuration("defaults.release", d.Release, defaults.Release, 0, time.Hour); err != nil {
			return alertsConfig{}, err
		}
		if defaults.Cooldown, err = parseAlertDuration("defaults.cooldown", d.Cooldown, defaults.Cooldown, 0, 7*24*time.Hour); err != nil {
			return alertsConfig{}, err
		}
		if d.Resolved != nil {
			defaults.Resolved = *d.Resolved
		}
	}
	if in.Hold > 0 {
		defaults.Hold = in.Hold
	}
	if in.Release > 0 {
		defaults.Release = in.Release
	}
	if in.Cooldown > 0 {
		defaults.Cooldown = in.Cooldown
	}
	if defaults.Hold > time.Hour || defaults.Release > time.Hour || defaults.Cooldown > 7*24*time.Hour {
		return alertsConfig{}, errors.New("alert hold and release are at most 1h, the cooldown at most 7 days")
	}

	byID := map[string]int{}
	add := func(rule alertRule) {
		if i, ok := byID[rule.ID]; ok {
			cfg.Rules[i] = rule
			return
		}
		byID[rule.ID] = len(cfg.Rules)
		cfg.Rules = append(cfg.Rules, rule)
	}
	for i, raw := range file.Rules {
		rule, enabled, err := buildAlertRule(raw, defaults)
		if err != nil {
			return alertsConfig{}, fmt.Errorf("rules[%d]: %w", i, err)
		}
		if _, dup := byID[rule.ID]; dup {
			return alertsConfig{}, fmt.Errorf("rules[%d]: id %q is used twice", i, rule.ID)
		}
		if enabled {
			add(rule)
		} else {
			// A disabled rule still claims its id, so a typo'd duplicate
			// is caught, but judges nothing.
			byID[rule.ID] = -1
		}
	}
	// Drop the disabled placeholders' bookkeeping before flags add rules.
	for id, i := range byID {
		if i < 0 {
			delete(byID, id)
		}
	}
	flagRules := []struct {
		metric string
		value  int
	}{
		{alertMetricCPU, in.CPU},
		{alertMetricMemory, in.Memory},
		{alertMetricGPUTemp, in.GPUTemp},
		{alertMetricCPUTemp, in.CPUTemp},
		{alertMetricDiskFree, in.DiskFree},
	}
	for _, fr := range flagRules {
		if fr.value == 0 {
			continue
		}
		value := float64(fr.value)
		raw := alertRuleFile{ID: fr.metric, Metric: fr.metric}
		if alertMetricSpecs[fr.metric].Below {
			raw.Below = &value
		} else {
			raw.Above = &value
		}
		rule, _, err := buildAlertRule(raw, defaults)
		if err != nil {
			return alertsConfig{}, fmt.Errorf("-alert-%s: %w", strings.ReplaceAll(fr.metric, "_", "-"), err)
		}
		add(rule)
	}
	if in.Update {
		rule, _, _ := buildAlertRule(alertRuleFile{ID: alertMetricUpdate, Metric: alertMetricUpdate}, defaults)
		add(rule)
	}
	if len(cfg.Rules) > maxAlertRules {
		return alertsConfig{}, fmt.Errorf("at most %d alert rules", maxAlertRules)
	}

	// Targets.
	if f := file.Ntfy; f != nil || in.NtfyURL != "" {
		var nf ntfyFile
		if f != nil {
			nf = *f
		}
		if in.NtfyURL != "" {
			nf.URL = in.NtfyURL
		}
		if in.NtfyTokenFile != "" {
			nf.TokenFile = in.NtfyTokenFile
		}
		token, err := alertToken("ntfy", in.NtfyToken, nf.TokenFile, readFile)
		if err != nil {
			return alertsConfig{}, err
		}
		target, err := buildNtfyConfig(nf, token)
		if err != nil {
			return alertsConfig{}, err
		}
		cfg.Ntfy = target
	} else if in.NtfyToken != "" || in.NtfyTokenFile != "" {
		return alertsConfig{}, errors.New("ntfy: a token was given without -ntfy-url")
	}
	if f := file.Webhook; f != nil || in.WebhookURL != "" {
		var wf webhookFile
		if f != nil {
			wf = *f
		}
		if in.WebhookURL != "" {
			wf.URL = in.WebhookURL
		}
		if in.WebhookTokenFile != "" {
			wf.TokenFile = in.WebhookTokenFile
		}
		token, err := alertToken("webhook", in.WebhookToken, wf.TokenFile, readFile)
		if err != nil {
			return alertsConfig{}, err
		}
		u, err := validateAlertTargetURL("webhook", wf.URL, token != "")
		if err != nil {
			return alertsConfig{}, err
		}
		cfg.Webhook = &webhookConfig{URL: u.String(), Token: token}
	} else if in.WebhookToken != "" || in.WebhookTokenFile != "" {
		return alertsConfig{}, errors.New("webhook: a token was given without -alert-webhook-url")
	}
	webPushOn := in.WebPush
	var wp webPushFile
	if f := file.WebPush; f != nil {
		wp = *f
		if f.Enabled == nil || *f.Enabled {
			webPushOn = true
		}
	}
	if webPushOn {
		subject := firstNonEmpty(strings.TrimSpace(in.WebPushSubject), strings.TrimSpace(wp.Subject), defaultWebPushSubject)
		if err := validateWebPushSubject(subject); err != nil {
			return alertsConfig{}, err
		}
		hosts, err := normalizeWebPushHosts(wp.AllowHosts)
		if err != nil {
			return alertsConfig{}, err
		}
		cfg.WebPush = &webPushConfig{Subject: subject, AllowHosts: hosts}
	}

	every := in.SampleEvery
	if every == 0 && file.SampleEvery != "" {
		d, err := parseAlertDuration("sample_every", file.SampleEvery, 0, 0, 0)
		if err != nil {
			return alertsConfig{}, err
		}
		every = d
	}
	if every != 0 {
		if every < 5*time.Second || every > 5*time.Minute {
			return alertsConfig{}, errors.New("alert sample interval must be between 5s and 5m")
		}
		cfg.SampleEvery = every
	}
	return cfg, nil
}

func parseAlertsFile(data []byte) (alertsFile, error) {
	if len(data) > maxAlertsConfigSize {
		return alertsFile{}, fmt.Errorf("larger than %d KiB", maxAlertsConfigSize>>10)
	}
	var file alertsFile
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return alertsFile{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return alertsFile{}, errors.New("must hold a single JSON object")
	}
	return file, nil
}

func buildAlertRule(raw alertRuleFile, defaults alertRule) (alertRule, bool, error) {
	metric := strings.TrimSpace(strings.ToLower(raw.Metric))
	spec, ok := alertMetricSpecs[metric]
	if !ok {
		return alertRule{}, false, fmt.Errorf("metric %q is not one of %s", raw.Metric, strings.Join(alertMetricNames(), ", "))
	}
	rule := defaults
	rule.Metric = metric
	rule.ID = strings.TrimSpace(raw.ID)
	if rule.ID == "" {
		rule.ID = metric
		if m := strings.TrimSpace(raw.Match); m != "" {
			rule.ID = metric + ":" + m
		}
	}
	if !validAlertID(rule.ID) {
		return alertRule{}, false, fmt.Errorf("id %q: use up to 64 letters, digits and . _ : / - characters", rule.ID)
	}
	rule.Match = strings.TrimSpace(raw.Match)
	if len(rule.Match) > 128 {
		return alertRule{}, false, errors.New("match is longer than 128 characters")
	}
	if metric == alertMetricUpdate {
		// One notice per new release: no hold, no release, nothing to
		// resolve, and the threshold is "an update is available".
		if raw.Above != nil || raw.Below != nil || raw.Hysteresis != nil {
			return alertRule{}, false, errors.New("the update rule takes no threshold")
		}
		rule.Threshold = 1
		rule.Hold, rule.Release, rule.Hysteresis = 0, 0, 0
		rule.Resolved = false
		rule.Match = ""
		if raw.Cooldown != "" {
			d, err := parseAlertDuration("cooldown", raw.Cooldown, 0, 0, 7*24*time.Hour)
			if err != nil {
				return alertRule{}, false, err
			}
			rule.Cooldown = d
		}
	} else {
		switch {
		case raw.Above != nil && raw.Below != nil:
			return alertRule{}, false, errors.New("give above or below, not both")
		case raw.Above != nil:
			rule.Threshold = *raw.Above
		case raw.Below != nil:
			rule.Threshold = *raw.Below
			rule.Below = true
		default:
			return alertRule{}, false, errors.New("needs a threshold: above or below")
		}
		if rule.Below != spec.Below {
			return alertRule{}, false, fmt.Errorf("%s alerts %s a threshold", metric, map[bool]string{true: "below", false: "above"}[spec.Below])
		}
		if math.IsNaN(rule.Threshold) || rule.Threshold < spec.Min || rule.Threshold > spec.Max {
			return alertRule{}, false, fmt.Errorf("threshold %v is outside %v-%v %s", rule.Threshold, spec.Min, spec.Max, spec.Unit)
		}
		rule.Hysteresis = defaultAlertHysteresis
		if raw.Hysteresis != nil {
			rule.Hysteresis = *raw.Hysteresis
		}
		if math.IsNaN(rule.Hysteresis) || rule.Hysteresis < 0 || rule.Hysteresis > 50 {
			return alertRule{}, false, errors.New("hysteresis must be between 0 and 50")
		}
		var err error
		if rule.Hold, err = parseAlertDuration("hold", raw.Hold, rule.Hold, 0, time.Hour); err != nil {
			return alertRule{}, false, err
		}
		if rule.Release, err = parseAlertDuration("release", raw.Release, rule.Release, 0, time.Hour); err != nil {
			return alertRule{}, false, err
		}
		if rule.Cooldown, err = parseAlertDuration("cooldown", raw.Cooldown, rule.Cooldown, 0, 7*24*time.Hour); err != nil {
			return alertRule{}, false, err
		}
		if raw.Resolved != nil {
			rule.Resolved = *raw.Resolved
		}
	}
	if raw.Priority != "" {
		p, err := normalizeNtfyPriority(raw.Priority)
		if err != nil {
			return alertRule{}, false, err
		}
		rule.Priority = p
	}
	enabled := raw.Enabled == nil || *raw.Enabled
	return rule, enabled, nil
}

func validAlertID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("._:/-", r):
		default:
			return false
		}
	}
	return true
}

func parseAlertDuration(name, value string, fallback, min, max time.Duration) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a duration such as 15s or 10m", name, value)
	}
	if d < min || (max > 0 && d > max) {
		return 0, fmt.Errorf("%s: %s is out of range", name, value)
	}
	return d, nil
}

var ntfyPriorities = map[string]string{
	"1": "min", "min": "min",
	"2": "low", "low": "low",
	"3": "default", "default": "default",
	"4": "high", "high": "high",
	"5": "urgent", "urgent": "urgent", "max": "urgent",
}

func normalizeNtfyPriority(value string) (string, error) {
	p, ok := ntfyPriorities[strings.ToLower(strings.TrimSpace(value))]
	if !ok {
		return "", fmt.Errorf("priority %q is not one of min, low, default, high, urgent", value)
	}
	return p, nil
}

// alertToken reads a bearer token from the environment, else from a file.
// The token is never logged, never put in an error and never served.
func alertToken(target, fromEnv, file string, readFile func(string) ([]byte, error)) (string, error) {
	token := strings.TrimSpace(fromEnv)
	if token == "" && strings.TrimSpace(file) != "" {
		data, err := readFile(strings.TrimSpace(file))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return "", fmt.Errorf("%s: the token file does not exist", target)
			}
			return "", fmt.Errorf("%s: the token file cannot be read", target)
		}
		if len(data) > maxAlertTokenSize {
			return "", fmt.Errorf("%s: the token file is larger than %d bytes", target, maxAlertTokenSize)
		}
		token = strings.TrimSpace(string(data))
		if token == "" {
			return "", fmt.Errorf("%s: the token file is empty", target)
		}
	}
	for _, r := range token {
		if r < 0x21 || r > 0x7e {
			return "", fmt.Errorf("%s: the token holds spaces or characters a header cannot carry", target)
		}
	}
	return token, nil
}

func buildNtfyConfig(f ntfyFile, token string) (*ntfyConfig, error) {
	u, err := validateAlertTargetURL("ntfy", f.URL, token != "")
	if err != nil {
		return nil, err
	}
	if strings.Trim(u.Path, "/") == "" {
		return nil, errors.New("ntfy: the URL needs a topic, such as https://ntfy.sh/<topic>")
	}
	cfg := &ntfyConfig{URL: u.String(), Token: token}
	if f.Priority != "" {
		p, err := normalizeNtfyPriority(f.Priority)
		if err != nil {
			return nil, fmt.Errorf("ntfy: %w", err)
		}
		cfg.Priority = p
	}
	for _, tag := range f.Tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if len(tag) > 64 || strings.ContainsAny(tag, ",\r\n") || !isPrintableASCII(tag) {
			return nil, fmt.Errorf("ntfy: tag %q must be short printable ASCII without commas", tag)
		}
		cfg.Tags = append(cfg.Tags, tag)
	}
	if len(cfg.Tags) > 8 {
		return nil, errors.New("ntfy: at most 8 tags")
	}
	return cfg, nil
}

// validateAlertTargetURL accepts http(s) URLs without credentials in them.
// A bearer token is refused over plain http to anything but this machine,
// since the network would see it.
func validateAlertTargetURL(target, raw string, hasToken bool) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("%s: a URL is needed", target)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("%s: the URL must be http:// or https:// with a host", target)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%s: put credentials in a token file, not in the URL", target)
	}
	if u.Fragment != "" {
		return nil, fmt.Errorf("%s: the URL must not have a #fragment", target)
	}
	if hasToken && u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return nil, fmt.Errorf("%s: a token is sent only over https (or http to this machine)", target)
	}
	return u, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isPrintableASCII(s string) bool {
	for _, r := range s {
		if r < 0x20 || r > 0x7e {
			return false
		}
	}
	return true
}

// alertTargetHost is the part of a target URL that may be logged: its host,
// never its path (an ntfy topic is the secret that lets anyone read it).
func alertTargetHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "?"
	}
	return u.Host
}

// ---- state machine ----

type alertPhase int

const (
	alertIdle alertPhase = iota
	alertPending
	alertFiring
	alertReleasing
)

func (p alertPhase) String() string {
	switch p {
	case alertPending:
		return "pending"
	case alertFiring:
		return "firing"
	case alertReleasing:
		return "releasing"
	default:
		return "idle"
	}
}

// alertObservation is one reading for a rule: one disk, one sensor, one GPU.
// Known false means the reading is missing or stale.
type alertObservation struct {
	RuleID   string
	Instance string
	Label    string
	Value    float64
	Known    bool
}

type alertInstance struct {
	RuleID   string
	Instance string
	Label    string
	Phase    alertPhase
	// Since is when the instance entered pending or releasing.
	Since   time.Time
	FiredAt time.Time
	Value   float64
	Known   bool
	// Notified is whether this episode's firing notice was sent.
	Notified bool
}

// alertEvent is a transition worth telling someone about.
type alertEvent struct {
	Kind     string // "firing" or "resolved"
	Rule     alertRule
	Instance string
	Label    string
	Value    float64
	Known    bool
	FiredAt  time.Time
	At       time.Time
	// Notify is false for a firing inside the cooldown, which is shown but
	// not sent.
	Notify bool
}

type alertEngine struct {
	rules     []alertRule
	instances map[string]*alertInstance
	// lastNotified is per rule and instance, and outlives the instance so a
	// disk that comes and goes cannot dodge the cooldown.
	lastNotified map[string]time.Time
}

func newAlertEngine(rules []alertRule) *alertEngine {
	return &alertEngine{rules: rules, instances: map[string]*alertInstance{}, lastNotified: map[string]time.Time{}}
}

func alertKey(ruleID, instance string) string { return ruleID + "\x00" + instance }

// Observe steps every instance with this round's readings and returns the
// transitions. Instances that got no reading this round are stepped as
// unknown (the update rule, whose instance is a version, treats a missing
// version as resolved), and dropped once idle.
func (e *alertEngine) Observe(now time.Time, observations []alertObservation) []alertEvent {
	seen := map[string]bool{}
	var events []alertEvent
	rules := map[string]alertRule{}
	for _, rule := range e.rules {
		rules[rule.ID] = rule
	}
	for _, obs := range observations {
		rule, ok := rules[obs.RuleID]
		if !ok {
			continue
		}
		key := alertKey(obs.RuleID, obs.Instance)
		if seen[key] {
			continue
		}
		seen[key] = true
		inst := e.instances[key]
		if inst == nil {
			if !obs.Known || !rule.breached(obs.Value) || len(e.instances) >= maxAlertInstances {
				continue
			}
			inst = &alertInstance{RuleID: obs.RuleID, Instance: obs.Instance}
			e.instances[key] = inst
		}
		inst.Label = obs.Label
		if ev := e.step(rule, inst, now, obs.Value, obs.Known); ev != nil {
			events = append(events, *ev)
		}
	}
	for key, inst := range e.instances {
		if !seen[key] {
			rule, ok := rules[inst.RuleID]
			if !ok {
				delete(e.instances, key)
				continue
			}
			known := rule.Metric == alertMetricUpdate
			if ev := e.step(rule, inst, now, 0, known); ev != nil {
				events = append(events, *ev)
			}
		}
		if inst.Phase == alertIdle {
			delete(e.instances, key)
		}
	}
	for key, at := range e.lastNotified {
		if now.Sub(at) > 7*24*time.Hour {
			delete(e.lastNotified, key)
		}
	}
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].Rule.ID != events[j].Rule.ID {
			return events[i].Rule.ID < events[j].Rule.ID
		}
		return events[i].Instance < events[j].Instance
	})
	return events
}

func (e *alertEngine) step(rule alertRule, inst *alertInstance, now time.Time, value float64, known bool) *alertEvent {
	if known {
		inst.Value = value
	}
	inst.Known = known
	breached := known && rule.breached(value)
	clear := known && rule.clear(value)
	switch inst.Phase {
	case alertIdle:
		if !breached {
			return nil
		}
		inst.Phase = alertPending
		inst.Since = now
		if rule.Hold > 0 {
			return nil
		}
		return e.fire(rule, inst, now)
	case alertPending:
		if !breached {
			inst.Phase = alertIdle
			return nil
		}
		if now.Sub(inst.Since) >= rule.Hold {
			return e.fire(rule, inst, now)
		}
	case alertFiring:
		if !clear {
			return nil
		}
		inst.Phase = alertReleasing
		inst.Since = now
		if rule.Release > 0 {
			return nil
		}
		return e.resolve(rule, inst, now)
	case alertReleasing:
		if !clear {
			// Back over the line, or the reading went missing: still active.
			inst.Phase = alertFiring
			return nil
		}
		if now.Sub(inst.Since) >= rule.Release {
			return e.resolve(rule, inst, now)
		}
	}
	return nil
}

func (e *alertEngine) fire(rule alertRule, inst *alertInstance, now time.Time) *alertEvent {
	inst.Phase = alertFiring
	inst.FiredAt = now
	key := alertKey(rule.ID, inst.Instance)
	last, ok := e.lastNotified[key]
	notify := !ok || now.Sub(last) >= rule.Cooldown
	if notify {
		e.lastNotified[key] = now
	}
	inst.Notified = notify
	return &alertEvent{Kind: "firing", Rule: rule, Instance: inst.Instance, Label: inst.Label, Value: inst.Value, Known: true, FiredAt: now, At: now, Notify: notify}
}

func (e *alertEngine) resolve(rule alertRule, inst *alertInstance, now time.Time) *alertEvent {
	inst.Phase = alertIdle
	notify := rule.Resolved && inst.Notified
	ev := &alertEvent{Kind: "resolved", Rule: rule, Instance: inst.Instance, Label: inst.Label, Value: inst.Value, Known: inst.Known, FiredAt: inst.FiredAt, At: now, Notify: notify}
	inst.Notified = false
	return ev
}

// Instances lists the instances that are not idle, ordered by rule and
// instance, for the API.
func (e *alertEngine) Instances() []alertInstance {
	out := make([]alertInstance, 0, len(e.instances))
	for _, inst := range e.instances {
		if inst.Phase != alertIdle {
			out = append(out, *inst)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RuleID != out[j].RuleID {
			return out[i].RuleID < out[j].RuleID
		}
		return out[i].Instance < out[j].Instance
	})
	return out
}

// ---- observations ----

// alertInputs is what one round judges.
type alertInputs struct {
	Metrics   Metrics
	Have      bool
	FastFresh bool
	SlowFresh bool
	Guard     *gpuguard.State
	Update    *UpdateStatus
	Now       time.Time
}

// alertObservations turns a snapshot into readings for each rule. A metric
// whose lane is stale or whose reading is unavailable gives an unknown
// reading for the instances it already has, which holds them where they are.
func alertObservations(rules []alertRule, in alertInputs) []alertObservation {
	var out []alertObservation
	for _, rule := range rules {
		readings := alertReadings(rule.Metric, in)
		for _, r := range readings {
			if rule.Match != "" && !alertMatches(rule.Match, r.Instance, r.Label) {
				continue
			}
			r.RuleID = rule.ID
			out = append(out, r)
		}
	}
	return out
}

func alertMatches(match, instance, label string) bool {
	m := strings.ToLower(match)
	return strings.Contains(strings.ToLower(instance), m) || strings.Contains(strings.ToLower(label), m)
}

func alertReadings(metric string, in alertInputs) []alertObservation {
	m := in.Metrics
	switch metric {
	case alertMetricCPU:
		if !in.Have || !in.FastFresh {
			return nil
		}
		return []alertObservation{numberReading("", "CPU", m.CPU)}
	case alertMetricMemory:
		if !in.Have || !in.FastFresh {
			return nil
		}
		obs := alertObservation{Label: "Memory", Value: m.Memory.Percent, Known: m.Memory.Available}
		return []alertObservation{obs}
	case alertMetricCPUTemp:
		if !in.Have || !in.SlowFresh {
			return nil
		}
		return []alertObservation{numberReading("", "CPU", m.CPUTemperature)}
	case alertMetricGPUTemp:
		return gpuTempReadings(in)
	case alertMetricSensorTemp:
		if !in.Have || !in.SlowFresh {
			return nil
		}
		var out []alertObservation
		names := uniqueNamer{}
		for _, sensor := range m.Temperatures.Sensors {
			name := firstNonEmpty(strings.TrimSpace(sensor.Name), "sensor")
			out = append(out, numberReading(names.next("sensor:"+name), name, sensor.Celsius))
		}
		for _, dev := range m.Storage.Devices {
			label := firstNonEmpty(strings.TrimSpace(dev.Model), strings.TrimSpace(dev.Name), "drive")
			out = append(out, numberReading(names.next("drive:"+firstNonEmpty(dev.Name, label)), label, dev.Temperature))
		}
		return out
	case alertMetricDiskFree:
		if !in.Have || !in.SlowFresh {
			return nil
		}
		var out []alertObservation
		names := uniqueNamer{}
		for _, disk := range m.Disks {
			label := firstNonEmpty(strings.TrimSpace(disk.Mountpoint), strings.TrimSpace(disk.Name), "disk")
			known := disk.Capacity.Available && disk.Capacity.TotalBytes > 0
			free := 100 - disk.Capacity.Percent
			out = append(out, alertObservation{Instance: names.next(label), Label: label, Value: math.Max(0, free), Known: known})
		}
		return out
	case alertMetricUpdate:
		if in.Update == nil || !in.Update.Available || in.Update.LatestVersion == "" {
			return nil
		}
		return []alertObservation{{Instance: in.Update.LatestVersion, Label: "sysmon-agent " + in.Update.LatestVersion, Value: 1, Known: true}}
	}
	return nil
}

func numberReading(instance, label string, n NumberMetric) alertObservation {
	known := n.Available && !math.IsNaN(n.Value) && !math.IsInf(n.Value, 0)
	return alertObservation{Instance: instance, Label: label, Value: n.Value, Known: known}
}

// gpuTempReadings is each GPU's hottest of core and hotspot. The GPU guard,
// when it runs and is current, is read for its GPU in place of the slow
// lane: it samples every two seconds whether or not the lane is parked.
func gpuTempReadings(in alertInputs) []alertObservation {
	var out []alertObservation
	guardName := ""
	if g := in.Guard; g != nil && g.Source != gpuguard.SourceNone && !g.UpdatedAt.IsZero() && in.Now.Sub(g.UpdatedAt) <= alertFastFreshFor {
		guardName = firstNonEmpty(strings.TrimSpace(g.GPU), "GPU")
		value, known := hottest(g.Hotspot, g.Core)
		out = append(out, alertObservation{Instance: guardName, Label: guardName, Value: value, Known: known})
	}
	if !in.Have || !in.SlowFresh {
		return out
	}
	names := uniqueNamer{}
	for _, dev := range in.Metrics.GPU.Devices {
		name := firstNonEmpty(strings.TrimSpace(dev.Name), "GPU")
		key := names.next(name)
		if guardName != "" && key == guardName {
			continue
		}
		value, known := hottestNumber(dev.Temperature, dev.HotspotTemperature)
		out = append(out, alertObservation{Instance: key, Label: name, Value: value, Known: known})
	}
	return out
}

func hottest(readings ...gpuguard.Reading) (float64, bool) {
	best, ok := 0.0, false
	for _, r := range readings {
		if r.Available && (!ok || r.Value > best) {
			best, ok = r.Value, true
		}
	}
	return best, ok
}

func hottestNumber(readings ...NumberMetric) (float64, bool) {
	best, ok := 0.0, false
	for _, r := range readings {
		if r.Available && (!ok || r.Value > best) {
			best, ok = r.Value, true
		}
	}
	return best, ok
}

// uniqueNamer suffixes repeated names (" #2") so two identical drives keep
// separate instances.
type uniqueNamer map[string]int

func (u uniqueNamer) next(name string) string {
	u[name]++
	if n := u[name]; n > 1 {
		return name + " #" + strconv.Itoa(n)
	}
	return name
}

// ---- API shapes ----

// HostAlert is one active (or pending) alert, as /api/alerts and the
// metrics snapshot's host_alerts serve it.
type HostAlert struct {
	Rule      string    `json:"rule"`
	Metric    string    `json:"metric"`
	Instance  string    `json:"instance,omitempty"`
	Label     string    `json:"label"`
	State     string    `json:"state"`
	Value     float64   `json:"value"`
	Known     bool      `json:"known"`
	Threshold float64   `json:"threshold"`
	Direction string    `json:"direction"`
	Unit      string    `json:"unit,omitempty"`
	Since     time.Time `json:"since"`
	Notified  bool      `json:"notified"`
	Message   string    `json:"message"`
}

// HostAlertsSummary rides on every published snapshot while alerts run,
// with only the firing ones, so the dashboard's alerts chip and peers see
// them without another request.
type HostAlertsSummary struct {
	Active    []HostAlert `json:"active"`
	UpdatedAt time.Time   `json:"updated_at"`
}

type AlertRuleStatus struct {
	ID              string  `json:"id"`
	Metric          string  `json:"metric"`
	Match           string  `json:"match,omitempty"`
	Direction       string  `json:"direction"`
	Threshold       float64 `json:"threshold"`
	Hysteresis      float64 `json:"hysteresis"`
	Unit            string  `json:"unit,omitempty"`
	HoldSeconds     float64 `json:"hold_seconds"`
	ReleaseSeconds  float64 `json:"release_seconds"`
	CooldownSeconds float64 `json:"cooldown_seconds"`
	Resolved        bool    `json:"resolved"`
}

func alertMessage(rule alertRule, label string, value float64, known bool) string {
	spec := rule.spec()
	if rule.Metric == alertMetricUpdate {
		return label + " is available"
	}
	name := spec.Name
	switch rule.Metric {
	case alertMetricGPUTemp, alertMetricSensorTemp:
		name = label + " temperature"
		if rule.Metric == alertMetricSensorTemp && strings.Contains(strings.ToLower(label), "temp") {
			name = label
		}
	case alertMetricDiskFree:
		name = "Free space on " + label
	}
	val := "unknown"
	if known {
		val = formatAlertValue(value, spec.Unit)
	}
	return fmt.Sprintf("%s %s (%s %s)", name, val, map[bool]string{true: "under", false: "over"}[rule.Below], formatAlertValue(rule.Threshold, spec.Unit))
}

func formatAlertValue(v float64, unit string) string {
	text := strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64)
	switch unit {
	case "%":
		return text + "%"
	case "C":
		return text + " C"
	}
	return text
}

func hostAlertFrom(rule alertRule, inst alertInstance) HostAlert {
	since := inst.FiredAt
	if inst.Phase == alertPending {
		since = inst.Since
	}
	return HostAlert{
		Rule:      rule.ID,
		Metric:    rule.Metric,
		Instance:  inst.Instance,
		Label:     inst.Label,
		State:     inst.Phase.String(),
		Value:     roundAlert(inst.Value),
		Known:     inst.Known,
		Threshold: rule.Threshold,
		Direction: rule.direction(),
		Unit:      rule.spec().Unit,
		Since:     since.UTC(),
		Notified:  inst.Notified,
		Message:   alertMessage(rule, inst.Label, inst.Value, true),
	}
}

func roundAlert(v float64) float64 { return math.Round(v*10) / 10 }

func alertRuleStatus(rule alertRule) AlertRuleStatus {
	return AlertRuleStatus{
		ID:              rule.ID,
		Metric:          rule.Metric,
		Match:           rule.Match,
		Direction:       rule.direction(),
		Threshold:       rule.Threshold,
		Hysteresis:      rule.Hysteresis,
		Unit:            rule.spec().Unit,
		HoldSeconds:     rule.Hold.Seconds(),
		ReleaseSeconds:  rule.Release.Seconds(),
		CooldownSeconds: rule.Cooldown.Seconds(),
		Resolved:        rule.Resolved,
	}
}

// attachHostAlerts puts the alert loop's newest summary on a snapshot.
func attachHostAlerts(m *Metrics, source hostAlertsSource) {
	if source == nil {
		return
	}
	m.HostAlerts = source.Summary()
}

// hostAlertsSource is what the sampler reads on publish.
type hostAlertsSource interface {
	Summary() *HostAlertsSummary
}
