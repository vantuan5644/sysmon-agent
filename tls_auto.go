package main

// Network TLS (docs/plans/pairing.md, "HTTPS on the network"). With pairing
// required, a device's cookie must not cross the network in the clear, so the
// agent answers network clients over HTTPS on its usual port, with -cert/-key
// when given or else a certificate it makes for itself. Plain HTTP on the same
// port is kept for this machine only (serve.sh, -wait-ready, the update
// helper, PexarBolt), and a network client that tries it is sent to https://.
//
// A self-signed certificate means each new device clicks through one browser
// warning. That is the price of encryption without a certificate authority;
// Tailscale Serve is the warning-free route and the installer prefers it.

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	networkTLSAuto = "auto"
	networkTLSOn   = "on"
	networkTLSOff  = "off"

	agentCertFile = "agent-cert.pem"
	agentKeyFile  = "agent-key.pem"

	// Apple refuses TLS server certificates valid for more than 825 days.
	agentCertLifetime = 825 * 24 * time.Hour
	agentCertRenewAt  = 30 * 24 * time.Hour

	// mixedPeekTimeout bounds how long a new connection may sit silent before
	// its first byte says whether it speaks TLS.
	mixedPeekTimeout = 10 * time.Second
)

func parseNetworkTLSMode(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", networkTLSAuto:
		return networkTLSAuto, nil
	case networkTLSOn, "true":
		return networkTLSOn, nil
	case networkTLSOff, "false":
		return networkTLSOff, nil
	}
	return "", fmt.Errorf("-network-tls must be auto, on or off, got %q", value)
}

// resolveNetworkTLS decides whether network clients get HTTPS. auto turns it
// on exactly when pairing is required, the agent is reachable from the
// network, and -tls (TLS only, with the caller's certificate) is not in use.
func resolveNetworkTLS(mode, pairingMode string, tlsEnabled bool, bind string) bool {
	if tlsEnabled {
		return false
	}
	switch mode {
	case networkTLSOn:
		return true
	case networkTLSOff:
		return false
	}
	return pairingMode == pairingModeRequired && !isLoopbackBind(bind)
}

// loadNetworkCertificate returns -cert/-key when both are set, else the
// agent's own certificate from dir (made on first use, and again when it
// nears expiry), or a fresh in-memory one when dir is empty.
func loadNetworkCertificate(certFile, keyFile, dir string, now time.Time) (tls.Certificate, error) {
	if certFile != "" || keyFile != "" {
		if certFile == "" || keyFile == "" {
			return tls.Certificate{}, errors.New("-cert and -key must be given together")
		}
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("load TLS certificate: %w", err)
		}
		return cert, nil
	}
	if dir == "" {
		certPEM, keyPEM, err := newAgentCertificate(now)
		if err != nil {
			return tls.Certificate{}, err
		}
		return tls.X509KeyPair(certPEM, keyPEM)
	}
	if err := ensurePrivateDir(dir); err != nil {
		return tls.Certificate{}, fmt.Errorf("certificate folder %s: %w", dir, err)
	}
	if cert, err := tls.LoadX509KeyPair(filepath.Join(dir, agentCertFile), filepath.Join(dir, agentKeyFile)); err == nil {
		if leaf, err := x509.ParseCertificate(cert.Certificate[0]); err == nil && leaf.NotAfter.Sub(now) > agentCertRenewAt {
			return cert, nil
		}
	}
	certPEM, keyPEM, err := newAgentCertificate(now)
	if err != nil {
		return tls.Certificate{}, err
	}
	// Key first: a certificate on disk without its key would be unusable.
	if err := writePrivateFile(dir, agentKeyFile, keyPEM); err != nil {
		return tls.Certificate{}, err
	}
	if err := writePrivateFile(dir, agentCertFile, certPEM); err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

// readAgentCertificateFingerprint returns the fingerprint of the certificate
// already in dir, or "" when there is none. It never creates one.
func readAgentCertificateFingerprint(dir string) string {
	if dir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(dir, agentCertFile))
	if err != nil {
		return ""
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return ""
	}
	return formatFingerprint(block.Bytes)
}

func certificateFingerprint(cert tls.Certificate) string {
	if len(cert.Certificate) == 0 {
		return ""
	}
	return formatFingerprint(cert.Certificate[0])
}

func formatFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

// newAgentCertificate makes a self-signed ECDSA P-256 server certificate
// naming this host, localhost and the machine's current addresses.
func newAgentCertificate(now time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, err
	}
	hostname, _ := os.Hostname()
	hostname = strings.TrimSpace(hostname)
	dnsNames := []string{"localhost"}
	if hostname != "" && !strings.EqualFold(hostname, "localhost") {
		dnsNames = append(dnsNames, hostname, hostname+".local")
	}
	ips := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP.IsLoopback() || ipNet.IP.IsLinkLocalUnicast() || ipNet.IP.IsMulticast() {
				continue
			}
			ips = append(ips, ipNet.IP)
		}
	}
	commonName := "Sysmon agent"
	if hostname != "" {
		commonName += " on " + hostname
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName, Organization: []string{"Sysmon agent (self-signed)"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(agentCertLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// mixedListener serves TLS and plain HTTP on one port. Each accepted
// connection is classified by its first byte (0x16 opens a TLS handshake) on
// its own goroutine, so a silent client never holds up the others. TLS
// connections come out as *tls.Conn, which net/http recognises: it runs the
// handshake and fills in Request.TLS.
type mixedListener struct {
	net.Listener
	tlsConfig *tls.Config
	conns     chan net.Conn
	errs      chan error
	done      chan struct{}
	closeOnce sync.Once
}

func newMixedListener(base net.Listener, tlsConfig *tls.Config) *mixedListener {
	l := &mixedListener{
		Listener:  base,
		tlsConfig: tlsConfig,
		conns:     make(chan net.Conn),
		errs:      make(chan error, 1),
		done:      make(chan struct{}),
	}
	go l.acceptLoop()
	return l
}

func (l *mixedListener) acceptLoop() {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			select {
			case <-l.done:
				return
			default:
			}
			var temporary interface{ Temporary() bool }
			if errors.As(err, &temporary) && temporary.Temporary() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			select {
			case l.errs <- err:
			case <-l.done:
			}
			return
		}
		go l.classify(conn)
	}
}

func (l *mixedListener) classify(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(mixedPeekTimeout))
	reader := bufio.NewReader(conn)
	first, err := reader.Peek(1)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		conn.Close()
		return
	}
	var out net.Conn = &peekedConn{Conn: conn, reader: reader}
	if first[0] == 0x16 {
		out = tls.Server(out, l.tlsConfig)
	}
	select {
	case l.conns <- out:
	case <-l.done:
		conn.Close()
	}
}

func (l *mixedListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case err := <-l.errs:
		return nil, err
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *mixedListener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return l.Listener.Close()
}

type peekedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// plainNetworkGuard lets plain HTTP through only from this machine. A network
// client is sent to the same address over https:// (reads) or refused.
func plainNetworkGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil || trustedForwardedHostSource(r.RemoteAddr) {
			next.ServeHTTP(w, r)
			return
		}
		if isReadMethod(r.Method) && strings.TrimSpace(r.Host) != "" {
			w.Header().Set("Cache-Control", "no-store")
			http.Redirect(w, r, "https://"+r.Host+r.URL.RequestURI(), http.StatusTemporaryRedirect)
			return
		}
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "this agent answers the network over HTTPS only; use its https:// address"})
	})
}
