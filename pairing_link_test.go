package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestPairingLink(t *testing.T) {
	for base, want := range map[string]string{
		"https://host.example.ts.net:9443":           "https://host.example.ts.net:9443/#pair=123456",
		"https://10.0.0.4:9199/":                     "https://10.0.0.4:9199/#pair=123456",
		" https://[fd00::4]:9199/dash?x=1#old ":      "https://[fd00::4]:9199/dash#pair=123456",
		"http://user:secret@sysmon.example:9099/app": "http://sysmon.example:9099/app#pair=123456",
	} {
		if got, err := pairingLink(base, "123456"); err != nil || got != want {
			t.Fatalf("pairingLink(%q) = %q, %v; want %q", base, got, err, want)
		}
	}
	for _, base := range []string{"", "sysmon.example:9443", "ftp://sysmon.example/", "https:///", "mailto:a@b"} {
		if got, err := pairingLink(base, "123456"); err == nil {
			t.Fatalf("pairingLink(%q) = %q, want an error", base, got)
		}
	}
	if got := pairingLinkAddress("https://10.0.0.4:9199/#pair=123456"); got != "https://10.0.0.4:9199/" {
		t.Fatalf("pairingLinkAddress = %q", got)
	}
}

func TestPreferredPairingBase(t *testing.T) {
	if got := preferredPairingBase(listenConfig{bind: "127.0.0.1", port: 9099}); got != "" {
		t.Fatalf("a loopback agent offered %q", got)
	}
	if got := preferredPairingBase(listenConfig{bind: "10.0.0.4", port: 9199, networkTLS: true}); got != "https://10.0.0.4:9199/" {
		t.Fatalf("a bound address gave %q", got)
	}
	for host, loopback := range map[string]bool{"127.0.0.1": true, "::1": true, "localhost": true, "app.localhost": true, "::ffff:127.0.0.1": true, "10.0.0.4": false, "sysmon.example": false} {
		if got := isLoopbackHostName(host); got != loopback {
			t.Fatalf("isLoopbackHostName(%q) = %v", host, got)
		}
	}
}

func TestPairCommandDrawsTheLinkAsAQRCode(t *testing.T) {
	settings := filepath.Join(t.TempDir(), "settings.json")
	listen := listenConfig{bind: "127.0.0.1", port: 9099}
	var out bytes.Buffer

	if err := runPairingCommand(pairingCommand{open: true, link: "sysmon.example:9443"}, settings, listen, &out); err == nil || !strings.Contains(err.Error(), "-pair-url") {
		t.Fatalf("a bad -pair-url gave %v", err)
	}
	if store, err := newPairingStore(pairingDirFor(settings)); err != nil || store.WindowOpen() {
		t.Fatalf("a bad -pair-url still opened a window (%v)", err)
	}

	// Redirected output and a loopback agent: no code, and nothing to point one at.
	if err := runPairingCommand(pairingCommand{open: true, qr: true}, settings, listen, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "█") || strings.Contains(out.String(), "scan") {
		t.Fatalf("a loopback agent drew a code with no address to open:\n%s", out.String())
	}

	out.Reset()
	if err := runPairingCommand(pairingCommand{open: true, link: "https://host.example.ts.net:9443"}, settings, listen, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "█") {
		t.Fatalf("-pair drew a code when not asked to:\n%s", out.String())
	}

	out.Reset()
	if err := runPairingCommand(pairingCommand{open: true, link: "https://host.example.ts.net:9443", qr: true}, settings, listen, &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "It opens https://host.example.ts.net:9443/ with the PIN filled in") || !strings.Contains(text, "█") {
		t.Fatalf("-pair -pair-url did not draw the link:\n%s", text)
	}
	pinLine := strings.TrimSpace(strings.SplitN(strings.SplitN(text, "Pairing PIN:", 2)[1], "\n", 2)[0])
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, "PIN "+pinLine+", valid until ") {
		t.Fatalf("the PIN was not repeated under the code: %q (PIN %q)", last, pinLine)
	}
	if strings.Index(text, "Pairing PIN:") > strings.Index(text, "█") {
		t.Fatalf("the code came before the PIN")
	}
}

func TestPairingWindowCarriesALinkAndQRCode(t *testing.T) {
	store, clock := newTestPairingStore(t, "")
	state := NewMemoryRuntimeState()
	state.SetPairing(store)
	handler, err := newHTTPHandlerWithController(fakeCollector{}, testStaticFS(), state, &fakeController{})
	if err != nil {
		t.Fatal(err)
	}
	cookie, _ := pairDevice(t, handler, store, clock, "")

	open := func(req *http.Request) pairingWindowResponse {
		t.Helper()
		req.AddCookie(cookie)
		rec := serve(handler, req)
		var window pairingWindowResponse
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &window) != nil {
			t.Fatalf("POST /api/pair/window = %d %s", rec.Code, rec.Body.String())
		}
		return window
	}

	// The dashboard's own origin is what the new device opens.
	window := open(networkRequest(http.MethodPost, "/api/pair/window", ""))
	if window.Link != pairingTestOrigin+"/#pair="+window.PIN {
		t.Fatalf("link = %q, want the dashboard's origin with the PIN", window.Link)
	}
	code, err := encodeQR(window.Link)
	if err != nil || strings.Join(window.QR, "\n") != strings.Join(code.rows(), "\n") {
		t.Fatalf("qr is not the link's code (%v)", err)
	}

	// A dashboard on this machine's loopback address points elsewhere, or
	// leaves the code out when nothing else is known.
	loopback := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9099/api/pair/window", nil)
		req.RemoteAddr = "127.0.0.1:52000"
		req.Header.Set("Origin", "http://127.0.0.1:9099")
		req.Header.Set("Content-Type", "application/json")
		return req
	}
	if window := open(loopback()); window.Link != "" || window.QR != nil || len(window.PIN) != 6 {
		t.Fatalf("a loopback dashboard with no fallback got link %q and %d rows", window.Link, len(window.QR))
	}
	state.SetPairingLinkFallback(func() string { return "https://10.0.0.4:9099/" })
	if window := open(loopback()); window.Link != "https://10.0.0.4:9099/#pair="+window.PIN || len(window.QR) == 0 {
		t.Fatalf("a loopback dashboard got link %q, want the fallback", window.Link)
	}
}
