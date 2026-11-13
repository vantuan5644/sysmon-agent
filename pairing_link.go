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

// pairingLinkLocalAddress is the link's host when it is a private network
// address, the kind a router's DHCP server hands out and may change. A Home
// Screen app and its pairing cookie belong to that address, so -pair says to
// reserve it. A Tailscale address (100.64.0.0/10) or a name is left alone.
func pairingLinkLocalAddress(link string) (string, bool) {
	u, err := url.Parse(link)
	if err != nil {
		return "", false
	}
	addr, err := netip.ParseAddr(u.Hostname())
	if err != nil || !addr.Unmap().IsPrivate() {
		return "", false
	}
	return addr.Unmap().String(), true
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

// pairingLinkFallback names where a QR code points when the request gives
// no usable origin: base is what the code opens, and address is the private
// address base's .local name was checked on, empty when base is an address.
type pairingLinkFallback func() (base, address string)

// pairingLinkBaseForRequest is where "Pair another device" points its QR
// code: the origin the dashboard is open at, which the new device can reach
// the same way, unless that is this machine's loopback address. Then it is
// the fallback, normally pairingBases. A dashboard open at the LAN address
// the host's .local name answers on gets that name instead, which a Home
// Screen app keeps through a DHCP change. address is that LAN address when
// base is the name, for a device that cannot look the name up.
func pairingLinkBaseForRequest(r *http.Request, fallback pairingLinkFallback) (base, address string) {
	if u, err := url.Parse(r.Header.Get("Origin")); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && !isLoopbackHostName(u.Hostname()) {
		origin := u.Scheme + "://" + u.Host + "/"
		if _, private := pairingLinkLocalAddress(origin); private && fallback != nil {
			if named, checkedOn := fallback(); checkedOn != "" && checkedOn == origin {
				return named, checkedOn
			}
		}
		return origin, ""
	}
	if fallback != nil {
		return fallback()
	}
	return "", ""
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
func (s *RuntimeState) SetPairingLinkFallback(fallback pairingLinkFallback) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pairingLinkFallback = fallback
}

func (s *RuntimeState) PairingLinkFallback() pairingLinkFallback {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pairingLinkFallback
}
