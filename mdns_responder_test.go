package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type mdnsTestQuestion struct {
	name          string
	qtype, qclass uint16
}

func mdnsTestQuery(id uint16, questions ...mdnsTestQuestion) []byte {
	msg := []byte{byte(id >> 8), byte(id), 0, 0, 0, byte(len(questions)), 0, 0, 0, 0, 0, 0}
	for _, q := range questions {
		name, ok := encodeDNSName(q.name)
		if !ok {
			panic("bad test name " + q.name)
		}
		msg = append(msg, name...)
		msg = binary.BigEndian.AppendUint16(msg, q.qtype)
		msg = binary.BigEndian.AppendUint16(msg, q.qclass)
	}
	return msg
}

type mdnsTestRR struct {
	name  string
	kind  uint16
	class uint16
	ttl   uint32
	data  []byte
}

// parseMDNSTestReply splits a reply into its header fields, question count,
// answers and additional records, failing the test on anything malformed.
func parseMDNSTestReply(t *testing.T, msg []byte) (id uint16, questions int, answers, additional []mdnsTestRR) {
	t.Helper()
	if len(msg) < 12 {
		t.Fatalf("reply too short: %x", msg)
	}
	if msg[2] != 0x84 || msg[3] != 0 {
		t.Fatalf("reply flags %02x%02x, want 8400 (response, authoritative)", msg[2], msg[3])
	}
	id = binary.BigEndian.Uint16(msg)
	questions = int(binary.BigEndian.Uint16(msg[4:]))
	counts := []int{int(binary.BigEndian.Uint16(msg[6:])), int(binary.BigEndian.Uint16(msg[8:])), int(binary.BigEndian.Uint16(msg[10:]))}
	off := 12
	for i := 0; i < questions; i++ {
		_, next, ok := readDNSName(msg, off)
		if !ok || next+4 > len(msg) {
			t.Fatalf("bad question in %x", msg)
		}
		off = next + 4
	}
	read := func(n int) []mdnsTestRR {
		var out []mdnsTestRR
		for i := 0; i < n; i++ {
			name, next, ok := readDNSName(msg, off)
			if !ok || next+10 > len(msg) {
				t.Fatalf("bad record in %x", msg)
			}
			length := int(binary.BigEndian.Uint16(msg[next+8:]))
			if next+10+length > len(msg) {
				t.Fatalf("record data past the end of %x", msg)
			}
			out = append(out, mdnsTestRR{
				name:  name,
				kind:  binary.BigEndian.Uint16(msg[next:]),
				class: binary.BigEndian.Uint16(msg[next+2:]),
				ttl:   binary.BigEndian.Uint32(msg[next+4:]),
				data:  msg[next+10 : next+10+length],
			})
			off = next + 10 + length
		}
		return out
	}
	answers = read(counts[0])
	if authority := read(counts[1]); len(authority) != 0 {
		t.Fatalf("reply has authority records: %v", authority)
	}
	additional = read(counts[2])
	if off != len(msg) {
		t.Fatalf("%d bytes after the last record", len(msg)-off)
	}
	return id, questions, answers, additional
}

// omarchyNetworks is the dual-boot machine under Linux: wired and Wi-Fi on
// the same LAN, plus Tailscale.
func omarchyNetworks() []netip.Prefix {
	return []netip.Prefix{
		netip.MustParsePrefix("10.0.0.4/24"),
		netip.MustParsePrefix("10.0.0.114/24"),
		netip.MustParsePrefix("100.69.74.46/32"),
	}
}

var phone = netip.MustParseAddrPort("10.0.0.50:5353")

