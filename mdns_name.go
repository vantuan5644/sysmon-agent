package main

import (
	"bytes"
	"crypto/rand"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// A QR code that opens a LAN address stops working for the Home Screen app
// made from it once DHCP gives the host a new address: the app and its
// pairing cookie belong to that one origin. The host's mDNS name,
// <hostname>.local, follows the host instead. Windows 10 and later, macOS and
// Linux with avahi or systemd-resolved answer for their own name, iOS and
// Android 12+ look such names up (Android not while a VPN is on), and the
// agent's own certificate already names <hostname>.local (tls_auto.go).
//
// The name goes into a QR code only when it answers on the LAN with the
// address the code would otherwise open, nothing answers for it with an
// address that is not this host's, and the certificate covers it; otherwise
// the code opens the address as before. Wherever the name is used, the
// address is shown beside it for a device that cannot look the name up.

// pairingHostname is os.Hostname, replaceable in tests.
var pairingHostname = os.Hostname

// mdnsOwnNameAnswers asks the local network who answers for name and reports
// whether source was among the answers and every answer was one of own.
// Tests replace it.
var mdnsOwnNameAnswers = queryMDNSOwnName

const mdnsCheckTimeout = 600 * time.Millisecond

// mdnsGroup is where the query goes: the mDNS group, or a test's responder.
var mdnsGroup = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}

// pairingBases picks where a QR code points when nothing names an address:
// the host's .local name when it checks out, with the private address as the
// fallback to print beside it, else that address alone.
func pairingBases(config listenConfig, dir string) (base, fallback string) {
	address := preferredPairingBase(config)
	if address == "" {
		return "", ""
	}
	if name, ok := pairingMDNSName(config, dir, address); ok {
		return dashboardURL(config.publicScheme(), name, config.port), address
	}
	return address, ""
}

// mdnsLinkHost is a link's host name, for saying which name a code opens.
func mdnsLinkHost(link string) string {
	if u, err := url.Parse(link); err == nil {
		return u.Hostname()
	}
	return link
}

func pairingMDNSName(config listenConfig, dir, address string) (string, bool) {
	u, err := url.Parse(address)
	if err != nil {
		return "", false
	}
	source, err := netip.ParseAddr(u.Hostname())
	if err != nil || !source.Is4() || !source.IsPrivate() {
		return "", false
	}
	label, ok := mdnsNameLabel(config)
	if !ok {
		return "", false
	}
	name := label + ".local"
	if !pairingCertificateCovers(config, dir, name) {
		return "", false
	}
	if !cachedMDNSOwnName(name, source) {
		return "", false
	}
	return name, true
}

// mdnsNameLabel is the label of the .local name this agent goes by: the one
// -mdns-name gives, which the agent answers for itself (mdns_responder.go),
// else the hostname's, which the operating system answers for.
func mdnsNameLabel(config listenConfig) (string, bool) {
	if config.mdnsName != "" {
		return mdnsLabel(config.mdnsName)
	}
	hostname, err := pairingHostname()
	if err != nil {
		return "", false
	}
	return mdnsLabel(hostname)
}

// mdnsLabel is the single DNS label a hostname is known by over mDNS, lower
// case: "BBLWIN" and "Tuans-MacBook-Pro.local" qualify, while a name with
// another domain, an underscore or anything past ASCII letters, digits and
// inner hyphens does not, since a browser or the certificate would not match
// it the same way.
func mdnsLabel(hostname string) (string, bool) {
	label := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(hostname), "."))
	label = strings.TrimSuffix(label, ".local")
	if label == "" || len(label) > 63 || label == "localhost" || label[0] == '-' || label[len(label)-1] == '-' {
		return "", false
	}
	for _, r := range label {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return "", false
		}
	}
	return label, true
}

// pairingCertificateCovers reports whether the certificate network clients
// get names host, so a QR code never opens a name the browser would refuse.
// Plain HTTP has no certificate to match.
func pairingCertificateCovers(config listenConfig, dir, host string) bool {
	if config.publicScheme() == "http" {
		return true
	}
	var der []byte
	switch {
	case config.networkCert != nil && len(config.networkCert.Certificate) > 0:
		der = config.networkCert.Certificate[0]
	case config.certFile != "":
		der = firstPEMCertificate(config.certFile)
	case config.networkTLS && dir != "":
		der = firstPEMCertificate(filepath.Join(dir, agentCertFile))
	}
	if der == nil {
		return false
	}
	cert, err := x509.ParseCertificate(der)
	return err == nil && cert.VerifyHostname(host) == nil
}

