package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const pairingTestOrigin = "https://sysmon.example:9099"

type pairingClock struct{ t time.Time }

func (c *pairingClock) now() time.Time      { return c.t }
func (c *pairingClock) add(d time.Duration) { c.t = c.t.Add(d) }

func newTestPairingStore(t *testing.T, dir string) (*pairingStore, *pairingClock) {
	t.Helper()
	store, err := newPairingStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	clock := &pairingClock{t: time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)}
	store.now = clock.now
	// The store stamped its first read with the real clock. Restamp it with
	// this one, or every run after the fixed date compares the two clocks.
	store.lastStat = clock.now()
	return store, clock
}

func newPairingTestHandler(t *testing.T, store *pairingStore) (http.Handler, *fakeController) {
	t.Helper()
	state := NewMemoryRuntimeState()
	state.SetPairing(store)
	controller := &fakeController{}
	handler, err := newHTTPHandlerWithController(fakeCollector{}, testStaticFS(), state, controller)
	if err != nil {
		t.Fatal(err)
	}
	return handler, controller
}

// networkRequest is a request from another device over HTTPS.
func networkRequest(method, path, body string) *http.Request {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, pairingTestOrigin+path, reader)
	req.RemoteAddr = "192.0.2.40:51000"
	req.TLS = &tls.ConnectionState{}
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("Origin", pairingTestOrigin)
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func serve(handler http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func pairDevice(t *testing.T, handler http.Handler, store *pairingStore, clock *pairingClock, kind string) (*http.Cookie, pairResponse) {
	t.Helper()
	pin, _, err := store.OpenWindow()
	if err != nil {
		t.Fatal(err)
	}
	clock.add(2 * time.Second)
	body := `{"pin":"` + formatPIN(pin) + `","name":"Test phone"`
	if kind != "" {
		body += `,"kind":"` + kind + `"`
	}
	rec := serve(handler, networkRequest(http.MethodPost, "/api/pair", body+"}"))
	if rec.Code != http.StatusOK {
		t.Fatalf("pair: %d %s", rec.Code, rec.Body.String())
	}
	var response pairResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == pairingCookieName {
			return &http.Cookie{Name: c.Name, Value: c.Value}, response
		}
	}
	return nil, response
}

func TestPairingRefusesUnpairedNetworkClients(t *testing.T) {
	store, _ := newTestPairingStore(t, "")
	handler, controller := newPairingTestHandler(t, store)

	for _, path := range []string{"/readyz", "/api/metrics", "/api/status", "/api/settings", "/api/quota", "/api/codex-usage",
		"/api/hosts", "/api/hosts/abc/metrics", "/api/client-checks", "/api/pair/devices", "/api/../api/metrics", "//api/metrics", "/api"} {
		rec := serve(handler, networkRequest(http.MethodGet, path, ""))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s = %d, want 401", path, rec.Code)
		}
		if path == "/api/metrics" && rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("401 without WWW-Authenticate")
		}
	}
	for path, body := range map[string]string{
		"/api/control":      `{"action":"lock_screen"}`,
		"/api/settings":     `{"dim":true}`,
		"/api/update":       "",
		"/api/client-check": `{}`,
		"/api/pair/window":  "",
		"/api/pair/logout":  "",
	} {
		if rec := serve(handler, networkRequest(http.MethodPost, path, body)); rec.Code != http.StatusUnauthorized {
			t.Errorf("POST %s = %d, want 401", path, rec.Code)
		}
	}
	if controller.calls != 0 {
		t.Fatalf("an unpaired request reached the controller")
	}
	for _, path := range []string{"/healthz", "/", "/api/pair/state"} {
		if rec := serve(handler, networkRequest(http.MethodGet, path, "")); rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 for an unpaired client", path, rec.Code)
		}
	}
	// The 401 still carries the security headers.
	rec := serve(handler, networkRequest(http.MethodGet, "/api/metrics", ""))
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("401 lost the security headers")
	}
}