func TestMDNSResponderAnswersAQuestionByMulticast(t *testing.T) {
	r := newMDNSResponder("bblwin.local")
	now := time.Unix(1000, 0)
	query := mdnsTestQuery(0, mdnsTestQuestion{"BBLWIN.local", mdnsTypeA, mdnsClassIN})
	reply, ok := r.reply(query, phone, omarchyNetworks, now)
	if !ok || reply.unicast {
		t.Fatalf("reply = %v, %v; want a multicast reply", reply, ok)
	}
	id, questions, answers, additional := parseMDNSTestReply(t, reply.msg)
	if id != 0 || questions != 0 {
		t.Fatalf("multicast reply id %d with %d questions; want 0 and 0", id, questions)
	}
	var got []string
	for _, a := range answers {
		if a.kind != mdnsTypeA || a.class != mdnsClassIN|mdnsClassTopBit || a.ttl != mdnsHostTTL || a.name != "bblwin.local" {
			t.Fatalf("answer %+v; want a cache-flush A record for bblwin.local with TTL %d", a, mdnsHostTTL)
		}
		got = append(got, netip.AddrFrom4([4]byte(a.data)).String())
	}
	if strings.Join(got, " ") != "10.0.0.4 10.0.0.114" {
		t.Fatalf("answered %v; want the two LAN addresses and not the Tailscale one", got)
	}
	if len(additional) != 1 || additional[0].kind != mdnsTypeNSEC {
		t.Fatalf("additional = %+v; want one NSEC saying there is no IPv6 address", additional)
	}
	assertNSECOnlyA(t, additional[0])

	// RFC 6762 section 6: no second multicast of the record within a second.
	if _, ok := r.reply(query, phone, omarchyNetworks, now.Add(500*time.Millisecond)); ok {
		t.Fatalf("multicast again after 500 ms")
	}
	if _, ok := r.reply(query, phone, omarchyNetworks, now.Add(1100*time.Millisecond)); !ok {
		t.Fatalf("no multicast after a second")
	}
}

func assertNSECOnlyA(t *testing.T, rr mdnsTestRR) {
	t.Helper()
	name, _ := encodeDNSName("bblwin.local")
	want := append(append([]byte{}, name...), 0, 1, 0x40)
	if rr.name != "bblwin.local" || !bytes.Equal(rr.data, want) {
		t.Fatalf("NSEC %+v; want next name bblwin.local and a bitmap of type A only (%x)", rr, want)
	}
}

func TestMDNSResponderUnicastAndLegacyQueries(t *testing.T) {
	r := newMDNSResponder("bblwin.local")
	now := time.Unix(1000, 0)

	// A QU question (top bit of its class) wants a unicast reply, which the
	// multicast spacing does not hold back.
	qu := mdnsTestQuery(0, mdnsTestQuestion{"bblwin.local", mdnsTypeA, mdnsClassIN | mdnsClassTopBit})
	for i := 0; i < 2; i++ {
		reply, ok := r.reply(qu, phone, omarchyNetworks, now)
		if !ok || !reply.unicast {
			t.Fatalf("QU question %d: reply = %v, %v; want unicast", i, reply.unicast, ok)
		}
	}

	// A plain resolver's query (not from port 5353) gets its ID and question
	// back, a short TTL and no cache-flush bit. The agent's own check is one.
	legacy := mdnsTestQuery(0x1234, mdnsTestQuestion{"bblwin.local", mdnsTypeA, mdnsClassIN})
	reply, ok := r.reply(legacy, netip.MustParseAddrPort("10.0.0.4:40001"), omarchyNetworks, now)
	if !ok || !reply.unicast {
		t.Fatalf("legacy query: reply = %v, %v; want unicast", reply.unicast, ok)
	}
	id, questions, answers, _ := parseMDNSTestReply(t, reply.msg)
	if id != 0x1234 || questions != 1 {
		t.Fatalf("legacy reply id %#x with %d questions; want the query's id and its question", id, questions)
	}
	for _, a := range answers {
		if a.class != mdnsClassIN || a.ttl != mdnsLegacyTTL {
			t.Fatalf("legacy answer %+v; want class IN with no cache-flush bit and TTL %d", a, mdnsLegacyTTL)
		}
	}
	addrs, ok := mdnsAnswerAddrs(reply.msg, 0x1234, "bblwin.local")
	if !ok || len(addrs) != 2 {
		t.Fatalf("the agent's own parser read %v, %v from the legacy reply", addrs, ok)
	}
}

func TestMDNSResponderSaysThereIsNoIPv6Address(t *testing.T) {
	r := newMDNSResponder("bblwin.local")
	query := mdnsTestQuery(0, mdnsTestQuestion{"bblwin.local", mdnsTypeAAAA, mdnsClassIN})
	reply, ok := r.reply(query, phone, omarchyNetworks, time.Unix(1000, 0))
	if !ok {
		t.Fatalf("no reply to an AAAA question")
	}
	_, _, answers, additional := parseMDNSTestReply(t, reply.msg)
	if len(answers) != 1 || answers[0].kind != mdnsTypeNSEC || len(additional) != 0 {
		t.Fatalf("answers %+v, additional %+v; want one NSEC answer", answers, additional)
	}
	assertNSECOnlyA(t, answers[0])
}

