package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path"
	"strings"
	"time"
)

// The HTTP half of device pairing (pairing.go has the store). requirePairing
// sits in front of the whole mux; the routes below are how a device pairs and
// how a paired one manages the others.

const (
	pairingCookieName   = "sysmon_device"
	pairingCookieMaxAge = 400 * 24 * time.Hour
)

// SetPairing wires the pairing store; nil (the default) means pairing is off
// and every request passes as before.
func (s *RuntimeState) SetPairing(store *pairingStore) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pairing = store
}

func (s *RuntimeState) Pairing() *pairingStore {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pairing
}

type pairedDeviceKey struct{}

func pairedDeviceFrom(ctx context.Context) (PairedDeviceInfo, bool) {
	device, ok := ctx.Value(pairedDeviceKey{}).(PairedDeviceInfo)
	return device, ok
}

// requestPairingToken reads a bearer token, else the pairing cookie.
func requestPairingToken(r *http.Request) (token string, viaCookie bool) {
	if header := strings.TrimSpace(r.Header.Get("Authorization")); header != "" {
		scheme, value, ok := strings.Cut(header, " ")
		if ok && strings.EqualFold(scheme, "Bearer") {
			return strings.TrimSpace(value), false
		}
	}
	if cookie, err := r.Cookie(pairingCookieName); err == nil {
		return cookie.Value, true
	}
	return "", false
}

func cleanRequestPath(r *http.Request) string {
	return path.Clean("/" + r.URL.Path)
}

func isReadMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}

// pairingOpenPath lists what an unpaired client may reach: the liveness
// check, the pairing routes, and the static dashboard shell, which is the
// same public source for every install and carries no host data.
func pairingOpenPath(r *http.Request) bool {
	p := cleanRequestPath(r)
	switch {
	case p == "/healthz":
		return true
	case p == "/api/pair/state":
		return isReadMethod(r.Method)
	case p == "/api/pair":
		return r.Method == http.MethodPost
	case p == "/readyz" || p == "/api" || strings.HasPrefix(p, "/api/"):
		return false
	}
	return isReadMethod(r.Method)
}

// proxyHeaders are added by reverse proxies. Tailscale Serve connects from
// loopback for every tailnet client, always with X-Forwarded-For, so a
// loopback connection carrying any of these is treated as remote. A proxy
// that adds none of them (nginx without proxy_set_header) would look local;
// the README says so.
var proxyHeaders = []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded", "X-Real-Ip", "Via"}

func hasProxyHeaders(header http.Header) bool {
	for _, name := range proxyHeaders {
		if len(header.Values(name)) > 0 {
			return true
		}
	}
	for name := range header {
		if strings.HasPrefix(http.CanonicalHeaderKey(name), "Tailscale-") {
			return true
		}
	}
	return false
}

