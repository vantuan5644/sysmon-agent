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

	// peerLockedStatus answers a relayed request to a peer that requires
	// pairing and holds no token from this agent. A plain 401 would read to
	// the dashboard as "this agent requires pairing" and send it to its own
	// PIN screen.
	peerLockedStatus = http.StatusLocked
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
	"history":     "/api/history",
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
	// Locked is an agent that requires pairing and holds no device for this
	// one. It answers only /api/pair/state, so it is listed without its OS or
	// hostname, and choosing it asks for its PIN (POST /api/hosts/{id}/pair).
	Locked bool `json:"locked,omitempty"`
	// CertSHA256 is the self-signed certificate a locked peer presented. The
	// PIN form shows it, to compare with what `sysmon-agent -pair` prints on
	// that host, and pairing pins it.
	CertSHA256 string `json:"cert_sha256,omitempty"`
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
	// Tokens holds what this agent was given by peers that require pairing.
	// Nil keeps them in memory.
	Tokens *peerTokenStore
}

// peerRegistry owns the host list and the relay.
type peerRegistry struct {
	configured []peerConfig
	discover   bool
	client     *http.Client
	tokens     *peerTokenStore
	// pinned holds one client per pinned certificate, each on a transport of
	// its own, so a connection opened under one pin is never reused for a
	// peer pinned to another, or for a verified one.
	pinnedMu sync.Mutex
	pinned   map[string]*http.Client
	// newUnverifiedClient reads a locked peer's open /api/pair/state over a
	// certificate nobody vouches for yet, recording what it presented. Only
	// that route goes over it, and never with a token.
	newUnverifiedClient func(record func(fingerprint string)) *http.Client
	// relayName is the device name this agent pairs under on a peer.
	relayName string
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
	// tokenKey names the peer in peers.json: "ts:<machine name>", or
	// "url:<origin>" for a -peers entry, which survives a reordered list.
	tokenKey string
	// pin is the certificate base was reached with, when it is the
	// self-signed one this agent paired the peer over.
	pin string
	// pairedAt keeps a scan that started before a pairing from marking the
	// peer locked again with what it saw before the token existed.
	pairedAt time.Time
}

func (record *peerRecord) target() peerTarget {
	return peerTarget{
		id:         record.entry.ID,
		name:       record.entry.Name,
		base:       *record.base,
		tokenKey:   record.tokenKey,
		pin:        record.pin,
		locked:     record.entry.Locked,
		certSHA256: record.entry.CertSHA256,
	}
}

func newPeerTransport(tlsConfig *tls.Config) *http.Transport {
	return &http.Transport{
		// Tailnet and LAN peers only: an HTTP(S)_PROXY meant for the internet
		// would break them.
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:     tlsConfig,
		TLSHandshakeTimeout: 4 * time.Second,
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
	}
}

func newPeerClient(transport http.RoundTripper) *http.Client {
	return &http.Client{
		Transport: transport,
		// A redirect could point the relay anywhere. The one a peer sends (an
		// agent serving its network over HTTPS) is followed by identify, by
		// hand, and only to https on the same host.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func newPeerRegistry(options peerRegistryOptions) *peerRegistry {
	tokens := options.Tokens
	if tokens == nil {
		tokens, _ = newPeerTokenStore("")
	}
	return &peerRegistry{
		configured:          options.Configured,
		discover:            options.Discover,
		client:              newPeerClient(newPeerTransport(nil)),
		tokens:              tokens,
		pinned:              map[string]*http.Client{},
		newUnverifiedClient: newUnverifiedPeerClient,
		relayName:           defaultRelayName(),
		listTailnet:         readTailscalePeers,
		tailnetBases:        defaultTailnetBases,
		now:                 time.Now,
		byID:                map[string]*peerRecord{},
		known:               map[string]*peerRecord{},
	}
}

// newUnverifiedPeerClient makes a one-request client that accepts any
// certificate and reports the one it saw. Keep-alives are off, so no
// connection it opened can carry a later request.
func newUnverifiedPeerClient(record func(fingerprint string)) *http.Client {
	transport := newPeerTransport(&tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, //nolint:gosec // reads the open pairing state only; see the field comment
		VerifyConnection: func(state tls.ConnectionState) error {
			record(leafFingerprint(state))
			return nil
		},
	})
	transport.DisableKeepAlives = true
	return newPeerClient(transport)
}

