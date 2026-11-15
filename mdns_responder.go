package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// -mdns-name gives the agent a .local name other than its hostname's
// (docs/plans/pairing.md, "One name for a dual-boot machine"). Each operating
// system answers mDNS for its own hostname only, so a machine that boots
// Windows as BBLWIN and Linux as omarchy is bblwin.local under one and
// omarchy.local under the other. A Home Screen app and its pairing cookie
// belong to one origin, so an app made from a QR code under Windows lost the
// machine under Linux. With -mdns-name bblwin, the Linux agent answers for
// bblwin.local itself, its certificate names it, and its QR codes open it.
//
// The responder answers one name and nothing else (RFC 6762): A questions
// with this host's IPv4 addresses on the asker's network, and AAAA questions
// with an NSEC record saying the name has no IPv6 address (section 6.1), so
// a device does not wait out an IPv6 lookup. It never answers a response. It
// asks for the name every mdnsResponderRecheck and goes quiet while another
// host answers for it, and it says goodbye (TTL 0) when it stops answering.

const (
	mdnsPort = 5353

	mdnsTypeA    = 1
	mdnsTypeAAAA = 28
	mdnsTypeNSEC = 47
	mdnsTypeANY  = 255

	mdnsClassIN  = 1
	mdnsClassANY = 255
	// The top bit of a question's class asks for a unicast reply (QU); the
	// same bit on a record's class tells caches to flush older records.
	mdnsClassTopBit = 0x8000

	mdnsHostTTL   = 120 // seconds, RFC 6762 section 10 for host records
	mdnsLegacyTTL = 10  // seconds, the cap for a legacy unicast reply

	// mdnsMulticastSpacing is the least time between two multicast replies,
	// RFC 6762 section 6.
	mdnsMulticastSpacing = time.Second
	// mdnsResponderRecheck is how often the responder looks for a new LAN
	// address or interface, and retries after a failure.
	mdnsResponderRecheck = time.Minute
)

// mdnsResponderGroup is where the responder listens and sends multicast
// replies.
var mdnsResponderGroup = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: mdnsPort}

// mdnsResponder builds the replies for one name. It does no I/O.
type mdnsResponder struct {
	name   string   // lower case, no trailing dot
	labels [][]byte // name's labels, to match questions without allocating

	// When the A records and the NSEC record were last multicast. RFC 6762
	// section 6 spaces each record on its own clock, so an AAAA question's
	// NSEC does not hold back the A answer that follows it.
	mu       sync.Mutex
	lastA    time.Time
	lastNSEC time.Time
}

func newMDNSResponder(name string) *mdnsResponder {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	var labels [][]byte
	for _, label := range strings.Split(name, ".") {
		labels = append(labels, []byte(label))
	}
	return &mdnsResponder{name: name, labels: labels}
}

// mdnsReply is a message to send: by unicast to the asker, or to the group.
type mdnsReply struct {
	msg     []byte
	unicast bool
}

type mdnsQuestion struct {
	qtype, qclass uint16
}

// reply answers one packet from a device at from. networks lists this host's
// IPv4 addresses with their prefix lengths and is called only for a question
// about the name. ok is false when there is nothing to send: a response, a
// malformed packet, another name, an asker on no network of this host, or a
// multicast reply sent less than mdnsMulticastSpacing ago.
func (r *mdnsResponder) reply(msg []byte, from netip.AddrPort, networks func() []netip.Prefix, now time.Time) (mdnsReply, bool) {
	questions, ok := r.questions(msg)
	if !ok || len(questions) == 0 {
		return mdnsReply{}, false
	}
	wantA, wantNSEC, everyQU := false, false, true
	for _, q := range questions {
		switch q.qtype {
		case mdnsTypeA, mdnsTypeANY:
			wantA = true
		case mdnsTypeAAAA:
			wantNSEC = true
		default:
			continue
		}
		if q.qclass&mdnsClassTopBit == 0 {
			everyQU = false
		}
	}
	if !wantA && !wantNSEC {
		return mdnsReply{}, false
	}
	addrs := mdnsAddrsOn(networks(), from.Addr().Unmap())
	if len(addrs) == 0 {
		return mdnsReply{}, false
	}
	// A query from a port other than 5353 comes from a plain DNS resolver
	// (RFC 6762 section 6.7): it gets a unicast reply carrying its ID and
	// question, a short TTL and no cache-flush bits, which it would misread.
	legacy := from.Port() != mdnsPort
	unicast := legacy || everyQU
	sendA, sendNSEC := wantA, true
	if !unicast {
		r.mu.Lock()
		sendA = wantA && mdnsSpaced(r.lastA, now)
		sendNSEC = (wantNSEC || sendA) && mdnsSpaced(r.lastNSEC, now)
		if sendA {
			r.lastA = now
		}
		if sendNSEC {
			r.lastNSEC = now
		}
		r.mu.Unlock()
		if !sendA && !sendNSEC {
			return mdnsReply{}, false
		}
	}
	var echo *mdnsQuestion
	if legacy {
		echo = &questions[0]
	}
	out := r.encode(binary.BigEndian.Uint16(msg), echo, addrs, sendA, sendNSEC, legacy)
	return mdnsReply{msg: out, unicast: unicast}, true
}