func firstPEMCertificate(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil
	}
	return block.Bytes
}

// The check costs up to mdnsCheckTimeout, and "Pair another device" asks on
// each PIN, so an answer is kept for a while.
var mdnsNameCache struct {
	sync.Mutex
	key     string
	ok      bool
	checked time.Time
}

func cachedMDNSOwnName(name string, source netip.Addr) bool {
	key := name + "|" + source.String()
	mdnsNameCache.Lock()
	defer mdnsNameCache.Unlock()
	keep := time.Minute
	if mdnsNameCache.ok {
		keep = 10 * time.Minute
	}
	if mdnsNameCache.key == key && time.Since(mdnsNameCache.checked) < keep {
		return mdnsNameCache.ok
	}
	ok := mdnsOwnNameAnswers(name, source, ownInterfaceAddrs(), mdnsCheckTimeout)
	mdnsNameCache.key, mdnsNameCache.ok, mdnsNameCache.checked = key, ok, time.Now()
	return ok
}

func ownInterfaceAddrs() map[netip.Addr]bool {
	own := map[netip.Addr]bool{}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return own
	}
	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok {
			if a, ok := netip.AddrFromSlice(ipNet.IP); ok {
				own[a.Unmap()] = true
			}
		}
	}
	return own
}

// queryMDNSOwnName reports whether name answers from source and from no
// address that is not one of own. A host with no responder stays silent; a
// second host using the name answers with an address that is not one of own.
// It listens out the whole timeout, since a second host's answer can come
// after this host's.
func queryMDNSOwnName(name string, source netip.Addr, own map[netip.Addr]bool, timeout time.Duration) bool {
	addrs, err := mdnsAnswers(name, source, timeout, func(addr netip.Addr) bool { return !own[addr] })
	if err != nil {
		return false
	}
	sawSource := false
	for _, addr := range addrs {
		if !own[addr] {
			return false
		}
		if addr == source {
			sawSource = true
		}
	}
	return sawSource
}

// mdnsAnswers sends one legacy unicast mDNS query (RFC 6762 section 6.7:
// from a port other than 5353, so a responder answers by unicast to that
// port, echoing the query ID) for name's A record and returns every address
// answered within timeout, stopping at the first one stop accepts. The
// socket is bound to source because multicast otherwise leaves by whichever
// interface the routing table prefers, which on a Windows host with WSL is
// its virtual adapter, and the answers would be for that network.
func mdnsAnswers(name string, source netip.Addr, timeout time.Duration, stop func(netip.Addr) bool) ([]netip.Addr, error) {
	conn, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(source, 0)))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	var idBytes [2]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return nil, err
	}
	id := binary.BigEndian.Uint16(idBytes[:])
	query, ok := mdnsQuery(id, name)
	if !ok {
		return nil, fmt.Errorf("%q cannot be asked for over mDNS", name)
	}
	if _, err := conn.WriteToUDP(query, mdnsGroup); err != nil {
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 9000)
	var answers []netip.Addr
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			return answers, nil
		}
		addrs, ok := mdnsAnswerAddrs(buf[:n], id, name)
		if !ok {
			continue
		}
		for _, addr := range addrs {
			answers = append(answers, addr)
			if stop != nil && stop(addr) {
				return answers, nil
			}
		}
	}
}

func mdnsQuery(id uint16, name string) ([]byte, bool) {
	encoded, ok := encodeDNSName(name)
	if !ok {
		return nil, false
	}
	msg := []byte{byte(id >> 8), byte(id), 0, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	msg = append(msg, encoded...)
	return append(msg, 0, 1, 0, 1), true // type A, class IN
}

// encodeDNSName writes name as uncompressed labels ending in the root label.
func encodeDNSName(name string) ([]byte, bool) {
	var out []byte
	for _, label := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		if label == "" || len(label) > 63 {
			return nil, false
		}
		out = append(out, byte(len(label)))
		out = append(out, label...)
	}
	return append(out, 0), true
}