// pinnedClient is the client for a peer pinned to one certificate.
func (r *peerRegistry) pinnedClient(fingerprint string) *http.Client {
	key := normalizeFingerprint(fingerprint)
	r.pinnedMu.Lock()
	defer r.pinnedMu.Unlock()
	if client := r.pinned[key]; client != nil {
		return client
	}
	client := newPeerClient(newPeerTransport(pinnedTLSConfig(key)))
	r.pinned[key] = client
	return client
}

// clientForPin is the client for one base of a peer: pinned when that base
// serves the self-signed certificate this agent paired it over, verified
// otherwise. The pin belongs to the base, since the same peer behind
// Tailscale Serve presents a certificate the system trusts.
func (r *peerRegistry) clientForPin(pin string) *http.Client {
	if pin == "" {
		return r.client
	}
	return r.pinnedClient(pin)
}

// peerTokenTransportOK is where a token may be sent: over TLS, or to this
// machine. Never across the network in plain HTTP, where it could be copied.
func peerTokenTransportOK(base *url.URL) bool {
	return base.Scheme == "https" || isLoopbackBind(base.Hostname())
}

// authorize adds this agent's token for the peer to an outgoing request and
// returns the token it sent, or "" when it sent none.
func (r *peerRegistry) authorize(request *http.Request, base *url.URL, tokenKey string) string {
	token, ok := r.tokens.Get(tokenKey)
	if !ok || !peerTokenTransportOK(base) {
		return ""
	}
	request.Header.Set("Authorization", "Bearer "+token.Token)
	return token.Token
}

func defaultRelayName() string {
	name, err := os.Hostname()
	if err != nil || strings.TrimSpace(name) == "" {
		return "sysmon relay"
	}
	name, _, _ = strings.Cut(strings.ToLower(strings.TrimSpace(name)), ".")
	if runes := []rune(name); len(runes) > 40 {
		name = string(runes[:40])
	}
	return name + " relay"
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
	tokenKey string
	name     string
	explicit bool
	source   string
	bases    []*url.URL
}

type peerCheckResult struct {
	// base is where the peer answered, which is the https:// form of the
	// address tried when the peer redirected there.
	base   *url.URL
	status peerIdentity
	err    error
	// locked: the peer requires pairing and holds no device for this agent.
	locked bool
	// certSHA256 is a locked peer's self-signed certificate; pin is the
	// certificate an unlocked base was reached with, when it is self-signed.
	certSHA256 string
	pin        string
	// note goes in the host's error line while it is still listed as online.
	note string
}

// redirectedToHTTPS is set when a plain-HTTP address answered with a
// redirect to the same address over https://.
type redirectedToHTTPS struct{ next *url.URL }

