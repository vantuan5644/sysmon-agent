package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Web Push for host alerts, standard library only:
//
//   - RFC 8291 message encryption (aes128gcm content coding, RFC 8188), with
//     HKDF written out below because the standard library's crypto/hkdf
//     arrived after this module's Go version. The RFC's own worked example is
//     the test (alerts_webpush_test.go).
//   - RFC 8292 VAPID: an ES256 JWT signed with a P-256 key the agent makes
//     once and keeps in <settings dir>/push/vapid-key.pem (0600).
//
// Subscriptions are kept in push/subscriptions.json (0600, written
// atomically) and never served back: /api/alerts reports only their count.
// A push service is any URL a browser hands over, so the agent only posts to
// the known browser push services (and hosts the config adds), never to an
// address a paired device made up.

const (
	webPushRecordSize       = 4096
	maxWebPushPayload       = 3000
	maxWebPushSubscriptions = 32
	maxWebPushEndpoint      = 1024
	webPushTTLSeconds       = 24 * 60 * 60
	webPushJWTLifetime      = 12 * time.Hour
	webPushKeyFile          = "vapid-key.pem"
	webPushSubscriptionFile = "subscriptions.json"
)

const defaultWebPushSubject = "https://github.com/" + updateRepo

// defaultWebPushHosts are the push services of Chrome, Edge, Firefox and
// Safari. A leading "*." matches any subdomain.
var defaultWebPushHosts = []string{
	"fcm.googleapis.com",
	"updates.push.services.mozilla.com",
	"web.push.apple.com",
	"*.notify.windows.com",
}

func validateWebPushSubject(subject string) error {
	if strings.HasPrefix(subject, "mailto:") && len(subject) > len("mailto:") {
		return nil
	}
	if u, err := url.Parse(subject); err == nil && u.Scheme == "https" && u.Host != "" {
		return nil
	}
	return errors.New("web push: the subject must be a mailto: or https:// URL")
}

func normalizeWebPushHosts(extra []string) ([]string, error) {
	hosts := append([]string(nil), defaultWebPushHosts...)
	for _, raw := range extra {
		host := strings.ToLower(strings.TrimSpace(raw))
		if host == "" {
			continue
		}
		bare := strings.TrimPrefix(host, "*.")
		if strings.ContainsAny(bare, "/:@* ") || !strings.Contains(bare, ".") || net.ParseIP(bare) != nil {
			return nil, fmt.Errorf("web push: allow_hosts entry %q must be a host name such as push.example.com or *.example.com", raw)
		}
		hosts = append(hosts, host)
	}
	return hosts, nil
}

func webPushHostAllowed(hosts []string, host string) bool {
	host = strings.ToLower(host)
	for _, allowed := range hosts {
		if suffix, ok := strings.CutPrefix(allowed, "*."); ok {
			if strings.HasSuffix(host, "."+suffix) {
				return true
			}
			continue
		}
		if host == allowed {
			return true
		}
	}
	return false
}

func validateWebPushEndpoint(raw string, hosts []string) (*url.URL, error) {
	if len(raw) > maxWebPushEndpoint {
		return nil, errors.New("endpoint is too long")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("endpoint must be an https:// URL")
	}
	if port := u.Port(); port != "" && port != "443" {
		return nil, errors.New("endpoint must use the https port")
	}
	if !webPushHostAllowed(hosts, u.Hostname()) {
		return nil, errors.New("endpoint is not a known push service; add its host to web_push.allow_hosts")
	}
	return u, nil
}

func decodeB64URL(value string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(strings.TrimSpace(value), "="))
}

func b64url(data []byte) string { return base64.RawURLEncoding.EncodeToString(data) }

// ---- HKDF (RFC 5869) with SHA-256 ----

func hkdfExtract(salt, ikm []byte) []byte {
	mac := hmac.New(sha256.New, salt)
	mac.Write(ikm)
	return mac.Sum(nil)
}

func hkdfExpand(prk, info []byte, length int) []byte {
	var out, previous []byte
	for counter := byte(1); len(out) < length; counter++ {
		mac := hmac.New(sha256.New, prk)
		mac.Write(previous)
		mac.Write(info)
		mac.Write([]byte{counter})
		previous = mac.Sum(nil)
		out = append(out, previous...)
	}
	return out[:length]
}

