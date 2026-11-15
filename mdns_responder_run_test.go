package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

// A query is read from anyone on the LAN, so a name is walked within bounds:
// at most 255 bytes, pointers only backwards, at most 16 of them.
func TestDNSNameWalkIsBounded(t *testing.T) {
	header := make([]byte, 12)
	name, _ := encodeDNSName("bblwin.local")
	labels := newMDNSResponder("bblwin.local").labels
	at := func(parts ...[]byte) []byte { return bytes.Join(append([][]byte{header}, parts...), nil) }
	pointer := func(off int) []byte { return []byte{0xC0 | byte(off>>8), byte(off)} }

	upper, _ := encodeDNSName("BBLWin.LOCAL")
	if next, ok, same := dnsNameIs(at(upper), 12, labels); !ok || !same || next != 12+len(upper) {
		t.Fatalf("BBLWin.LOCAL: next %d ok %v same %v; want the name, any case", next, ok, same)
	}
	compressed := at(name, pointer(12))
	if next, ok, same := dnsNameIs(compressed, 12+len(name), labels); !ok || !same || next != len(compressed) {
		t.Fatalf("a pointer back to the name: next %d ok %v same %v", next, ok, same)
	}
	for _, other := range []string{"bblwin", "bblwin.local.lan", "omarchy.local"} {
		encoded, _ := encodeDNSName(other)
		if _, ok, same := dnsNameIs(at(encoded), 12, labels); !ok || same {
			t.Fatalf("%s: ok %v same %v; want a different, well-formed name", other, ok, same)
		}
	}

	for label, msg := range map[string][]byte{
		"a pointer forwards":     at(pointer(14), name),
		"a pointer to itself":    at(pointer(12)),
		"a pointer past the end": at(pointer(0x3FFF)),
		"a label past the end":   at([]byte{9, 'b', 'b'}),
		"a reserved label type":  at([]byte{0x40, 0}),
	} {
		if _, ok, _ := dnsNameIs(msg, 12, labels); ok {
			t.Errorf("%s: read as well-formed", label)
		}
	}

	// 127 one-byte labels are 254 bytes and fit; 128 are 256 and do not.
	long := func(n int) []byte { return append(bytes.Repeat([]byte{1, 'a'}, n), 0) }
	if _, _, ok := readDNSName(at(long(127)), 12); !ok {
		t.Fatalf("a 255-byte name was refused")
	}
	if _, _, ok := readDNSName(at(long(128)), 12); ok {
		t.Fatalf("a 257-byte name was read")
	}

	// Each pointer points at the one before it; the name follows the chain.
	chain := at([]byte{1, 'a', 0})
	offsets := []int{12}
	for i := 0; i < 17; i++ {
		offsets = append(offsets, len(chain))
		chain = append(chain, pointer(offsets[i])...)
	}
	if _, _, ok := readDNSName(chain, offsets[16]); !ok {
		t.Fatalf("16 pointers were refused")
	}
	if _, _, ok := readDNSName(chain, offsets[17]); ok {
		t.Fatalf("17 pointers were followed")
	}

	// The review's packet: a 4.4 KB name and 764 questions pointing back at it.
	attack := at(bytes.Repeat([]byte{1, 'a'}, 2200), []byte{0, 0, 1, 0, 1})
	for i := 0; i < 764; i++ {
		attack = append(append(attack, pointer(12)...), 0, 1, 0, 1)
	}
	binary.BigEndian.PutUint16(attack[4:], 765)
	if _, ok := newMDNSResponder("bblwin.local").reply(attack, phone, omarchyNetworks, time.Unix(1000, 0)); ok {
		t.Fatalf("answered a packet whose name is 4.4 KB long")
	}
}

// BenchmarkMDNSResponderWorstQuery is the costliest packet the bounds allow:
// a 254-byte name and as many questions pointing back at it as fit in 9000
// bytes. Run with go test -run '^$' -bench WorstQuery.
func BenchmarkMDNSResponderWorstQuery(b *testing.B) {
	msg := append(make([]byte, 12), bytes.Repeat([]byte{1, 'a'}, 127)...)
	msg = append(msg, 0, 0, 1, 0, 1)
	count := 1
	for len(msg)+6 <= 9000 {
		msg = append(msg, 0xC0, 12, 0, 1, 0, 1)
		count++
	}
	binary.BigEndian.PutUint16(msg[4:], uint16(count))
	r := newMDNSResponder("bblwin.local")
	now := time.Unix(1000, 0)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, ok := r.reply(msg, phone, omarchyNetworks, now); ok {
			b.Fatal("answered a question about another name")
		}
	}
}