func (e redirectedToHTTPS) Error() string { return "redirected to " + e.next.String() }

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
			tokenKey: "url:" + peerOrigin(*peer.Base),
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
					key:      "ts:" + peer.Name,
					tokenKey: "ts:" + peer.Name,
					name:     peer.Name,
					source:   "tailscale",
					bases:    r.tailnetBases(peer),
				})
			}
		}
	}

	started := r.now()
	results := r.checkCandidates(ctx, candidates)
	r.applyScan(candidates, results, selfName, tailnetAvailable, tailnetErr, started)
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
				perBase[j] = r.identify(ctx, base, candidate.tokenKey)
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
// an agent, preferring one this agent can read over one that is locked;
// failing that, the first error, which describes the preferred base.
func pickCheckResult(perBase []peerCheckResult) peerCheckResult {
	for _, result := range perBase {
		if result.err == nil && !result.locked {
			return result
		}
	}
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

// identify asks one address whether a sysmon agent answers there. An agent
// that serves its network over HTTPS redirects a plain-HTTP address to itself
// over https://; that redirect is followed once, and to that address only.
func (r *peerRegistry) identify(ctx context.Context, base *url.URL, tokenKey string) peerCheckResult {
	result := r.identifyAt(ctx, base, tokenKey)
	var redirect redirectedToHTTPS
	if errors.As(result.err, &redirect) {
		result = r.identifyAt(ctx, redirect.next, tokenKey)
		if errors.As(result.err, &redirect) {
			result.err = errors.New("redirect loop")
		}
	}
	return result
}

func (r *peerRegistry) identifyAt(ctx context.Context, base *url.URL, tokenKey string) peerCheckResult {
	ctx, cancel := context.WithTimeout(ctx, peerCheckTimeout)
	defer cancel()
	result := peerCheckResult{base: base}
	client := r.client
	response, sent, err := r.getPeer(ctx, client, base, "/api/status", tokenKey)
	if err != nil && isCertificateError(err) {
		// A certificate the system does not trust: the self-signed one of an
		// agent serving its network over HTTPS. It is reached with the pin
		// when this agent paired it over that certificate; otherwise it is
		// only asked whether it requires pairing.
		token, paired := r.tokens.Get(tokenKey)
		if paired && token.CertSHA256 != "" {
			result.pin = token.CertSHA256
			client = r.pinnedClient(token.CertSHA256)
			response, sent, err = r.getPeer(ctx, client, base, "/api/status", tokenKey)
		}
		if err != nil && isCertificateError(err) {
			return r.identifyUnverified(ctx, base, paired && token.CertSHA256 != "")
		}
	}
	if err != nil {
		result.err = errors.New(relayErrorReason(err))
		return result
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return r.identifyLocked(ctx, client, result, tokenKey, sent)
	case http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		if next := httpsRedirectTarget(base, response.Header.Get("Location")); next != nil {
			result.err = redirectedToHTTPS{next: next}
			return result
		}
		result.err = fmt.Errorf("HTTP %d", response.StatusCode)
		return result
	default:
		result.err = fmt.Errorf("HTTP %d", response.StatusCode)
		return result
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result.status); err != nil {
		result.err = errNotSysmonAgent
		return result
	}
	if !strings.HasPrefix(result.status.DashboardBuild, "sysmon-static-") {
		result.status = peerIdentity{}
		result.err = errNotSysmonAgent
	}
	return result
}

// getPeer sends one GET, with this agent's token for the peer when it holds
// one and the address may carry it. It returns the token it sent.
func (r *peerRegistry) getPeer(ctx context.Context, client *http.Client, base *url.URL, path, tokenKey string) (*http.Response, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, peerURL(base, path), nil)
	if err != nil {
		return nil, "", err
	}
	request.Header.Set("Accept", "application/json")
	sent := r.authorize(request, base, tokenKey)
	response, err := client.Do(request)
	return response, sent, err
}

// identifyLocked reads the open /api/pair/state of a peer that refused
// /api/status. A sysmon agent that requires pairing is listed as locked, and
// a token it refused is dropped: the peer revoked it.
func (r *peerRegistry) identifyLocked(ctx context.Context, client *http.Client, result peerCheckResult, tokenKey, sent string) peerCheckResult {
	pairing, err := r.readPairingState(ctx, client, result.base)
	if err != nil || pairing.Pairing != pairingModeRequired || pairing.Paired {
		result.err = fmt.Errorf("HTTP %d", http.StatusUnauthorized)
		return result
	}
	if sent != "" {
		_ = r.tokens.Forget(tokenKey, sent)
	}
	result.locked = true
	result.status = peerIdentity{DashboardBuild: pairing.DashboardBuild}
	// Reached over the pinned certificate: that is the one to pair again.
	result.certSHA256, result.pin = result.pin, ""
	return result
}

// identifyUnverified reads /api/pair/state over a certificate nobody vouches
// for yet, sending nothing secret, and records the certificate the peer
// presented. Only an agent that requires pairing is listed (locked); the PIN
// form shows the certificate's SHA-256 to compare with `sysmon-agent -pair`
// on that host, and pairing pins it.
func (r *peerRegistry) identifyUnverified(ctx context.Context, base *url.URL, pinChanged bool) peerCheckResult {
	result := peerCheckResult{base: base}
	var mu sync.Mutex
	var seen string
	client := r.newUnverifiedClient(func(fingerprint string) {
		mu.Lock()
		seen = fingerprint
		mu.Unlock()
	})
	pairing, err := r.readPairingState(ctx, client, base)
	mu.Lock()
	fingerprint := seen
	mu.Unlock()
	if err != nil || pairing.Pairing != pairingModeRequired || pairing.Paired || fingerprint == "" {
		result.err = errors.New("TLS certificate rejected")
		return result
	}
	result.locked = true
	result.certSHA256 = fingerprint
	result.status = peerIdentity{DashboardBuild: pairing.DashboardBuild}
	if pinChanged {
		result.note = "certificate changed since it was paired"
	}
	return result
}

func (r *peerRegistry) readPairingState(ctx context.Context, client *http.Client, base *url.URL) (PairingState, error) {
	var pairing PairingState
	response, _, err := r.getPeer(ctx, client, base, "/api/pair/state", "")
	if err != nil {
		return pairing, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return pairing, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&pairing); err != nil {
		return pairing, errNotSysmonAgent
	}
	if !strings.HasPrefix(pairing.DashboardBuild, "sysmon-static-") {
		return pairing, errNotSysmonAgent
	}
	return pairing, nil
}

