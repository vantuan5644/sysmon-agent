package main

// Host switching. The dashboard is a Home Screen app pinned to one agent's
// origin, and leaving that origin inside an iOS standalone app drops the user
// into a Safari sheet, so the app cannot simply navigate to another agent. It
// also cannot fetch another agent directly: an app served over Tailscale Serve
// HTTPS may not read a plain-HTTP agent (mixed content), and the CSP pins
// connect-src to 'self'. So the agent that serves the app relays: the client
// names a host by id, and this agent forwards a fixed set of routes to the peer
// it knows under that id. Addresses come only from the agent's own list, so
// the relay can reach the agents it found and nothing else.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// localHostID names the agent serving the dashboard. Reserved, so a peer
	// called "local" gets "local-2".
	localHostID = "local"

	// peerScanTTL bounds how stale the host list may get while dashboards are
	// open. Scans are demand-driven: the tailnet is only contacted when a
	// dashboard asks for the host list or names a host the agent has not seen.
	peerScanTTL = 5 * time.Minute
	// peerRescanMinInterval rate-limits explicit rescans (opening the host
	// picker) and lookups of unknown ids.
	peerRescanMinInterval = 10 * time.Second
	peerScanTimeout       = 5 * time.Second
	peerCheckTimeout      = 2500 * time.Millisecond
	// peerListWait is how long a request that needs a scan waits for it. A
	// slower scan keeps running and the response says so (scanning:true).
	peerListWait         = 3 * time.Second
	peerCheckConcurrency = 16

	peerRelayMaxBody        = 8 << 20
	peerRelayMetricsTimeout = 4 * time.Second
	peerRelayReadTimeout    = 4500 * time.Millisecond
	peerRelayControlTimeout = controlTimeout + time.Second
	// peerStreamIdleLimit ends a relayed stream whose peer has gone silent. A
	// live agent writes a keepalive every streamKeepaliveInterval even when
	// nothing changes, so three missed keepalives is a dead peer.
	peerStreamIdleLimit = 3 * streamKeepaliveInterval

	defaultPeerHTTPPort  = 9099
	defaultPeerHTTPSPort = 9443
)

// peerRelayRoutes are the read routes a dashboard may fetch from a peer. The
// stream and control relays are registered separately. Settings, client
// checks and self-update are deliberately absent: those belong to the agent
// serving the app (they describe the viewing device, or replace this binary).
var peerRelayRoutes = map[string]string{
	"metrics":     "/api/metrics",
	"status":      "/api/status",
	"quota":       "/api/quota",
	"codex-usage": "/api/codex-usage",
}

// agentInstanceID identifies this agent process. Discovery uses it to drop an
// entry that is this agent itself (the same -peers list deployed to every
// host), and to merge one agent reached under two addresses.
var agentInstanceID = newAgentInstanceID()

func newAgentInstanceID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("pid-%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// HostEntry is one row of the host picker.
type HostEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Self marks the agent serving the dashboard, which answers on its plain
	// /api/* routes.
	Self bool `json:"self,omitempty"`
	// Source is "local", "configured" (-peers) or "tailscale" (discovered).
	Source         string `json:"source"`
	Online         bool   `json:"online"`
	OS             string `json:"os,omitempty"`
	Hostname       string `json:"hostname,omitempty"`
	Version        string `json:"version,omitempty"`
	DashboardBuild string `json:"dashboard_build,omitempty"`
	Address        string `json:"address,omitempty"`
	// Pointers, so a time that never happened is omitted. A zero time.Time
	// serializes as year 1, which the picker would render as
	// "seen 17000000h ago".
	CheckedAt *time.Time `json:"checked_at,omitempty"`
	LastSeen  *time.Time `json:"last_seen,omitempty"`
	Error     string     `json:"error,omitempty"`
}

type PeerDiscoveryStatus struct {
	Enabled bool `json:"enabled"`
	// Available reports whether the last scan could read `tailscale status`.
	Available bool       `json:"available"`
	Error     string     `json:"error,omitempty"`
	Scanning  bool       `json:"scanning"`
	ScannedAt *time.Time `json:"scanned_at,omitempty"`
}

