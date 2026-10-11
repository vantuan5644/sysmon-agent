package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// peerAgent is a real agent handler on a test listener. Every agent in a test
// binary shares agentInstanceID, so the peer's /api/status is rewritten to
// carry its own instance id -- otherwise the registry would take every peer
// for itself.
type peerAgent struct {
	server     *httptest.Server
	controller *fakeController
}

func newPeerAgent(t *testing.T, instanceID, hostname string) *peerAgent {
	t.Helper()
	controller := &fakeController{
		capabilities: capabilitiesFor(func(ControlAction) bool { return true }),
		result:       ControlResult{Applied: true, State: "locked"},
	}
	metrics := baseMetrics(hostname)
	handler, err := newHTTPHandlerWithController(fakeCollector{metrics: metrics}, testStaticFS(), NewMemoryRuntimeState(), controller)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/status" {
			handler.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		var status map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
			t.Errorf("peer status JSON: %v", err)
		}
		status["instance_id"] = instanceID
		status["hostname"] = hostname
		writeJSON(w, rec.Code, status)
	})
	agent := &peerAgent{server: httptest.NewServer(wrapped), controller: controller}
	t.Cleanup(agent.server.Close)
	return agent
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

// newTestRegistry builds a registry whose tailnet is the given name -> URL
// map, standing in for `tailscale status`.
func newTestRegistry(t *testing.T, configured []peerConfig, tailnet map[string]string) *peerRegistry {
	t.Helper()
	registry := newPeerRegistry(peerRegistryOptions{Configured: configured, Discover: tailnet != nil})
	var mu sync.Mutex
	registry.listTailnet = func(context.Context) (string, []tailscalePeer, error) {
		mu.Lock()
		defer mu.Unlock()
		peers := make([]tailscalePeer, 0, len(tailnet))
		for name := range tailnet {
			peers = append(peers, tailscalePeer{Name: name, DNSName: name + ".tailnet.example", IPv4: "100.64.0.1"})
		}
		return "home", peers, nil
	}
	registry.tailnetBases = func(peer tailscalePeer) []*url.URL {
		return []*url.URL{mustParseURL(t, tailnet[peer.Name])}
	}
	return registry
}

func newHomeHandler(t *testing.T, registry *peerRegistry) http.Handler {
	t.Helper()
	state := NewMemoryRuntimeState()
	if registry != nil {
		state.SetPeerRegistry(registry)
	}
	handler, err := newHTTPHandlerWithController(fakeCollector{metrics: baseMetrics("home")}, testStaticFS(), state, &fakeController{})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func getHosts(t *testing.T, handler http.Handler, path string) HostsResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body.String())
	}
	var hosts HostsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &hosts); err != nil {
		t.Fatal(err)
	}
	return hosts
}

func hostIDs(hosts HostsResponse) []string {
	ids := make([]string, 0, len(hosts.Hosts))
	for _, host := range hosts.Hosts {
		ids = append(ids, host.ID)
	}
	return ids
}

func findHost(hosts HostsResponse, id string) (HostEntry, bool) {
	for _, host := range hosts.Hosts {
		if host.ID == id {
			return host, true
		}
	}
	return HostEntry{}, false
}

func TestParsePeerList(t *testing.T) {
	peers, err := parsePeerList(" minipc=https://minipc.example.ts.net:9443/ ,http://10.0.0.5:9099\nlab-box=http://[fd7a::1]:9099")
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ name, base string }{
		{"minipc", "https://minipc.example.ts.net:9443"},
		{"", "http://10.0.0.5:9099"},
		{"lab-box", "http://[fd7a::1]:9099"},
	}
	if len(peers) != len(want) {
		t.Fatalf("parsed %d peers, want %d", len(peers), len(want))
	}
	for i, w := range want {
		if peers[i].Name != w.name || peers[i].Base.String() != w.base {
			t.Errorf("peer %d = %q %q, want %q %q", i, peers[i].Name, peers[i].Base, w.name, w.base)
		}
	}
	if peers, err := parsePeerList("  "); err != nil || len(peers) != 0 {
		t.Fatalf("blank list = %v, %v; want none", peers, err)
	}
}

func TestParsePeerListRejectsAnythingButABareOrigin(t *testing.T) {
	for _, raw := range []string{
		"minipc",                            // no scheme
		"ftp://minipc:21",                   // wrong scheme
		"http://",                           // no host
		"=http://minipc:9099",               // empty name
		"http://minipc:9099/api",            // path
		"http://minipc:9099/?x=1",           // query
		"http://user:pass@minipc:9099",      // credentials
		"https://minipc.example:9443/#frag", // fragment
	} {
		if _, err := parsePeerList(raw); err == nil {
			t.Errorf("parsePeerList(%q) accepted it", raw)
		}
	}
}

