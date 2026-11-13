package main

// Tokens this agent holds for peers that require pairing (docs/plans/pairing.md,
// "Peers that require pairing"). A dashboard pairs a locked peer through this
// agent with the peer's PIN; the peer issues a token-kind device, and this
// agent keeps the token in peers.json in its private pairing folder and sends
// it on every relayed request. Without -settings the tokens are kept in
// memory and end with the process.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const peerTokensFile = "peers.json"

type peerToken struct {
	// Key names the peer the way the registry finds it again: "ts:<machine>"
	// for a tailnet peer, "url:<origin>" for a -peers entry.
	Key      string `json:"key"`
	Name     string `json:"name"`
	DeviceID string `json:"device_id"`
	Token    string `json:"token"`
	// CertSHA256 pins a peer that serves its own self-signed certificate:
	// every connection to it must present this certificate. Empty for a peer
	// with a certificate the system trusts (Tailscale Serve).
	CertSHA256 string    `json:"cert_sha256,omitempty"`
	Created    time.Time `json:"created"`
}

type peerTokensDoc struct {
	Peers []peerToken `json:"peers"`
}

type peerTokenStore struct {
	dir    string // empty: memory only
	mu     sync.Mutex
	tokens map[string]peerToken
}

// newPeerTokenStore loads dir/peers.json when it exists. The folder itself is
// created on the first write, so an agent that never pairs a peer leaves
// nothing behind.
func newPeerTokenStore(dir string) (*peerTokenStore, error) {
	store := &peerTokenStore{dir: dir, tokens: map[string]peerToken{}}
	if dir == "" {
		return store, nil
	}
	data, err := os.ReadFile(filepath.Join(dir, peerTokensFile))
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return store, fmt.Errorf("read %s: %w", peerTokensFile, err)
	}
	var doc peerTokensDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return store, fmt.Errorf("parse %s: %w", peerTokensFile, err)
	}
	for _, token := range doc.Peers {
		if token.Key != "" && token.Token != "" {
			store.tokens[token.Key] = token
		}
	}
	return store, nil
}

