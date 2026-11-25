package main

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestHostAllowlistAcceptsWhatNamesThisAgent(t *testing.T) {
	allow := newHostAllowlist([]string{"WorkStation"}, "", "dualboot", []string{"dash.example.org", "*.corp.example.org"})
	for _, host := range []string{
		// IP literals, with and without ports, brackets and zones.
		"127.0.0.1", "127.0.0.1:9099", "10.1.2.3:9099", "100.64.0.7",
		"::1", "[::1]", "[::1]:9099", "[fe80::1%25eth0]:9099", "[2001:db8::5]:443",
		// Loopback names.
		"localhost", "LOCALHOST:9099", "localhost.", "app.localhost:9099",
		// mDNS and Tailscale.
		"anything.local", "Anything.Local.:9099", "machine.tailnet.ts.net", "machine.tailnet.ts.net:9443", "sysmon.tailnet.ts.net.",
		// This machine's hostname, any case.
		"workstation", "WORKSTATION:9099",
		// -mdns-name, bare and .local.
		"dualboot", "dualboot.local:9099",
		// -allowed-hosts: exact, and anything below a wildcard.
		"dash.example.org", "DASH.example.org.:8443", "a.corp.example.org", "a.b.corp.example.org:9099",
	} {
		if !allow.Allows(host) {
			t.Errorf("Allows(%q) = false, want true", host)
		}
	}
}

func TestHostAllowlistRejectsRebindingNames(t *testing.T) {
	allow := newHostAllowlist([]string{"workstation"}, "", "dualboot", []string{"dash.example.org", "*.corp.example.org"})
	for _, host := range []string{
		"attacker.test", "attacker.test:9099", "attacker.test.",
		// A known name used as a label of the attacker's domain.
		"workstation.attacker.test", "localhost.attacker.test", "dualboot.attacker.test",
		"machine.ts.net.attacker.test", "x.local.attacker.test", "127.0.0.1.attacker.test",
		// The wildcard covers names below it, not the name itself or lookalikes.
		"corp.example.org", "evilcorp.example.org", "dash.example.org.attacker.test",
		// Bare suffix labels are not names under them.
		"local", "ts.net", "",
	} {
		if allow.Allows(host) {
			t.Errorf("Allows(%q) = true, want false", host)
		}
	}
}

func TestHostAllowlistTakesTheShortNameOfAnFQDNHostname(t *testing.T) {
	allow := newHostAllowlist([]string{"box.lan.example.net"}, "", "", nil)
	for _, host := range []string{"box", "box.lan.example.net:9099"} {
		if !allow.Allows(host) {
			t.Errorf("Allows(%q) = false, want true", host)
		}
	}
	if allow.Allows("box.attacker.test") {
		t.Error("an FQDN hostname must not allow its short name under other domains")
	}
}

func TestHostAllowlistAcceptsABindName(t *testing.T) {
	allow := newHostAllowlist(nil, "sysmon-box", "", nil)
	if !allow.Allows("sysmon-box:9099") {
		t.Error("a -bind host name is how -wait-health reaches the agent and must be allowed")
	}
	if wildcard := newHostAllowlist(nil, "0.0.0.0", "", nil); len(wildcard.names) != 0 {
		t.Errorf("an IP bind added names %v", wildcard.names)
	}
}

func TestParseAllowedHosts(t *testing.T) {
	got, err := parseAllowedHosts(" Dash.Example.org. , *.corp.example.org,, my_host ")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"dash.example.org", "*.corp.example.org", "my_host"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseAllowedHosts = %q, want %q", got, want)
	}
	if got, err := parseAllowedHosts(""); err != nil || len(got) != 0 {
		t.Fatalf("empty flag = %q, %v", got, err)
	}
	for _, bad := range []string{"http://dash.example.org", "dash.example.org:9099", "dash.example.org/x", "*", "*.", "a..b", "-a.example.org", "a b", "**.example.org", "x.*.example.org"} {
		if _, err := parseAllowedHosts(bad); err == nil {
			t.Errorf("parseAllowedHosts(%q) accepted a bad entry", bad)
		} else if !strings.Contains(err.Error(), "-allowed-hosts") {
			t.Errorf("error for %q does not name the flag: %v", bad, err)
		}
	}
}