func TestParseTailscalePeersKeepsOnlyAgentCandidatesInThisTailnet(t *testing.T) {
	data := []byte(`{
	  "MagicDNSSuffix": "bigeye-symmetric.ts.net",
	  "Self": {"HostName": "BBLWIN", "DNSName": "bblwin.bigeye-symmetric.ts.net.", "Online": true},
	  "Peer": {
	    "a": {"HostName": "omarchy", "DNSName": "omarchy-hanh.bigeye-symmetric.ts.net.", "OS": "linux", "TailscaleIPs": ["100.76.145.41", "fd7a:115c::1"], "Online": true},
	    "b": {"HostName": "iPhone", "DNSName": "iphone-13-pro-max.bigeye-symmetric.ts.net.", "OS": "iOS", "TailscaleIPs": ["100.109.134.98"], "Online": true},
	    "c": {"HostName": "minipc", "DNSName": "minipc.bigeye-symmetric.ts.net.", "OS": "linux", "TailscaleIPs": ["100.83.206.61"], "Online": false},
	    "d": {"HostName": "gpu", "DNSName": "azure-gpu-h100.tail857a7c.ts.net.", "OS": "linux", "TailscaleIPs": ["100.78.107.11"], "Online": true},
	    "e": {"HostName": "shared", "DNSName": "shared.bigeye-symmetric.ts.net.", "OS": "linux", "TailscaleIPs": ["100.1.1.1"], "Online": true, "ShareeNode": true}
	  }
	}`)
	self, peers, err := parseTailscalePeers(data)
	if err != nil {
		t.Fatal(err)
	}
	if self != "bblwin" {
		t.Errorf("self = %q, want the machine name bblwin (the OS hostname is BBLWIN)", self)
	}
	if len(peers) != 1 {
		t.Fatalf("peers = %+v, want only omarchy-hanh (phone, offline, other-tailnet and shared-in nodes dropped)", peers)
	}
	got := peers[0]
	if got.Name != "omarchy-hanh" || got.DNSName != "omarchy-hanh.bigeye-symmetric.ts.net" || got.IPv4 != "100.76.145.41" {
		t.Errorf("peer = %+v", got)
	}
}

func TestAssignPeerIDsKeepsExistingIDsAndAvoidsCollisions(t *testing.T) {
	records := []*peerRecord{
		{entry: HostEntry{Name: "Local"}},
		{entry: HostEntry{Name: "mini pc"}},
		{entry: HostEntry{Name: "mini-pc"}},
		{entry: HostEntry{Name: "!!!"}},
		{entry: HostEntry{ID: "kept", Name: "renamed"}},
	}
	assignPeerIDs(records)
	want := []string{"local-2", "mini-pc", "mini-pc-2", "host", "kept"}
	for i, record := range records {
		if record.entry.ID != want[i] {
			t.Errorf("record %d (%q) id = %q, want %q", i, record.entry.Name, record.entry.ID, want[i])
		}
	}
}

func TestPeerRegistryListsConfiguredAndDiscoveredAgents(t *testing.T) {
	alpha := newPeerAgent(t, "alpha-instance", "ALPHA")
	beta := newPeerAgent(t, "beta-instance", "beta-os-name")
	itself := newPeerAgent(t, agentInstanceID, "home")
	notAnAgent := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(notAnAgent.Close)

	registry := newTestRegistry(t,
		[]peerConfig{
			{Base: mustParseURL(t, alpha.server.URL)},
			{Name: "me-again", Base: mustParseURL(t, itself.server.URL)},
		},
		map[string]string{
			"beta":      beta.server.URL,
			"gamma":     notAnAgent.URL,
			"alpha-dup": alpha.server.URL,
		})
	hosts := getHosts(t, newHomeHandler(t, registry), "/api/hosts")

	if got, want := strings.Join(hostIDs(hosts), ","), "local,alpha,beta"; got != want {
		t.Fatalf("host ids = %s, want %s (self dropped by instance id, non-agent and duplicate dropped)", got, want)
	}
	local, _ := findHost(hosts, localHostID)
	if !local.Self || local.Name != "home" {
		t.Errorf("local entry = %+v, want self named after the tailnet machine", local)
	}
	alphaEntry, _ := findHost(hosts, "alpha")
	if alphaEntry.Source != "configured" || !alphaEntry.Online || alphaEntry.Hostname != "ALPHA" || !strings.HasPrefix(alphaEntry.DashboardBuild, "sysmon-static-") {
		t.Errorf("alpha = %+v", alphaEntry)
	}
	betaEntry, _ := findHost(hosts, "beta")
	if betaEntry.Source != "tailscale" || betaEntry.Name != "beta" {
		t.Errorf("beta = %+v, want the tailnet machine name beta", betaEntry)
	}
	if !hosts.Discovery.Enabled || !hosts.Discovery.Available || hosts.Discovery.ScannedAt == nil {
		t.Errorf("discovery = %+v", hosts.Discovery)
	}
}

