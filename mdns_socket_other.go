//go:build !linux

package main

import "net"

// listenMDNSGroup opens the -mdns-name responder's socket. Elsewhere than
// Linux a socket hears a group only on the interfaces it joined it on, so the
// standard library's socket is enough (mdns_socket_linux.go says what Linux
// needs on top).
func listenMDNSGroup(iface *net.Interface) (*net.UDPConn, error) {
	return net.ListenMulticastUDP("udp4", iface, mdnsResponderGroup)
}
