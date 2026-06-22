package main

import (
	"fmt"
	"log"
	"net"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

type accessHostCandidate struct {
	host     string
	priority int
}

type accessInterfaceAddrStrings struct {
	name        string
	addrStrings []string
}

const (
	accessPriorityTailscale = iota
	accessPriorityPrivate
	accessPriorityPublicIPv4
	accessPriorityOther
)

func logAccessURLs(config listenConfig) {
	logAccessURLsWithLogger(config, log.Printf)
}

func logAccessURLsWithLogger(config listenConfig, logf logFunc) {
	logAccessURLCandidatesWithLogger(config, accessURLCandidates(config), logf)
}

func logAccessURLCandidatesWithLogger(config listenConfig, urls []string, logf logFunc) {
	if len(urls) == 0 {
		if isWildcardBind(config.bind) {
			logf("no non-loopback dashboard URL detected; connect the host to LAN/Tailscale or bind to a specific address before opening from your phone or device")
		}
		return
	}
	label := "dashboard URL"
	if isWildcardBind(config.bind) {
		label = "open dashboard from your device"
	} else if isLoopbackBind(config.bind) {
		label = "local dashboard URL"
	}
	logf("%s: %s", label, strings.Join(urls, "  "))
}

func accessURLCandidates(config listenConfig) []string {
	hosts := accessHostCandidates(config.bind)
	urls := make([]string, 0, len(hosts))
	for _, host := range hosts {
		urls = append(urls, dashboardURL(config.scheme(), host.host, config.port))
	}
	return urls
}

func accessHostCandidates(bind string) []accessHostCandidate {
	bind = strings.TrimSpace(bind)
	if !isWildcardBind(bind) {
		return []accessHostCandidate{{host: normalizeBindHost(bind), priority: 0}}
	}
	return localAccessHostCandidates()
}

func localAccessHostCandidates() []accessHostCandidate {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}

	var interfaceAddrStrings []accessInterfaceAddrStrings
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		addrStrings := make([]string, 0, len(addrs))
		for _, addr := range addrs {
			addrStrings = append(addrStrings, addr.String())
		}
		interfaceAddrStrings = append(interfaceAddrStrings, accessInterfaceAddrStrings{
			name:        iface.Name,
			addrStrings: addrStrings,
		})
	}
	return accessHostCandidatesFromInterfaceAddrStrings(interfaceAddrStrings)
}

func accessHostCandidatesFromAddrStrings(addrStrings []string) []accessHostCandidate {
	return accessHostCandidatesFromInterfaceAddrStrings([]accessInterfaceAddrStrings{{
		name:        "host",
		addrStrings: addrStrings,
	}})
}

func accessHostCandidatesFromInterfaceAddrStrings(interfaces []accessInterfaceAddrStrings) []accessHostCandidate {
	seen := map[string]bool{}
	var candidates []accessHostCandidate
	for _, iface := range interfaces {
		if !shouldIncludeAccessInterfaceName(iface.name) {
			continue
		}
		for _, value := range iface.addrStrings {
			candidate, ok := accessHostCandidateFromAddrString(value)
			if !ok || seen[candidate.host] {
				continue
			}
			seen[candidate.host] = true
			candidates = append(candidates, candidate)
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].priority != candidates[j].priority {
			return candidates[i].priority < candidates[j].priority
		}
		return candidates[i].host < candidates[j].host
	})
	if len(candidates) > 4 {
		candidates = candidates[:4]
	}
	return candidates
}

func shouldIncludeAccessInterfaceName(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.Split(name, "@")[0]
	if name == "" || name == "lo" {
		return false
	}
	for _, prefix := range skippedAccessInterfacePrefixes {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	for _, fragment := range skippedAccessInterfaceFragments {
		if strings.Contains(name, fragment) {
			return false
		}
	}
	return true
}

var skippedAccessInterfacePrefixes = []string{
	"br-",
	"cni",
	"docker",
	"flannel",
	"hyper-v",
	"kube-ipvs",
	"nerdctl",
	"npcap loopback",
	"podman",
	"veth",
	"vethernet",
	"virbr",
	"virtualbox host-only",
	"vmware network adapter",
}

var skippedAccessInterfaceFragments = []string{
	"default switch",
	"docker",
	"hyper-v",
	"loopback",
	"microsoft wi-fi direct virtual adapter",
	"nat network",
	"nat switch",
	"npcap",
	"virtualbox host-only",
	"vmware network adapter",
	"wi-fi direct virtual adapter",
	"wsl",
}

func accessHostCandidateFromAddrString(value string) (accessHostCandidate, bool) {
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		return accessHostCandidate{}, false
	}
	addr := prefix.Addr()
	if addr.IsLoopback() || addr.IsUnspecified() || !addr.IsGlobalUnicast() {
		return accessHostCandidate{}, false
	}
	if addr.Is6() && addr.IsLinkLocalUnicast() {
		return accessHostCandidate{}, false
	}

	priority := accessPriorityOther
	if isTailscaleAddr(addr) {
		priority = accessPriorityTailscale
	} else if addr.IsPrivate() {
		priority = accessPriorityPrivate
	} else if addr.Is4() {
		priority = accessPriorityPublicIPv4
	}
	return accessHostCandidate{host: addr.String(), priority: priority}, true
}

func isWildcardBind(bind string) bool {
	switch strings.TrimSpace(bind) {
	case "", "*", "0.0.0.0", "::", "[::]":
		return true
	default:
		return false
	}
}

func isLoopbackBind(bind string) bool {
	bind = strings.ToLower(normalizeBindHost(bind))
	if bind == "localhost" {
		return true
	}
	addr, err := netip.ParseAddr(bind)
	return err == nil && addr.IsLoopback()
}

func normalizeBindHost(bind string) string {
	bind = strings.TrimSpace(bind)
	if strings.HasPrefix(bind, "[") && strings.HasSuffix(bind, "]") {
		return strings.TrimSuffix(strings.TrimPrefix(bind, "["), "]")
	}
	return bind
}

func isTailscaleAddr(addr netip.Addr) bool {
	if !addr.Is4() {
		return false
	}
	octets := addr.As4()
	return octets[0] == 100 && octets[1] >= 64 && octets[1] <= 127
}

func dashboardURL(scheme, host string, port int) string {
	return (&url.URL{
		Scheme: scheme,
		Host:   net.JoinHostPort(host, strconv.Itoa(port)),
		Path:   "/",
	}).String()
}

func formatAccessCandidates(candidates []accessHostCandidate) string {
	values := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		values = append(values, fmt.Sprintf("%s:%d", candidate.host, candidate.priority))
	}
	return strings.Join(values, ",")
}