type HostsResponse struct {
	Hosts     []HostEntry         `json:"hosts"`
	Discovery PeerDiscoveryStatus `json:"discovery"`
}

// peerConfig is one -peers entry: an explicit name (optional) and the base URL
// of the peer agent.
type peerConfig struct {
	Name string
	Base *url.URL
}

// parsePeerList parses -peers / SYSMON_PEERS: entries separated by commas or
// whitespace, each "[name=]URL", e.g.
// "minipc=https://minipc.example.ts.net:9443, http://10.0.0.5:9099". The URL
// must be a bare http(s) origin: no path, query, fragment or credentials, since
// the relay appends its own fixed routes.
func parsePeerList(raw string) ([]peerConfig, error) {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	peers := make([]peerConfig, 0, len(fields))
	for _, field := range fields {
		name, rawURL := "", field
		if before, after, ok := strings.Cut(field, "="); ok && !strings.Contains(before, "://") {
			name, rawURL = strings.TrimSpace(before), strings.TrimSpace(after)
			if name == "" {
				return nil, fmt.Errorf("peer %q: empty name before '='", field)
			}
		}
		base, err := url.Parse(rawURL)
		if err != nil {
			return nil, fmt.Errorf("peer %q: %v", field, err)
		}
		if base.Scheme != "http" && base.Scheme != "https" {
			return nil, fmt.Errorf("peer %q: URL must start with http:// or https://", field)
		}
		if base.Hostname() == "" {
			return nil, fmt.Errorf("peer %q: URL has no host", field)
		}
		if base.User != nil || base.RawQuery != "" || base.Fragment != "" || strings.Trim(base.Path, "/") != "" {
			return nil, fmt.Errorf("peer %q: URL must be a bare origin such as http://host:9099", field)
		}
		base = &url.URL{Scheme: base.Scheme, Host: base.Host}
		peers = append(peers, peerConfig{Name: name, Base: base})
	}
	return peers, nil
}

type peerRegistryOptions struct {
	Configured []peerConfig
	Discover   bool
}

// peerRegistry owns the host list and the relay.
type peerRegistry struct {
	configured []peerConfig
	discover   bool
	client     *http.Client
	// Injectable for tests; the defaults run `tailscale status --json` and
	// try the canonical ports.
	listTailnet  func(context.Context) (string, []tailscalePeer, error)
	tailnetBases func(tailscalePeer) []*url.URL
	now          func() time.Time

	mu      sync.Mutex
	records []*peerRecord
	byID    map[string]*peerRecord
	// known keeps every record ever listed, by key, so a discovered agent that
	// drops off the tailnet stays in the picker as offline, and keeps its id
	// for when it comes back.
	known            map[string]*peerRecord
	selfName         string
	scannedAt        time.Time
	scan             chan struct{}
	tailnetAvailable bool
	tailnetErr       string
}

type peerRecord struct {
	entry HostEntry
	base  *url.URL
	// key is stable across scans: "cfg:<index>" or "ts:<machine name>".
	key string
}

