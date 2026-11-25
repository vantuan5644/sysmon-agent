package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"sysmon-agent/internal/gpuguard"
)

// alertService runs the host alert rules (alerts.go) on their own two-second
// loop, like the GPU guard, because an unwatched host is when an alert
// matters and the dashboard's demand is exactly what is missing then.
//
// The loop itself only reads the sampler's warm snapshot, so a tick costs a
// struct copy and a few comparisons. What does cost CPU is keeping that
// snapshot current: the fast lane (CPU, memory) always runs anyway, and for
// rules on slow-lane metrics (temperatures, disks, GPU) the sampler runs one
// slow pass every -alert-sample-every (15 s by default) while nobody watches,
// a tenth of the watched rate (sampler.SetBackgroundSampling). Rules on CPU
// and memory alone add no collection at all. The GPU rule reads the GPU
// guard's own two-second state first when the guard runs.

type alertSnapshot struct {
	Metrics   Metrics
	Have      bool
	FastFresh bool
	SlowFresh bool
}

type alertSnapshotSource interface {
	AlertSnapshot(now time.Time) alertSnapshot
	GPUGuardState() *gpuguard.State
}

type alertServiceOptions struct {
	Config       alertsConfig
	Host         string
	Version      string
	Source       alertSnapshotSource
	Updates      func() *UpdateStatus
	PushDir      string
	DeviceExists func(id string) bool
	Logf         func(string, ...any)
}

type alertService struct {
	cfg        alertsConfig
	host       string
	source     alertSnapshotSource
	updates    func() *UpdateStatus
	dispatcher *alertDispatcher
	webPush    *webPushTarget
	ntfy       *ntfyTarget
	webhook    *webhookTarget
	now        func() time.Time
	logf       func(string, ...any)

	mu         sync.Mutex
	engine     *alertEngine
	alerts     []HostAlert
	summary    *HostAlertsSummary
	lastTickAt time.Time
	lastTestAt time.Time
}

const alertTestGap = 10 * time.Second

func newAlertService(opts alertServiceOptions) (*alertService, error) {
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	a := &alertService{
		cfg:     opts.Config,
		host:    firstNonEmpty(opts.Host, "sysmon-agent"),
		source:  opts.Source,
		updates: opts.Updates,
		now:     time.Now,
		logf:    logf,
		engine:  newAlertEngine(opts.Config.Rules),
	}
	client := newAlertHTTPClient()
	userAgent := "sysmon-agent/" + firstNonEmpty(opts.Version, "dev")
	var targets []alertTarget
	if c := opts.Config.Ntfy; c != nil {
		a.ntfy = &ntfyTarget{cfg: *c, client: client, userAgent: userAgent}
		targets = append(targets, a.ntfy)
	}
	if c := opts.Config.Webhook; c != nil {
		a.webhook = &webhookTarget{cfg: *c, client: client, userAgent: userAgent, version: firstNonEmpty(opts.Version, "dev")}
		targets = append(targets, a.webhook)
	}
	if c := opts.Config.WebPush; c != nil {
		store, err := openWebPushStore(opts.PushDir)
		if err != nil {
			return nil, err
		}
		a.webPush = &webPushTarget{store: store, cfg: *c, client: client, userAgent: userAgent, deviceExists: opts.DeviceExists, logf: logf}
		targets = append(targets, a.webPush)
	}
	a.dispatcher = newAlertDispatcher(targets, logf)
	return a, nil
}

// Run judges the rules every alertTick until ctx ends.
func (a *alertService) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		a.dispatcher.Run(ctx)
	}()
	ticker := time.NewTicker(alertTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-ticker.C:
			a.tick(a.now())
		}
	}
}

func (a *alertService) tick(now time.Time) {
	in := alertInputs{Now: now}
	if a.source != nil {
		snap := a.source.AlertSnapshot(now)
		in.Metrics, in.Have, in.FastFresh, in.SlowFresh = snap.Metrics, snap.Have, snap.FastFresh, snap.SlowFresh
		in.Guard = a.source.GPUGuardState()
	}
	if a.updates != nil {
		in.Update = a.updates()
	}
	observations := alertObservations(a.cfg.Rules, in)

	a.mu.Lock()
	events := a.engine.Observe(now, observations)
	rules := map[string]alertRule{}
	for _, rule := range a.cfg.Rules {
		rules[rule.ID] = rule
	}
	var all, firing []HostAlert
	for _, inst := range a.engine.Instances() {
		alert := hostAlertFrom(rules[inst.RuleID], inst)
		all = append(all, alert)
		if inst.Phase == alertFiring || inst.Phase == alertReleasing {
			firing = append(firing, alert)
		}
	}
	if firing == nil {
		firing = []HostAlert{}
	}
	a.alerts = all
	a.summary = &HostAlertsSummary{Active: firing, UpdatedAt: now.UTC()}
	a.lastTickAt = now
	a.mu.Unlock()

	for _, ev := range events {
		message := alertMessage(ev.Rule, ev.Label, ev.Value, ev.Known)
		switch {
		case ev.Kind == "firing" && !ev.Notify:
			a.logf("alerts: firing %s: %s (cooldown, not sent)", ev.Rule.ID, message)
		case ev.Kind == "firing":
			a.logf("alerts: firing %s: %s", ev.Rule.ID, message)
		default:
			a.logf("alerts: resolved %s: %s", ev.Rule.ID, alertResolvedMessage(ev.Rule, ev.Label, ev.Value, ev.Known))
		}
		if ev.Notify {
			a.dispatcher.Enqueue(notificationFromEvent(a.host, ev))
		}
	}
}