func TestPeerRegistryKeepsAVanishedDiscoveredHostAsOffline(t *testing.T) {
	beta := newPeerAgent(t, "beta-instance", "beta")
	tailnet := map[string]string{"beta": beta.server.URL}
	registry := newTestRegistry(t, nil, tailnet)
	handler := newHomeHandler(t, registry)

	first := getHosts(t, handler, "/api/hosts")
	if entry, ok := findHost(first, "beta"); !ok || !entry.Online {
		t.Fatalf("first scan = %+v, want beta online", first.Hosts)
	}
	registry.listTailnet = func(context.Context) (string, []tailscalePeer, error) { return "home", nil, nil }
	registry.mu.Lock()
	registry.scannedAt = registry.scannedAt.Add(-peerScanTTL - 1)
	registry.mu.Unlock()
	registry.Hosts(context.Background(), true)

	second := getHosts(t, handler, "/api/hosts")
	entry, ok := findHost(second, "beta")
	if !ok || entry.Online || entry.Error != "offline in tailnet" || entry.LastSeen == nil {
		t.Fatalf("second scan beta = %+v (found %v), want it kept, offline, same id", entry, ok)
	}
}

func TestHostListOmitsTimesThatNeverHappened(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	registry := newTestRegistry(t, []peerConfig{{Name: "never", Base: mustParseURL(t, deadURL)}}, nil)
	rec := httptest.NewRecorder()
	newHomeHandler(t, registry).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/hosts", nil))
	if strings.Contains(rec.Body.String(), "0001-01-01") {
		t.Fatalf("host list carries a zero time: %s", rec.Body.String())
	}
	var hosts HostsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &hosts); err != nil {
		t.Fatal(err)
	}
	never, ok := findHost(hosts, "never")
	if !ok || never.Online || never.LastSeen != nil || never.CheckedAt == nil {
		t.Fatalf("never-seen host = %+v, want offline, checked, never seen", never)
	}
}

func TestPeerRegistryWithNothingToFindNeverScans(t *testing.T) {
	registry := newPeerRegistry(peerRegistryOptions{})
	registry.listTailnet = func(context.Context) (string, []tailscalePeer, error) {
		t.Error("listTailnet called with discovery off and no peers configured")
		return "", nil, nil
	}
	hosts := getHosts(t, newHomeHandler(t, registry), "/api/hosts?refresh=1")
	if got := strings.Join(hostIDs(hosts), ","); got != localHostID {
		t.Fatalf("hosts = %s, want only local", got)
	}
	if hosts.Discovery.Enabled {
		t.Errorf("discovery = %+v, want disabled", hosts.Discovery)
	}
}

func TestHostsWithoutARegistryListOnlyThisAgent(t *testing.T) {
	hosts := getHosts(t, newHomeHandler(t, nil), "/api/hosts")
	if len(hosts.Hosts) != 1 || !hosts.Hosts[0].Self || hosts.Hosts[0].ID != localHostID {
		t.Fatalf("hosts = %+v", hosts.Hosts)
	}
}