func TestMDNSResponderStaysQuiet(t *testing.T) {
	now := time.Unix(1000, 0)
	askedNetworks := false
	networks := func() []netip.Prefix { askedNetworks = true; return omarchyNetworks() }
	aQuestion := mdnsTestQuery(0, mdnsTestQuestion{"bblwin.local", mdnsTypeA, mdnsClassIN})
	response := append([]byte(nil), aQuestion...)
	response[2] = 0x84
	for _, tc := range []struct {
		label string
		msg   []byte
		from  netip.AddrPort
	}{
		{"another name", mdnsTestQuery(0, mdnsTestQuestion{"omarchy.local", mdnsTypeA, mdnsClassIN}), phone},
		{"a name that only starts the same", mdnsTestQuery(0, mdnsTestQuestion{"bblwin.local.example", mdnsTypeA, mdnsClassIN}), phone},
		{"a type it has no record for", mdnsTestQuery(0, mdnsTestQuestion{"bblwin.local", 16, mdnsClassIN}), phone},
		{"another class", mdnsTestQuery(0, mdnsTestQuestion{"bblwin.local", mdnsTypeA, 3}), phone},
		{"a response", response, phone},
		{"no questions", mdnsTestQuery(0), phone},
		{"too short", aQuestion[:11], phone},
		{"a cut question", aQuestion[:len(aQuestion)-2], phone},
		{"an asker on no network of this host", aQuestion, netip.MustParseAddrPort("192.168.1.9:5353")},
		{"an asker reached only over Tailscale", aQuestion, netip.MustParseAddrPort("100.110.71.14:5353")},
	} {
		askedNetworks = false
		if reply, ok := newMDNSResponder("bblwin.local").reply(tc.msg, tc.from, networks, now); ok {
			t.Errorf("%s: replied %x", tc.label, reply.msg)
		}
		if tc.label == "another name" && askedNetworks {
			t.Errorf("another name: listed the interfaces for a question that was not about this name")
		}
	}
}

// The responder answers the agent's own check, which is what lets a QR code
// open the name: a real socket on loopback stands in for the mDNS group.
func TestMDNSResponderAnswersTheAgentsOwnCheck(t *testing.T) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Skipf("no loopback UDP: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveMDNS(conn, newMDNSResponder("bblwin.local"), func() []netip.Prefix {
			return []netip.Prefix{netip.MustParsePrefix("127.0.0.1/8")}
		}, conn.LocalAddr())
	}()
	oldGroup := mdnsGroup
	mdnsGroup = conn.LocalAddr().(*net.UDPAddr)
	defer func() { mdnsGroup = oldGroup }()

	source := netip.MustParseAddr("127.0.0.1")
	if !queryMDNSOwnName("bblwin.local", source, map[netip.Addr]bool{source: true}, 500*time.Millisecond) {
		t.Fatalf("the agent's own check did not see the responder answer for bblwin.local")
	}
	if queryMDNSOwnName("omarchy.local", source, map[netip.Addr]bool{source: true}, 200*time.Millisecond) {
		t.Fatalf("the responder answered for a name it does not have")
	}
	conn.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("serveMDNS did not return after its socket closed")
	}
}

func TestParseMDNSNameFlag(t *testing.T) {
	for value, want := range map[string]string{
		"":              "",
		"  ":            "",
		"bblwin":        "bblwin",
		"BBLWIN.local":  "bblwin",
		"bbl-pc.local.": "bbl-pc",
	} {
		got, err := parseMDNSNameFlag(value)
		if err != nil || got != want {
			t.Errorf("parseMDNSNameFlag(%q) = %q, %v; want %q", value, got, err, want)
		}
	}
	for _, value := range []string{"my_pc", "host.example.com", "localhost", "-pc"} {
		if got, err := parseMDNSNameFlag(value); err == nil {
			t.Errorf("parseMDNSNameFlag(%q) = %q; want an error", value, got)
		}
	}
}

