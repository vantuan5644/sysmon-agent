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

func (c listenConfig) validate() error {
	if c.port <= 0 || c.port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	if c.tlsEnabled && (c.certFile == "" || c.keyFile == "") {
		return fmt.Errorf("-cert and -key are required when -tls is enabled")
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