// ---- RFC 8291 encryption ----

// webPushEncrypt encrypts plaintext for one subscription as a single
// aes128gcm record. asKey and salt are fresh for every message; tests pass
// the RFC's.
func webPushEncrypt(plaintext, uaPublic, authSecret []byte, asKey *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	if len(authSecret) != 16 {
		return nil, errors.New("auth secret must be 16 bytes")
	}
	if len(salt) != 16 {
		return nil, errors.New("salt must be 16 bytes")
	}
	if len(plaintext)+1+16 > webPushRecordSize {
		return nil, errors.New("payload is too large for one record")
	}
	uaKey, err := ecdh.P256().NewPublicKey(uaPublic)
	if err != nil {
		return nil, errors.New("subscription key is not a P-256 point")
	}
	secret, err := asKey.ECDH(uaKey)
	if err != nil {
		return nil, err
	}
	asPublic := asKey.PublicKey().Bytes()

	keyInfo := append([]byte("WebPush: info\x00"), uaPublic...)
	keyInfo = append(keyInfo, asPublic...)
	ikm := hkdfExpand(hkdfExtract(authSecret, secret), keyInfo, 32)
	prk := hkdfExtract(salt, ikm)
	cek := hkdfExpand(prk, []byte("Content-Encoding: aes128gcm\x00"), 16)
	nonce := hkdfExpand(prk, []byte("Content-Encoding: nonce\x00"), 12)

	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// One record, so the last-record delimiter 0x02 and no padding.
	record := append(append([]byte(nil), plaintext...), 0x02)
	ciphertext := gcm.Seal(nil, nonce, record, nil)

	header := make([]byte, 0, 16+4+1+len(asPublic)+len(ciphertext))
	header = append(header, salt...)
	header = binary.BigEndian.AppendUint32(header, webPushRecordSize)
	header = append(header, byte(len(asPublic)))
	header = append(header, asPublic...)
	return append(header, ciphertext...), nil
}

// ---- RFC 8292 VAPID ----

func vapidPublicKey(key *ecdsa.PrivateKey) ([]byte, error) {
	pub, err := key.PublicKey.ECDH()
	if err != nil {
		return nil, err
	}
	return pub.Bytes(), nil
}

