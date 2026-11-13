package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strconv"
)

type listenConfig struct {
	bind       string
	port       int
	tlsEnabled bool
	certFile   string
	keyFile    string
	// networkTLS serves network clients over HTTPS with networkCert while
	// plain HTTP on the same port stays open to loopback (tls_auto.go).
	networkTLS  bool
	networkCert *tls.Certificate
}

type listenFunc func(network, address string) (net.Listener, error)
type logFunc func(format string, args ...any)

func (c listenConfig) addr() string {
	return net.JoinHostPort(c.bind, strconv.Itoa(c.port))
}

func (c listenConfig) scheme() string {
	if c.tlsEnabled {
		return "https"
	}
	return "http"
}

// publicScheme is the scheme other devices use, for the URLs the agent prints.
// scheme() names the listener in its startup log line: plain HTTP still answers loopback
// when only network clients get TLS.
func (c listenConfig) publicScheme() string {
	if c.tlsEnabled || c.networkTLS {
		return "https"
	}
	return "http"
}

func (c listenConfig) validate() error {
	if c.port <= 0 || c.port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	if c.tlsEnabled && (c.certFile == "" || c.keyFile == "") {
		return fmt.Errorf("-cert and -key are required when -tls is enabled")
	}
	return nil
}

// validateServe adds what only the serving process needs. The -wait-health
// and -wait-ready checks use validate alone: they reach the agent over
// loopback (or over HTTPS when bound to one address, see healthCheckScheme)
// and never load its certificate.
func (c listenConfig) validateServe() error {
	if err := c.validate(); err != nil {
		return err
	}
	if c.networkTLS && !c.tlsEnabled && c.networkCert == nil {
		return fmt.Errorf("network TLS needs a certificate")
	}
	return nil
}

func serveConfiguredHTTP(server *http.Server, config listenConfig, listen listenFunc, logf logFunc) error {
	if config.tlsEnabled {
		if err := configureServerTLS(server, config); err != nil {
			return err
		}
	}
	listener, err := listen("tcp", config.addr())
	if err != nil {
		return err
	}
	if config.networkTLS && !config.tlsEnabled && config.networkCert != nil {
		tlsConfig := &tls.Config{
			Certificates: []tls.Certificate{*config.networkCert},
			MinVersion:   tls.VersionTLS12,
			NextProtos:   []string{"http/1.1"},
		}
		server.Handler = plainNetworkGuard(server.Handler)
		logf("sysmon-agent listening on https://%s (plain http only from this machine)", config.addr())
		if fingerprint := certificateFingerprint(*config.networkCert); fingerprint != "" {
			logf("TLS certificate SHA-256 %s", fingerprint)
		}
		logAccessURLsWithLogger(config, logf)
		return server.Serve(newMixedListener(listener, tlsConfig))
	}
	logf("sysmon-agent listening on %s://%s", config.scheme(), config.addr())
	logAccessURLsWithLogger(config, logf)
	if config.tlsEnabled {
		return server.ServeTLS(listener, "", "")
	}
	return server.Serve(listener)
}

func configureServerTLS(server *http.Server, config listenConfig) error {
	cert, err := tls.LoadX509KeyPair(config.certFile, config.keyFile)
	if err != nil {
		return fmt.Errorf("load TLS certificate: %w", err)
	}
	tlsConfig := &tls.Config{}
	if server.TLSConfig != nil {
		tlsConfig = server.TLSConfig.Clone()
	}
	tlsConfig.Certificates = append([]tls.Certificate{cert}, tlsConfig.Certificates...)
	server.TLSConfig = tlsConfig
	return nil
}