// RFC 6762 section 6 spaces each record on its own clock: an AAAA question's
// NSEC does not hold back the A answer that follows, and a host asking for
// AAAA every 900 ms cannot keep the A record from being multicast.
func TestMDNSResponderSpacesEachRecord(t *testing.T) {
	r := newMDNSResponder("bblwin.local")
	start := time.Unix(1000, 0)
	aaaa := mdnsTestQuery(0, mdnsTestQuestion{"bblwin.local", mdnsTypeAAAA, mdnsClassIN})
	a := mdnsTestQuery(0, mdnsTestQuestion{"bblwin.local", mdnsTypeA, mdnsClassIN})
	kinds := func(rrs []mdnsTestRR) string {
		var out []string
		for _, rr := range rrs {
			out = append(out, map[uint16]string{mdnsTypeA: "A", mdnsTypeNSEC: "NSEC"}[rr.kind])
		}
		return strings.Join(out, ",")
	}
	for _, step := range []struct {
		at         time.Duration
		query      []byte
		answers    string // "" with additional "" means no reply at all
		additional string
	}{
		{0, aaaa, "NSEC", ""},
		{200 * time.Millisecond, a, "A,A", ""}, // the NSEC went 200 ms ago
		{400 * time.Millisecond, a, "", ""},    // the A records went 200 ms ago
		{900 * time.Millisecond, aaaa, "", ""}, // the NSEC went 900 ms ago
		{1100 * time.Millisecond, aaaa, "NSEC", ""},
		{1300 * time.Millisecond, a, "A,A", ""},
		{2400 * time.Millisecond, a, "A,A", "NSEC"},
	} {
		reply, ok := r.reply(step.query, phone, omarchyNetworks, start.Add(step.at))
		if step.answers == "" && step.additional == "" {
			if ok {
				t.Fatalf("at %s: multicast %x; want nothing", step.at, reply.msg)
			}
			continue
		}
		if !ok || reply.unicast {
			t.Fatalf("at %s: reply %v, unicast %v; want a multicast reply", step.at, ok, reply.unicast)
		}
		_, _, answers, additional := parseMDNSTestReply(t, reply.msg)
		if kinds(answers) != step.answers || kinds(additional) != step.additional {
			t.Fatalf("at %s: answers [%s] additional [%s]; want [%s] [%s]", step.at, kinds(answers), kinds(additional), step.answers, step.additional)
		}
	}
}

func TestMDNSResponderGoodbye(t *testing.T) {
	msg := newMDNSResponder("bblwin.local").goodbye([]netip.Addr{netip.MustParseAddr("10.0.0.4"), netip.MustParseAddr("10.0.0.114")})
	assertGoodbye(t, msg, "10.0.0.4 10.0.0.114")
}

func assertGoodbye(t *testing.T, msg []byte, want string) {
	t.Helper()
	id, questions, answers, additional := parseMDNSTestReply(t, msg)
	if id != 0 || questions != 0 || len(additional) != 0 {
		t.Fatalf("goodbye id %d, %d questions, %d additional; want 0, 0, 0", id, questions, len(additional))
	}
	var got []string
	for _, rr := range answers {
		if rr.kind != mdnsTypeA || rr.class != mdnsClassIN|mdnsClassTopBit || rr.ttl != 0 || rr.name != "bblwin.local" {
			t.Fatalf("goodbye record %+v; want a cache-flush A record for bblwin.local with TTL 0", rr)
		}
		got = append(got, netip.AddrFrom4([4]byte(rr.data)).String())
	}
	if strings.Join(got, " ") != want {
		t.Fatalf("goodbye withdrew %v; want %s", got, want)
	}
}

// mdnsFakeSystem stands in for the network: loopback sockets in place of the
// group, and an interface, an address and another host the test controls.
type mdnsFakeSystem struct {
	t      *testing.T
	group  *net.UDPConn // where multicast replies and goodbyes land
	opened chan net.PacketConn
	logs   chan string

	mu      sync.Mutex
	iface   net.Interface
	other   netip.Addr // another host answering for the name, if valid
	located bool
}