func newPeerRegistry(options peerRegistryOptions) *peerRegistry {
	transport := &http.Transport{
		// Tailnet and LAN peers only: an HTTP(S)_PROXY meant for the internet
		// would break them.
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 4 * time.Second,
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
	}
	return &peerRegistry{
		configured: options.Configured,
		discover:   options.Discover,
		client: &http.Client{
			Transport: transport,
			// A redirect could point the relay anywhere; peers never send one.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		listTailnet:  readTailscalePeers,
		tailnetBases: defaultTailnetBases,
		now:          time.Now,
		byID:         map[string]*peerRecord{},
		known:        map[string]*peerRecord{},
	}
}

// defaultTailnetBases lists where a tailnet peer's agent may answer, in order
// of preference: the canonical Tailscale Serve HTTPS port first, then the
// agent's own plain-HTTP port. Both are tried at once and the first one in
// this order that answers wins.
func defaultTailnetBases(peer tailscalePeer) []*url.URL {
	var bases []*url.URL
	if peer.DNSName != "" {
		bases = append(bases, &url.URL{Scheme: "https", Host: net.JoinHostPort(peer.DNSName, strconv.Itoa(defaultPeerHTTPSPort))})
	}
	if peer.IPv4 != "" {
		bases = append(bases, &url.URL{Scheme: "http", Host: net.JoinHostPort(peer.IPv4, strconv.Itoa(defaultPeerHTTPPort))})
	}
	return bases
}

func (r *peerRegistry) active() bool {
	return r != nil && (len(r.configured) > 0 || r.discover)
}

// Hosts returns the host list. A first call, or refresh, waits (bounded) for a
// scan; otherwise a stale list is returned at once while a scan refreshes it
// in the background.
func (r *peerRegistry) Hosts(ctx context.Context, refresh bool) HostsResponse {
	if !r.active() {
		return localOnlyHostsResponse(false)
	}
	r.mu.Lock()
	age := r.now().Sub(r.scannedAt)
	never := r.scannedAt.IsZero()
	var done chan struct{}
	if never || age > peerScanTTL || (refresh && age > peerRescanMinInterval) {
		done = r.startScanLocked()
	}
	wait := never || refresh
	if wait && done == nil {
		done = r.scan
	}
	r.mu.Unlock()
	if wait && done != nil {
		waitForScan(ctx, done)
	}
	return r.snapshot()
}

func waitForScan(ctx context.Context, done <-chan struct{}) {
	timer := time.NewTimer(peerListWait)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	case <-ctx.Done():
	}
}

func (r *peerRegistry) startScanLocked() chan struct{} {
	if r.scan != nil {
		return r.scan
	}
	done := make(chan struct{})
	r.scan = done
	go func() {
		r.runScan()
		r.mu.Lock()
		r.scan = nil
		r.mu.Unlock()
		close(done)
	}()
	return done
}

func (r *peerRegistry) snapshot() HostsResponse {
	r.mu.Lock()
	defer r.mu.Unlock()
	hosts := make([]HostEntry, 0, len(r.records)+1)
	hosts = append(hosts, localHostEntry(r.selfName))
	for _, record := range r.records {
		hosts = append(hosts, record.entry)
	}
	discovery := PeerDiscoveryStatus{
		Enabled:   r.discover,
		Available: r.tailnetAvailable,
		Error:     r.tailnetErr,
		Scanning:  r.scan != nil,
	}
	if !r.scannedAt.IsZero() {
		scannedAt := r.scannedAt
		discovery.ScannedAt = &scannedAt
	}
	return HostsResponse{Hosts: hosts, Discovery: discovery}
}

func localOnlyHostsResponse(discover bool) HostsResponse {
	return HostsResponse{
		Hosts:     []HostEntry{localHostEntry("")},
		Discovery: PeerDiscoveryStatus{Enabled: discover},
	}
}

func localHostEntry(tailnetName string) HostEntry {
	hostname, _ := os.Hostname()
	name := tailnetName
	if name == "" {
		name = strings.ToLower(hostname)
	}
	if name == "" {
		name = "this host"
	}
	return HostEntry{
		ID:             localHostID,
		Name:           name,
		Self:           true,
		Source:         "local",
		Online:         true,
		OS:             runtime.GOOS,
		Hostname:       hostname,
		Version:        version,
		DashboardBuild: dashboardBuild,
	}
}

type peerCandidate struct {
	key      string
	name     string
	explicit bool
	source   string
	bases    []*url.URL
}

type peerCheckResult struct {
	base   *url.URL
	status peerIdentity
	err    error
}

// peerIdentity is the slice of a peer's /api/status that identifies it.
type peerIdentity struct {
	DashboardBuild string `json:"dashboard_build"`
	OS             string `json:"os"`
	Version        string `json:"version"`
	Hostname       string `json:"hostname"`
	InstanceID     string `json:"instance_id"`
}

