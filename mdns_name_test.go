package main

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// No test sends a query onto the network of the machine running the suite.
// TestQueryMDNSOwnName drives the real socket against a responder of its own.
func init() {
	mdnsOwnNameAnswers = func(string, netip.Addr, map[netip.Addr]bool, time.Duration) bool { return false }
}

func resetMDNSNameCache() {
	mdnsNameCache.Lock()
	mdnsNameCache.key, mdnsNameCache.ok, mdnsNameCache.checked = "", false, time.Time{}
	mdnsNameCache.Unlock()
}

// fakeMDNSName makes this host hostname and has the name check answer
// answers, recording each name asked and the address it was asked from.
func fakeMDNSName(t *testing.T, hostname string, answers bool) *[]string {
	t.Helper()
	oldHostname, oldAnswers := pairingHostname, mdnsOwnNameAnswers
	asked := []string{}
	pairingHostname = func() (string, error) { return hostname, nil }
	mdnsOwnNameAnswers = func(name string, source netip.Addr, _ map[netip.Addr]bool, _ time.Duration) bool {
		asked = append(asked, name+" from "+source.String())
		return answers
	}
	resetMDNSNameCache()
	t.Cleanup(func() {
		pairingHostname, mdnsOwnNameAnswers = oldHostname, oldAnswers
		resetMDNSNameCache()
	})
	return &asked
}

func TestMDNSLabel(t *testing.T) {
	for hostname, want := range map[string]string{
		"BBLWIN":                  "bblwin",
		" omarchy ":               "omarchy",
		"Tuans-MacBook-Pro.local": "tuans-macbook-pro",
		"minipc.":                 "minipc",
		"host.example.com":        "",
		"my_pc":                   "",
		"-pc":                     "",
		"pc-":                     "",
		"localhost":               "",
		"":                        "",
		"café":                    "",
		strings.Repeat("a", 63):   strings.Repeat("a", 63),
		strings.Repeat("a", 64):   "",
	} {
		got, ok := mdnsLabel(hostname)
		if got != want || ok != (want != "") {
			t.Errorf("mdnsLabel(%q) = %q, %v; want %q", hostname, got, ok, want)
		}
	}
}

// A QR code opens the host's .local name only when the name answers on the
// LAN address it would otherwise open; anything else keeps the address.
func TestPairingBasesPrefersACheckedLocalName(t *testing.T) {
	asked := fakeMDNSName(t, "BBLWIN", true)
	lan := listenConfig{bind: "10.0.0.4", port: 9199}
	base, fallback := pairingBases(lan, "")
	if base != "http://bblwin.local:9199/" || fallback != "http://10.0.0.4:9199/" {
		t.Fatalf("pairingBases = %q, %q; want the name with the address as fallback", base, fallback)
	}
	if strings.Join(*asked, ";") != "bblwin.local from 10.0.0.4" {
		t.Fatalf("asked %q, want bblwin.local from the LAN address", *asked)
	}
	if base, _ := pairingBases(lan, ""); base != "http://bblwin.local:9199/" || len(*asked) != 1 {
		t.Fatalf("a second code asked again (%q) or changed to %q", *asked, base)
	}

	asked = fakeMDNSName(t, "BBLWIN", false)
	if base, fallback := pairingBases(lan, ""); base != "http://10.0.0.4:9199/" || fallback != "" {
		t.Fatalf("a name that did not answer gave %q, %q", base, fallback)
	}

	for _, tc := range []struct {
		hostname string
		listen   listenConfig
		want     string
	}{
		{"my_pc", lan, "http://10.0.0.4:9199/"},
		{"BBLWIN", listenConfig{bind: "127.0.0.1", port: 9099}, ""},
		{"BBLWIN", listenConfig{bind: "fd00::4", port: 9199}, "http://[fd00::4]:9199/"},
		{"BBLWIN", listenConfig{bind: "100.110.71.14", port: 9199}, "http://100.110.71.14:9199/"},
	} {
		asked = fakeMDNSName(t, tc.hostname, true)
		if base, fallback := pairingBases(tc.listen, ""); base != tc.want || fallback != "" || len(*asked) != 0 {
			t.Errorf("%s on %s: pairingBases = %q, %q after asking %q; want %q and no question", tc.hostname, tc.listen.bind, base, fallback, *asked, tc.want)
		}
	}
}