func newHostGuardTestHandler(t *testing.T, allow *hostAllowlist, controller SystemController) http.Handler {
	t.Helper()
	state := NewMemoryRuntimeState()
	if allow != nil {
		state.SetHostAllowlist(allow)
	}
	handler, err := newHTTPHandlerWithController(fakeCollector{}, testStaticFS(), state, controller)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func TestHostGuardAnswers421ToARebindingName(t *testing.T) {
	controller := &fakeController{result: ControlResult{Available: true, Applied: true}}
	handler := newHostGuardTestHandler(t, newHostAllowlist([]string{"workstation"}, "", "", nil), controller)

	for _, path := range []string{"/healthz", "/api/status", "/api/metrics", "/"} {
		req := httptest.NewRequest(http.MethodGet, "http://attacker.test:9099"+path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusMisdirectedRequest {
			t.Fatalf("GET %s via attacker.test = %d, want 421", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Fatalf("421 Content-Type = %q", ct)
		}
		if body := rec.Body.String(); !strings.Contains(body, "-allowed-hosts attacker.test") || !strings.Contains(body, "SYSMON_ALLOWED_HOSTS") {
			t.Fatalf("421 body does not explain the flag: %q", body)
		}
		if rec.Header().Get("Content-Security-Policy") == "" {
			t.Fatal("421 answer lost the security headers")
		}
	}

	// The rebinding page's control post is same-origin as far as Origin goes;
	// the Host still gives it away.
	req := httptest.NewRequest(http.MethodPost, "http://attacker.test:9099/api/control", strings.NewReader(`{"action":"lock_screen"}`))
	req.Header.Set("Origin", "http://attacker.test:9099")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMisdirectedRequest || controller.calls != 0 {
		t.Fatalf("rebinding control post = %d with %d controller calls, want 421 and none", rec.Code, controller.calls)
	}
}

func TestHostGuardPassesAllowedNames(t *testing.T) {
	handler := newHostGuardTestHandler(t, newHostAllowlist([]string{"workstation"}, "", "dualboot", []string{"*.corp.example.org"}), &fakeController{})
	for _, base := range []string{
		"http://127.0.0.1:9099", "http://[::1]:9099", "http://localhost:9099", "http://workstation:9099",
		"https://workstation.local:9099", "https://dualboot.local:9099", "https://machine.tailnet.ts.net:9443",
		"https://dash.corp.example.org",
	} {
		req := httptest.NewRequest(http.MethodGet, base+"/healthz", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s/healthz = %d, want 200", base, rec.Code)
		}
	}
}

func TestHostGuardChecksForwardedHostsOnlyFromATrustedSource(t *testing.T) {
	handler := newHostGuardTestHandler(t, newHostAllowlist([]string{"workstation"}, "", "", nil), &fakeController{})
	cases := []struct {
		name       string
		remoteAddr string
		host       string
		headers    map[string]string
		want       int
	}{
		{"Tailscale Serve keeps the Host and adds X-Forwarded-Host", "127.0.0.1:50000", "machine.tailnet.ts.net", map[string]string{"X-Forwarded-Host": "machine.tailnet.ts.net", "X-Forwarded-For": "100.64.0.9"}, http.StatusOK},
		{"a local proxy to 127.0.0.1 forwarding an allowed name", "127.0.0.1:50000", "127.0.0.1:9099", map[string]string{"X-Forwarded-Host": "machine.tailnet.ts.net:9443"}, http.StatusOK},
		{"a local proxy forwarding an unknown name", "127.0.0.1:50000", "127.0.0.1:9099", map[string]string{"X-Forwarded-Host": "attacker.test"}, http.StatusMisdirectedRequest},
		{"RFC 7239 Forwarded host", "[::1]:50000", "127.0.0.1:9099", map[string]string{"Forwarded": `for=100.64.0.9;host="attacker.test";proto=https`}, http.StatusMisdirectedRequest},
		{"a rebound loopback page cannot launder its Host with a forwarded one", "127.0.0.1:50000", "attacker.test:9099", map[string]string{"X-Forwarded-Host": "localhost:9099"}, http.StatusMisdirectedRequest},
		{"a rebound loopback page without forwarded headers", "127.0.0.1:50000", "attacker.test:9099", nil, http.StatusMisdirectedRequest},
		{"forwarded headers from the network are ignored, as for same-origin", "192.0.2.7:50000", "10.1.2.3:9099", map[string]string{"X-Forwarded-Host": "attacker.test"}, http.StatusOK},
		{"the network Host is still checked", "192.0.2.7:50000", "attacker.test:9099", map[string]string{"X-Forwarded-Host": "10.1.2.3:9099"}, http.StatusMisdirectedRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://placeholder/healthz", nil)
			req.Host = tc.host
			req.RemoteAddr = tc.remoteAddr
			for key, value := range tc.headers {
				req.Header.Set(key, value)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestHostGuardIsOnWithoutConfiguration(t *testing.T) {
	// No SetHostAllowlist: the default list (which in this test binary also
	// carries the reserved example names) still refuses other names.
	handler := newHostGuardTestHandler(t, nil, &fakeController{})
	req := httptest.NewRequest(http.MethodGet, "http://attacker.test:9099/api/status", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("unconfigured agent answered attacker.test with %d, want 421", rec.Code)
	}
}

func TestHostGuardRunsBeforePairing(t *testing.T) {
	state := NewMemoryRuntimeState()
	state.SetHostAllowlist(newHostAllowlist([]string{"workstation"}, "", "", nil))
	store, err := newPairingStore("")
	if err != nil {
		t.Fatal(err)
	}
	state.SetPairing(store)
	handler, err := newHTTPHandlerWithController(fakeCollector{}, testStaticFS(), state, &fakeController{})
	if err != nil {
		t.Fatal(err)
	}
	// /api/pair/state is open without pairing; the guard still applies.
	req := httptest.NewRequest(http.MethodGet, "http://attacker.test/api/pair/state", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("pair state via attacker.test = %d, want 421", rec.Code)
	}
}