// mdnsSpaced reports whether a record last multicast at last may go again.
func mdnsSpaced(last, now time.Time) bool {
	return last.IsZero() || now.Sub(last) >= mdnsMulticastSpacing
}

// mdnsAddrsOn returns this host's IPv4 addresses on the network of addr:
// what an asker at addr is told, since it can reach them.
func mdnsAddrsOn(networks []netip.Prefix, addr netip.Addr) []netip.Addr {
	var addrs []netip.Addr
	for _, network := range networks {
		if network.Addr().Is4() && network.Contains(addr) && !containsAddr(addrs, network.Addr()) {
			addrs = append(addrs, network.Addr())
		}
	}
	return addrs
}

// questions returns the questions in a query that are about this name.
func (r *mdnsResponder) questions(msg []byte) ([]mdnsQuestion, bool) {
	if len(msg) < 12 {
		return nil, false
	}
	// A response (QR bit) is never answered, and only standard queries are.
	if msg[2]&0x80 != 0 || (msg[2]>>3)&0x0F != 0 {
		return nil, false
	}
	count := int(binary.BigEndian.Uint16(msg[4:]))
	off := 12
	var matched []mdnsQuestion
	for i := 0; i < count; i++ {
		next, ok, same := dnsNameIs(msg, off, r.labels)
		if !ok || next+4 > len(msg) {
			return nil, false
		}
		q := mdnsQuestion{qtype: binary.BigEndian.Uint16(msg[next:]), qclass: binary.BigEndian.Uint16(msg[next+2:])}
		off = next + 4
		class := q.qclass &^ mdnsClassTopBit
		if same && (class == mdnsClassIN || class == mdnsClassANY) {
			matched = append(matched, q)
		}
	}
	return matched, true
}

// encode builds a reply: the A records as answers with the NSEC beside them
// in the additional section, or the NSEC alone as the answer when no A
// record goes. A legacy reply repeats the query's ID and question.
func (r *mdnsResponder) encode(id uint16, echo *mdnsQuestion, addrs []netip.Addr, sendA, sendNSEC, legacy bool) []byte {
	name, _ := encodeDNSName(r.name)
	ttl := uint32(mdnsHostTTL)
	class := uint16(mdnsClassIN | mdnsClassTopBit)
	if legacy {
		ttl, class = mdnsLegacyTTL, mdnsClassIN
	} else {
		id = 0
	}
	var answers, additional [][]byte
	nsec := mdnsRecord(name, mdnsTypeNSEC, class, ttl, append(append([]byte{}, name...), 0, 1, 0x40)) // window 0: type A only
	switch {
	case sendA:
		for _, addr := range addrs {
			a := addr.As4()
			answers = append(answers, mdnsRecord(name, mdnsTypeA, class, ttl, a[:]))
		}
		if sendNSEC {
			additional = append(additional, nsec)
		}
	case sendNSEC:
		answers = append(answers, nsec)
	}
	questions := 0
	if echo != nil {
		questions = 1
	}
	msg := mdnsHeader(id, questions, len(answers), len(additional))
	if echo != nil {
		msg = append(msg, name...)
		msg = binary.BigEndian.AppendUint16(msg, echo.qtype)
		msg = binary.BigEndian.AppendUint16(msg, echo.qclass&^mdnsClassTopBit)
	}
	for _, record := range append(answers, additional...) {
		msg = append(msg, record...)
	}
	return msg
}

// goodbye withdraws addrs from every cache that holds them: the A records
// again with TTL 0 (RFC 6762 section 10.1).
func (r *mdnsResponder) goodbye(addrs []netip.Addr) []byte {
	name, _ := encodeDNSName(r.name)
	msg := mdnsHeader(0, 0, len(addrs), 0)
	for _, addr := range addrs {
		a := addr.As4()
		msg = append(msg, mdnsRecord(name, mdnsTypeA, mdnsClassIN|mdnsClassTopBit, 0, a[:])...)
	}
	return msg
}

