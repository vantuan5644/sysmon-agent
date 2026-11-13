package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// pairingPeer is an agent that requires pairing, the way the public release
// runs it. Every request is made to look as if it came from another machine:
// a loopback read would pass without a token, and plain HTTP from loopback
// would get no redirect.
type pairingPeer struct {
	url   string
	store *pairingStore
	clock *pairingClock
	cert  *x509.Certificate
	mu    sync.Mutex
	seen  []string // "<METHOD> <path> <Authorization>" per request
}

func newPairingPeer(t *testing.T, hostname string) (*pairingPeer, http.Handler) {
	t.Helper()
	store, clock := newTestPairingStore(t, "")
	handler, _ := newPairingTestHandler(t, store)
	peer := &pairingPeer{store: store, clock: clock}
	identity := rewriteStatusIdentity(handler, "instance-"+hostname, hostname)
	return peer, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = "192.0.2.50:41000"
		peer.mu.Lock()
		peer.seen = append(peer.seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		peer.mu.Unlock()
		identity.ServeHTTP(w, r)
	})
}

// newNetworkTLSPeer serves the peer on one port with network TLS: HTTPS with
// the agent's own self-signed certificate, and a redirect there for plain
// HTTP. url is the plain-HTTP address, which is what discovery tries.
func newNetworkTLSPeer(t *testing.T, hostname string) *pairingPeer {
	t.Helper()
	peer, handler := newPairingPeer(t, hostname)
	certPEM, keyPEM, err := newAgentCertificate(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	if peer.cert, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// The guard sees the request as from the network too, so plain HTTP
	// gets the redirect a phone on the LAN would get.
	guarded := plainNetworkGuard(handler)
	// ErrorLog: the home agent first tries the system trust store, so every
	// scan logs a refused handshake here.
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, ErrorLog: log.New(io.Discard, "", 0), Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = "192.0.2.50:41000"
		guarded.ServeHTTP(w, r)
	})}
	mixed := newMixedListener(listener, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	go func() { _ = server.Serve(mixed) }()
	t.Cleanup(func() { _ = server.Close() })
	peer.url = "http://" + listener.Addr().String()
	return peer
}

// newVerifiedTLSPeer serves the peer over a certificate the home agent
// trusts, as Tailscale Serve does.
func newVerifiedTLSPeer(t *testing.T, hostname string) (*pairingPeer, *x509.CertPool) {
	t.Helper()
	peer, handler := newPairingPeer(t, hostname)
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	peer.url = server.URL
	peer.cert = server.Certificate()
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	return peer, pool
}

func (p *pairingPeer) fingerprint() string {
	return normalizeFingerprint(formatFingerprint(p.cert.Raw))
}

func (p *pairingPeer) openPIN(t *testing.T) string {
	t.Helper()
	pin, _, err := p.store.OpenWindow()
	if err != nil {
		t.Fatal(err)
	}
	p.clock.add(2 * time.Second)
	return pin
}

func (p *pairingPeer) requests() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seen...)
}

func (p *pairingPeer) sawAuthorization() bool {
	for _, line := range p.requests() {
		if strings.Contains(line, "Bearer") {
			return true
		}
	}
	return false
}

// rewriteStatusIdentity gives the peer an identity of its own: every test
// agent runs in this process and would otherwise report this agent's
// instance id, which discovery drops as itself.
func rewriteStatusIdentity(next http.Handler, instanceID, hostname string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/status" {
			next.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			for key, values := range rec.Header() {
				w.Header()[key] = values
			}
			w.WriteHeader(rec.Code)
			_, _ = w.Write(rec.Body.Bytes())
			return
		}
		var status map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		status["instance_id"] = instanceID
		status["hostname"] = hostname
		writeJSON(w, http.StatusOK, status)
	})
}

// newPairingHome is the agent a dashboard is served from, with -peers naming
// the peer and its peer tokens kept in dir.
func newPairingHome(t *testing.T, peer *pairingPeer, dir string) (*peerRegistry, http.Handler) {
	t.Helper()
	registry := newTestRegistry(t, []peerConfig{{Name: "gamma", Base: mustParseURL(t, peer.url)}}, nil)
	tokens, err := newPeerTokenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	registry.tokens = tokens
	registry.relayName = "home relay"
	return registry, newHomeHandler(t, registry)
}