// vapidAuthorization is the Authorization header for one push service.
func vapidAuthorization(key *ecdsa.PrivateKey, endpoint *url.URL, subject string, now time.Time) (string, error) {
	header := b64url([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, err := json.Marshal(struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}{endpoint.Scheme + "://" + endpoint.Hostname(), now.Add(webPushJWTLifetime).Unix(), subject})
	if err != nil {
		return "", err
	}
	signingInput := header + "." + b64url(claims)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", err
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	pub, err := vapidPublicKey(key)
	if err != nil {
		return "", err
	}
	return "vapid t=" + signingInput + "." + b64url(signature) + ", k=" + b64url(pub), nil
}

// ---- subscription store ----

type webPushSubscription struct {
	Endpoint string    `json:"endpoint"`
	P256dh   string    `json:"p256dh"`
	Auth     string    `json:"auth"`
	DeviceID string    `json:"device_id,omitempty"`
	Created  time.Time `json:"created"`
}

type webPushStore struct {
	dir string // empty: memory only
	key *ecdsa.PrivateKey

	mu   sync.Mutex
	subs []webPushSubscription
}

// openWebPushStore loads (or makes) the VAPID key and the subscriptions
// under dir. An empty dir keeps both in memory, so subscriptions end with
// the process.
func openWebPushStore(dir string) (*webPushStore, error) {
	store := &webPushStore{dir: dir}
	if dir == "" {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		store.key = key
		return store, nil
	}
	if err := ensurePrivateDir(dir); err != nil {
		return nil, fmt.Errorf("web push folder: %w", err)
	}
	key, err := loadOrCreateVAPIDKey(dir)
	if err != nil {
		return nil, err
	}
	store.key = key
	data, err := os.ReadFile(filepath.Join(dir, webPushSubscriptionFile))
	switch {
	case err == nil:
		var subs []webPushSubscription
		if err := json.Unmarshal(data, &subs); err != nil {
			return nil, fmt.Errorf("%s: %w", webPushSubscriptionFile, err)
		}
		if len(subs) > maxWebPushSubscriptions {
			subs = subs[len(subs)-maxWebPushSubscriptions:]
		}
		store.subs = subs
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, fmt.Errorf("read %s: %w", webPushSubscriptionFile, err)
	}
	return store, nil
}

func loadOrCreateVAPIDKey(dir string) (*ecdsa.PrivateKey, error) {
	path := filepath.Join(dir, webPushKeyFile)
	data, err := os.ReadFile(path)
	if err == nil {
		block, _ := pem.Decode(data)
		if block == nil || block.Type != "EC PRIVATE KEY" {
			return nil, fmt.Errorf("%s holds no EC private key", webPushKeyFile)
		}
		key, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", webPushKeyFile, err)
		}
		if key.Curve != elliptic.P256() {
			return nil, fmt.Errorf("%s is not a P-256 key", webPushKeyFile)
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", webPushKeyFile, err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := writePrivateFile(dir, webPushKeyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})); err != nil {
		return nil, err
	}
	return key, nil
}

func (s *webPushStore) PublicKey() string {
	pub, err := vapidPublicKey(s.key)
	if err != nil {
		return ""
	}
	return b64url(pub)
}

func (s *webPushStore) saveLocked() error {
	if s.dir == "" {
		return nil
	}
	data, err := json.MarshalIndent(s.subs, "", "  ")
	if err != nil {
		return err
	}
	return writePrivateFile(s.dir, webPushSubscriptionFile, append(data, '\n'))
}

// Add stores sub, replacing one with the same endpoint. The oldest
// subscription makes room when the store is full.
func (s *webPushStore) Add(sub webPushSubscription) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.subs[:0:0]
	for _, existing := range s.subs {
		if existing.Endpoint != sub.Endpoint {
			kept = append(kept, existing)
		}
	}
	kept = append(kept, sub)
	if len(kept) > maxWebPushSubscriptions {
		kept = kept[len(kept)-maxWebPushSubscriptions:]
	}
	previous := s.subs
	s.subs = kept
	if err := s.saveLocked(); err != nil {
		s.subs = previous
		return len(previous), err
	}
	return len(s.subs), nil
}

// Remove drops the subscriptions keep rejects and reports how many went.
func (s *webPushStore) Remove(drop func(webPushSubscription) bool) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.subs[:0:0]
	for _, sub := range s.subs {
		if !drop(sub) {
			kept = append(kept, sub)
		}
	}
	removed := len(s.subs) - len(kept)
	if removed == 0 {
		return 0, nil
	}
	s.subs = kept
	return removed, s.saveLocked()
}

func (s *webPushStore) RemoveEndpoint(endpoint string) (bool, error) {
	n, err := s.Remove(func(sub webPushSubscription) bool { return sub.Endpoint == endpoint })
	return n > 0, err
}

func (s *webPushStore) List() []webPushSubscription {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]webPushSubscription(nil), s.subs...)
}

func (s *webPushStore) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs)
}

// parseWebPushSubscription checks what a browser's PushSubscription.toJSON()
// gives before it is stored.
func parseWebPushSubscription(endpoint, p256dh, auth string, hosts []string) (webPushSubscription, error) {
	if _, err := validateWebPushEndpoint(endpoint, hosts); err != nil {
		return webPushSubscription{}, err
	}
	pub, err := decodeB64URL(p256dh)
	if err != nil || len(pub) != 65 {
		return webPushSubscription{}, errors.New("keys.p256dh must be a base64url P-256 public key")
	}
	if _, err := ecdh.P256().NewPublicKey(pub); err != nil {
		return webPushSubscription{}, errors.New("keys.p256dh is not a P-256 point")
	}
	secret, err := decodeB64URL(auth)
	if err != nil || len(secret) != 16 {
		return webPushSubscription{}, errors.New("keys.auth must be 16 bytes, base64url")
	}
	return webPushSubscription{Endpoint: endpoint, P256dh: b64url(pub), Auth: b64url(secret)}, nil
}

// ---- the target ----

type webPushTarget struct {
	store     *webPushStore
	cfg       webPushConfig
	client    *http.Client
	userAgent string
	now       func() time.Time
	// deviceExists reports whether a paired device still exists; nil when
	// pairing is off. A revoked device's subscriptions are dropped before
	// each send.
	deviceExists func(id string) bool
	logf         func(string, ...any)
}