func TestStatusReportsIdentityForPeerChecks(t *testing.T) {
	rec := httptest.NewRecorder()
	newHomeHandler(t, nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	var status AgentStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.InstanceID != agentInstanceID || status.InstanceID == "" {
		t.Errorf("instance_id = %q, want %q", status.InstanceID, agentInstanceID)
	}
}

func configuredRegistry(t *testing.T, name string, agent *httptest.Server) *peerRegistry {
	t.Helper()
	return newTestRegistry(t, []peerConfig{{Name: name, Base: mustParseURL(t, agent.URL)}}, nil)
}

func TestRelayReadPassesThePeersJSONThrough(t *testing.T) {
	alpha := newPeerAgent(t, "alpha-instance", "alpha-host")
	handler := newHomeHandler(t, configuredRegistry(t, "alpha", alpha.server))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/hosts/alpha/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("relayed metrics = %d: %s", rec.Code, rec.Body.String())
	}
	var metrics Metrics
	if err := json.Unmarshal(rec.Body.Bytes(), &metrics); err != nil {
		t.Fatal(err)
	}
	if metrics.Hostname != "alpha-host" {
		t.Errorf("hostname = %q, want the peer's", metrics.Hostname)
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != contentSecurityPolicy {
		t.Errorf("relayed response CSP = %q, want this agent's own headers", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

func TestRelayRefusesUnknownHostsAndRoutes(t *testing.T) {
	alpha := newPeerAgent(t, "alpha-instance", "alpha")
	handler := newHomeHandler(t, configuredRegistry(t, "alpha", alpha.server))
	for _, path := range []string{
		"/api/hosts/nobody/metrics",
		"/api/hosts/local/metrics",
		"/api/hosts/alpha/settings",
		"/api/hosts/alpha/client-checks",
		"/api/hosts/alpha/update",
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/hosts/alpha/update", nil))
	if rec.Code == http.StatusOK || rec.Code == http.StatusAccepted {
		t.Errorf("POST relayed update = %d, want it refused", rec.Code)
	}
}

func TestRelayNamesAnUnreachablePeer(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	handler := newHomeHandler(t, newTestRegistry(t, []peerConfig{{Name: "gone", Base: mustParseURL(t, deadURL)}}, nil))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/hosts/gone/status", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	// "connection refused" on both OSes. Windows' own WSAECONNREFUSED text is a
	// whole sentence, which the host picker would cut off mid-word.
	if body["error"] != "gone unreachable: connection refused" {
		t.Errorf("error = %q, want the host name and a short cause without the URL", body["error"])
	}
}

func relayControlRequest(id, body, origin string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "https://home.tailnet.example:9443/api/hosts/"+id+"/control", strings.NewReader(body))
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	return req
}

func TestRelayControlPassesThePeersOwnOriginCheck(t *testing.T) {
	alpha := newPeerAgent(t, "alpha-instance", "alpha")
	handler := newHomeHandler(t, configuredRegistry(t, "alpha", alpha.server))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, relayControlRequest("alpha", `{"action":"lock_screen"}`, "https://home.tailnet.example:9443"))
	if rec.Code != http.StatusOK {
		t.Fatalf("relayed control = %d: %s", rec.Code, rec.Body.String())
	}
	if alpha.controller.calls != 1 || alpha.controller.gotAction != "lock_screen" {
		t.Fatalf("peer controller calls = %d action = %q", alpha.controller.calls, alpha.controller.gotAction)
	}
	var result ControlResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || !result.Applied {
		t.Fatalf("result = %+v (%v)", result, err)
	}
}

func TestRelayControlIsCheckedAndValidatedBeforeReachingThePeer(t *testing.T) {
	alpha := newPeerAgent(t, "alpha-instance", "alpha")
	handler := newHomeHandler(t, configuredRegistry(t, "alpha", alpha.server))
	cases := []struct {
		name   string
		body   string
		origin string
		want   int
	}{
		{"no origin", `{"action":"lock_screen"}`, "", http.StatusForbidden},
		{"cross-site origin", `{"action":"lock_screen"}`, "https://evil.example", http.StatusForbidden},
		{"unknown action", `{"action":"shutdown"}`, "https://home.tailnet.example:9443", http.StatusBadRequest},
		{"extra field", `{"action":"lock_screen","target":"x"}`, "https://home.tailnet.example:9443", http.StatusBadRequest},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, relayControlRequest("alpha", c.body, c.origin))
		if rec.Code != c.want {
			t.Errorf("%s: status = %d, want %d", c.name, rec.Code, c.want)
		}
	}
	if alpha.controller.calls != 0 {
		t.Fatalf("peer controller reached %d times by rejected requests", alpha.controller.calls)
	}
}

func TestRelayStreamPassesEventsThrough(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/status" {
			writeJSON(w, http.StatusOK, map[string]string{"dashboard_build": dashboardBuild, "instance_id": "streamer"})
			return
		}
		if r.URL.Path != "/api/stream" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i := 1; i <= 3; i++ {
			fmt.Fprintf(w, "data: {\"seq\":%d}\n\n", i)
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(peer.Close)
	home := httptest.NewServer(newHomeHandler(t, configuredRegistry(t, "streamer", peer)))
	t.Cleanup(home.Close)

	response, err := http.Get(home.URL + "/api/hosts/streamer/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream = %d %q", response.StatusCode, response.Header.Get("Content-Type"))
	}
	body, err := io.ReadAll(response.Body)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	want := "data: {\"seq\":1}\n\ndata: {\"seq\":2}\n\ndata: {\"seq\":3}\n\n"
	if string(body) != want {
		t.Fatalf("relayed stream = %q, want %q", body, want)
	}
}

// An agent without /api/stream must make the relayed stream fail with a non-200,
// so the browser's EventSource closes and the dashboard falls back to polling.
func TestRelayStreamPassesAMissingStreamThrough(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/status" {
			writeJSON(w, http.StatusOK, map[string]string{"dashboard_build": dashboardBuild, "instance_id": "old"})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(peer.Close)
	handler := newHomeHandler(t, configuredRegistry(t, "old", peer))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/hosts/old/stream", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("stream from an agent without one = %d, want the peer's 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json for the error body", got)
	}
}