func TestPairingLoopbackReadsPassButNotWritesOrProxies(t *testing.T) {
	store, _ := newTestPairingStore(t, "")
	handler, _ := newPairingTestHandler(t, store)

	local := func(method, host, path string) *http.Request {
		req := httptest.NewRequest(method, "http://"+host+path, strings.NewReader(""))
		req.RemoteAddr = "127.0.0.1:50000"
		if method == http.MethodPost {
			req.Header.Set("Origin", "http://"+host)
		}
		return req
	}
	for _, host := range []string{"127.0.0.1:9099", "localhost:9099", "[::1]:9099"} {
		req := local(http.MethodGet, host, "/api/status")
		if host == "[::1]:9099" {
			req.RemoteAddr = "[::1]:50000"
		}
		if rec := serve(handler, req); rec.Code != http.StatusOK {
			t.Errorf("loopback GET via %s = %d, want 200", host, rec.Code)
		}
	}
	if rec := serve(handler, local(http.MethodPost, "127.0.0.1:9099", "/api/settings")); rec.Code != http.StatusUnauthorized {
		t.Errorf("loopback POST /api/settings = %d, want 401", rec.Code)
	}
	for name, value := range map[string]string{
		"X-Forwarded-For":      "100.64.0.9",
		"X-Forwarded-Proto":    "https",
		"Forwarded":            "for=100.64.0.9",
		"X-Real-IP":            "100.64.0.9",
		"Via":                  "1.1 proxy",
		"Tailscale-User-Login": "someone@example.com",
	} {
		req := local(http.MethodGet, "127.0.0.1:9099", "/api/status")
		req.Header.Set(name, value)
		if rec := serve(handler, req); rec.Code != http.StatusUnauthorized {
			t.Errorf("loopback GET with %s = %d, want 401 (treated as proxied)", name, rec.Code)
		}
	}
	// A loopback connection addressed to a public name came through a proxy.
	if rec := serve(handler, local(http.MethodGet, "sysmon.example:9443", "/api/status")); rec.Code != http.StatusUnauthorized {
		t.Errorf("loopback GET with a non-loopback Host = %d, want 401", rec.Code)
	}
}

func TestPairingFlowSetsCookieAndAuthorizes(t *testing.T) {
	store, clock := newTestPairingStore(t, "")
	handler, controller := newPairingTestHandler(t, store)

	cookie, response := pairDevice(t, handler, store, clock, "")
	if cookie == nil {
		t.Fatalf("no cookie after pairing")
	}
	if response.Token != "" {
		t.Fatalf("a browser pairing returned the token in the body")
	}
	if response.Device.Name != "Test phone" || response.Device.Kind != pairedKindBrowser {
		t.Fatalf("device = %+v", response.Device)
	}

	for _, path := range []string{"/api/metrics", "/api/status", "/api/pair/devices"} {
		req := networkRequest(http.MethodGet, path, "")
		req.AddCookie(cookie)
		if rec := serve(handler, req); rec.Code != http.StatusOK {
			t.Errorf("paired GET %s = %d", path, rec.Code)
		}
	}
	// /readyz judges the fake collector's old timestamp; it only has to get past pairing.
	req := networkRequest(http.MethodGet, "/readyz", "")
	req.AddCookie(cookie)
	if rec := serve(handler, req); rec.Code == http.StatusUnauthorized {
		t.Errorf("paired GET /readyz = 401")
	}
	req = networkRequest(http.MethodPost, "/api/control", `{"action":"lock_screen"}`)
	req.AddCookie(cookie)
	if rec := serve(handler, req); rec.Code != http.StatusOK || controller.calls != 1 {
		t.Fatalf("paired POST /api/control = %d, calls %d", rec.Code, controller.calls)
	}
	// The same-origin check still applies to a paired cookie (CSRF).
	req = networkRequest(http.MethodPost, "/api/control", `{"action":"lock_screen"}`)
	req.Header.Set("Origin", "https://evil.example")
	req.AddCookie(cookie)
	if rec := serve(handler, req); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST with a cookie = %d, want 403", rec.Code)
	}
	// The PIN worked once.
	if store.WindowOpen() {
		t.Fatalf("window still open after a successful pairing")
	}
}