// Summary is what the sampler puts on each snapshot as host_alerts.
func (a *alertService) Summary() *HostAlertsSummary {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.summary == nil {
		return &HostAlertsSummary{Active: []HostAlert{}}
	}
	return a.summary
}

// AlertsStatus is GET /api/alerts. It carries no URL path, token or
// subscription: targets are named by host, subscriptions by count.
type AlertsStatus struct {
	Enabled            bool                `json:"enabled"`
	Rules              []AlertRuleStatus   `json:"rules"`
	Alerts             []HostAlert         `json:"alerts"`
	Targets            AlertTargetsStatus  `json:"targets"`
	Delivery           alertDeliveryStatus `json:"delivery"`
	SampleEverySeconds float64             `json:"sample_every_seconds,omitempty"`
	ConfigFile         bool                `json:"config_file"`
	UpdatedAt          *time.Time          `json:"updated_at,omitempty"`
}

type AlertTargetsStatus struct {
	Ntfy    *AlertTargetInfo `json:"ntfy,omitempty"`
	Webhook *AlertTargetInfo `json:"webhook,omitempty"`
	WebPush *WebPushStatus   `json:"web_push,omitempty"`
}

type AlertTargetInfo struct {
	Host  string `json:"host"`
	Token bool   `json:"token"`
}

type WebPushStatus struct {
	PublicKey     string `json:"public_key"`
	Subscriptions int    `json:"subscriptions"`
}

func disabledAlertsStatus() AlertsStatus {
	return AlertsStatus{Rules: []AlertRuleStatus{}, Alerts: []HostAlert{}}
}

func (a *alertService) Status() AlertsStatus {
	if a == nil {
		return disabledAlertsStatus()
	}
	status := AlertsStatus{
		Enabled:    true,
		Rules:      make([]AlertRuleStatus, 0, len(a.cfg.Rules)),
		Delivery:   a.dispatcher.Status(),
		ConfigFile: a.cfg.File != "",
	}
	for _, rule := range a.cfg.Rules {
		status.Rules = append(status.Rules, alertRuleStatus(rule))
	}
	if a.cfg.needsSlowSampling(true) || a.cfg.needsSlowSampling(false) {
		status.SampleEverySeconds = a.cfg.SampleEvery.Seconds()
	}
	if a.ntfy != nil {
		status.Targets.Ntfy = &AlertTargetInfo{Host: alertTargetHost(a.ntfy.cfg.URL), Token: a.ntfy.cfg.Token != ""}
	}
	if a.webhook != nil {
		status.Targets.Webhook = &AlertTargetInfo{Host: alertTargetHost(a.webhook.cfg.URL), Token: a.webhook.cfg.Token != ""}
	}
	if a.webPush != nil {
		status.Targets.WebPush = &WebPushStatus{PublicKey: a.webPush.store.PublicKey(), Subscriptions: a.webPush.store.Count()}
	}
	a.mu.Lock()
	status.Alerts = append([]HostAlert{}, a.alerts...)
	if !a.lastTickAt.IsZero() {
		at := a.lastTickAt.UTC()
		status.UpdatedAt = &at
	}
	a.mu.Unlock()
	return status
}

var (
	errAlertNoTargets = errors.New("no notification target is configured")
	errAlertTestSoon  = errors.New("a test notification was sent less than 10 seconds ago")
	errAlertRateLimit = errors.New("the notification rate limit is reached; try again in a minute")
)

// SendTest queues a test notice to every target, at most one per
// alertTestGap.
func (a *alertService) SendTest() error {
	if !a.cfg.hasTargets() {
		return errAlertNoTargets
	}
	a.mu.Lock()
	now := a.now()
	if !a.lastTestAt.IsZero() && now.Sub(a.lastTestAt) < alertTestGap {
		a.mu.Unlock()
		return errAlertTestSoon
	}
	a.lastTestAt = now
	a.mu.Unlock()
	if !a.dispatcher.Enqueue(testNotification(a.host, now)) {
		return errAlertRateLimit
	}
	return nil
}