var errNotSysmonAgent = errors.New("not a sysmon agent")

func (r *peerRegistry) runScan() {
	ctx, cancel := context.WithTimeout(context.Background(), peerScanTimeout)
	defer cancel()

	candidates := make([]peerCandidate, 0, len(r.configured))
	for i, peer := range r.configured {
		candidates = append(candidates, peerCandidate{
			key:      fmt.Sprintf("cfg:%d", i),
			name:     peer.Name,
			explicit: peer.Name != "",
			source:   "configured",
			bases:    []*url.URL{peer.Base},
		})
	}
	selfName, tailnetAvailable, tailnetErr := "", false, ""
	if r.discover {
		name, peers, err := r.listTailnet(ctx)
		if err != nil {
			tailnetErr = err.Error()
		} else {
			selfName, tailnetAvailable = name, true
			sort.Slice(peers, func(i, j int) bool { return peers[i].Name < peers[j].Name })
			for _, peer := range peers {
				// A configured entry already names this machine (by tailnet
				// IP or MagicDNS name). Checking it twice would list it twice
				// for agents too old to report an instance id.
				if r.configuredNames(peer) {
					continue
				}
				candidates = append(candidates, peerCandidate{
					key:    "ts:" + peer.Name,
					name:   peer.Name,
					source: "tailscale",
					bases:  r.tailnetBases(peer),
				})
			}
		}
	}

	results := r.checkCandidates(ctx, candidates)
	r.applyScan(candidates, results, selfName, tailnetAvailable, tailnetErr)
}

func (r *peerRegistry) configuredNames(peer tailscalePeer) bool {
	for _, configured := range r.configured {
		host := strings.ToLower(strings.TrimSuffix(configured.Base.Hostname(), "."))
		if host == peer.IPv4 || host == peer.DNSName || host == peer.Name {
			return true
		}
	}
	return false
}

func (r *peerRegistry) checkCandidates(ctx context.Context, candidates []peerCandidate) []peerCheckResult {
	results := make([]peerCheckResult, len(candidates))
	semaphore := make(chan struct{}, peerCheckConcurrency)
	var wg sync.WaitGroup
	for i, candidate := range candidates {
		perBase := make([]peerCheckResult, len(candidate.bases))
		var candidateWG sync.WaitGroup
		for j, base := range candidate.bases {
			candidateWG.Add(1)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer candidateWG.Done()
				semaphore <- struct{}{}
				defer func() { <-semaphore }()
				status, err := r.identify(ctx, base)
				perBase[j] = peerCheckResult{base: base, status: status, err: err}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			candidateWG.Wait()
			results[i] = pickCheckResult(perBase)
		}()
	}
	wg.Wait()
	return results
}

// pickCheckResult takes the first base, in preference order, that answered as
// an agent; failing that, the first error, which describes the preferred base.
func pickCheckResult(perBase []peerCheckResult) peerCheckResult {
	for _, result := range perBase {
		if result.err == nil {
			return result
		}
	}
	if len(perBase) == 0 {
		return peerCheckResult{err: errors.New("no address to try")}
	}
	return perBase[0]
}

func (r *peerRegistry) identify(ctx context.Context, base *url.URL) (peerIdentity, error) {
	ctx, cancel := context.WithTimeout(ctx, peerCheckTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, peerURL(base, "/api/status"), nil)
	if err != nil {
		return peerIdentity{}, err
	}
	request.Header.Set("Accept", "application/json")
	response, err := r.client.Do(request)
	if err != nil {
		return peerIdentity{}, errors.New(relayErrorReason(err))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return peerIdentity{}, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	var status peerIdentity
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&status); err != nil {
		return peerIdentity{}, errNotSysmonAgent
	}
	if !strings.HasPrefix(status.DashboardBuild, "sysmon-static-") {
		return peerIdentity{}, errNotSysmonAgent
	}
	return status, nil
}

