//go:build linux

package main

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// ipMulticastAll is IP_MULTICAST_ALL from linux/in.h; package syscall lacks it.
const ipMulticastAll = 49

// listenMDNSGroup opens the -mdns-name responder's socket. Two things differ
// from net.ListenMulticastUDP, which binds 0.0.0.0:5353, and both matter on a
// host where avahi and browsers share the port:
//
//   - It binds the group address, not 0.0.0.0, so unicast to port 5353 never
//     reaches it. Linux gives each unicast datagram to one of the sockets
//     sharing the port, the one bound last, and that would have been the agent:
//     avahi and the browsers would lose the unicast replies to their QU
//     questions. Sent from here, a reply still carries the interface's
//     address, since Linux picks the source for a socket bound to a group.
//   - IP_MULTICAST_ALL is off, so it hears the group only on iface, the one it
//     joined. Linux otherwise delivers the group from every interface any
//     program joined it on (avahi joins docker0 and the rest), and the reply,
//     multicast on iface, would carry another network's address with the
//     cache-flush bit.
//
// Multicast loopback stays on, as the kernel defaults it, so avahi on this
// host hears the replies and the host itself can look the name up. Hop limit
// 255, as RFC 6762 section 11 asks.
func listenMDNSGroup(iface *net.Interface) (*net.UDPConn, error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, syscall.IPPROTO_UDP)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "mdns")
	defer file.Close() // net.FilePacketConn keeps its own copy
	group := [4]byte{224, 0, 0, 251}
	for _, step := range []struct {
		what string
		do   func() error
	}{
		{"SO_REUSEADDR", func() error { return syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1) }},
		{"IP_MULTICAST_ALL", func() error { return syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, ipMulticastAll, 0) }},
		{"bind 224.0.0.251:5353", func() error {
			return syscall.Bind(fd, &syscall.SockaddrInet4{Port: mdnsPort, Addr: group})
		}},
		{"join on " + iface.Name, func() error {
			return syscall.SetsockoptIPMreqn(fd, syscall.IPPROTO_IP, syscall.IP_ADD_MEMBERSHIP,
				&syscall.IPMreqn{Multiaddr: group, Ifindex: int32(iface.Index)})
		}},
		{"IP_MULTICAST_IF", func() error {
			return syscall.SetsockoptIPMreqn(fd, syscall.IPPROTO_IP, syscall.IP_MULTICAST_IF,
				&syscall.IPMreqn{Ifindex: int32(iface.Index)})
		}},
		{"IP_MULTICAST_TTL", func() error { return syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_MULTICAST_TTL, 255) }},
		{"IP_TTL", func() error { return syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_TTL, 255) }},
	} {
		if err := step.do(); err != nil {
			return nil, fmt.Errorf("%s: %w", step.what, err)
		}
	}
	conn, err := net.FilePacketConn(file)
	if err != nil {
		return nil, err
	}
	udp, ok := conn.(*net.UDPConn)
	if !ok {
		conn.Close()
		return nil, fmt.Errorf("mdns socket is a %T, not UDP", conn)
	}
	return udp, nil
}