// loopbackDirectRequest is a request from this machine, not through a proxy,
// addressed to a loopback name. Such requests may read without pairing, so
// local tooling (serve.sh, -wait-ready, the update helper, installer status,
// the PexarBolt plugin) keeps working.
func loopbackDirectRequest(r *http.Request) bool {
	if !trustedForwardedHostSource(r.RemoteAddr) || hasProxyHeaders(r.Header) {
		return false
	}
	host := strings.TrimSpace(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// requestIsHTTPS is true for direct TLS, or for a loopback proxy that says
// the client used HTTPS (Tailscale Serve sets X-Forwarded-Proto).
func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if !trustedForwardedHostSource(r.RemoteAddr) {
		return false
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		first, _, _ := strings.Cut(proto, ",")
		return strings.EqualFold(strings.TrimSpace(first), "https")
	}
	for _, value := range r.Header.Values("Forwarded") {
		first, _, _ := strings.Cut(value, ",")
		for _, pair := range strings.Split(first, ";") {
			key, val, ok := strings.Cut(strings.TrimSpace(pair), "=")
			if ok && strings.EqualFold(strings.TrimSpace(key), "proto") {
				return strings.EqualFold(strings.Trim(strings.TrimSpace(val), `"`), "https")
			}
		}
	}
	return false
}

// pairingTransportOK refuses to hand out a token over plain HTTP from the
// network, where it could be copied off the wire.
func pairingTransportOK(r *http.Request) bool {
	return requestIsHTTPS(r) || loopbackDirectRequest(r)
}

func isStreamPath(p string) bool {
	return strings.HasPrefix(p, "/api/") && strings.HasSuffix(p, "/stream")
}

func writePairingRequired(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="sysmon"`)
	writeJSON(w, http.StatusUnauthorized, map[string]string{
		"error":   "pairing_required",
		"message": "this device is not paired with the agent",
	})
}

// requirePairing answers 401 to anything but the open paths unless the
// request carries a paired device's token, or is a read from this machine.
// A stream from a device that is revoked later is cut off.
func requirePairing(state *RuntimeState, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		store := state.Pairing()
		if store == nil {
			next.ServeHTTP(w, r)
			return
		}
		if token, _ := requestPairingToken(r); token != "" {
			if device, gone, ok := store.Authenticate(token); ok {
				ctx := context.WithValue(r.Context(), pairedDeviceKey{}, device)
				if gone != nil && isStreamPath(cleanRequestPath(r)) {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					defer cancel()
					go func() {
						select {
						case <-gone:
							cancel()
						case <-ctx.Done():
						}
					}()
				}
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}
		if pairingOpenPath(r) || (isReadMethod(r.Method) && loopbackDirectRequest(r)) {
			next.ServeHTTP(w, r)
			return
		}
		writePairingRequired(w)
	})
}

func setPairingCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     pairingCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(pairingCookieMaxAge / time.Second),
		HttpOnly: true,
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteStrictMode,
	})
}

func clearPairingCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     pairingCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteStrictMode,
	})
}

// PairingState is GET /api/pair/state, which an unpaired dashboard reads to
// decide whether to show the PIN form. It names no host.
type PairingState struct {
	Pairing        string            `json:"pairing"`
	Paired         bool              `json:"paired"`
	Local          bool              `json:"local"`
	Secure         bool              `json:"secure"`
	WindowOpen     bool              `json:"window_open"`
	Device         *PairedDeviceInfo `json:"device,omitempty"`
	DashboardBuild string            `json:"dashboard_build"`
}

type pairRequest struct {
	PIN  string `json:"pin"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type pairResponse struct {
	Device PairedDeviceInfo `json:"device"`
	// Token is returned only for kind "token" (scripts, other agents). A
	// browser gets it as an HttpOnly cookie, out of reach of page scripts.
	Token string `json:"token,omitempty"`
}

type pairingWindowResponse struct {
	PIN        string    `json:"pin"`
	PINDisplay string    `json:"pin_display"`
	ExpiresAt  time.Time `json:"expires_at"`
	// Link opens the dashboard with the PIN filled in, and QR is that link as
	// a QR code, one string of '0' (light) and '1' (dark) per module row.
	// Both are left out when no address another device can reach is known.
	Link string   `json:"link,omitempty"`
	QR   []string `json:"qr,omitempty"`
	// Address is set when Link opens the host's .local name: the LAN address
	// that name answered with, for a device that cannot look the name up.
	Address string `json:"address,omitempty"`
}

func writePairingOff(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "pairing is off on this agent"})
}

func decodeSingleJSON(w http.ResponseWriter, r *http.Request, limit int64, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request body must contain a single JSON object"})
		return false
	}
	return true
}