func newMDNSFakeSystem(t *testing.T) *mdnsFakeSystem {
	group, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Skipf("no loopback UDP: %v", err)
	}
	t.Cleanup(func() { group.Close() })
	return &mdnsFakeSystem{
		t:       t,
		group:   group,
		opened:  make(chan net.PacketConn, 16),
		logs:    make(chan string, 256),
		iface:   net.Interface{Index: 3, Name: "wlp9s0"},
		located: true,
	}
}

func (f *mdnsFakeSystem) env() mdnsResponderEnv {
	return mdnsResponderEnv{
		locate: func() (netip.Addr, *net.Interface, bool) {
			f.mu.Lock()
			defer f.mu.Unlock()
			iface := f.iface
			return netip.MustParseAddr("127.0.0.1"), &iface, f.located
		},
		elsewhere: func(string, netip.Addr) (netip.Addr, bool) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.other, f.other.IsValid()
		},
		listen: func(*net.Interface) (net.PacketConn, error) {
			conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err == nil {
				f.opened <- conn
			}
			return conn, err
		},
		networks: func() []netip.Prefix { return []netip.Prefix{netip.MustParsePrefix("127.0.0.1/8")} },
		group:    f.group.LocalAddr(),
		recheck:  20 * time.Millisecond,
	}
}

func (f *mdnsFakeSystem) logf(format string, args ...any) {
	select {
	case f.logs <- fmt.Sprintf(format, args...):
	default:
	}
}

func (f *mdnsFakeSystem) waitLog(contains string) {
	f.t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case line := <-f.logs:
			if strings.Contains(line, contains) {
				return
			}
		case <-deadline:
			f.t.Fatalf("no log line with %q", contains)
		}
	}
}

func (f *mdnsFakeSystem) waitOpened() net.PacketConn {
	f.t.Helper()
	select {
	case conn := <-f.opened:
		return conn
	case <-time.After(3 * time.Second):
		f.t.Fatalf("the responder opened no socket")
		return nil
	}
}

func (f *mdnsFakeSystem) waitGoodbye() {
	f.t.Helper()
	buf := make([]byte, 1500)
	_ = f.group.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, _, err := f.group.ReadFrom(buf)
	if err != nil {
		f.t.Fatalf("no goodbye reached the group: %v", err)
	}
	assertGoodbye(f.t, buf[:n], "127.0.0.1")
}

func (f *mdnsFakeSystem) set(change func(*mdnsFakeSystem)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

func assertClosed(t *testing.T, conn net.PacketConn, why string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, err := conn.WriteTo([]byte{0}, conn.LocalAddr())
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: the socket is still open", why)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The responder keeps asking for the name while it answers, rejoins when its
// socket dies or its interface is made again, and says goodbye when it stops.
func TestMDNSResponderSupervisor(t *testing.T) {
	f := newMDNSFakeSystem(t)
	stop := runMDNSResponder("bblwin.local", f.env(), f.logf)
	stopped := false
	defer func() {
		if !stopped {
			stop()
		}
	}()

	first := f.waitOpened()
	f.waitLog("answering for bblwin.local on wlp9s0")

	// Another host takes the name: goodbye, close, and say so.
	f.set(func(f *mdnsFakeSystem) { f.other = netip.MustParseAddr("10.0.0.99") })
	f.waitGoodbye()
	assertClosed(t, first, "another host answers")
	f.waitLog("10.0.0.99 already answers for bblwin.local")

	// It goes away again.
	f.set(func(f *mdnsFakeSystem) { f.other = netip.Addr{} })
	second := f.waitOpened()
	f.waitLog("answering for bblwin.local")

	// The socket dies under it: no goodbye (it cannot send), a log line, and a
	// new socket on the next look.
	second.Close()
	f.waitLog("stopped answering for bblwin.local")
	third := f.waitOpened()
	f.waitLog("answering for bblwin.local")

	// The interface is made again under the same name: rejoin.
	f.set(func(f *mdnsFakeSystem) { f.iface.Index = 7 })
	f.waitGoodbye()
	fourth := f.waitOpened()
	assertClosed(t, third, "the interface was made again")

	// No address to answer from.
	f.set(func(f *mdnsFakeSystem) { f.located = false })
	f.waitGoodbye()
	assertClosed(t, fourth, "no address")
	f.waitLog("no private IPv4 address")
	f.set(func(f *mdnsFakeSystem) { f.located = true })
	fifth := f.waitOpened()

	// stop says goodbye and closes the socket before it returns.
	stop()
	stopped = true
	f.waitGoodbye()
	assertClosed(t, fifth, "stop")
}