func (t *webPushTarget) Name() string { return "web_push" }

// webPushMessage is what the service worker receives.
type webPushMessage struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Tag   string `json:"tag"`
	Kind  string `json:"kind"`
	Host  string `json:"host"`
	URL   string `json:"url"`
}

func webPushPayload(n alertNotification) ([]byte, error) {
	tag := "sysmon-" + n.Rule
	if n.Instance != "" {
		tag += ":" + n.Instance
	}
	msg := webPushMessage{Title: truncateRunes(n.Title, 120), Body: truncateRunes(n.Message, 600), Tag: truncateRunes(tag, 120), Kind: n.Kind, Host: truncateRunes(n.Host, 120), URL: "/"}
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	if len(data) > maxWebPushPayload {
		return nil, &alertFinalError{msg: "payload too large"}
	}
	return data, nil
}

func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}

func (t *webPushTarget) Deliver(ctx context.Context, n alertNotification, retry alertRetryFunc) error {
	if t.deviceExists != nil {
		if removed, err := t.store.Remove(func(sub webPushSubscription) bool {
			return sub.DeviceID != "" && !t.deviceExists(sub.DeviceID)
		}); removed > 0 && t.logf != nil {
			t.logf("alerts: dropped %d web push subscription(s) of revoked devices", removed)
			if err != nil {
				t.logf("alerts: saving web push subscriptions: %v", err)
			}
		}
	}
	subs := t.store.List()
	if len(subs) == 0 {
		return nil
	}
	payload, err := webPushPayload(n)
	if err != nil {
		return errors.New("web push: " + err.Error())
	}
	urgency := "normal"
	if n.Kind == "firing" {
		urgency = "high"
	}
	var failures []string
	for _, sub := range subs {
		endpoint, err := validateWebPushEndpoint(sub.Endpoint, t.cfg.AllowHosts)
		if err != nil {
			failures = append(failures, "web push: a stored endpoint is no longer allowed")
			continue
		}
		err = retry(ctx, func(ctx context.Context) error {
			return t.send(ctx, endpoint, sub, payload, urgency)
		})
		var httpErr *alertHTTPError
		if errors.As(err, &httpErr) && (httpErr.Status == http.StatusNotFound || httpErr.Status == http.StatusGone) {
			// The browser unsubscribed or the subscription expired.
			if _, saveErr := t.store.RemoveEndpoint(sub.Endpoint); saveErr != nil && t.logf != nil {
				t.logf("alerts: saving web push subscriptions: %v", saveErr)
			}
			if t.logf != nil {
				t.logf("alerts: dropped an expired web push subscription (%s)", endpoint.Host)
			}
			continue
		}
		if err != nil {
			failures = append(failures, sanitizeAlertError("web push", endpoint.Host, err, endpoint.Path, sub.Auth, sub.P256dh))
		}
	}
	switch len(failures) {
	case 0:
		return nil
	case 1:
		return errors.New(failures[0])
	default:
		return fmt.Errorf("%s (and %d more)", failures[0], len(failures)-1)
	}
}

func (t *webPushTarget) send(ctx context.Context, endpoint *url.URL, sub webPushSubscription, payload []byte, urgency string) error {
	uaPublic, err := decodeB64URL(sub.P256dh)
	if err != nil {
		return &alertFinalError{msg: "bad subscription key"}
	}
	authSecret, err := decodeB64URL(sub.Auth)
	if err != nil {
		return &alertFinalError{msg: "bad subscription secret"}
	}
	asKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	body, err := webPushEncrypt(payload, uaPublic, authSecret, asKey, salt)
	if err != nil {
		return &alertFinalError{msg: err.Error()}
	}
	now := time.Now
	if t.now != nil {
		now = t.now
	}
	authorization, err := vapidAuthorization(t.store.key, endpoint, t.cfg.Subject, now())
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("TTL", strconv.Itoa(webPushTTLSeconds))
	req.Header.Set("Urgency", urgency)
	req.Header.Set("Authorization", authorization)
	req.Header.Set("User-Agent", t.userAgent)
	_, err = doAlertRequest(t.client, req)
	return err
}