func TestPairingCookieFlags(t *testing.T) {
	store, clock := newTestPairingStore(t, "")
	handler, _ := newPairingTestHandler(t, store)

	pin, _, _ := store.OpenWindow()
	clock.add(2 * time.Second)
	rec := serve(handler, networkRequest(http.MethodPost, "/api/pair", `{"pin":"`+pin+`"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("pair = %d %s", rec.Code, rec.Body.String())
	}
	header := rec.Header().Get("Set-Cookie")
	for _, want := range []string{pairingCookieName + "=", "HttpOnly", "Secure", "SameSite=Strict", "Path=/", "Max-Age=34560000"} {
		if !strings.Contains(header, want) {
			t.Errorf("Set-Cookie %q missing %q", header, want)
		}
	}

	// Through a loopback HTTPS proxy (Tailscale Serve) the cookie is Secure too.
	pin, _, _ = store.OpenWindow()
	clock.add(2 * time.Second)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9099/api/pair", strings.NewReader(`{"pin":"`+pin+`"}`))
	req.Host = "sysmon.example.ts.net"
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("X-Forwarded-For", "100.64.0.9")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "sysmon.example.ts.net")
	req.Header.Set("Origin", "https://sysmon.example.ts.net")
	rec = serve(handler, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Set-Cookie"), "Secure") {
		t.Fatalf("proxied HTTPS pair = %d, Set-Cookie %q", rec.Code, rec.Header().Get("Set-Cookie"))
	}
}

func TestPairingRefusedOverPlainNetworkHTTP(t *testing.T) {
	store, clock := newTestPairingStore(t, "")
	handler, _ := newPairingTestHandler(t, store)
	pin, _, _ := store.OpenWindow()
	clock.add(2 * time.Second)

	req := networkRequest(http.MethodPost, "/api/pair", `{"pin":"`+pin+`"}`)
	req.TLS = nil
	req.Header.Set("Origin", "http://sysmon.example:9099")
	req.URL.Scheme = "http"
	if rec := serve(handler, req); rec.Code != http.StatusForbidden {
		t.Fatalf("plain network pair = %d, want 403", rec.Code)
	}
	// A plain-HTTP proxy is no better.
	req = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9099/api/pair", strings.NewReader(`{"pin":"`+pin+`"}`))
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("X-Forwarded-For", "192.0.2.40")
	req.Header.Set("X-Forwarded-Proto", "http")
	req.Header.Set("Origin", "http://127.0.0.1:9099")
	if rec := serve(handler, req); rec.Code != http.StatusForbidden {
		t.Fatalf("plain proxied pair = %d, want 403", rec.Code)
	}
	if !store.WindowOpen() {
		t.Fatalf("a refused request used up the window")
	}
	// Loopback direct is allowed: the traffic never leaves the machine.
	req = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9099/api/pair", strings.NewReader(`{"pin":"`+pin+`"}`))
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("Origin", "http://127.0.0.1:9099")
	if rec := serve(handler, req); rec.Code != http.StatusOK {
		t.Fatalf("loopback pair = %d %s", rec.Code, rec.Body.String())
	}
}

func TestPairingWrongPINsCloseTheWindow(t *testing.T) {
	store, clock := newTestPairingStore(t, "")
	handler, _ := newPairingTestHandler(t, store)
	pin, _, _ := store.OpenWindow()
	wrong := "000000"
	if pin == wrong {
		wrong = "111111"
	}
	for i := 1; i <= pairingMaxFailures; i++ {
		clock.add(2 * time.Second)
		rec := serve(handler, networkRequest(http.MethodPost, "/api/pair", `{"pin":"`+wrong+`"}`))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("wrong PIN %d = %d", i, rec.Code)
		}
		var body struct{ Remaining int }
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body.Remaining != pairingMaxFailures-i {
			t.Fatalf("wrong PIN %d remaining = %d", i, body.Remaining)
		}
	}
	clock.add(2 * time.Second)
	if rec := serve(handler, networkRequest(http.MethodPost, "/api/pair", `{"pin":"`+pin+`"}`)); rec.Code != http.StatusConflict {
		t.Fatalf("right PIN after the window closed = %d, want 409", rec.Code)
	}
}

func TestPairingRateLimitAndExpiry(t *testing.T) {
	store, clock := newTestPairingStore(t, "")
	handler, _ := newPairingTestHandler(t, store)
	pin, _, _ := store.OpenWindow()

	clock.add(2 * time.Second)
	_ = serve(handler, networkRequest(http.MethodPost, "/api/pair", `{"pin":"x"}`))
	rec := serve(handler, networkRequest(http.MethodPost, "/api/pair", `{"pin":"`+pin+`"}`))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("second attempt in the same second = %d", rec.Code)
	}
	clock.add(pairingWindowTTL)
	if rec := serve(handler, networkRequest(http.MethodPost, "/api/pair", `{"pin":"`+pin+`"}`)); rec.Code != http.StatusConflict {
		t.Fatalf("expired window = %d, want 409", rec.Code)
	}
}

func TestPairingTokenKindReturnsBearerToken(t *testing.T) {
	store, clock := newTestPairingStore(t, "")
	handler, _ := newPairingTestHandler(t, store)
	cookie, response := pairDevice(t, handler, store, clock, pairedKindToken)
	if cookie != nil {
		t.Fatalf("a token pairing set a cookie")
	}
	if response.Token == "" || response.Device.Kind != pairedKindToken {
		t.Fatalf("token pairing response = %+v", response)
	}
	req := networkRequest(http.MethodGet, "/api/metrics", "")
	req.Header.Set("Authorization", "Bearer "+response.Token)
	if rec := serve(handler, req); rec.Code != http.StatusOK {
		t.Fatalf("bearer GET = %d", rec.Code)
	}
	req = networkRequest(http.MethodGet, "/api/metrics", "")
	req.Header.Set("Authorization", "Bearer "+response.Token[:len(response.Token)-1]+"x")
	if rec := serve(handler, req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("altered bearer GET = %d", rec.Code)
	}
}

func TestPairingStateEndpoint(t *testing.T) {
	store, clock := newTestPairingStore(t, "")
	handler, _ := newPairingTestHandler(t, store)

	read := func(cookie *http.Cookie) (PairingState, *httptest.ResponseRecorder) {
		req := networkRequest(http.MethodGet, "/api/pair/state", "")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := serve(handler, req)
		var state PairingState
		if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
			t.Fatal(err)
		}
		return state, rec
	}
	state, _ := read(nil)
	if state.Pairing != pairingModeRequired || state.Paired || state.WindowOpen || !state.Secure || state.DashboardBuild != dashboardBuild {
		t.Fatalf("unpaired state = %+v", state)
	}
	cookie, _ := pairDevice(t, handler, store, clock, "")
	state, rec := read(cookie)
	if !state.Paired || state.Device == nil || state.Device.Name != "Test phone" {
		t.Fatalf("paired state = %+v", state)
	}
	if !strings.Contains(rec.Header().Get("Set-Cookie"), "Max-Age=34560000") {
		t.Fatalf("state did not extend the cookie: %q", rec.Header().Get("Set-Cookie"))
	}
	_, rec = read(&http.Cookie{Name: pairingCookieName, Value: "0123456789abcdef.notavalidsecretnotavalidsecret00"})
	if !strings.Contains(rec.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("a stale cookie was not cleared: %q", rec.Header().Get("Set-Cookie"))
	}
}

func TestPairingWindowRevokeAndLogoutRoutes(t *testing.T) {
	store, clock := newTestPairingStore(t, "")
	handler, _ := newPairingTestHandler(t, store)
	first, _ := pairDevice(t, handler, store, clock, "")

	req := networkRequest(http.MethodPost, "/api/pair/window", "")
	req.AddCookie(first)
	rec := serve(handler, req)
	var window pairingWindowResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &window) != nil || len(window.PIN) != 6 {
		t.Fatalf("POST /api/pair/window = %d %s", rec.Code, rec.Body.String())
	}
	clock.add(2 * time.Second)
	rec = serve(handler, networkRequest(http.MethodPost, "/api/pair", `{"pin":"`+window.PINDisplay+`","name":"Second"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("pairing with the dashboard's PIN = %d %s", rec.Code, rec.Body.String())
	}
	var second pairResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &second)
	secondCookie := rec.Result().Cookies()[0]

	req = networkRequest(http.MethodGet, "/api/pair/devices", "")
	req.AddCookie(first)
	rec = serve(handler, req)
	var list struct{ Devices []PairedDeviceInfo }
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Devices) != 2 {
		t.Fatalf("devices = %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "token") {
		t.Fatalf("device list leaks token data: %s", rec.Body.String())
	}
	current := 0
	for _, d := range list.Devices {
		if d.Current {
			current++
		}
	}
	if current != 1 {
		t.Fatalf("want exactly one current device, got %d", current)
	}

	// The first device revokes the second, whose cookie then fails.
	req = networkRequest(http.MethodDelete, "/api/pair/devices/"+second.Device.ID, "")
	req.AddCookie(first)
	if rec := serve(handler, req); rec.Code != http.StatusOK {
		t.Fatalf("DELETE = %d %s", rec.Code, rec.Body.String())
	}
	req = networkRequest(http.MethodGet, "/api/metrics", "")
	req.AddCookie(&http.Cookie{Name: secondCookie.Name, Value: secondCookie.Value})
	if rec := serve(handler, req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked cookie = %d, want 401", rec.Code)
	}

	req = networkRequest(http.MethodPost, "/api/pair/logout", "")
	req.AddCookie(first)
	rec = serve(handler, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("logout = %d, Set-Cookie %q", rec.Code, rec.Header().Get("Set-Cookie"))
	}
	if len(store.Devices()) != 0 {
		t.Fatalf("devices left after logout: %+v", store.Devices())
	}
}

func TestPairingRevokeEndsAnOpenStream(t *testing.T) {
	store, _ := newTestPairingStore(t, "")
	state := NewMemoryRuntimeState()
	state.SetPairing(store)
	token, device, err := store.AddDevice("phone", pairedKindBrowser)
	if err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{})
	started := make(chan struct{})
	handler := requirePairing(state, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(ended)
	}))
	go func() {
		req := networkRequest(http.MethodGet, "/api/stream", "")
		req.Header.Set("Authorization", "Bearer "+token)
		serve(handler, req)
	}()
	<-started
	if _, err := store.Revoke(device.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatalf("the stream outlived its device")
	}
}