func relayPairRequest(id, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "https://home.tailnet.example:9443/api/hosts/"+id+"/pair", strings.NewReader(body))
	req.Header.Set("Origin", "https://home.tailnet.example:9443")
	req.Header.Set("Content-Type", "application/json")
	return req
}

func pairBody(pin, fingerprint string) string {
	data, _ := json.Marshal(hostPairRequest{PIN: pin, CertSHA256: fingerprint})
	return string(data)
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return body
}

func relayGet(handler http.Handler, path string) *httptest.ResponseRecorder {
	return serve(handler, httptest.NewRequest(http.MethodGet, path, nil))
}

func mustHost(t *testing.T, hosts HostsResponse, id string) HostEntry {
	t.Helper()
	host, ok := findHost(hosts, id)
	if !ok {
		t.Fatalf("host %q not listed: %v", id, hostIDs(hosts))
	}
	return host
}

func pairThroughRelay(t *testing.T, handler http.Handler, peer *pairingPeer, fingerprint string) {
	t.Helper()
	rec := serve(handler, relayPairRequest("gamma", pairBody(formatPIN(peer.openPIN(t)), fingerprint)))
	if rec.Code != http.StatusOK {
		t.Fatalf("relay pairing = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPeerThatRequiresPairingIsListedLockedBehindItsRedirect(t *testing.T) {
	peer := newNetworkTLSPeer(t, "gamma-host")
	registry, handler := newPairingHome(t, peer, "")

	host := mustHost(t, getHosts(t, handler, "/api/hosts"), "gamma")
	if !host.Online || !host.Locked {
		t.Fatalf("gamma = %+v, want online and locked", host)
	}
	if host.CertSHA256 != peer.fingerprint() {
		t.Errorf("certificate = %q, want the peer's %q", host.CertSHA256, peer.fingerprint())
	}
	if host.OS != "" || host.Hostname != "" {
		t.Errorf("a locked peer named itself: os %q hostname %q", host.OS, host.Hostname)
	}
	registry.mu.Lock()
	scheme := registry.byID["gamma"].base.Scheme
	registry.mu.Unlock()
	if scheme != "https" {
		t.Errorf("base scheme = %q, want the https:// address the peer redirected to", scheme)
	}

	for _, path := range []string{"/api/hosts/gamma/metrics", "/api/hosts/gamma/stream"} {
		rec := relayGet(handler, path)
		if rec.Code != http.StatusLocked {
			t.Fatalf("GET %s = %d: %s, want 423", path, rec.Code, rec.Body.String())
		}
		body := decodeBody(t, rec)
		if body["host_locked"] != true || body["cert_sha256"] != peer.fingerprint() {
			t.Errorf("GET %s body = %v, want host_locked and the certificate", path, body)
		}
	}
	if peer.sawAuthorization() {
		t.Errorf("a token was sent before any pairing: %v", peer.requests())
	}
}

func TestRelayPairingPinsTheCertificateTheDashboardShowed(t *testing.T) {
	peer := newNetworkTLSPeer(t, "gamma-host")
	dir := t.TempDir()
	registry, handler := newPairingHome(t, peer, dir)
	getHosts(t, handler, "/api/hosts")
	pin := formatPIN(peer.openPIN(t))

	// A fingerprint other than the one shown never sends the PIN.
	other := strings.Repeat("AB", 32)
	rec := serve(handler, relayPairRequest("gamma", pairBody(pin, other)))
	if rec.Code != http.StatusPreconditionFailed || decodeBody(t, rec)["cert_changed"] != true {
		t.Fatalf("wrong certificate = %d: %s, want 412 cert_changed", rec.Code, rec.Body.String())
	}
	for _, line := range peer.requests() {
		if strings.HasPrefix(line, "POST /api/pair") {
			t.Fatalf("the PIN reached a peer whose certificate did not match: %v", peer.requests())
		}
	}

	// A wrong PIN comes back as the peer said it.
	rec = serve(handler, relayPairRequest("gamma", pairBody("000000", peer.fingerprint())))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong PIN = %d: %s, want the peer's 403", rec.Code, rec.Body.String())
	}
	if remaining, _ := decodeBody(t, rec)["remaining"].(float64); remaining != pairingMaxFailures-1 {
		t.Errorf("remaining = %v, want %d", remaining, pairingMaxFailures-1)
	}
	peer.clock.add(2 * time.Second)

	// Lowercase with colons, as a user might copy it, is the same certificate.
	shown := strings.ToLower(formatFingerprint(peer.cert.Raw))
	rec = serve(handler, relayPairRequest("gamma", pairBody(pin, shown)))
	if rec.Code != http.StatusOK {
		t.Fatalf("pairing = %d: %s", rec.Code, rec.Body.String())
	}
	if body := decodeBody(t, rec); body["paired"] != true || body["id"] != "gamma" || body["token"] != nil {
		t.Errorf("pairing body = %v, want paired, the id, and no token", body)
	}

	devices := peer.store.Devices()
	if len(devices) != 1 || devices[0].Kind != pairedKindToken || devices[0].Name != "home relay" {
		t.Fatalf("peer devices = %+v, want one token device named after this agent", devices)
	}
	key := "url:" + peer.url
	token, ok := registry.tokens.Get(key)
	if !ok || token.CertSHA256 != peer.fingerprint() || token.DeviceID != devices[0].ID {
		t.Fatalf("stored token = %+v (%v), want the pin and the peer's device id", token, ok)
	}

	rec = relayGet(handler, "/api/hosts/gamma/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("relayed metrics after pairing = %d: %s", rec.Code, rec.Body.String())
	}
	sent := false
	for _, line := range peer.requests() {
		if line == "GET /api/metrics Bearer "+token.Token {
			sent = true
		}
	}
	if !sent {
		t.Errorf("the peer never saw the token on metrics: %v", peer.requests())
	}

	host := mustHost(t, getHosts(t, handler, "/api/hosts?refresh=1"), "gamma")
	if host.Locked || host.CertSHA256 != "" || host.Hostname != "gamma-host" || host.OS == "" {
		t.Errorf("gamma after pairing = %+v, want unlocked with its OS and hostname", host)
	}

	data, err := os.ReadFile(filepath.Join(dir, peerTokensFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), token.Token) || !strings.Contains(string(data), peer.fingerprint()) {
		t.Errorf("peers.json lacks the token or its pin:\n%s", data)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, peerTokensFile))
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("peers.json mode = %o, want 600", perm)
		}
	}

	// A restarted agent reads the token back and reaches the peer at once.
	_, restarted := newPairingHome(t, peer, dir)
	if host := mustHost(t, getHosts(t, restarted, "/api/hosts"), "gamma"); host.Locked {
		t.Errorf("gamma after a restart = %+v, want unlocked", host)
	}
	if rec := relayGet(restarted, "/api/hosts/gamma/status"); rec.Code != http.StatusOK {
		t.Errorf("relayed status after a restart = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRevokedRelayTokenIsDroppedAndThePeerPairsAgain(t *testing.T) {
	peer := newNetworkTLSPeer(t, "gamma-host")
	registry, handler := newPairingHome(t, peer, "")
	getHosts(t, handler, "/api/hosts")
	pairThroughRelay(t, handler, peer, peer.fingerprint())
	if rec := relayGet(handler, "/api/hosts/gamma/metrics"); rec.Code != http.StatusOK {
		t.Fatalf("metrics after pairing = %d", rec.Code)
	}

	devices := peer.store.Devices()
	if _, err := peer.store.Revoke(devices[0].ID); err != nil {
		t.Fatal(err)
	}
	rec := relayGet(handler, "/api/hosts/gamma/metrics")
	if rec.Code != http.StatusLocked {
		t.Fatalf("metrics after a revoke = %d: %s, want 423", rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, rec)["cert_sha256"]; got != peer.fingerprint() {
		t.Errorf("423 certificate = %v, want the pinned one to pair again over", got)
	}
	if _, ok := registry.tokens.Get("url:" + peer.url); ok {
		t.Error("the refused token was kept")
	}
	host := mustHost(t, getHosts(t, handler, "/api/hosts"), "gamma")
	if !host.Locked || host.CertSHA256 != peer.fingerprint() {
		t.Errorf("gamma after a revoke = %+v, want locked with its certificate", host)
	}

	pairThroughRelay(t, handler, peer, peer.fingerprint())
	if rec := relayGet(handler, "/api/hosts/gamma/metrics"); rec.Code != http.StatusOK {
		t.Errorf("metrics after pairing again = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPeerWhoseCertificateChangedIsLockedAgainWithANote(t *testing.T) {
	peer := newNetworkTLSPeer(t, "gamma-host")
	registry, handler := newPairingHome(t, peer, "")
	getHosts(t, handler, "/api/hosts")
	pairThroughRelay(t, handler, peer, peer.fingerprint())

	// As if the peer had made a new certificate: the pin no longer matches.
	key := "url:" + peer.url
	token, _ := registry.tokens.Get(key)
	token.CertSHA256 = strings.Repeat("CD", 32)
	if err := registry.tokens.Put(token); err != nil {
		t.Fatal(err)
	}
	registry.mu.Lock()
	registry.byID["gamma"].pin = token.CertSHA256
	registry.mu.Unlock()
	before := len(peer.requests())

	if rec := relayGet(handler, "/api/hosts/gamma/metrics"); rec.Code != http.StatusBadGateway {
		t.Fatalf("metrics over a mismatched pin = %d: %s, want 502", rec.Code, rec.Body.String())
	}
	host := mustHost(t, getHosts(t, handler, "/api/hosts?refresh=1"), "gamma")
	if !host.Locked || host.CertSHA256 != peer.fingerprint() || host.Error != "certificate changed since it was paired" {
		t.Errorf("gamma = %+v, want locked, the new certificate, and the note", host)
	}
	for _, line := range peer.requests()[before:] {
		if strings.Contains(line, "Bearer") {
			t.Errorf("the token went to a certificate other than the pinned one: %q", line)
		}
	}
	if _, ok := registry.tokens.Get(key); !ok {
		t.Error("a certificate change dropped the token; only the peer refusing it may")
	}
}

func TestPeerBehindATrustedCertificatePairsWithoutAFingerprint(t *testing.T) {
	peer, roots := newVerifiedTLSPeer(t, "delta-host")
	registry, handler := newPairingHome(t, peer, "")
	registry.client = newPeerClient(newPeerTransport(&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}))

	host := mustHost(t, getHosts(t, handler, "/api/hosts"), "gamma")
	if !host.Locked || host.CertSHA256 != "" {
		t.Fatalf("gamma = %+v, want locked with no certificate to compare", host)
	}
	pairThroughRelay(t, handler, peer, "")
	token, ok := registry.tokens.Get("url:" + peer.url)
	if !ok || token.CertSHA256 != "" {
		t.Fatalf("token = %+v (%v), want one with no pin", token, ok)
	}
	if rec := relayGet(handler, "/api/hosts/gamma/metrics"); rec.Code != http.StatusOK {
		t.Errorf("metrics after pairing = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRelayControlToALockedPeerAsksForItsPIN(t *testing.T) {
	peer, roots := newVerifiedTLSPeer(t, "delta-host")
	registry, handler := newPairingHome(t, peer, "")
	registry.client = newPeerClient(newPeerTransport(&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}))
	getHosts(t, handler, "/api/hosts")

	rec := serve(handler, relayControlRequest("gamma", `{"action":"lock_screen"}`, "https://home.tailnet.example:9443"))
	if rec.Code != http.StatusLocked {
		t.Fatalf("relayed control = %d: %s, want 423 (a 401 would lock this agent's own screen)", rec.Code, rec.Body.String())
	}
}

func TestRelayPairingRefusesAPeerReachedOverPlainHTTP(t *testing.T) {
	registry := newTestRegistry(t, nil, nil)
	handler := newHomeHandler(t, registry)
	base := mustParseURL(t, "http://192.0.2.10:9099")
	record := &peerRecord{key: "cfg:0", tokenKey: "url:" + base.String(), base: base}
	record.entry = HostEntry{ID: "plain", Name: "plain", Online: true, Locked: true}
	registry.mu.Lock()
	registry.configured = []peerConfig{{Base: base}}
	registry.records = []*peerRecord{record}
	registry.byID = map[string]*peerRecord{"plain": record}
	registry.scannedAt = registry.now()
	registry.mu.Unlock()

	rec := serve(handler, relayPairRequest("plain", pairBody("123456", "")))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "plain HTTP") {
		t.Fatalf("pairing over plain HTTP = %d: %s, want 403", rec.Code, rec.Body.String())
	}
}

func TestRelayPairingNeedsASameOriginRequest(t *testing.T) {
	peer := newNetworkTLSPeer(t, "gamma-host")
	_, handler := newPairingHome(t, peer, "")
	getHosts(t, handler, "/api/hosts")
	req := relayPairRequest("gamma", pairBody("123456", peer.fingerprint()))
	req.Header.Set("Origin", "https://elsewhere.example")
	if rec := serve(handler, req); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin pairing = %d, want 403", rec.Code)
	}
}

func TestPeerTokenIsSentOnlyOverHTTPSOrToThisMachine(t *testing.T) {
	registry := newTestRegistry(t, nil, nil)
	if err := registry.tokens.Put(peerToken{Key: "k", Token: "abc.def"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		base string
		want bool
	}{
		{"https://10.0.0.4:9443", true},
		{"https://minipc.tailnet.example:9443", true},
		{"http://127.0.0.1:9099", true},
		{"http://[::1]:9099", true},
		{"http://localhost:9099", true},
		{"http://10.0.0.4:9099", false},
		{"http://minipc.tailnet.example:9099", false},
	} {
		base := mustParseURL(t, tc.base)
		req := httptest.NewRequest(http.MethodGet, peerURL(base, "/api/status"), nil)
		sent := registry.authorize(req, base, "k")
		if (sent != "") != tc.want || (req.Header.Get("Authorization") != "") != tc.want {
			t.Errorf("%s: sent %q header %q, want sent=%v", tc.base, sent, req.Header.Get("Authorization"), tc.want)
		}
	}
}

func TestHTTPSRedirectIsFollowedToTheSameAddressOnly(t *testing.T) {
	base := mustParseURL(t, "http://10.0.0.4:9099")
	for _, tc := range []struct {
		location string
		want     string
	}{
		{"https://10.0.0.4:9099/api/status", "https://10.0.0.4:9099"},
		{"https://10.0.0.5:9099/api/status", ""},
		{"https://10.0.0.4:9443/api/status", ""},
		{"http://10.0.0.4:9099/api/status", ""},
		{"/api/status", ""},
		{"", ""},
	} {
		got := httpsRedirectTarget(base, tc.location)
		if (got == nil && tc.want != "") || (got != nil && got.String() != tc.want) {
			t.Errorf("%q -> %v, want %q", tc.location, got, tc.want)
		}
	}
	if got := httpsRedirectTarget(mustParseURL(t, "https://10.0.0.4:9099"), "https://10.0.0.4:9099/"); got != nil {
		t.Errorf("an https:// address followed a redirect: %v", got)
	}
}

func TestPeerTokenForgetDropsOnlyTheRefusedToken(t *testing.T) {
	store, err := newPeerTokenStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(peerToken{Key: "k", Token: "new"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Forget("k", "old"); err != nil {
		t.Fatal(err)
	}
	if token, ok := store.Get("k"); !ok || token.Token != "new" {
		t.Fatalf("a refusal of an older token dropped the new one: %+v %v", token, ok)
	}
	if err := store.Forget("k", "new"); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Get("k"); ok {
		t.Fatal("the refused token was kept")
	}
}

func TestNormalizeFingerprintAcceptsThePrintedForm(t *testing.T) {
	hex := strings.Repeat("0a", 32)
	want := strings.ToUpper(hex)
	colons := strings.TrimSuffix(strings.Repeat("0A:", 32), ":")
	for _, input := range []string{hex, want, colons, " " + colons + " "} {
		if got := normalizeFingerprint(input); got != want {
			t.Errorf("normalizeFingerprint(%q) = %q", input, got)
		}
	}
	for _, input := range []string{"", "abc", strings.Repeat("zz", 32), strings.Repeat("0a", 31)} {
		if got := normalizeFingerprint(input); got != "" {
			t.Errorf("normalizeFingerprint(%q) = %q, want empty", input, got)
		}
	}
}
