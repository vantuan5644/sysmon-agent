package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type healthDoer interface {
	Do(*http.Request) (*http.Response, error)
}

func waitForConfiguredHealth(config listenConfig, timeout time.Duration) error {
	if err := config.validate(); err != nil {
		return err
	}
	if timeout <= 0 {
		return fmt.Errorf("wait-health-timeout must be positive")
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	return waitForHealthy(ctx, healthCheckClient(config), healthCheckURL(config), 250*time.Millisecond)
}

func waitForConfiguredReady(config listenConfig, timeout time.Duration) error {
	if err := config.validate(); err != nil {
		return err
	}
	if timeout <= 0 {
		return fmt.Errorf("wait-ready-timeout must be positive")
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	return waitForReady(ctx, healthCheckClient(config), readinessCheckURL(config), 250*time.Millisecond)
}

func healthCheckClient(config listenConfig) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if config.tlsEnabled {
		// This is a local readiness probe for the running agent. Direct TLS may
		// use a self-signed certificate or a certificate for the public tailnet
		// name, so hostname verification would make the startup check brittle.
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	return &http.Client{
		Timeout:   time.Second,
		Transport: transport,
	}
}

func healthCheckURL(config listenConfig) string {
	return probeURL(config, "/healthz")
}

func readinessCheckURL(config listenConfig) string {
	return probeURL(config, "/readyz")
}

func probeURL(config listenConfig, path string) string {
	return (&url.URL{
		Scheme: config.scheme(),
		Host:   net.JoinHostPort(healthCheckHost(config.bind), strconv.Itoa(config.port)),
		Path:   path,
	}).String()
}

func healthCheckHost(bind string) string {
	raw := strings.TrimSpace(bind)
	switch raw {
	case "::", "[::]":
		return "::1"
	}
	if isWildcardBind(raw) {
		return "127.0.0.1"
	}
	normalized := normalizeBindHost(raw)
	if normalized == "" {
		return "127.0.0.1"
	}
	return normalized
}

func waitForHealthy(ctx context.Context, client healthDoer, healthURL string, interval time.Duration) error {
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastErr error
	for {
		if err := checkHealth(ctx, client, healthURL); err == nil {
			return nil
		} else {
			lastErr = err
		}

		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("%s did not become healthy: %w", healthURL, lastErr)
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func waitForReady(ctx context.Context, client healthDoer, readyURL string, interval time.Duration) error {
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastErr error
	for {
		if err := checkReadiness(ctx, client, readyURL); err == nil {
			return nil
		} else {
			lastErr = err
		}

		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("%s did not become ready: %w", readyURL, lastErr)
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func checkHealth(ctx context.Context, client healthDoer, healthURL string) error {
	body, err := fetchProbe(ctx, client, healthURL)
	if err != nil {
		return err
	}
	var parsed struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("decode health response: %w", err)
	}
	if parsed.Status != "ok" {
		return fmt.Errorf("health status = %q", parsed.Status)
	}
	return nil
}

func checkReadiness(ctx context.Context, client healthDoer, readyURL string) error {
	body, err := fetchProbe(ctx, client, readyURL)
	if err != nil {
		return err
	}
	var parsed struct {
		Status  string `json:"status"`
		Metrics bool   `json:"metrics"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("decode readiness response: %w", err)
	}
	if parsed.Status != "ok" {
		if strings.TrimSpace(parsed.Error) != "" {
			return fmt.Errorf("readiness status = %q: %s", parsed.Status, parsed.Error)
		}
		return fmt.Errorf("readiness status = %q", parsed.Status)
	}
	if !parsed.Metrics {
		return fmt.Errorf("readiness metrics = false")
	}
	return nil
}

func fetchProbe(ctx context.Context, client healthDoer, probeURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s returned %s", probeURL, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return nil, err
	}
	return body, nil
}