func TestMDNSResponderNeededOnlyForANameTheSystemDoesNotAnswer(t *testing.T) {
	fakeMDNSName(t, "omarchy", false)
	for _, tc := range []struct {
		config listenConfig
		want   string
	}{
		{listenConfig{bind: "0.0.0.0", mdnsName: "bblwin"}, "bblwin.local"},
		{listenConfig{bind: "10.0.0.4", mdnsName: "bblwin"}, "bblwin.local"},
		{listenConfig{bind: "0.0.0.0", mdnsName: "omarchy"}, ""},
		{listenConfig{bind: "0.0.0.0"}, ""},
		{listenConfig{bind: "127.0.0.1", mdnsName: "bblwin"}, ""},
	} {
		got, ok := mdnsResponderNeeded(tc.config)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("mdnsResponderNeeded(%+v) = %q, %v; want %q", tc.config, got, ok, tc.want)
		}
	}
}

// Under Linux the dual-boot machine is omarchy, but its QR codes open the
// name its Home Screen apps were made with under Windows.
func TestPairingBasesUseTheMDNSName(t *testing.T) {
	asked := fakeMDNSName(t, "omarchy", true)
	lan := listenConfig{bind: "10.0.0.4", port: 9199, mdnsName: "bblwin"}
	base, fallback := pairingBases(lan, "")
	if base != "http://bblwin.local:9199/" || fallback != "http://10.0.0.4:9199/" {
		t.Fatalf("pairingBases = %q, %q; want bblwin.local with the address as fallback", base, fallback)
	}
	if strings.Join(*asked, ";") != "bblwin.local from 10.0.0.4" {
		t.Fatalf("asked %q, want bblwin.local from the LAN address", *asked)
	}
}

func TestAgentCertificateNamesTheMDNSName(t *testing.T) {
	fakeMDNSName(t, "omarchy", false)
	certPEM, keyPEM, err := newAgentCertificate(time.Now(), "bblwin.local", "OMARCHY.local", "")
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"omarchy.local", "bblwin.local", "BBLWIN.local"} {
		if err := leaf.VerifyHostname(name); err != nil {
			t.Errorf("certificate does not name %s: %v", name, err)
		}
	}
	if strings.Join(leaf.DNSNames, " ") != "localhost omarchy omarchy.local bblwin.local" {
		t.Errorf("DNS names %q; want the extra name once and no empty one", leaf.DNSNames)
	}
}

// A certificate copied from the other system of a dual-boot machine is kept
// while it names the -mdns-name, so a device that trusted it there trusts it
// here; one that does not name it is replaced.
func TestNetworkCertificateIsKeptOnlyWhileItNamesTheMDNSName(t *testing.T) {
	now := time.Now()
	writeCert := func(dir, hostname string) string {
		t.Helper()
		fakeMDNSName(t, hostname, false)
		certPEM, keyPEM, err := newAgentCertificate(now)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, agentCertFile), certPEM, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, agentKeyFile), keyPEM, 0o600); err != nil {
			t.Fatal(err)
		}
		return readAgentCertificateFingerprint(dir)
	}

	fromWindows := t.TempDir()
	windowsPrint := writeCert(fromWindows, "BBLWIN")
	fakeMDNSName(t, "omarchy", false)
	cert, err := loadNetworkCertificate("", "", fromWindows, now, "bblwin.local")
	if err != nil {
		t.Fatal(err)
	}
	if got := certificateFingerprint(cert); got != windowsPrint || readAgentCertificateFingerprint(fromWindows) != windowsPrint {
		t.Fatalf("the copied Windows certificate was replaced (%s, want %s)", got, windowsPrint)
	}

	ownOld := t.TempDir()
	oldPrint := writeCert(ownOld, "omarchy")
	fakeMDNSName(t, "omarchy", false)
	cert, err = loadNetworkCertificate("", "", ownOld, now, "bblwin.local")
	if err != nil {
		t.Fatal(err)
	}
	if certificateFingerprint(cert) == oldPrint || readAgentCertificateFingerprint(ownOld) != certificateFingerprint(cert) {
		t.Fatalf("a certificate without bblwin.local was kept, or its replacement was not saved")
	}
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])
	if err := leaf.VerifyHostname("bblwin.local"); err != nil {
		t.Fatalf("the replacement does not name bblwin.local: %v", err)
	}
	again, err := loadNetworkCertificate("", "", ownOld, now, "bblwin.local")
	if err != nil || certificateFingerprint(again) != certificateFingerprint(cert) {
		t.Fatalf("the replacement was replaced again on the next start")
	}
}