// mdnsHeader is an authoritative response's header with these counts.
func mdnsHeader(id uint16, questions, answers, additional int) []byte {
	msg := binary.BigEndian.AppendUint16(nil, id)
	msg = append(msg, 0x84, 0x00)
	for _, count := range []int{questions, answers, 0, additional} {
		msg = binary.BigEndian.AppendUint16(msg, uint16(count))
	}
	return msg
}

func mdnsRecord(name []byte, kind, class uint16, ttl uint32, data []byte) []byte {
	record := append([]byte{}, name...)
	record = binary.BigEndian.AppendUint16(record, kind)
	record = binary.BigEndian.AppendUint16(record, class)
	record = binary.BigEndian.AppendUint32(record, ttl)
	record = binary.BigEndian.AppendUint16(record, uint16(len(data)))
	return append(record, data...)
}

func containsAddr(addrs []netip.Addr, addr netip.Addr) bool {
	return slices.Contains(addrs, addr)
}

// serveMDNS answers packets read from conn until reading fails, and returns
// that error (net.ErrClosed once conn is closed). Multicast replies go to
// group.
func serveMDNS(conn net.PacketConn, r *mdnsResponder, networks func() []netip.Prefix, group net.Addr) error {
	buf := make([]byte, 9000)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			var timeout interface{ Timeout() bool }
			if errors.As(err, &timeout) && timeout.Timeout() {
				continue
			}
			return err
		}
		udp, ok := from.(*net.UDPAddr)
		if !ok {
			continue
		}
		reply, ok := r.reply(buf[:n], udp.AddrPort(), networks, time.Now())
		if !ok {
			continue
		}
		to := group
		if reply.unicast {
			to = from
		}
		_, _ = conn.WriteTo(reply.msg, to)
	}
}

// mdnsLocalNetworks lists the IPv4 addresses, with prefix lengths, the agent
// answers with: those of interfaces the dashboard URLs come from (no
// container or VM bridges), or only the bind address when it names one.
func mdnsLocalNetworks(bind string) []netip.Prefix {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var only netip.Addr
	if !isWildcardBind(bind) {
		only, _ = netip.ParseAddr(normalizeBindHost(bind))
	}
	var networks []netip.Prefix
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || !shouldIncludeAccessInterfaceName(iface.Name) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			prefix, err := netip.ParsePrefix(addr.String())
			if err != nil || !prefix.Addr().Is4() || prefix.Addr().IsLoopback() || prefix.Addr().IsLinkLocalUnicast() {
				continue
			}
			if only.IsValid() && prefix.Addr() != only {
				continue
			}
			networks = append(networks, prefix)
		}
	}
	return networks
}

// mdnsResponderInterface is the private IPv4 address QR codes open and the
// interface that holds it, where the responder joins the group.
func mdnsResponderInterface(config listenConfig) (netip.Addr, *net.Interface, bool) {
	u, err := url.Parse(preferredPairingBase(config))
	if err != nil {
		return netip.Addr{}, nil, false
	}
	source, err := netip.ParseAddr(u.Hostname())
	if err != nil || !source.Is4() || !source.IsPrivate() {
		return netip.Addr{}, nil, false
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return netip.Addr{}, nil, false
	}
	for i := range interfaces {
		addrs, err := interfaces[i].Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if prefix, err := netip.ParsePrefix(addr.String()); err == nil && prefix.Addr() == source {
				return source, &interfaces[i], true
			}
		}
	}
	return netip.Addr{}, nil, false
}

// mdnsAnsweredElsewhere asks for name from source and returns an address
// that answered for it and is not this host's, if any.
var mdnsAnsweredElsewhere = func(name string, source netip.Addr) (netip.Addr, bool) {
	own := ownInterfaceAddrs()
	addrs, err := mdnsAnswers(name, source, mdnsCheckTimeout, func(addr netip.Addr) bool { return !own[addr] })
	if err != nil {
		return netip.Addr{}, false
	}
	for _, addr := range addrs {
		if !own[addr] {
			return addr, true
		}
	}
	return netip.Addr{}, false
}

// parseMDNSNameFlag accepts -mdns-name as a label ("bblwin") or a .local
// name ("BBLWIN.local") and returns the label; empty stays empty.
func parseMDNSNameFlag(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil
	}
	label, ok := mdnsLabel(value)
	if !ok {
		return "", fmt.Errorf("-mdns-name %q must be one DNS label of ASCII letters, digits and inner hyphens, optionally ending in .local", value)
	}
	return label, nil
}

// mdnsCertificateNames are the names the agent's own certificate must carry
// besides its hostname's.
func mdnsCertificateNames(config listenConfig) []string {
	if config.mdnsName == "" {
		return nil
	}
	return []string{config.mdnsName + ".local"}
}