func (r *peerRegistry) applyScan(candidates []peerCandidate, results []peerCheckResult, selfName string, tailnetAvailable bool, tailnetErr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()

	seenInstances := map[string]bool{}
	inScan := map[string]bool{}
	var configured, discovered []*peerRecord
	for i, candidate := range candidates {
		result := results[i]
		inScan[candidate.key] = true
		if result.err == nil && result.status.InstanceID != "" {
			if result.status.InstanceID == agentInstanceID || seenInstances[result.status.InstanceID] {
				continue
			}
			seenInstances[result.status.InstanceID] = true
		}
		record := r.known[candidate.key]
		if record == nil {
			// Only an address that answered as an agent at least once becomes
			// a discovered host; the rest of the tailnet is not listed.
			if candidate.source == "tailscale" && result.err != nil {
				continue
			}
			record = &peerRecord{key: candidate.key, base: candidate.bases[0]}
			r.known[candidate.key] = record
		}
		entry := &record.entry
		entry.Source = candidate.source
		entry.CheckedAt = &now
		if result.err == nil {
			record.base = result.base
			entry.Online = true
			entry.Error = ""
			entry.LastSeen = &now
			entry.OS = result.status.OS
			entry.Version = result.status.Version
			entry.DashboardBuild = result.status.DashboardBuild
			entry.Hostname = result.status.Hostname
		} else {
			entry.Online = false
			entry.Error = result.err.Error()
		}
		entry.Address = record.base.Host
		entry.Name = peerDisplayName(candidate, entry.Hostname, record.base)
		if candidate.source == "configured" {
			configured = append(configured, record)
		} else {
			discovered = append(discovered, record)
		}
	}
	// A discovered agent whose machine is now offline in the tailnet drops out
	// of the candidates, but it is still a host the user knows about.
	for key, record := range r.known {
		if inScan[key] || !strings.HasPrefix(key, "ts:") {
			continue
		}
		record.entry.Online = false
		record.entry.Error = "offline in tailnet"
		record.entry.CheckedAt = &now
		discovered = append(discovered, record)
	}
	sort.SliceStable(discovered, func(i, j int) bool { return discovered[i].entry.Name < discovered[j].entry.Name })

	records := append(configured, discovered...)
	assignPeerIDs(records)
	r.records = records
	r.byID = make(map[string]*peerRecord, len(records))
	for _, record := range records {
		r.byID[record.entry.ID] = record
	}
	if tailnetAvailable {
		r.selfName = selfName
	}
	r.tailnetAvailable = tailnetAvailable
	r.tailnetErr = tailnetErr
	r.scannedAt = now
}

func peerDisplayName(candidate peerCandidate, hostname string, base *url.URL) string {
	if candidate.explicit || candidate.source == "tailscale" {
		return candidate.name
	}
	if hostname = strings.TrimSpace(hostname); hostname != "" {
		return strings.ToLower(hostname)
	}
	host := base.Hostname()
	if net.ParseIP(host) == nil {
		host, _, _ = strings.Cut(host, ".")
	}
	return host
}

// assignPeerIDs gives every record a URL-safe id. A record keeps the id it
// already has whenever possible, so a dashboard's remembered selection
// survives rescans; new records derive theirs from the name.
func assignPeerIDs(records []*peerRecord) {
	used := map[string]bool{localHostID: true}
	for _, record := range records {
		if id := record.entry.ID; id != "" && !used[id] {
			used[id] = true
		} else {
			record.entry.ID = ""
		}
	}
	for _, record := range records {
		if record.entry.ID != "" {
			continue
		}
		base := peerIDSlug(record.entry.Name)
		id := base
		for n := 2; used[id]; n++ {
			id = fmt.Sprintf("%s-%d", base, n)
		}
		used[id] = true
		record.entry.ID = id
	}
}

func peerIDSlug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	slug := strings.TrimSuffix(b.String(), "-")
	if len(slug) > 48 {
		slug = strings.TrimSuffix(slug[:48], "-")
	}
	if slug == "" {
		return "host"
	}
	return slug
}

// peerTarget is a resolved relay destination.
type peerTarget struct {
	name string
	base url.URL
}