func (s *peerTokenStore) Get(key string) (peerToken, bool) {
	if s == nil || key == "" {
		return peerToken{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	token, ok := s.tokens[key]
	return token, ok
}

func (s *peerTokenStore) Put(token peerToken) error {
	if s == nil {
		return errors.New("this agent keeps no peer tokens")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, had := s.tokens[token.Key]
	s.tokens[token.Key] = token
	if err := s.writeLocked(); err != nil {
		if had {
			s.tokens[token.Key] = previous
		} else {
			delete(s.tokens, token.Key)
		}
		return err
	}
	return nil
}

// Forget drops a peer's token once the peer has refused it. It takes the
// refused token, so a request that was already in flight when the peer was
// paired again cannot delete the new one. A missing key is not an error.
func (s *peerTokenStore) Forget(key, refused string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, ok := s.tokens[key]; !ok || current.Token != refused {
		return nil
	}
	delete(s.tokens, key)
	return s.writeLocked()
}

func (s *peerTokenStore) writeLocked() error {
	if s.dir == "" {
		return nil
	}
	doc := peerTokensDoc{Peers: make([]peerToken, 0, len(s.tokens))}
	for _, token := range s.tokens {
		doc.Peers = append(doc.Peers, token)
	}
	sort.Slice(doc.Peers, func(i, j int) bool { return doc.Peers[i].Key < doc.Peers[j].Key })
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := ensurePrivateDir(s.dir); err != nil {
		return err
	}
	return writePrivateFile(s.dir, peerTokensFile, append(data, '\n'))
}

// normalizeFingerprint accepts "AB:CD:..." (as -pair prints it) or bare hex
// in either case, and returns uppercase hex without separators.
func normalizeFingerprint(value string) string {
	cleaned := strings.ToUpper(strings.NewReplacer(":", "", " ", "", "-", "").Replace(strings.TrimSpace(value)))
	if len(cleaned) != sha256.Size*2 {
		return ""
	}
	if _, err := hex.DecodeString(cleaned); err != nil {
		return ""
	}
	return cleaned
}

func leafFingerprint(state tls.ConnectionState) string {
	if len(state.PeerCertificates) == 0 {
		return ""
	}
	return normalizeFingerprint(formatFingerprint(state.PeerCertificates[0].Raw))
}

// pinnedTLSConfig accepts exactly one certificate, by its SHA-256, in place of
// the system's trust store. It is used only for a peer whose self-signed
// certificate the user saw and confirmed while pairing it.
func pinnedTLSConfig(fingerprint string) *tls.Config {
	want := normalizeFingerprint(fingerprint)
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		// Verification is replaced, not skipped: VerifyConnection below
		// refuses every certificate but the pinned one.
		InsecureSkipVerify: true, //nolint:gosec
		VerifyConnection: func(state tls.ConnectionState) error {
			if want == "" || leafFingerprint(state) != want {
				return &tls.CertificateVerificationError{
					UnverifiedCertificates: state.PeerCertificates,
					Err:                    errors.New("certificate does not match the one pinned when this peer was paired"),
				}
			}
			return nil
		},
	}
}

// isCertificateError is a failed verification, as opposed to a refused
// connection or a timeout.
func isCertificateError(err error) bool {
	var verifyErr *tls.CertificateVerificationError
	if errors.As(err, &verifyErr) {
		return true
	}
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	return errors.As(err, &unknown) || errors.As(err, &hostname) || errors.As(err, &invalid)
}

// hostPairRequest is POST /api/hosts/{id}/pair: the PIN that `sysmon-agent
// -pair` printed on the peer and, for a peer behind its own self-signed
// certificate, the SHA-256 the dashboard showed beside the PIN field.
type hostPairRequest struct {
	PIN        string `json:"pin"`
	CertSHA256 string `json:"cert_sha256"`
}

// serveRelayPair pairs this agent with a locked peer for a dashboard. The PIN
// goes to the peer's own POST /api/pair, asking for a token-kind device named
// after this agent; the token stays here and never reaches the browser. Every
// device that can use this agent's dashboard then views the peer through it.
func (r *peerRegistry) serveRelayPair(w http.ResponseWriter, req *http.Request) {
	if !sameOriginBrowserRequest(req) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "pairing a host requires same-origin browser requests"})
		return
	}
	var request hostPairRequest
	if !decodeSingleJSON(w, req, 4<<10, &request) {
		return
	}
	target, ok := r.resolve(req.Context(), req.PathValue("id"))
	if !ok {
		writeUnknownHost(w)
		return
	}
	if !peerTokenTransportOK(&target.base) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": target.name + " is reached over plain HTTP, where its PIN and token could be read on the way. Serve it over HTTPS (Tailscale Serve, or its own certificate) and rescan."})
		return
	}
	pin := target.pin
	if target.certSHA256 != "" {
		// The certificate is trusted because the user compared it: pair
		// over exactly the one the dashboard showed.
		pin = normalizeFingerprint(target.certSHA256)
		if pin == "" || normalizeFingerprint(request.CertSHA256) != pin {
			writeJSON(w, http.StatusPreconditionFailed, map[string]any{
				"error":        target.name + " presents a different certificate from the one shown. Reopen the host list and compare it again.",
				"cert_changed": true,
			})
			return
		}
	}
	body, _ := json.Marshal(pairRequest{PIN: request.PIN, Name: r.relayName, Kind: pairedKindToken})
	ctx, cancel := context.WithTimeout(req.Context(), peerRelayReadTimeout)
	defer cancel()
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, peerURL(&target.base, "/api/pair"), bytes.NewReader(body))
	if err != nil {
		writeRelayError(w, target, err)
		return
	}
	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("Accept", "application/json")
	// The peer's same-origin check stops cross-site browser requests; this
	// one passed this agent's own check above.
	upstream.Header.Set("Origin", peerOrigin(target.base))
	response, err := r.clientForPin(pin).Do(upstream)
	if err != nil {
		writeRelayError(w, target, err)
		return
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		writeRelayError(w, target, err)
		return
	}
	if response.StatusCode != http.StatusOK {
		// Wrong PIN (403, with the tries left), no PIN open (409), too fast
		// (429): the peer's own answer is what the dashboard shows.
		if !json.Valid(raw) {
			writeJSON(w, response.StatusCode, map[string]string{"error": fmt.Sprintf("%s: HTTP %d", target.name, response.StatusCode)})
			return
		}
		if retry := response.Header.Get("Retry-After"); retry != "" {
			w.Header().Set("Retry-After", retry)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(raw)
		return
	}
	var paired pairResponse
	if err := json.Unmarshal(raw, &paired); err != nil || paired.Token == "" {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": target.name + " accepted the PIN but sent no token"})
		return
	}
	if err := r.tokens.Put(peerToken{
		Key:        target.tokenKey,
		Name:       target.name,
		DeviceID:   paired.Device.ID,
		Token:      paired.Token,
		CertSHA256: pin,
		Created:    r.now().UTC(),
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "paired with " + target.name + ", but could not save its token: " + err.Error()})
		return
	}
	r.markPaired(target, pin)
	writeJSON(w, http.StatusOK, map[string]any{"paired": true, "id": target.id})
}

// markPaired lists the peer as readable at once, and starts a scan that reads
// its OS and version over the new token.
func (r *peerRegistry) markPaired(target peerTarget, pin string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if record := r.byID[target.id]; record != nil && record.tokenKey == target.tokenKey {
		record.pin = pin
		record.pairedAt = r.now()
		record.entry.Locked = false
		record.entry.CertSHA256 = ""
		record.entry.Error = ""
	}
	r.startScanLocked()
}
