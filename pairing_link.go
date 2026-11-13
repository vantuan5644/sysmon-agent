package main

import (
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

// A pairing link opens a dashboard with the PIN already filled in, as
// https://host:port/#pair=123456. The PIN rides in the fragment, which a
// browser never sends, so it reaches no server log or proxy on the way; the
// dashboard reads it once and takes it out of the address bar. `-pair` and
// "Pair another device" both show the link as a QR code (qr.go).

func pairingLink(base, pin string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Opaque != "" {
		return "", fmt.Errorf("%q is not an http:// or https:// address", base)
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = "pair=" + pin
	u.RawFragment = ""
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), nil
}

// pairingLinkAddress is a link without its PIN, for saying what a QR code opens.
func pairingLinkAddress(link string) string {
	address, _, _ := strings.Cut(link, "#")
	return address
}

// preferredPairingBase is where a QR code points when nothing names an
// address: a private network address of this host when it has one, since the
// device being paired is most often on the same network, else the first
// address the agent lists. An agent bound to loopback has none to offer.
func preferredPairingBase(config listenConfig) string {
	if isLoopbackBind(config.bind) {
		return ""
	}
	hosts := accessHostCandidates(config.bind)
	if len(hosts) == 0 {
		return ""
	}
	pick := hosts[0]
	if isWildcardBind(config.bind) {
		for _, host := range hosts {
			if host.priority == accessPriorityPrivate {
				pick = host
				break
			}
		}
	}
	return dashboardURL(config.publicScheme(), pick.host, config.port)
}

// pairingLinkBaseForRequest is where "Pair another device" points its QR
// code: the origin the dashboard is open at, which the new device can reach
// the same way, unless that is this machine's loopback address. Then it is
// the fallback, normally preferredPairingBase.
func pairingLinkBaseForRequest(r *http.Request, fallback func() string) string {
	if u, err := url.Parse(r.Header.Get("Origin")); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && !isLoopbackHostName(u.Hostname()) {
		return u.Scheme + "://" + u.Host + "/"
	}
	if fallback != nil {
		return fallback()
	}
	return ""
}

func isLoopbackHostName(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.Unmap().IsLoopback()
}

// SetPairingLinkFallback names the address a QR code from "Pair another
// device" opens when the dashboard asking for it runs on this machine's
// loopback address, which no other device can reach.
func (s *RuntimeState) SetPairingLinkFallback(fallback func() string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pairingLinkFallback = fallback
}

func (s *RuntimeState) PairingLinkFallback() func() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pairingLinkFallback
}
