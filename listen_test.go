package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestListenConfigAddrAndScheme(t *testing.T) {
	cfg := listenConfig{bind: "127.0.0.1", port: 9099}
	if cfg.addr() != "127.0.0.1:9099" {
		t.Fatalf("addr = %q", cfg.addr())
	}
	if cfg.scheme() != "http" {
		t.Fatalf("scheme = %q", cfg.scheme())
	}

	cfg.tlsEnabled = true
	if cfg.scheme() != "https" {
		t.Fatalf("TLS scheme = %q", cfg.scheme())
	}
}

func TestListenConfigRequiresTLSFiles(t *testing.T) {
	cfg := listenConfig{bind: "127.0.0.1", port: 9099, tlsEnabled: true}
	if err := cfg.validate(); err == nil {
		t.Fatal("validate accepted TLS without cert/key")
	}

	cfg.certFile = "cert.pem"
	cfg.keyFile = "key.pem"
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate rejected TLS with cert/key: %v", err)
	}
}

func TestListenConfigRejectsInvalidPort(t *testing.T) {
	for _, port := range []int{0, -1, 65536} {
		cfg := listenConfig{bind: "127.0.0.1", port: port}
		if err := cfg.validate(); err == nil {
			t.Fatalf("validate accepted port %d", port)
		}
	}
}

func TestServeConfiguredHTTPDoesNotLogBeforeBind(t *testing.T) {
	cfg := listenConfig{bind: "127.0.0.1", port: 9099}
	var logs []string

	err := serveConfiguredHTTP(&http.Server{}, cfg, func(network, address string) (net.Listener, error) {
		return nil, errors.New("bind failed")
	}, func(format string, args ...any) {
		logs = append(logs, format)
	})

	if err == nil || !strings.Contains(err.Error(), "bind failed") {
		t.Fatalf("error = %v, want bind failed", err)
	}
	if len(logs) != 0 {
		t.Fatalf("logs before bind failure = %+v, want none", logs)
	}
}

func TestServeConfiguredHTTPDoesNotBindOrLogBeforeTLSLoad(t *testing.T) {
	cfg := listenConfig{
		bind:       "127.0.0.1",
		port:       9099,
		tlsEnabled: true,
		certFile:   "missing-cert.pem",
		keyFile:    "missing-key.pem",
	}
	var logs []string
	listenCalled := false

	err := serveConfiguredHTTP(&http.Server{}, cfg, func(network, address string) (net.Listener, error) {
		listenCalled = true
		return nil, errors.New("listen should not be called")
	}, func(format string, args ...any) {
		logs = append(logs, format)
	})

	if err == nil || !strings.Contains(err.Error(), "load TLS certificate") {
		t.Fatalf("error = %v, want TLS load failure", err)
	}
	if listenCalled {
		t.Fatal("listen was called before TLS certificate load succeeded")
	}
	if len(logs) != 0 {
		t.Fatalf("logs before TLS load failure = %+v, want none", logs)
	}
}

func TestServeConfiguredHTTPLogsAfterBind(t *testing.T) {
	cfg := listenConfig{bind: "127.0.0.1", port: 9099}
	listener := failingListener{
		addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9099},
		err:  errors.New("accept stopped"),
	}
	var logs []string

	err := serveConfiguredHTTP(&http.Server{}, cfg, func(network, address string) (net.Listener, error) {
		if network != "tcp" || address != "127.0.0.1:9099" {
			t.Fatalf("listen(%q, %q), want tcp 127.0.0.1:9099", network, address)
		}
		return listener, nil
	}, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})

	if err == nil || !strings.Contains(err.Error(), "accept stopped") {
		t.Fatalf("error = %v, want accept stopped", err)
	}
	if len(logs) == 0 || !strings.Contains(logs[0], "sysmon-agent listening") {
		t.Fatalf("logs after bind = %+v, want listening log", logs)
	}
	if len(logs) < 2 || !strings.Contains(logs[1], "local dashboard URL: http://127.0.0.1:9099/") {
		t.Fatalf("logs after bind = %+v, want rendered dashboard URL", logs)
	}
}

type failingListener struct {
	addr net.Addr
	err  error
}

func (l failingListener) Accept() (net.Conn, error) {
	return nil, l.err
}

func (l failingListener) Close() error {
	return nil
}

func (l failingListener) Addr() net.Addr {
	return l.addr
}