// resolve maps a client-supplied id to a known peer. An id the registry has
// not seen (the agent restarted while a dashboard remembered a host) triggers
// a bounded scan first, and only then a 404.
func (r *peerRegistry) resolve(ctx context.Context, id string) (peerTarget, bool) {
	if !r.active() || id == "" || id == localHostID {
		return peerTarget{}, false
	}
	r.mu.Lock()
	if record := r.byID[id]; record != nil {
		target := peerTarget{name: record.entry.Name, base: *record.base}
		r.mu.Unlock()
		return target, true
	}
	var done chan struct{}
	if r.scannedAt.IsZero() || r.now().Sub(r.scannedAt) > peerRescanMinInterval {
		done = r.startScanLocked()
	} else {
		done = r.scan
	}
	r.mu.Unlock()
	if done == nil {
		return peerTarget{}, false
	}
	waitForScan(ctx, done)
	r.mu.Lock()
	defer r.mu.Unlock()
	if record := r.byID[id]; record != nil {
		return peerTarget{name: record.entry.Name, base: *record.base}, true
	}
	return peerTarget{}, false
}

func peerURL(base *url.URL, path string) string {
	target := url.URL{Scheme: base.Scheme, Host: base.Host, Path: path}
	return target.String()
}

func peerOrigin(base url.URL) string {
	return base.Scheme + "://" + base.Host
}

// relayErrorReason condenses a transport error to its cause, short enough for
// one line of the host picker. The full error repeats the method and peer URL,
// and on Windows a refused dial is a whole sentence ("No connection could be
// made because the target machine actively refused it.").
func relayErrorReason(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timed out"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "name not resolved"
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return "TLS certificate rejected"
	}
	switch lower := strings.ToLower(err.Error()); {
	case strings.Contains(lower, "refused"):
		return "connection refused"
	case strings.Contains(lower, "no route") || strings.Contains(lower, "unreachable"):
		return "no route to host"
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		err = urlErr.Err
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Err != nil {
		return opErr.Op + ": " + opErr.Err.Error()
	}
	return err.Error()
}

func writeRelayError(w http.ResponseWriter, target peerTarget, err error) {
	status := http.StatusBadGateway
	if errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusGatewayTimeout
	}
	writeJSON(w, status, map[string]string{"error": target.name + " unreachable: " + relayErrorReason(err)})
}

func writeUnknownHost(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown host"})
}

// serveRelayRead forwards one allow-listed GET route and passes the peer's
// JSON body and status code through. Upstream headers are not copied: this
// agent's own security headers and no-store apply to the relayed body too.
func (r *peerRegistry) serveRelayRead(w http.ResponseWriter, req *http.Request) {
	path, ok := peerRelayRoutes[req.PathValue("route")]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "route is not relayed"})
		return
	}
	target, ok := r.resolve(req.Context(), req.PathValue("id"))
	if !ok {
		writeUnknownHost(w)
		return
	}
	timeout := peerRelayReadTimeout
	if path == "/api/metrics" {
		timeout = peerRelayMetricsTimeout
	}
	ctx, cancel := context.WithTimeout(req.Context(), timeout)
	defer cancel()
	upstream, err := http.NewRequestWithContext(ctx, http.MethodGet, peerURL(&target.base, path), nil)
	if err != nil {
		writeRelayError(w, target, err)
		return
	}
	upstream.Header.Set("Accept", "application/json")
	r.forwardJSON(w, upstream, target)
}