func TestPairingOffLeavesEverythingOpen(t *testing.T) {
	handler, err := newHTTPHandler(fakeCollector{}, testStaticFS())
	if err != nil {
		t.Fatal(err)
	}
	if rec := serve(handler, networkRequest(http.MethodGet, "/api/metrics", "")); rec.Code != http.StatusOK {
		t.Fatalf("GET /api/metrics with pairing off = %d", rec.Code)
	}
	rec := serve(handler, networkRequest(http.MethodGet, "/api/pair/state", ""))
	var state PairingState
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil || state.Pairing != pairingModeOff || !state.Paired {
		t.Fatalf("pair state with pairing off = %s", rec.Body.String())
	}
	if rec := serve(handler, networkRequest(http.MethodPost, "/api/pair", `{"pin":"123456"}`)); rec.Code != http.StatusNotFound {
		t.Fatalf("POST /api/pair with pairing off = %d, want 404", rec.Code)
	}
}

func TestPairingStoreWritesHashesOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pairing")
	store, clock := newTestPairingStore(t, dir)
	handler, _ := newPairingTestHandler(t, store)
	_, response := pairDevice(t, handler, store, clock, pairedKindToken)

	data, err := os.ReadFile(filepath.Join(dir, pairingDevicesFile))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(response.Token)) {
		t.Fatalf("devices.json holds the token itself")
	}
	if !bytes.Contains(data, []byte(hashPairingToken(response.Token))) {
		t.Fatalf("devices.json lacks the token hash: %s", data)
	}
	if _, err := os.Stat(filepath.Join(dir, pairingWindowFile)); !os.IsNotExist(err) {
		t.Fatalf("window.json survived a successful pairing: %v", err)
	}
	if runtime.GOOS != "windows" {
		for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, pairingDevicesFile): 0o600} {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != want {
				t.Errorf("%s mode = %o, want %o", path, got, want)
			}
		}
	}
	// A restart keeps the device.
	reopened, _ := newTestPairingStore(t, dir)
	if _, _, ok := reopened.Authenticate(response.Token); !ok {
		t.Fatalf("token stopped working after reopening the store")
	}
}