// mdnsResponderNeeded reports the .local name the agent must answer for
// itself: an -mdns-name that is not the hostname's, which the operating
// system already answers for, on an agent other devices can reach.
func mdnsResponderNeeded(config listenConfig) (string, bool) {
	if config.mdnsName == "" || isLoopbackBind(config.bind) {
		return "", false
	}
	label, ok := mdnsLabel(config.mdnsName)
	if !ok {
		return "", false
	}
	if hostname, err := pairingHostname(); err == nil {
		if own, ok := mdnsLabel(hostname); ok && own == label {
			return "", false
		}
	}
	return label + ".local", true
}

// mdnsResponderEnv is how the responder reaches the system; tests swap it.
type mdnsResponderEnv struct {
	// locate returns the address QR codes open and the interface holding it.
	locate func() (netip.Addr, *net.Interface, bool)
	// elsewhere returns an address of another host that answers for name.
	elsewhere func(name string, source netip.Addr) (netip.Addr, bool)
	// listen opens a socket that hears the group on iface.
	listen   func(iface *net.Interface) (net.PacketConn, error)
	networks func() []netip.Prefix
	group    net.Addr
	recheck  time.Duration
}

// startMDNSResponder answers for name until the returned stop is called. It
// joins the group on the interface of the address QR codes open, and every
// mdnsResponderRecheck asks for the name in case another host has taken it,
// and looks for a new address or interface.
func startMDNSResponder(name string, config listenConfig, logf logFunc) (stop func()) {
	return runMDNSResponder(name, mdnsResponderEnv{
		locate: func() (netip.Addr, *net.Interface, bool) { return mdnsResponderInterface(config) },
		elsewhere: func(name string, source netip.Addr) (netip.Addr, bool) {
			return mdnsAnsweredElsewhere(name, source)
		},
		listen: func(iface *net.Interface) (net.PacketConn, error) {
			conn, err := listenMDNSGroup(iface)
			if err != nil {
				return nil, err
			}
			return conn, nil
		},
		networks: func() []netip.Prefix { return mdnsLocalNetworks(config.bind) },
		group:    mdnsResponderGroup,
		recheck:  mdnsResponderRecheck,
	}, logf)
}

func runMDNSResponder(name string, env mdnsResponderEnv, logf logFunc) (stop func()) {
	responder := newMDNSResponder(name)
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		var (
			conn    net.PacketConn
			served  chan error // conn's serveMDNS result; nil once this loop closed conn
			joined  string     // the interface (name and index) and address conn answers for
			source  netip.Addr
			lastLog string
		)
		say := func(format string, args ...any) {
			if line := fmt.Sprintf(format, args...); line != lastLog {
				lastLog = line
				logf("%s", line)
			}
		}
		leave := func(goodbye bool) {
			if conn == nil {
				return
			}
			if addrs := mdnsAddrsOn(env.networks(), source); goodbye && len(addrs) > 0 {
				_, _ = conn.WriteTo(responder.goodbye(addrs), env.group)
			}
			conn.Close()
			conn, served, joined = nil, nil, ""
		}
		defer leave(true)
		ticker := time.NewTicker(env.recheck)
		defer ticker.Stop()
		for {
			addr, iface, ok := env.locate()
			switch {
			case !ok:
				leave(true)
				say("mdns: no private IPv4 address to answer for %s from; looking again every %s", name, env.recheck)
			default:
				// This agent's own answers carry only its addresses, so they
				// do not count: another host has the name (a VM, a second
				// machine) only if one of its addresses answers.
				if other, taken := env.elsewhere(name, addr); taken {
					leave(true)
					say("mdns: %s already answers for %s, so this agent does not; QR codes keep the address", other, name)
					break
				}
				// The index changes when an interface is made again under the
				// same name (a USB adapter plugged back in), and the old
				// membership went with the old one.
				key := fmt.Sprintf("%s#%d %s", iface.Name, iface.Index, addr)
				if conn != nil && key == joined {
					break
				}
				leave(true)
				c, err := env.listen(iface)
				if err != nil {
					say("mdns: cannot answer for %s on %s: %v", name, iface.Name, err)
					break
				}
				conn, joined, source = c, key, addr
				served = make(chan error, 1)
				go func(served chan<- error) {
					served <- serveMDNS(c, responder, env.networks, env.group)
				}(served)
				say("mdns: answering for %s on %s (%s)", name, iface.Name, addr)
			}
			select {
			case <-done:
				return
			case err := <-served:
				leave(false)
				say("mdns: stopped answering for %s: %v; trying again in %s", name, err, env.recheck)
				select {
				case <-done:
					return
				case <-ticker.C:
				}
			case <-ticker.C:
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			<-finished
		})
	}
}