func registerPairingRoutes(mux *http.ServeMux, state *RuntimeState) {
	mux.HandleFunc("GET /api/pair/state", func(w http.ResponseWriter, r *http.Request) {
		store := state.Pairing()
		if store == nil {
			writeJSON(w, http.StatusOK, PairingState{Pairing: pairingModeOff, Paired: true, Secure: requestIsHTTPS(r), DashboardBuild: dashboardBuild})
			return
		}
		response := PairingState{
			Pairing:        pairingModeRequired,
			Local:          loopbackDirectRequest(r),
			Secure:         pairingTransportOK(r),
			WindowOpen:     store.WindowOpen(),
			DashboardBuild: dashboardBuild,
		}
		token, viaCookie := requestPairingToken(r)
		if device, ok := pairedDeviceFrom(r.Context()); ok {
			response.Paired = true
			response.Device = &device
			// Each dashboard load pushes the cookie's expiry out again, so a
			// device in use never ages out.
			if viaCookie {
				setPairingCookie(w, r, token)
			}
		} else if viaCookie {
			clearPairingCookie(w, r)
		}
		writeJSON(w, http.StatusOK, response)
	})

	mux.HandleFunc("POST /api/pair", func(w http.ResponseWriter, r *http.Request) {
		store := state.Pairing()
		if store == nil {
			writePairingOff(w)
			return
		}
		if !sameOriginBrowserRequest(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "pairing requires same-origin requests"})
			return
		}
		if !pairingTransportOK(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "pairing needs HTTPS; open the dashboard at its https:// address"})
			return
		}
		var request pairRequest
		if !decodeSingleJSON(w, r, 4<<10, &request) {
			return
		}
		kind := strings.TrimSpace(request.Kind)
		if kind == "" {
			kind = pairedKindBrowser
		}
		if kind != pairedKindBrowser && kind != pairedKindToken {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "kind must be browser or token"})
			return
		}
		token, device, remaining, err := store.Pair(request.PIN, request.Name, kind)
		switch {
		case err == nil:
		case errors.Is(err, errPairingRateLimited):
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": err.Error()})
			return
		case errors.Is(err, errPairingWrongPIN):
			writeJSON(w, http.StatusForbidden, map[string]any{"error": err.Error(), "remaining": remaining})
			return
		case errors.Is(err, errPairingClosed):
			writeJSON(w, http.StatusForbidden, map[string]any{"error": err.Error(), "remaining": 0})
			return
		case errors.Is(err, errPairingNoWindow), errors.Is(err, errPairingTooMany):
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		default:
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		response := pairResponse{Device: device}
		if kind == pairedKindToken {
			response.Token = token
		} else {
			setPairingCookie(w, r, token)
		}
		writeJSON(w, http.StatusOK, response)
	})

	mux.HandleFunc("GET /api/pair/devices", func(w http.ResponseWriter, r *http.Request) {
		store := state.Pairing()
		if store == nil {
			writePairingOff(w)
			return
		}
		devices := store.Devices()
		if current, ok := pairedDeviceFrom(r.Context()); ok {
			for i := range devices {
				devices[i].Current = devices[i].ID == current.ID
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"devices": devices})
	})

	mux.HandleFunc("DELETE /api/pair/devices/{id}", func(w http.ResponseWriter, r *http.Request) {
		store := state.Pairing()
		if store == nil {
			writePairingOff(w)
			return
		}
		if !sameOriginBrowserRequest(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "revoking a device requires same-origin requests"})
			return
		}
		removed, err := store.Revoke(r.PathValue("id"))
		if errors.Is(err, errPairingUnknown) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if current, ok := pairedDeviceFrom(r.Context()); ok && current.ID == removed.ID {
			clearPairingCookie(w, r)
		}
		writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
	})

	mux.HandleFunc("POST /api/pair/window", func(w http.ResponseWriter, r *http.Request) {
		store := state.Pairing()
		if store == nil {
			writePairingOff(w)
			return
		}
		if !sameOriginBrowserRequest(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "opening a pairing window requires same-origin requests"})
			return
		}
		if _, ok := pairedDeviceFrom(r.Context()); !ok {
			writePairingRequired(w)
			return
		}
		pin, expires, err := store.OpenWindow()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		response := pairingWindowResponse{PIN: pin, PINDisplay: formatPIN(pin), ExpiresAt: expires}
		if base, address := pairingLinkBaseForRequest(r, state.PairingLinkFallback()); base != "" {
			if link, err := pairingLink(base, pin); err == nil {
				if code, err := encodeQR(link); err == nil {
					response.Link, response.QR, response.Address = link, code.rows(), address
				}
			}
		}
		writeJSON(w, http.StatusOK, response)
	})

	mux.HandleFunc("POST /api/pair/logout", func(w http.ResponseWriter, r *http.Request) {
		store := state.Pairing()
		if store == nil {
			writePairingOff(w)
			return
		}
		if !sameOriginBrowserRequest(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "unpairing requires same-origin requests"})
			return
		}
		device, ok := pairedDeviceFrom(r.Context())
		if !ok {
			writePairingRequired(w)
			return
		}
		if _, err := store.Revoke(device.ID); err != nil && !errors.Is(err, errPairingUnknown) {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		clearPairingCookie(w, r)
		writeJSON(w, http.StatusOK, map[string]any{"removed": device})
	})
}