// now() is wall time, so a host clock stepped back (NTP correcting a fast
// clock at boot) gives negative spans. Neither the reread of devices.json nor
// the PIN attempt gap may wait for the clock to catch up: the first would hold
// off a revoke from the command line, the second would refuse every PIN.
func TestPairingStoreSurvivesAClockSteppedBack(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pairing")
	agent, agentClock := newTestPairingStore(t, dir)
	cli, _ := newTestPairingStore(t, dir)

	agentClock.add(-time.Hour)
	token, device, err := cli.AddDevice("script", pairedKindToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := agent.Authenticate(token); !ok {
		t.Fatalf("the agent did not pick up a token minted after its clock stepped back")
	}
	if _, err := cli.Revoke(device.ID); err != nil {
		t.Fatal(err)
	}
	agentClock.add(-time.Hour)
	if _, _, ok := agent.Authenticate(token); ok {
		t.Fatalf("a token revoked after the clock stepped back still works in the agent")
	}

	pin, _, err := agent.OpenWindow()
	if err != nil {
		t.Fatal(err)
	}
	wrong := "000000"
	if pin == wrong {
		wrong = "111111"
	}
	if _, _, _, err := agent.Pair(wrong, "phone", pairedKindBrowser); err != errPairingWrongPIN {
		t.Fatalf("wrong PIN: %v, want %v", err, errPairingWrongPIN)
	}
	agentClock.add(-time.Hour)
	if _, _, _, err := agent.Pair(pin, "phone", pairedKindBrowser); err != nil {
		t.Fatalf("the right PIN after the clock stepped back: %v", err)
	}
}

func TestPairingStorePicksUpCommandLineChanges(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pairing")
	agent, agentClock := newTestPairingStore(t, dir)
	cli, _ := newTestPairingStore(t, dir)

	token, device, err := cli.AddDevice("script", pairedKindToken)
	if err != nil {
		t.Fatal(err)
	}
	agentClock.add(pairingReloadEvery)
	_, gone, ok := agent.Authenticate(token)
	if !ok {
		t.Fatalf("the agent did not pick up a token minted from the command line")
	}
	agentClock.add(time.Hour) // marks last-seen dirty on the next use
	agent.Authenticate(token)

	if _, err := cli.Revoke(device.ID); err != nil {
		t.Fatal(err)
	}
	// Flushing the agent's last-seen times must not bring the device back.
	if err := agent.Flush(); err != nil {
		t.Fatal(err)
	}
	agentClock.add(pairingReloadEvery)
	if _, _, ok := agent.Authenticate(token); ok {
		t.Fatalf("a revoked token still works in the agent")
	}
	select {
	case <-gone:
	default:
		t.Fatalf("revoking did not signal the device's open streams")
	}
	if doc, _ := readPairingDevices(filepath.Join(dir, pairingDevicesFile)); len(doc.Devices) != 0 {
		t.Fatalf("revoked device back on disk: %+v", doc.Devices)
	}
}

func TestPairingCommands(t *testing.T) {
	settings := filepath.Join(t.TempDir(), "settings.json")
	listen := listenConfig{bind: "127.0.0.1", port: 9099}
	var out bytes.Buffer

	if err := runPairingCommand(pairingCommand{open: true}, "", listen, &out); err == nil {
		t.Fatalf("-pair without -settings should fail")
	}
	if err := runPairingCommand(pairingCommand{open: true}, settings, listen, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Pairing PIN:") {
		t.Fatalf("-pair output: %s", out.String())
	}
	store, err := newPairingStore(pairingDirFor(settings))
	if err != nil {
		t.Fatal(err)
	}
	if !store.WindowOpen() {
		t.Fatalf("-pair did not open a window the agent can see")
	}

	out.Reset()
	if err := runPairingCommand(pairingCommand{token: "backup script"}, settings, listen, &out); err != nil {
		t.Fatal(err)
	}
	var token string
	for _, line := range strings.Split(out.String(), "\n") {
		if _, _, ok := splitPairingToken(strings.TrimSpace(line)); ok {
			token = strings.TrimSpace(line)
		}
	}
	// A running agent rereads within pairingReloadEvery; a fresh store reads now.
	if store, err = newPairingStore(pairingDirFor(settings)); err != nil {
		t.Fatal(err)
	}
	info, _, ok := store.Authenticate(token)
	if !ok || info.Name != "backup script" || info.Kind != pairedKindToken {
		t.Fatalf("-pair-token printed %q, which does not authenticate (%+v)", token, info)
	}
	if strings.Contains(out.String(), "SHA-256") {
		t.Fatalf("-pair-token printed a fingerprint without network TLS: %s", out.String())
	}

	// With network TLS, a token's client also needs the certificate to pin.
	cert, err := loadNetworkCertificate("", "", pairingDirFor(settings), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	tlsListen := listen
	tlsListen.bind, tlsListen.networkTLS = "0.0.0.0", true
	out.Reset()
	if err := runPairingCommand(pairingCommand{token: "osd"}, settings, tlsListen, &out); err != nil {
		t.Fatal(err)
	}
	if fingerprint := certificateFingerprint(cert); !strings.Contains(out.String(), fingerprint) {
		t.Fatalf("-pair-token with network TLS did not print %s: %s", fingerprint, out.String())
	}

	out.Reset()
	if err := runPairingCommand(pairingCommand{list: true}, settings, listen, &out); err != nil || !strings.Contains(out.String(), info.ID) {
		t.Fatalf("-pair-list: %v %s", err, out.String())
	}
	out.Reset()
	if err := runPairingCommand(pairingCommand{revoke: info.ID}, settings, listen, &out); err != nil {
		t.Fatal(err)
	}
	if err := runPairingCommand(pairingCommand{revoke: info.ID}, settings, listen, &out); err == nil {
		t.Fatalf("revoking twice should fail")
	}
}

func TestParsePairingAndNetworkTLSModes(t *testing.T) {
	for in, want := range map[string]string{"": "required", "required": "required", "ON": "required", "off": "off", "false": "off"} {
		if got, err := parsePairingMode(in); err != nil || got != want {
			t.Errorf("parsePairingMode(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := parsePairingMode("maybe"); err == nil {
		t.Errorf("parsePairingMode accepted junk")
	}
	cases := []struct {
		mode, pairing, bind string
		tls                 bool
		want                bool
	}{
		{"auto", "required", "0.0.0.0", false, true},
		{"auto", "required", "127.0.0.1", false, false},
		{"auto", "off", "0.0.0.0", false, false},
		{"auto", "required", "0.0.0.0", true, false},
		{"on", "off", "0.0.0.0", false, true},
		{"off", "required", "0.0.0.0", false, false},
	}
	for _, c := range cases {
		if got := resolveNetworkTLS(c.mode, c.pairing, c.tls, c.bind); got != c.want {
			t.Errorf("resolveNetworkTLS(%+v) = %v", c, got)
		}
	}
}

func TestAgentCertificateIsReusedAndRenewed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pairing")
	now := time.Now()
	first, err := loadNetworkCertificate("", "", dir, now)
	if err != nil {
		t.Fatal(err)
	}
	again, err := loadNetworkCertificate("", "", dir, now.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if certificateFingerprint(first) != certificateFingerprint(again) {
		t.Fatalf("certificate was not reused")
	}
	if readAgentCertificateFingerprint(dir) != certificateFingerprint(first) {
		t.Fatalf("fingerprint on disk does not match")
	}
	leaf, err := x509.ParseCertificate(first.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if leaf.NotAfter.Sub(leaf.NotBefore) > agentCertLifetime+2*time.Hour {
		t.Fatalf("validity %v exceeds Apple's 825 days", leaf.NotAfter.Sub(leaf.NotBefore))
	}
	if err := leaf.VerifyHostname("localhost"); err != nil {
		t.Fatal(err)
	}
	if err := leaf.VerifyHostname("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Fatalf("ext key usage = %v", leaf.ExtKeyUsage)
	}
	renewed, err := loadNetworkCertificate("", "", dir, leaf.NotAfter.Add(-agentCertRenewAt+time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if certificateFingerprint(renewed) == certificateFingerprint(first) {
		t.Fatalf("certificate near expiry was not renewed")
	}
}

func TestMixedListenerServesTLSAndLoopbackHTTP(t *testing.T) {
	cert, err := loadNetworkCertificate("", "", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil {
			io.WriteString(w, "tls")
			return
		}
		io.WriteString(w, "plain")
	})}
	listen := listenConfig{bind: "127.0.0.1", port: 9099, networkTLS: true, networkCert: &cert}
	done := make(chan error, 1)
	go func() {
		done <- serveConfiguredHTTP(server, listen, func(string, string) (net.Listener, error) { return base, nil }, func(string, ...any) {})
	}()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		<-done
	}()
	addr := base.Addr().String()

	// A silent connection must not hold up the others.
	idle, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()

	get := func(client *http.Client, url string) string {
		t.Helper()
		resp, err := client.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}
	plain := &http.Client{Timeout: 5 * time.Second}
	if got := get(plain, "http://"+addr+"/"); got != "plain" {
		t.Fatalf("loopback plain HTTP = %q", got)
	}
	secure := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	if got := get(secure, "https://"+addr+"/"); got != "tls" {
		t.Fatalf("HTTPS = %q", got)
	}
}

// The -wait-health and -wait-ready checks run in their own process, which never
// loads the agent's certificate. With network TLS on they must still work:
// the systemd units and the release installer gate the service on them.
func TestWaitChecksWorkWithNetworkTLS(t *testing.T) {
	wildcard := listenConfig{bind: "0.0.0.0", port: 9099, networkTLS: true}
	if err := wildcard.validate(); err != nil {
		t.Fatalf("a check with network TLS on was refused: %v", err)
	}
	if err := wildcard.validateServe(); err == nil {
		t.Fatal("serving with network TLS on and no certificate was accepted")
	}
	if got := healthCheckURL(wildcard); got != "http://127.0.0.1:9099/healthz" {
		t.Fatalf("wildcard-bind check URL = %s, want plain HTTP on loopback", got)
	}
	specific := listenConfig{bind: "10.0.0.4", port: 9099, networkTLS: true}
	if got := readinessCheckURL(specific); got != "https://10.0.0.4:9099/readyz" {
		t.Fatalf("specific-bind check URL = %s, want HTTPS: plain HTTP to a network address is redirected", got)
	}
	if got := healthCheckURL(listenConfig{bind: "10.0.0.4", port: 9099}); got != "http://10.0.0.4:9099/healthz" {
		t.Fatalf("check URL without network TLS = %s", got)
	}

	// End to end: the agent's mixed listener on loopback answers the check.
	cert, err := loadNetworkCertificate("", "", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: plainNetworkGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"status":"ok"}`)
	}))}
	serving := listenConfig{bind: "127.0.0.1", networkTLS: true, networkCert: &cert}
	done := make(chan error, 1)
	go func() {
		done <- serveConfiguredHTTP(server, serving, func(string, string) (net.Listener, error) { return base, nil }, func(string, ...any) {})
	}()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		<-done
	}()
	waiting := listenConfig{bind: "0.0.0.0", port: base.Addr().(*net.TCPAddr).Port, networkTLS: true}
	if err := waitForConfiguredHealth(waiting, 5*time.Second); err != nil {
		t.Fatalf("-wait-health against an agent with network TLS: %v", err)
	}
}

func TestPlainNetworkGuardRedirectsToHTTPS(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	guard := plainNetworkGuard(next)

	req := httptest.NewRequest(http.MethodGet, "http://192.0.2.5:9099/api/metrics?x=1", nil)
	req.RemoteAddr = "192.0.2.40:51000"
	rec := serve(guard, req)
	if rec.Code != http.StatusTemporaryRedirect || rec.Header().Get("Location") != "https://192.0.2.5:9099/api/metrics?x=1" {
		t.Fatalf("plain network GET = %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	req = httptest.NewRequest(http.MethodPost, "http://192.0.2.5:9099/api/control", nil)
	req.RemoteAddr = "192.0.2.40:51000"
	if rec := serve(guard, req); rec.Code != http.StatusForbidden {
		t.Fatalf("plain network POST = %d, want 403", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9099/readyz", nil)
	req.RemoteAddr = "127.0.0.1:50000"
	if rec := serve(guard, req); rec.Code != http.StatusTeapot {
		t.Fatalf("loopback plain GET did not reach the handler: %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "https://192.0.2.5:9099/", nil)
	req.RemoteAddr = "192.0.2.40:51000"
	req.TLS = &tls.ConnectionState{}
	if rec := serve(guard, req); rec.Code != http.StatusTeapot {
		t.Fatalf("network HTTPS did not reach the handler: %d", rec.Code)
	}
}

func TestPairingUpkeepCutsStreamsRevokedFromCommandLine(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pairing")
	agent, err := newPairingStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	stop := agent.Start(nil)
	defer stop()
	cli, err := newPairingStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	token, device, err := cli.AddDevice("phone", pairedKindBrowser)
	if err != nil {
		t.Fatal(err)
	}
	// The agent learns of the device on the next request, as it would.
	agent.mu.Lock()
	agent.lastStat = time.Time{}
	agent.mu.Unlock()
	_, gone, ok := agent.Authenticate(token)
	if !ok {
		t.Fatal("agent did not accept the new token")
	}
	if _, err := cli.Revoke(device.ID); err != nil {
		t.Fatal(err)
	}
	// No further request arrives; the upkeep loop alone must notice.
	select {
	case <-gone:
	case <-time.After(3*pairingReloadEvery + time.Second):
		t.Fatal("a device revoked from the command line kept its open stream")
	}
}