// mdnsAnswerAddrs reads the A records for name out of a response to query
// id. ok is false for anything else: another ID, a query, a malformed packet.
func mdnsAnswerAddrs(msg []byte, id uint16, name string) (addrs []netip.Addr, ok bool) {
	if len(msg) < 12 || binary.BigEndian.Uint16(msg) != id || msg[2]&0x80 == 0 {
		return nil, false
	}
	questions := int(binary.BigEndian.Uint16(msg[4:]))
	answers := int(binary.BigEndian.Uint16(msg[6:]))
	off := 12
	for i := 0; i < questions; i++ {
		if _, next, ok := readDNSName(msg, off); ok && next+4 <= len(msg) {
			off = next + 4
		} else {
			return nil, false
		}
	}
	for i := 0; i < answers; i++ {
		owner, next, ok := readDNSName(msg, off)
		if !ok || next+10 > len(msg) {
			return nil, false
		}
		kind := binary.BigEndian.Uint16(msg[next:])
		length := int(binary.BigEndian.Uint16(msg[next+8:]))
		data := next + 10
		if data+length > len(msg) {
			return nil, false
		}
		if kind == 1 && length == 4 && strings.EqualFold(owner, name) {
			addrs = append(addrs, netip.AddrFrom4([4]byte(msg[data:data+4])))
		}
		off = data + length
	}
	return addrs, true
}

// readDNSName reads a possibly compressed name at off and returns it with the
// offset just past it in the message.
func readDNSName(msg []byte, off int) (string, int, bool) {
	var labels []string
	next, ok := walkDNSName(msg, off, func(label []byte) bool {
		labels = append(labels, string(label))
		return true
	})
	if !ok {
		return "", 0, false
	}
	return strings.Join(labels, "."), next, true
}

// maxDNSNameLength is the longest a name may be on the wire (RFC 1035).
const maxDNSNameLength = 255

// walkDNSName calls visit with each label of the possibly compressed name at
// off, stopping early when visit returns false, and returns the offset just
// past the name in the message. The packets come from anyone on the LAN, so
// the walk is bounded: a name is at most 255 bytes, a compression pointer may
// only point back to an earlier offset, and a name follows at most 16 of them.
// Once visit has its answer and a pointer has been met, the end of the name
// in the message is known and the walk stops, so a packet of many questions
// pointing at one long name costs no more than its own bytes.
func walkDNSName(msg []byte, off int, visit func(label []byte) bool) (int, bool) {
	next := -1
	size, jumps := 0, 0
	answered := false
	for {
		if off >= len(msg) {
			return 0, false
		}
		length := int(msg[off])
		switch {
		case length == 0:
			if next < 0 {
				next = off + 1
			}
			return next, true
		case length&0xC0 == 0xC0:
			if off+1 >= len(msg) {
				return 0, false
			}
			target := int(binary.BigEndian.Uint16(msg[off:]) & 0x3FFF)
			if jumps++; target >= off || jumps > 16 {
				return 0, false
			}
			if next < 0 {
				next = off + 2
			}
			if answered {
				return next, true
			}
			off = target
		case length&0xC0 != 0:
			return 0, false
		default:
			if off+1+length > len(msg) {
				return 0, false
			}
			if size += length + 1; size > maxDNSNameLength {
				return 0, false
			}
			if !answered && !visit(msg[off+1:off+1+length]) {
				// The caller has its answer; only where the name ends is left.
				answered = true
				if next >= 0 {
					return next, true
				}
			}
			off += 1 + length
		}
	}
}

// dnsNameIs reports whether the name at off is the one whose labels are want,
// ignoring ASCII case and without allocating, and returns the offset just
// past it. ok is false for a malformed name.
func dnsNameIs(msg []byte, off int, want [][]byte) (next int, ok, equal bool) {
	i := 0
	equal = true
	next, ok = walkDNSName(msg, off, func(label []byte) bool {
		if i >= len(want) || !bytes.EqualFold(label, want[i]) {
			equal = false
			return false
		}
		i++
		return true
	})
	return next, ok, ok && equal && i == len(want)
}