// ---- RuntimeState and routes ----

func (s *RuntimeState) SetAlerts(service *alertService) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.alerts = service
}

func (s *RuntimeState) Alerts() *alertService {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.alerts
}

type webPushSubscribeRequest struct {
	Endpoint       string   `json:"endpoint"`
	ExpirationTime *float64 `json:"expirationTime"`
	Keys           struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

type webPushUnsubscribeRequest struct {
	Endpoint string `json:"endpoint"`
}

// decodeSameOriginJSON applies /api/control's guards: same origin, a capped
// body, no unknown fields and a single JSON object.
func decodeSameOriginJSON(w http.ResponseWriter, r *http.Request, what string, into any) bool {
	if !sameOriginBrowserRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": what + " requires same-origin browser requests"})
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request body must be a JSON object with known fields"})
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request body must contain a single JSON object"})
		return false
	}
	return true
}

// registerAlertRoutes adds the host alert routes. They read the service from
// state at request time, since it starts after the handler is built. None of
// them is in peerRelayRoutes: a peer's alerts reach a dashboard on its
// metrics as host_alerts, and its notification settings stay its own.
func registerAlertRoutes(mux *http.ServeMux, state *RuntimeState) {
	mux.HandleFunc("GET /api/alerts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, state.Alerts().Status())
	})
	mux.HandleFunc("POST /api/alerts/test", func(w http.ResponseWriter, r *http.Request) {
		if !emptySameOriginPost(w, r, "a test notification") {
			return
		}
		service := state.Alerts()
		if service == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "host alerts are off"})
			return
		}
		switch err := service.SendTest(); {
		case err == nil:
			writeJSON(w, http.StatusAccepted, map[string]bool{"queued": true})
		case errors.Is(err, errAlertNoTargets):
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		default:
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": err.Error()})
		}
	})
	mux.HandleFunc("POST /api/alerts/push/subscribe", func(w http.ResponseWriter, r *http.Request) {
		var request webPushSubscribeRequest
		if !decodeSameOriginJSON(w, r, "a push subscription", &request) {
			return
		}
		service := state.Alerts()
		if service == nil || service.webPush == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "web push is off"})
			return
		}
		sub, err := parseWebPushSubscription(request.Endpoint, request.Keys.P256dh, request.Keys.Auth, service.webPush.cfg.AllowHosts)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if device, ok := pairedDeviceFrom(r.Context()); ok {
			sub.DeviceID = device.ID
		}
		sub.Created = time.Now().UTC()
		count, err := service.webPush.store.Add(sub)
		if err != nil {
			service.logf("alerts: saving web push subscriptions: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "the subscription could not be saved"})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"subscribed": true, "subscriptions": count})
	})
	mux.HandleFunc("POST /api/alerts/push/unsubscribe", func(w http.ResponseWriter, r *http.Request) {
		var request webPushUnsubscribeRequest
		if !decodeSameOriginJSON(w, r, "a push subscription", &request) {
			return
		}
		service := state.Alerts()
		if service == nil || service.webPush == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "web push is off"})
			return
		}
		removed, err := service.webPush.store.RemoveEndpoint(request.Endpoint)
		if err != nil {
			service.logf("alerts: saving web push subscriptions: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "the subscription could not be removed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"removed": removed, "subscriptions": service.webPush.store.Count()})
	})
}

// alertPushDir is where Web Push keeps its key and subscriptions: push/
// beside the settings file, or memory only without one.
func alertPushDir(settingsPath string) string {
	if strings.TrimSpace(settingsPath) == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(settingsPath), "push")
}

// AlertStatus is the update check's status for the update rule, nil while
// the check is off.
func (c *UpdateChecker) AlertStatus() *UpdateStatus {
	if c == nil || !c.Enabled() {
		return nil
	}
	status := c.Status()
	return &status
}

// pairedDeviceExists reports whether a Web Push subscription's device is
// still paired. Without pairing every subscription stands.
func (s *RuntimeState) pairedDeviceExists(id string) bool {
	store := s.Pairing()
	if store == nil {
		return true
	}
	for _, device := range store.Devices() {
		if device.ID == id {
			return true
		}
	}
	return false
}

// alertTargetNames lists the configured targets for the startup log, by
// kind and host only.
func alertTargetNames(cfg alertsConfig) string {
	var names []string
	if cfg.Ntfy != nil {
		names = append(names, "ntfy ("+alertTargetHost(cfg.Ntfy.URL)+")")
	}
	if cfg.Webhook != nil {
		names = append(names, "webhook ("+alertTargetHost(cfg.Webhook.URL)+")")
	}
	if cfg.WebPush != nil {
		names = append(names, "web push")
	}
	if len(names) == 0 {
		return "no target (dashboard only)"
	}
	return strings.Join(names, ", ")
}