// httpsRedirectTarget accepts only a redirect from a plain-HTTP address to
// the same host and port over https://, which is what an agent serving its
// network over HTTPS sends. Anything else could point the scan anywhere.
func httpsRedirectTarget(base *url.URL, location string) *url.URL {
	if base.Scheme != "http" || location == "" {
		return nil
	}
	target, err := base.Parse(location)
	if err != nil || target.Scheme != "https" || !strings.EqualFold(target.Host, base.Host) {
		return nil
	}
	return &url.URL{Scheme: "https", Host: base.Host}
}

func (r *peerRegistry) applyScan(candidates []peerCandidate, results []peerCheckResult, selfName string, tailnetAvailable bool, tailnetErr string, started time.Time) {
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
		record.tokenKey = candidate.tokenKey
		entry := &record.entry
		entry.Source = candidate.source
		entry.CheckedAt = &now
		switch {
		case result.err == nil && result.locked && record.pairedAt.After(started):
			// Paired while this scan ran: it saw the peer before the token
			// existed. The next scan reads it with the token.
		case result.err == nil:
			record.base = result.base
			record.pin = result.pin
			entry.Online = true
			entry.Error = result.note
			entry.LastSeen = &now
			entry.DashboardBuild = result.status.DashboardBuild
			entry.Locked = result.locked
			entry.CertSHA256 = result.certSHA256
			// A locked peer names nothing about itself, so the last
			// description it gave while readable stays.
			if !result.locked {
				entry.OS = result.status.OS
				entry.Version = result.status.Version
				entry.Hostname = result.status.Hostname
			}
		default:
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
	id       string
	name     string
	base     url.URL
	tokenKey string
	// pin: base serves the self-signed certificate this agent paired over.
	pin string
	// locked and certSHA256 are what the last scan saw.
	locked     bool
	certSHA256 string
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
		target := record.target()
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
		return record.target(), true
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

// send forwards one relayed request with this agent's token for the peer,
// and returns the token it sent, so a refusal drops exactly that one.
func (r *peerRegistry) send(upstream *http.Request, target peerTarget) (*http.Response, string, error) {
	sent := r.authorize(upstream, &target.base, target.tokenKey)
	response, err := r.clientForPin(target.pin).Do(upstream)
	return response, sent, err
}

// writeSendError answers a relayed request that got no response. A locked
// peer behind its own self-signed certificate fails the TLS check before it
// can answer 401, so for a peer the last scan saw locked that is the same
// answer: it wants a PIN.
func (r *peerRegistry) writeSendError(w http.ResponseWriter, target peerTarget, err error) {
	if target.locked && isCertificateError(err) {
		r.writePeerLocked(w, target, "")
		return
	}
	writeRelayError(w, target, err)
}

// writePeerLocked answers for a peer that refused this agent with 423, so
// the dashboard asks for that host's PIN and never shows this agent's own
// pairing screen. A token the peer refused was revoked there and is dropped.
func (r *peerRegistry) writePeerLocked(w http.ResponseWriter, target peerTarget, sent string) {
	if sent != "" {
		_ = r.tokens.Forget(target.tokenKey, sent)
	}
	certSHA256 := target.certSHA256
	r.mu.Lock()
	if record := r.byID[target.id]; record != nil && record.tokenKey == target.tokenKey {
		record.entry.Locked = true
		// A peer reached over a pinned certificate is paired again over it.
		if record.entry.CertSHA256 == "" {
			record.entry.CertSHA256 = record.pin
		}
		certSHA256 = record.entry.CertSHA256
	}
	r.mu.Unlock()
	body := map[string]any{"error": target.name + " requires pairing", "host_locked": true}
	if certSHA256 != "" {
		body["cert_sha256"] = certSHA256
	}
	writeJSON(w, peerLockedStatus, body)
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
	response, sent, err := r.send(upstream, target)
	if err != nil {
		r.writeSendError(w, target, err)
		return
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		r.writePeerLocked(w, target, sent)
		return
	}
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
	response, sent, err := r.send(upstream, target)
	headerTimer.Stop()
	if err != nil {
		if ctx.Err() != nil && req.Context().Err() == nil {
			err = context.DeadlineExceeded
		}
		r.writeSendError(w, target, err)
		return
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		r.writePeerLocked(w, target, sent)
		return
	}
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
