package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func newHTTPHandler(collector MetricsCollector, static fs.FS) (http.Handler, error) {
	return newHTTPHandlerWithState(collector, static, NewMemoryRuntimeState())
}

func newHTTPHandlerWithState(collector MetricsCollector, static fs.FS, state *RuntimeState) (http.Handler, error) {
	return newHTTPHandlerWithController(collector, static, state, NewSystemController())
}

func newHTTPHandlerWithController(collector MetricsCollector, static fs.FS, state *RuntimeState, controller SystemController) (http.Handler, error) {
	staticSub, err := fs.Sub(static, "static")
	if err != nil {
		return nil, err
	}
	metricsCollector := newCachedMetricsCollector(newCoalescingMetricsCollector(collector), metricsCacheTTL)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(context.Background(), metricsCollectTimeout)
		defer cancel()

		metrics, err := metricsCollector.Collect(ctx)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, ReadinessStatus{Status: "not_ready", Error: err.Error()})
			return
		}
		if err := validateMetricsTimestampFreshness(metrics.Timestamp, time.Now()); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, ReadinessStatus{Status: "not_ready", Error: "metrics timestamp: " + err.Error()})
			return
		}
		if err := validateMetricsShape(metrics); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, ReadinessStatus{Status: "not_ready", Error: "metrics schema: " + err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, ReadinessStatus{
			Status:           "ok",
			Metrics:          true,
			Hostname:         metrics.Hostname,
			Timestamp:        metrics.Timestamp,
			CollectionErrors: metrics.CollectionErrors,
		})
	})
	mux.HandleFunc("GET /api/metrics", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(context.Background(), metricsCollectTimeout)
		defer cancel()

		metrics, err := metricsCollector.Collect(ctx)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, metrics)
	})
	// Server-Sent Events stream of warm snapshots, registered only when the
	// underlying collector is a resident sampler (metricsStreamer). The detection
	// uses the original collector, before the caching/coalescing wrappers, because
	// those wrappers do not implement Subscribe. Clients fall back to polling
	// /api/metrics when this route is absent.
	if streamer, ok := collector.(metricsStreamer); ok {
		mux.HandleFunc("GET /api/stream", func(w http.ResponseWriter, r *http.Request) {
			flusher, ok := w.(http.Flusher)
			if !ok {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
				return
			}
			// This is a long-lived response, so clear the server's WriteTimeout for
			// this connection; otherwise the stream would be torn down every few
			// seconds. Connection-level deadline reset via the response controller.
			if rc := http.NewResponseController(w); rc != nil {
				_ = rc.SetWriteDeadline(time.Time{})
			}

			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(http.StatusOK)
			flusher.Flush()

			ch, cancel := streamer.Subscribe()
			defer cancel()

			ctx := r.Context()
			keepalive := time.NewTicker(streamKeepaliveInterval)
			defer keepalive.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case data, ok := <-ch:
					if !ok {
						return
					}
					if err := writeSSE(w, data); err != nil {
						return
					}
					flusher.Flush()
				case <-keepalive.C:
					if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
						return
					}
					flusher.Flush()
				}
			}
		})
	}
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, newAgentStatus(state, controller.Capabilities(), time.Now().UTC()))
	})
	mux.HandleFunc("GET /api/settings", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, state.GetSettings())
	})
	mux.HandleFunc("GET /api/client-check", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, state.GetClientCheck())
	})
	mux.HandleFunc("GET /api/client-checks", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, state.GetClientCheckHistory())
	})
	mux.HandleFunc("POST /api/client-check", func(w http.ResponseWriter, r *http.Request) {
		if !sameOriginBrowserRequest(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "client checks require same-origin browser requests"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		defer r.Body.Close()

		var update ClientCheckUpdate
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&update); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request body must contain a single JSON object"})
			return
		}
		check, err := state.RecordClientCheck(update, r.UserAgent(), time.Now().UTC())
		if err != nil {
			var validationErr *SettingsValidationError
			if errors.As(err, &validationErr) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, check)
	})
	mux.HandleFunc("POST /api/settings", func(w http.ResponseWriter, r *http.Request) {
		if !sameOriginBrowserRequest(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "settings updates require same-origin browser requests"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		defer r.Body.Close()

		var update DashboardSettingsUpdate
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&update); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request body must contain a single JSON object"})
			return
		}
		settings, err := state.UpdateSettings(update)
		if err != nil {
			var validationErr *SettingsValidationError
			if errors.As(err, &validationErr) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, settings)
	})
	mux.HandleFunc("POST /api/control", func(w http.ResponseWriter, r *http.Request) {
		if !sameOriginBrowserRequest(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "control actions require same-origin browser requests"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
		defer r.Body.Close()

		var request ControlRequest
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request body must contain a single JSON object"})
			return
		}
		if !isKnownControlAction(request.Action) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "action must be one of mic_mute, media_toggle, volume_mute, lock_screen"})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), controlTimeout)
		defer cancel()
		writeJSON(w, http.StatusOK, controller.Apply(ctx, request.Action))
	})
	mux.Handle("GET /", staticAssetHandler(staticSub))
	return securityHeaders(mux), nil
}

