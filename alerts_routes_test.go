package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func newAlertRoutesHandler(t *testing.T, cfg *alertsConfig, store *pairingStore) (http.Handler, *alertService) {
	t.Helper()
	state := NewMemoryRuntimeState()
	if store != nil {
		state.SetPairing(store)
	}
	var service *alertService
	if cfg != nil {
		var err error
		service, err = newAlertService(alertServiceOptions{Config: *cfg, Host: "test-host", Version: "0.0.1", DeviceExists: state.pairedDeviceExists})
		if err != nil {
			t.Fatal(err)
		}
		service.now = func() time.Time { return alertT0 }
		state.SetAlerts(service)
	}
	handler, err := newHTTPHandlerWithState(fakeCollector{}, testStaticFS(), state)
	if err != nil {
		t.Fatal(err)
	}
	return handler, service
}

func webPushOnlyConfig() *alertsConfig {
	return &alertsConfig{WebPush: &webPushConfig{Subject: defaultWebPushSubject, AllowHosts: defaultWebPushHosts}}
}

func subscribeBody(t *testing.T, endpoint string) string {
	t.Helper()
	key, auth := newTestSubscriber(t)
	return `{"endpoint":"` + endpoint + `","expirationTime":null,"keys":{"p256dh":"` + b64url(key.PublicKey().Bytes()) + `","auth":"` + b64url(auth) + `"}}`
}

func TestAlertRoutesWhenAlertsAreOff(t *testing.T) {
	handler, _ := newAlertRoutesHandler(t, nil, nil)
	rec := serve(handler, networkRequest(http.MethodGet, "/api/alerts", ""))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Fatalf("GET /api/alerts = %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if rec := serve(handler, networkRequest(http.MethodPost, "/api/alerts/test", "")); rec.Code != http.StatusNotFound {
		t.Fatalf("test while off = %d", rec.Code)
	}
	if rec := serve(handler, networkRequest(http.MethodPost, "/api/alerts/push/subscribe", subscribeBody(t, "https://fcm.googleapis.com/fcm/send/x"))); rec.Code != http.StatusNotFound {
		t.Fatalf("subscribe while off = %d", rec.Code)
	}
}

func TestAlertRoutesGuardWrites(t *testing.T) {
	handler, _ := newAlertRoutesHandler(t, webPushOnlyConfig(), nil)
	body := subscribeBody(t, "https://fcm.googleapis.com/fcm/send/x")
	for _, path := range []string{"/api/alerts/test", "/api/alerts/push/subscribe", "/api/alerts/push/unsubscribe"} {
		req := networkRequest(http.MethodPost, path, body)
		req.Header.Del("Origin")
		if rec := serve(handler, req); rec.Code != http.StatusForbidden {
			t.Errorf("%s without Origin = %d", path, rec.Code)
		}
		req = networkRequest(http.MethodPost, path, body)
		req.Header.Set("Origin", "https://evil.example")
		if rec := serve(handler, req); rec.Code != http.StatusForbidden {
			t.Errorf("%s cross-site = %d", path, rec.Code)
		}
	}
	bad := map[string]string{
		"unknown field": `{"endpoint":"https://fcm.googleapis.com/fcm/send/x","keys":{"p256dh":"a","auth":"b"},"extra":1}`,
		"two objects":   body + body,
		"not json":      `endpoint=x`,
		"too large":     `{"endpoint":"` + strings.Repeat("a", 9<<10) + `"}`,
		"bad endpoint":  subscribeBody(t, "https://evil.example/x"),
	}
	for name, b := range bad {
		if rec := serve(handler, networkRequest(http.MethodPost, "/api/alerts/push/subscribe", b)); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if rec := serve(handler, networkRequest(http.MethodPost, "/api/alerts/test", `{"x":1}`)); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("test with a body = %d", rec.Code)
	}
}