// Over HTTPS the name must be one the certificate covers, or the browser
// would refuse the page the code opens.
func TestPairingBasesNeedsACertificateThatNamesTheHost(t *testing.T) {
	certFor := func(hostname string) (tls.Certificate, []byte) {
		t.Helper()
		fakeMDNSName(t, hostname, true)
		certPEM, keyPEM, err := newAgentCertificate(time.Now())
		if err != nil {
			t.Fatal(err)
		}
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		return cert, certPEM
	}
	other, _ := certFor("otherbox")
	own, ownPEM := certFor("BBLWIN")

	served := listenConfig{bind: "10.0.0.4", port: 9199, networkTLS: true, networkCert: &own}
	if base, _ := pairingBases(served, ""); base != "https://bblwin.local:9199/" {
		t.Fatalf("the agent's own certificate gave %q", base)
	}
	resetMDNSNameCache()
	served.networkCert = &other
	if base, fallback := pairingBases(served, ""); base != "https://10.0.0.4:9199/" || fallback != "" {
		t.Fatalf("a certificate for another name gave %q, %q", base, fallback)
	}

	// -pair runs before any certificate is loaded and reads the pairing folder's.
	dir := t.TempDir()
	cli := listenConfig{bind: "10.0.0.4", port: 9199, networkTLS: true}
	if base, _ := pairingBases(cli, dir); base != "https://10.0.0.4:9199/" {
		t.Fatalf("no certificate on disk gave %q", base)
	}
	if err := os.WriteFile(filepath.Join(dir, agentCertFile), ownPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if base, _ := pairingBases(cli, dir); base != "https://bblwin.local:9199/" {
		t.Fatalf("the folder's certificate gave %q", base)
	}
	given := listenConfig{bind: "10.0.0.4", port: 9199, tlsEnabled: true, certFile: filepath.Join(dir, agentCertFile)}
	if base, _ := pairingBases(given, ""); base != "https://bblwin.local:9199/" {
		t.Fatalf("a -cert naming the host gave %q", base)
	}
}

func TestPairCommandOpensTheLocalNameAndPrintsTheAddress(t *testing.T) {
	fakeMDNSName(t, "BBLWIN", true)
	settings := filepath.Join(t.TempDir(), "settings.json")
	listen := listenConfig{bind: "10.0.0.4", port: 9199}
	var out bytes.Buffer
	if err := runPairingCommand(pairingCommand{open: true, qr: true}, settings, listen, &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{
		"opens bblwin.local, this host's name on the local network",
		"open http://10.0.0.4:9199/ there instead.",
		"It opens http://bblwin.local:9199/ with the PIN filled in",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("-pair output lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "DHCP") {
		t.Fatalf("a .local code still suggested a DHCP reservation:\n%s", text)
	}
	if strings.Index(text, "there instead") > strings.Index(text, "█") {
		t.Fatalf("the note came after the code, which must stay last")
	}

	fakeMDNSName(t, "BBLWIN", false)
	out.Reset()
	if err := runPairingCommand(pairingCommand{open: true, qr: true}, settings, listen, &out); err != nil {
		t.Fatal(err)
	}
	if text := out.String(); !strings.Contains(text, "reserve 10.0.0.4 for it in the router's DHCP settings") || strings.Contains(text, ".local") {
		t.Fatalf("a name that did not answer gave:\n%s", text)
	}
}

func TestPairingLinkBaseUsesTheNameOnlyForItsOwnAddress(t *testing.T) {
	calls := 0
	named := func() (string, string) {
		calls++
		return "https://bblwin.local:9199/", "https://10.0.0.4:9199/"
	}
	request := func(origin string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/api/pair/window", nil)
		req.Header.Set("Origin", origin)
		return req
	}
	for _, tc := range []struct {
		origin, base, address string
	}{
		{"https://10.0.0.4:9199", "https://bblwin.local:9199/", "https://10.0.0.4:9199/"},
		{"https://10.0.0.7:9199", "https://10.0.0.7:9199/", ""},
		{"http://10.0.0.4:9199", "http://10.0.0.4:9199/", ""},
		{"https://host.example.ts.net:9443", "https://host.example.ts.net:9443/", ""},
		{"https://127.0.0.1:9199", "https://bblwin.local:9199/", "https://10.0.0.4:9199/"},
	} {
		if base, address := pairingLinkBaseForRequest(request(tc.origin), named); base != tc.base || address != tc.address {
			t.Errorf("origin %s: got %q, %q; want %q, %q", tc.origin, base, address, tc.base, tc.address)
		}
	}
	calls = 0
	pairingLinkBaseForRequest(request("https://host.example.ts.net:9443"), named)
	if calls != 0 {
		t.Fatalf("a Tailscale origin ran the name check")
	}
	unnamed := func() (string, string) { return "https://10.0.0.4:9199/", "" }
	if base, address := pairingLinkBaseForRequest(request("https://10.0.0.4:9199"), unnamed); base != "https://10.0.0.4:9199/" || address != "" {
		t.Fatalf("no name gave %q, %q", base, address)
	}

	// Through the route, the window says which address the name answered with.
	store, clock := newTestPairingStore(t, "")
	state := NewMemoryRuntimeState()
	state.SetPairing(store)
	state.SetPairingLinkFallback(named)
	handler, err := newHTTPHandlerWithController(fakeCollector{}, testStaticFS(), state, &fakeController{})
	if err != nil {
		t.Fatal(err)
	}
	cookie, _ := pairDevice(t, handler, store, clock, "")
	req := httptest.NewRequest(http.MethodPost, "https://10.0.0.4:9199/api/pair/window", nil)
	req.RemoteAddr = "10.0.0.20:51000"
	req.TLS = &tls.ConnectionState{}
	req.Header.Set("Origin", "https://10.0.0.4:9199")
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := serve(handler, req)
	var window pairingWindowResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &window) != nil {
		t.Fatalf("POST /api/pair/window = %d %s", rec.Code, rec.Body.String())
	}
	if window.Link != "https://bblwin.local:9199/#pair="+window.PIN || window.Address != "https://10.0.0.4:9199/" {
		t.Fatalf("window link %q, address %q", window.Link, window.Address)
	}
}

type mdnsTestRecord struct {
	name string // "" points back at the question's name
	kind uint16
	data []byte
}

func mdnsTestResponse(id uint16, question string, answers ...mdnsTestRecord) []byte {
	msg := []byte{byte(id >> 8), byte(id), 0x84, 0, 0, 1, 0, byte(len(answers)), 0, 0, 0, 0}
	writeName := func(name string) {
		for _, label := range strings.Split(name, ".") {
			msg = append(msg, byte(len(label)))
			msg = append(msg, label...)
		}
		msg = append(msg, 0)
	}
	writeName(question)
	msg = append(msg, 0, 1, 0, 1)
	for _, answer := range answers {
		if answer.name == "" {
			msg = append(msg, 0xC0, 12)
		} else {
			writeName(answer.name)
		}
		msg = binary.BigEndian.AppendUint16(msg, answer.kind)
		msg = append(msg, 0x80, 1, 0, 0, 0, 120)
		msg = binary.BigEndian.AppendUint16(msg, uint16(len(answer.data)))
		msg = append(msg, answer.data...)
	}
	return msg
}

func aRecord(name string, ip string) mdnsTestRecord {
	addr := netip.MustParseAddr(ip).As4()
	return mdnsTestRecord{name: name, kind: 1, data: addr[:]}
}

func TestMDNSAnswerAddrs(t *testing.T) {
	const name = "bblwin.local"
	msg := mdnsTestResponse(7, name,
		aRecord("", "10.0.0.4"),
		aRecord("BBLWIN.local", "10.0.0.5"),
		aRecord("other.local", "10.0.0.6"),
		mdnsTestRecord{name: name, kind: 28, data: make([]byte, 16)},
	)
	addrs, ok := mdnsAnswerAddrs(msg, 7, name)
	if !ok || len(addrs) != 2 || addrs[0].String() != "10.0.0.4" || addrs[1].String() != "10.0.0.5" {
		t.Fatalf("answers = %v, %v; want 10.0.0.4 and 10.0.0.5 only", addrs, ok)
	}
	if _, ok := mdnsAnswerAddrs(msg, 8, name); ok {
		t.Fatalf("another query's ID was read")
	}
	query := append([]byte(nil), msg...)
	query[2] = 0
	if _, ok := mdnsAnswerAddrs(query, 7, name); ok {
		t.Fatalf("a query was read as an answer")
	}
	for cut := 0; cut < len(msg)-1; cut++ {
		if addrs, ok := mdnsAnswerAddrs(msg[:cut], 7, name); ok && len(addrs) == 2 {
			t.Fatalf("a message cut to %d bytes gave both answers", cut)
		}
	}
	loop := mdnsTestResponse(7, name, aRecord("", "10.0.0.4"))
	loop[12+len(name)+2+4] = 0xC0
	loop[12+len(name)+2+5] = byte(12 + len(name) + 2 + 4)
	if _, ok := mdnsAnswerAddrs(loop, 7, name); ok {
		t.Fatalf("a name pointing at itself was read")
	}
	if query, ok := mdnsQuery(7, name); !ok || !bytes.Equal(query[12:], mdnsTestResponse(7, name)[12:]) {
		t.Fatalf("mdnsQuery = %x", query)
	}
}

// The real socket against a responder on loopback: the check passes only when
// the code's own address answers and nothing answers with a stranger's.
func TestQueryMDNSOwnName(t *testing.T) {
	responder, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Skipf("no loopback UDP: %v", err)
	}
	defer responder.Close()
	oldGroup := mdnsGroup
	mdnsGroup = responder.LocalAddr().(*net.UDPAddr)
	defer func() { mdnsGroup = oldGroup }()

	const name = "bblwin.local"
	source := netip.MustParseAddr("127.0.0.1")
	own := map[netip.Addr]bool{source: true, netip.MustParseAddr("127.0.0.9"): true}
	for _, tc := range []struct {
		label   string
		replies func(id uint16) [][]byte
		want    bool
	}{
		{"own address", func(id uint16) [][]byte {
			return [][]byte{mdnsTestResponse(id, name, aRecord("", "127.0.0.1"))}
		}, true},
		{"a second host also answers", func(id uint16) [][]byte {
			return [][]byte{mdnsTestResponse(id, name, aRecord("", "127.0.0.1")), mdnsTestResponse(id, name, aRecord(name, "10.9.9.9"))}
		}, false},
		{"nothing answers", func(uint16) [][]byte { return nil }, false},
		{"another query's answer first", func(id uint16) [][]byte {
			return [][]byte{mdnsTestResponse(id+1, name, aRecord("", "10.9.9.9")), mdnsTestResponse(id, name, aRecord("", "127.0.0.1"))}
		}, true},
		{"only another name answers", func(id uint16) [][]byte {
			return [][]byte{mdnsTestResponse(id, name, aRecord("other.local", "127.0.0.1"))}
		}, false},
		{"another address of this host, but not the code's", func(id uint16) [][]byte {
			return [][]byte{mdnsTestResponse(id, name, aRecord("", "127.0.0.9"))}
		}, false},
	} {
		done := make(chan error, 1)
		go func() {
			buf := make([]byte, 512)
			_ = responder.SetReadDeadline(time.Now().Add(2 * time.Second))
			n, from, err := responder.ReadFromUDP(buf)
			if err != nil {
				done <- err
				return
			}
			if want, _ := mdnsQuery(binary.BigEndian.Uint16(buf), name); !bytes.Equal(buf[:n], want) {
				done <- &net.AddrError{Err: "unexpected query", Addr: string(buf[:n])}
				return
			}
			for _, reply := range tc.replies(binary.BigEndian.Uint16(buf)) {
				if _, err := responder.WriteToUDP(reply, from); err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}()
		got := queryMDNSOwnName(name, source, own, 300*time.Millisecond)
		if err := <-done; err != nil {
			t.Fatalf("%s: responder: %v", tc.label, err)
		}
		if got != tc.want {
			t.Errorf("%s: queryMDNSOwnName = %v, want %v", tc.label, got, tc.want)
		}
	}
}
