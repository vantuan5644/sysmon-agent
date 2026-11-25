package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"
)

// Host allowlist: a guard against DNS rebinding.
//
// A page on evil.example can point its own name at this agent's address
// (127.0.0.1, or a LAN address) once the browser has loaded it. Its scripts
// then talk to the agent as same-origin requests to evil.example:9099, so the
// Origin check that guards the writes passes, and with pairing off every read
// and /api/control are open to it. What such a request cannot change is the
// Host header: it still names evil.example. So every request must name a host
// this agent answers to, or it gets 421 Misdirected Request.
//
// Allowed without configuration: IP literals, localhost and *.localhost,
// *.local (mDNS), *.ts.net (Tailscale), this machine's hostname (short and,
// once resolved, fully qualified), a -bind that is a name, and the -mdns-name
// label. Anything else is added with -allowed-hosts / SYSMON_ALLOWED_HOSTS.

// allowedHostsFlagName is the flag the 421 answer points to.
const allowedHostsFlagName = "-allowed-hosts"

// hostAllowlist decides which Host header values the agent answers. It is safe
// for concurrent use: the fully qualified hostname is added after start.
type hostAllowlist struct {
	mu       sync.RWMutex
	names    map[string]bool
	suffixes []string // ".example.org" for an entry "*.example.org"
}

// newHostAllowlist builds the allowlist from this machine's hostnames, a -bind
// value (used only when it is a name), the -mdns-name label and the entries of
// -allowed-hosts, already validated by parseAllowedHosts.
func newHostAllowlist(hostnames []string, bind, mdnsName string, extra []string) *hostAllowlist {
	a := &hostAllowlist{names: map[string]bool{}}
	for _, name := range hostnames {
		a.addHostname(name)
	}
	if bindHost := normalizeAllowHost(bind); bindHost != "" && !isIPLiteralHost(bindHost) {
		a.addName(bindHost)
	}
	if label := normalizeAllowHost(mdnsName); label != "" {
		a.addName(label)
		a.addName(label + ".local")
	}
	for _, entry := range extra {
		entry = normalizeAllowHost(entry)
		if suffix, ok := strings.CutPrefix(entry, "*."); ok {
			if suffix != "" {
				a.suffixes = append(a.suffixes, "."+suffix)
			}
			continue
		}
		a.addName(entry)
	}
	return a
}

func (a *hostAllowlist) addName(name string) {
	name = normalizeAllowHost(name)
	if name == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.names[name] = true
}

// addHostname adds this machine's hostname and, for a dotted one, its first
// label, since a hostname set to an FQDN is still reached by its short name.
func (a *hostAllowlist) addHostname(name string) {
	name = normalizeAllowHost(name)
	if name == "" {
		return
	}
	a.addName(name)
	if short, _, ok := strings.Cut(name, "."); ok && short != "" {
		a.addName(short)
	}
}

// Allows reports whether a Host (or forwarded host) value names this agent.
// The port, brackets, case and one trailing dot are ignored.
func (a *hostAllowlist) Allows(rawHost string) bool {
	host := normalizeAllowHost(rawHost)
	if host == "" {
		return false
	}
	if isIPLiteralHost(host) {
		return true
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".ts.net") {
		return true
	}
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.names[host] {
		return true
	}
	for _, suffix := range a.suffixes {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// normalizeAllowHost strips a port and IPv6 brackets, lowercases and drops
// one trailing dot: "Host.Example.:9099" -> "host.example", "[::1]:80" -> "::1".
func normalizeAllowHost(raw string) string {
	host := strings.TrimSpace(raw)
	if host == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return host
}

func isIPLiteralHost(host string) bool {
	// netip accepts a zone ("fe80::1%eth0"); a browser sends one as %25.
	host = strings.Replace(host, "%25", "%", 1)
	_, err := netip.ParseAddr(host)
	return err == nil
}

// parseAllowedHosts reads -allowed-hosts: comma-separated host names, each
// optionally starting with "*." to allow every name below it.
func parseAllowedHosts(value string) ([]string, error) {
	var hosts []string
	for _, raw := range strings.Split(value, ",") {
		entry := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
		if entry == "" {
			continue
		}
		name := strings.TrimPrefix(entry, "*.")
		if !validAllowHostName(name) {
			return nil, fmt.Errorf("%s entry %q must be a host name such as dash.example.org or *.example.org (no scheme, port or path)", allowedHostsFlagName, strings.TrimSpace(raw))
		}
		hosts = append(hosts, entry)
	}
	return hosts, nil
}

func validAllowHostName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

// osHostnames is this machine's hostname as the OS reports it.
func osHostnames() []string {
	name, err := os.Hostname()
	if err != nil || strings.TrimSpace(name) == "" {
		return nil
	}
	return []string{name}
}

// defaultHostAllowlist is the allowlist of a RuntimeState that was given none
// (tests, and anything built with newHTTPHandler). It is a variable only so
// the test binary can add httptest's reserved example names; main always sets
// an explicit one.
var defaultHostAllowlist = sync.OnceValue(func() *hostAllowlist {
	return newHostAllowlist(osHostnames(), "", "", nil)
})

// ResolveFQDN adds the fully qualified form of this machine's hostname, when
// the resolver knows one (an /etc/hosts line, the DNS suffix). It runs once
// on the serve path, never under -self-check, and is bounded by timeout.
func (a *hostAllowlist) ResolveFQDN(timeout time.Duration) {
	for _, name := range osHostnames() {
		short := normalizeAllowHost(name)
		if short == "" || strings.Contains(short, ".") {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		cname, err := net.DefaultResolver.LookupCNAME(ctx, short)
		cancel()
		if err != nil {
			continue
		}
		fqdn := normalizeAllowHost(cname)
		// Only a name below the short one: a resolver that answers with
		// something else (a search-domain wildcard) must not widen the list.
		if strings.HasPrefix(fqdn, short+".") {
			a.addName(fqdn)
		}
	}
}

func (s *RuntimeState) SetHostAllowlist(allow *hostAllowlist) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hostAllowlist = allow
}

func (s *RuntimeState) HostAllowlist() *hostAllowlist {
	s.mu.RLock()
	allow := s.hostAllowlist
	s.mu.RUnlock()
	if allow == nil {
		return defaultHostAllowlist()
	}
	return allow
}

// requireAllowedHost answers 421 to a request naming a host the agent does
// not answer to. Every host the request names counts: the Host header and,
// from a trusted forwarding source (loopback, i.e. Tailscale Serve or a local
// reverse proxy, as requestHosts already decides), X-Forwarded-Host and
// Forwarded host=. Requiring all of them keeps a page rebinding a name to
// 127.0.0.1 from slipping past with a forwarded header of its own choosing.
func requireAllowedHost(state *RuntimeState, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allow := state.HostAllowlist()
		for _, host := range requestHosts(r) {
			if !allow.Allows(host) {
				writeMisdirectedHost(w, host)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeMisdirectedHost(w http.ResponseWriter, host string) {
	name := normalizeAllowHost(host)
	if len(name) > 80 {
		name = name[:80] + "..."
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusMisdirectedRequest)
	fmt.Fprintf(w, "sysmon-agent does not answer to the host name %q.\n"+
		"If that is how you reach it, start the agent with %s %s (env SYSMON_ALLOWED_HOSTS).\n",
		name, allowedHostsFlagName, name)
}