func TestAlertRoutesSubscribeAndStatusLeakNothing(t *testing.T) {
	handler, service := newAlertRoutesHandler(t, webPushOnlyConfig(), nil)
	endpoint := "https://fcm.googleapis.com/fcm/send/secret-endpoint-id"
	body := subscribeBody(t, endpoint)
	rec := serve(handler, networkRequest(http.MethodPost, "/api/alerts/push/subscribe", body))
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"subscriptions":1`) {
		t.Fatalf("subscribe = %d %s", rec.Code, rec.Body.String())
	}
	var sub struct {
		Keys struct{ Auth string } `json:"keys"`
	}
	_ = json.Unmarshal([]byte(body), &sub)

	rec = serve(handler, networkRequest(http.MethodGet, "/api/alerts", ""))
	var status AlertsStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.Enabled || status.Targets.WebPush == nil || status.Targets.WebPush.Subscriptions != 1 || status.Targets.WebPush.PublicKey != service.webPush.store.PublicKey() {
		t.Fatalf("status = %s", rec.Body.String())
	}
	for _, secret := range []string{"secret-endpoint-id", sub.Keys.Auth, "fcm.googleapis.com"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("GET /api/alerts leaks %q: %s", secret, rec.Body.String())
		}
	}

	rec = serve(handler, networkRequest(http.MethodPost, "/api/alerts/push/unsubscribe", `{"endpoint":"`+endpoint+`"}`))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"removed":true`) || !strings.Contains(rec.Body.String(), `"subscriptions":0`) {
		t.Fatalf("unsubscribe = %d %s", rec.Code, rec.Body.String())
	}
}

func TestAlertRoutesTestNotification(t *testing.T) {
	handler, _ := newAlertRoutesHandler(t, &alertsConfig{Rules: []alertRule{cpuRule()}}, nil)
	if rec := serve(handler, networkRequest(http.MethodPost, "/api/alerts/test", "")); rec.Code != http.StatusConflict {
		t.Fatalf("test without targets = %d %s", rec.Code, rec.Body.String())
	}

	handler, service := newAlertRoutesHandler(t, webPushOnlyConfig(), nil)
	if rec := serve(handler, networkRequest(http.MethodPost, "/api/alerts/test", "")); rec.Code != http.StatusAccepted {
		t.Fatalf("test = %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(handler, networkRequest(http.MethodPost, "/api/alerts/test", "")); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second test inside the gap = %d", rec.Code)
	}
	service.now = func() time.Time { return alertT0.Add(alertTestGap) }
	if rec := serve(handler, networkRequest(http.MethodPost, "/api/alerts/test", "")); rec.Code != http.StatusAccepted {
		t.Fatalf("test after the gap = %d %s", rec.Code, rec.Body.String())
	}
}

func TestAlertRoutesNeedPairingAndRecordTheDevice(t *testing.T) {
	store, clock := newTestPairingStore(t, "")
	handler, service := newAlertRoutesHandler(t, webPushOnlyConfig(), store)
	body := subscribeBody(t, "https://fcm.googleapis.com/fcm/send/paired")

	for _, req := range []*http.Request{
		networkRequest(http.MethodGet, "/api/alerts", ""),
		networkRequest(http.MethodPost, "/api/alerts/test", ""),
		networkRequest(http.MethodPost, "/api/alerts/push/subscribe", body),
	} {
		if rec := serve(handler, req); rec.Code != http.StatusUnauthorized {
			t.Fatalf("unpaired %s %s = %d", req.Method, req.URL.Path, rec.Code)
		}
	}

	cookie, paired := pairDevice(t, handler, store, clock, "")
	req := networkRequest(http.MethodPost, "/api/alerts/push/subscribe", body)
	req.AddCookie(cookie)
	if rec := serve(handler, req); rec.Code != http.StatusCreated {
		t.Fatalf("paired subscribe = %d %s", rec.Code, rec.Body.String())
	}
	subs := service.webPush.store.List()
	if len(subs) != 1 || subs[0].DeviceID != paired.Device.ID || paired.Device.ID == "" {
		t.Fatalf("subscriptions = %+v, device %q", subs, paired.Device.ID)
	}

	// Revoking the device drops its subscription at the next send.
	if _, err := store.Revoke(paired.Device.ID); err != nil {
		t.Fatal(err)
	}
	service.webPush.client = nil // nothing may be sent
	if err := service.webPush.Deliver(context.Background(), firingNotification(), func(ctx context.Context, f func(context.Context) error) error {
		t.Fatal("a revoked device's subscription was sent to")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n := service.webPush.store.Count(); n != 0 {
		t.Fatalf("subscriptions after revoke = %d", n)
	}
}