func staticAssetHandler(static fs.FS) http.Handler {
	files := http.FileServer(http.FS(static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}

// metricsCollectTimeout bounds a single metrics collection pass. Windows hosts
// running the LibreHardwareMonitor bridge plus GPU and CIM queries can need
// several seconds on a cold sample, so this is more generous than per-route
// health windows.
const metricsCollectTimeout = 8 * time.Second

// streamKeepaliveInterval bounds how long an idle SSE connection waits before a
// comment line is written. It keeps intermediaries (and Tailscale Serve) from
// dropping the connection and lets the server notice a dead client via the
// write error during quiet periods.
const streamKeepaliveInterval = 15 * time.Second

// writeSSE writes one Server-Sent Events "data:" frame. The payload is a single
// line of compact JSON (json.Marshal escapes any newlines inside strings), so a
// single data line followed by the blank-line terminator is a valid event.
func writeSSE(w io.Writer, data []byte) error {
	if _, err := io.WriteString(w, "data: "); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n\n")
	return err
}

const contentSecurityPolicy = "default-src 'self'; " +
	"connect-src 'self'; " +
	"img-src 'self' data:; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"worker-src 'self'; " +
	"manifest-src 'self'; " +
	"object-src 'none'; " +
	"base-uri 'none'; " +
	"form-action 'none'; " +
	"frame-ancestors 'none'"

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func sameOriginRequest(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	return sameOriginOriginMatchesRequest(r, origin)
}

func sameOriginBrowserRequest(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return false
	}
	return sameOriginOriginMatchesRequest(r, origin)
}

func sameOriginOriginMatchesRequest(r *http.Request, origin string) bool {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	originHosts := originHostVariants(parsed.Scheme, parsed.Host)
	for _, host := range requestHosts(r) {
		for _, requestHost := range originHostVariants(parsed.Scheme, host) {
			for _, originHost := range originHosts {
				if originHost == requestHost {
					return true
				}
			}
		}
	}
	return false
}

func originHostVariants(scheme, rawHost string) []string {
	host := strings.TrimSpace(rawHost)
	if host == "" {
		return nil
	}

	var variants []string
	add := func(value string) {
		value = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
		if value == "" {
			return
		}
		for _, existing := range variants {
			if existing == value {
				return
			}
		}
		variants = append(variants, value)
	}

	add(host)
	if splitHost, port, err := net.SplitHostPort(host); err == nil {
		normalizedHost := normalizeOriginHost(splitHost)
		add(net.JoinHostPort(normalizedHost, port))
		if port == defaultPortForScheme(scheme) {
			add(normalizedHost)
		}
		return variants
	}

	normalizedHost := normalizeOriginHost(host)
	add(normalizedHost)
	if defaultPort := defaultPortForScheme(scheme); defaultPort != "" {
		add(net.JoinHostPort(normalizedHost, defaultPort))
	}
	return variants
}

func normalizeOriginHost(host string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimSuffix(strings.TrimSpace(host), "]"), "["), ".")
}

func defaultPortForScheme(scheme string) string {
	switch strings.ToLower(strings.TrimSpace(scheme)) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

func requestHosts(r *http.Request) []string {
	hosts := []string{}
	if host := strings.TrimSpace(r.Host); host != "" {
		hosts = append(hosts, host)
	}
	if !trustedForwardedHostSource(r.RemoteAddr) {
		return hosts
	}
	for _, forwardedHost := range r.Header.Values("X-Forwarded-Host") {
		for _, host := range strings.Split(forwardedHost, ",") {
			if trimmed := strings.TrimSpace(host); trimmed != "" {
				hosts = append(hosts, trimmed)
			}
		}
	}
	hosts = append(hosts, forwardedHeaderHosts(r.Header.Values("Forwarded"))...)
	return hosts
}

func forwardedHeaderHosts(values []string) []string {
	var hosts []string
	for _, value := range values {
		for _, element := range strings.Split(value, ",") {
			for _, pair := range strings.Split(element, ";") {
				key, rawValue, ok := strings.Cut(strings.TrimSpace(pair), "=")
				if !ok || !strings.EqualFold(strings.TrimSpace(key), "host") {
					continue
				}
				host := strings.TrimSpace(rawValue)
				if len(host) >= 2 && host[0] == '"' && host[len(host)-1] == '"' {
					host = strings.ReplaceAll(host[1:len(host)-1], `\"`, `"`)
				}
				if host != "" {
					hosts = append(hosts, host)
				}
			}
		}
	}
	return hosts
}

func trustedForwardedHostSource(remoteAddr string) bool {
	host := strings.TrimSpace(remoteAddr)
	if host == "" {
		return false
	}
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
	}
	host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	data, err := json.Marshal(value)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"json encode failed"}` + "\n"))
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}