// serveRelayControl forwards a host-control action. The request is validated
// here exactly as /api/control validates it, so nothing malformed reaches the
// peer, and it must pass this agent's same-origin check first. The forwarded
// request carries the peer's own origin: the peer's check exists to stop
// cross-site browser requests, and this one was already checked here.
func (r *peerRegistry) serveRelayControl(w http.ResponseWriter, req *http.Request) {
	if !sameOriginBrowserRequest(req) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "control actions require same-origin browser requests"})
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, 8<<10)
	defer req.Body.Close()
	var request ControlRequest
	decoder := json.NewDecoder(req.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request body must contain a single JSON object"})
		return
	}
	if !isKnownControlAction(request.Action) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "action must be one of mic_mute, camera_toggle, media_toggle, volume_mute, lock_screen"})
		return
	}
	target, ok := r.resolve(req.Context(), req.PathValue("id"))
	if !ok {
		writeUnknownHost(w)
		return
	}
	body, _ := json.Marshal(ControlRequest{Action: request.Action})
	ctx, cancel := context.WithTimeout(req.Context(), peerRelayControlTimeout)
	defer cancel()
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, peerURL(&target.base, "/api/control"), bytes.NewReader(body))
	if err != nil {
		writeRelayError(w, target, err)
		return
	}
	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("Accept", "application/json")
	upstream.Header.Set("Origin", peerOrigin(target.base))
	r.forwardJSON(w, upstream, target)
}

func (r *peerRegistry) forwardJSON(w http.ResponseWriter, upstream *http.Request, target peerTarget) {
	response, err := r.client.Do(upstream)
	if err != nil {
		writeRelayError(w, target, err)
		return
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, peerRelayMaxBody+1))
	if err != nil {
		writeRelayError(w, target, err)
		return
	}
	if len(body) > peerRelayMaxBody {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": target.name + " sent an oversized response"})
		return
	}
	// A peer's mux answers an unknown route with a text/plain 404. Re-wrap
	// anything that is not JSON so the dashboard always parses an object.
	if !json.Valid(body) {
		writeJSON(w, response.StatusCode, map[string]string{"error": fmt.Sprintf("%s: HTTP %d", target.name, response.StatusCode)})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(body)
}

// serveRelayStream relays a peer's SSE stream byte for byte. The peer's
// response status passes through, so an agent without /api/stream answers
// non-200, the browser's EventSource closes, and the dashboard falls back to
// polling the relayed /metrics, exactly as it does against an old local agent.
func (r *peerRegistry) serveRelayStream(w http.ResponseWriter, req *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
		return
	}
	target, ok := r.resolve(req.Context(), req.PathValue("id"))
	if !ok {
		writeUnknownHost(w)
		return
	}
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	upstream, err := http.NewRequestWithContext(ctx, http.MethodGet, peerURL(&target.base, "/api/stream"), nil)
	if err != nil {
		writeRelayError(w, target, err)
		return
	}
	upstream.Header.Set("Accept", "text/event-stream")
	// Bound the wait for response headers only; the body is unbounded.
	headerTimer := time.AfterFunc(peerRelayReadTimeout, cancel)
	response, err := r.client.Do(upstream)
	headerTimer.Stop()
	if err != nil {
		if ctx.Err() != nil && req.Context().Err() == nil {
			err = context.DeadlineExceeded
		}
		writeRelayError(w, target, err)
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		status := response.StatusCode
		if status == http.StatusOK {
			status = http.StatusBadGateway
		}
		writeJSON(w, status, map[string]string{"error": fmt.Sprintf("%s: stream unavailable (HTTP %d)", target.name, response.StatusCode)})
		return
	}

	// Same per-write deadline as the local stream handler: the server-wide
	// WriteTimeout would end the stream, and no deadline at all would let a
	// vanished viewer pin this goroutine and the upstream connection.
	rc := http.NewResponseController(w)
	armWriteDeadline := func() {
		_ = rc.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
	}
	armWriteDeadline()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	chunks := make(chan []byte)
	go func() {
		defer close(chunks)
		buffer := make([]byte, 32<<10)
		for {
			n, err := response.Body.Read(buffer)
			if n > 0 {
				chunk := append([]byte(nil), buffer[:n]...)
				select {
				case chunks <- chunk:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	idle := time.NewTimer(peerStreamIdleLimit)
	defer idle.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-idle.C:
			return
		case chunk, ok := <-chunks:
			if !ok {
				return
			}
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(peerStreamIdleLimit)
			armWriteDeadline()
			if _, err := w.Write(chunk); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
