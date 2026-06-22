package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestAccessURLCandidatesForDirectBind(t *testing.T) {
	got := accessURLCandidates(listenConfig{bind: "127.0.0.1", port: 9099})
	want := []string{"http://127.0.0.1:9099/"}
	assertStringSlice(t, got, want)
}

func TestAccessURLCandidatesNormalizesBracketedIPv6Bind(t *testing.T) {
	got := accessURLCandidates(listenConfig{bind: "[::1]", port: 9099})
	want := []string{"http://[::1]:9099/"}
	assertStringSlice(t, got, want)
}

func TestDashboardURLFormatsIPv6(t *testing.T) {
	got := dashboardURL("https", "fd7a:115c:a1e0::1", 9099)
	want := "https://[fd7a:115c:a1e0::1]:9099/"
	if got != want {
		t.Fatalf("dashboardURL = %q, want %q", got, want)
	}
}

func TestAccessHostCandidatesFromAddrStrings(t *testing.T) {
	got := accessHostCandidatesFromAddrStrings([]string{
		"127.0.0.1/8",
		"0.0.0.0/0",
		"10.0.0.5/24",
		"192.168.1.20/24",
		"100.90.1.2/32",
		"8.8.8.8/32",
		"fd7a:115c:a1e0::1/64",
		"fe80::1/64",
		"not-an-address",
	})

	want := []accessHostCandidate{
		{host: "100.90.1.2", priority: accessPriorityTailscale},
		{host: "10.0.0.5", priority: accessPriorityPrivate},
		{host: "192.168.1.20", priority: accessPriorityPrivate},
		{host: "fd7a:115c:a1e0::1", priority: accessPriorityPrivate},
	}
	if len(got) != len(want) {
		t.Fatalf("candidates = %s, want %s", formatAccessCandidates(got), formatAccessCandidates(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidate[%d] = %+v, want %+v; all = %s", i, got[i], want[i], formatAccessCandidates(got))
		}
	}
}

func TestAccessHostCandidatesSkipVirtualInterfaces(t *testing.T) {
	got := accessHostCandidatesFromInterfaceAddrStrings([]accessInterfaceAddrStrings{
		{name: "docker0", addrStrings: []string{"172.17.0.1/16"}},
		{name: "br-1234567890ab", addrStrings: []string{"172.18.0.1/16"}},
		{name: "vethabc@if3", addrStrings: []string{"192.168.200.10/24"}},
		{name: "vEthernet (WSL)", addrStrings: []string{"172.29.64.1/20"}},
		{name: "Npcap Loopback Adapter", addrStrings: []string{"169.254.100.1/16"}},
		{name: "tailscale0", addrStrings: []string{"100.90.1.2/32"}},
		{name: "Ethernet", addrStrings: []string{"10.0.0.8/24"}},
		{name: "wlan0", addrStrings: []string{"192.168.1.40/24"}},
		{name: "WireGuard Tunnel", addrStrings: []string{"10.7.0.2/24"}},
	})

	want := []accessHostCandidate{
		{host: "100.90.1.2", priority: accessPriorityTailscale},
		{host: "10.0.0.8", priority: accessPriorityPrivate},
		{host: "10.7.0.2", priority: accessPriorityPrivate},
		{host: "192.168.1.40", priority: accessPriorityPrivate},
	}
	if len(got) != len(want) {
		t.Fatalf("candidates = %s, want %s", formatAccessCandidates(got), formatAccessCandidates(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidate[%d] = %+v, want %+v; all = %s", i, got[i], want[i], formatAccessCandidates(got))
		}
	}
}

func TestShouldIncludeAccessInterfaceName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"", false},
		{"lo", false},
		{"docker0", false},
		{"br-1234567890ab", false},
		{"vethabc@if3", false},
		{"virbr0", false},
		{"cni0", false},
		{"flannel.1", false},
		{"vEthernet (Default Switch)", false},
		{"vEthernet (WSL)", false},
		{"VirtualBox Host-Only Network", false},
		{"VMware Network Adapter VMnet8", false},
		{"Npcap Loopback Adapter", false},
		{"Local Area Connection* 1 Microsoft Wi-Fi Direct Virtual Adapter", false},
		{"eth0", true},
		{"enp3s0", true},
		{"wlan0", true},
		{"Ethernet", true},
		{"Wi-Fi", true},
		{"tailscale0", true},
		{"WireGuard Tunnel", true},
		{"wg0", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldIncludeAccessInterfaceName(tc.name); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAccessHostCandidatePriorityForPublicIPv4(t *testing.T) {
	candidate, ok := accessHostCandidateFromAddrString("8.8.8.8/32")
	if !ok {
		t.Fatal("accessHostCandidateFromAddrString rejected public IPv4")
	}
	want := accessHostCandidate{host: "8.8.8.8", priority: accessPriorityPublicIPv4}
	if candidate != want {
		t.Fatalf("candidate = %+v, want %+v", candidate, want)
	}
}

func TestWildcardBindDetection(t *testing.T) {
	for _, bind := range []string{"", "*", "0.0.0.0", "::", "[::]"} {
		if !isWildcardBind(bind) {
			t.Fatalf("isWildcardBind(%q) = false, want true", bind)
		}
	}
	if isWildcardBind("127.0.0.1") {
		t.Fatal("isWildcardBind accepted direct loopback bind")
	}
}

func TestLoopbackBindDetection(t *testing.T) {
	for _, bind := range []string{"localhost", "127.0.0.1", "::1", "[::1]"} {
		if !isLoopbackBind(bind) {
			t.Fatalf("isLoopbackBind(%q) = false, want true", bind)
		}
	}
	if isLoopbackBind("192.168.1.20") {
		t.Fatal("isLoopbackBind accepted LAN address")
	}
}

func TestLogAccessURLsReportsMissingWildcardCandidates(t *testing.T) {
	var logs []string
	logAccessURLCandidatesWithLogger(listenConfig{bind: "0.0.0.0", port: 9099}, nil, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})

	if len(logs) != 1 || !strings.Contains(logs[0], "no non-loopback dashboard URL detected") {
		t.Fatalf("logs = %+v, want missing dashboard URL diagnostic", logs)
	}
}

func assertStringSlice(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
