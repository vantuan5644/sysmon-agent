package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHealthCheckURLUsesReachableLoopbackForWildcardBinds(t *testing.T) {
	tests := []struct {
		name string
		cfg  listenConfig
		want string
	}{
		{
			name: "ipv4 wildcard",
			cfg:  listenConfig{bind: "0.0.0.0", port: 9099},
			want: "http://127.0.0.1:9099/healthz",
		},
		{
			name: "empty wildcard",
			cfg:  listenConfig{port: 9099},
			want: "http://127.0.0.1:9099/healthz",
		},
		{
			name: "ipv6 wildcard",
			cfg:  listenConfig{bind: "::", port: 9099},
			want: "http://[::1]:9099/healthz",
		},
		{
			name: "loopback",
			cfg:  listenConfig{bind: "127.0.0.1", port: 9099},
			want: "http://127.0.0.1:9099/healthz",
		},
		{
			name: "tls direct bind",
			cfg:  listenConfig{bind: "192.168.1.20", port: 9443, tlsEnabled: true},
			want: "https://192.168.1.20:9443/healthz",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := healthCheckURL(tt.cfg); got != tt.want {
				t.Fatalf("healthCheckURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReadinessCheckURLUsesReadyzPath(t *testing.T) {
	cfg := listenConfig{bind: "0.0.0.0", port: 9099}
	if got, want := readinessCheckURL(cfg), "http://127.0.0.1:9099/readyz"; got != want {
		t.Fatalf("readinessCheckURL() = %q, want %q", got, want)
	}
}

func TestCheckHealthValidatesStatusPayload(t *testing.T) {
	client := healthDoerFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.Path != "/healthz" {
			t.Fatalf("request = %s %s, want GET /healthz", req.Method, req.URL.Path)
		}
		return healthResponse(http.StatusOK, `{"status":"ok"}`), nil
	})

	if err := checkHealth(context.Background(), client, "http://127.0.0.1:9099/healthz"); err != nil {
		t.Fatalf("checkHealth returned %v", err)
	}
}

func TestCheckReadinessValidatesMetricsPayload(t *testing.T) {
	client := healthDoerFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.Path != "/readyz" {
			t.Fatalf("request = %s %s, want GET /readyz", req.Method, req.URL.Path)
		}
		return healthResponse(http.StatusOK, `{"status":"ok","metrics":true}`), nil
	})

	if err := checkReadiness(context.Background(), client, "http://127.0.0.1:9099/readyz"); err != nil {
		t.Fatalf("checkReadiness returned %v", err)
	}
}

func TestCheckReadinessRejectsUnreadyResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "not ready with error", body: `{"status":"not_ready","error":"collector offline"}`},
		{name: "missing metrics true", body: `{"status":"ok","metrics":false}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := healthDoerFunc(func(req *http.Request) (*http.Response, error) {
				return healthResponse(http.StatusOK, tt.body), nil
			})
			if err := checkReadiness(context.Background(), client, "http://127.0.0.1:9099/readyz"); err == nil {
				t.Fatal("checkReadiness returned nil, want error")
			}
		})
	}
}

func TestCheckHealthRejectsUnhealthyResponses(t *testing.T) {
	tests := []struct {
		name string
		resp *http.Response
		err  error
	}{
		{
			name: "transport error",
			err:  errors.New("connection refused"),
		},
		{
			name: "bad status",
			resp: healthResponse(http.StatusServiceUnavailable, `{"status":"starting"}`),
		},
		{
			name: "bad payload",
			resp: healthResponse(http.StatusOK, `{"status":"starting"}`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := healthDoerFunc(func(req *http.Request) (*http.Response, error) {
				return tt.resp, tt.err
			})
			if err := checkHealth(context.Background(), client, "http://127.0.0.1:9099/healthz"); err == nil {
				t.Fatal("checkHealth returned nil, want error")
			}
		})
	}
}

func TestWaitForReadyRetriesUntilSuccess(t *testing.T) {
	attempts := 0
	client := healthDoerFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		if attempts < 3 {
			return healthResponse(http.StatusOK, `{"status":"not_ready","error":"warming up"}`), nil
		}
		return healthResponse(http.StatusOK, `{"status":"ok","metrics":true}`), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := waitForReady(ctx, client, "http://127.0.0.1:9099/readyz", time.Nanosecond); err != nil {
		t.Fatalf("waitForReady returned %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestWaitForHealthyRetriesUntilSuccess(t *testing.T) {
	attempts := 0
	client := healthDoerFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		if attempts < 3 {
			return nil, errors.New("connection refused")
		}
		return healthResponse(http.StatusOK, `{"status":"ok"}`), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := waitForHealthy(ctx, client, "http://127.0.0.1:9099/healthz", time.Nanosecond); err != nil {
		t.Fatalf("waitForHealthy returned %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

type healthDoerFunc func(*http.Request) (*http.Response, error)

func (f healthDoerFunc) Do(req *http.Request) (*http.Response, error) {
	return f(req)
}

func healthResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
