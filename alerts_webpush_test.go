package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := decodeB64URL(s)
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return b
}

// TestWebPushEncryptRFC8291Vector is the worked example of RFC 8291,
// section 5 and appendix A: every intermediate and the final message.
func TestWebPushEncryptRFC8291Vector(t *testing.T) {
	plaintext := []byte("When I grow up, I want to be a watermelon")
	asPrivate := mustB64(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw")
	asPublic := mustB64(t, "BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8")
	uaPublic := mustB64(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4")
	uaPrivate := mustB64(t, "q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94")
	salt := mustB64(t, "DGv6ra1nlYgDCS1FRnbzlw")
	auth := mustB64(t, "BTBZMqHH6r4Tts7J_aSIgg")

	asKey, err := ecdh.P256().NewPrivateKey(asPrivate)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(asKey.PublicKey().Bytes(), asPublic) {
		t.Fatal("as_public does not match as_private")
	}
	uaKey, err := ecdh.P256().NewPrivateKey(uaPrivate)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := asKey.ECDH(uaKey.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	check := func(name string, got []byte, want string) {
		t.Helper()
		if b64url(got) != want {
			t.Fatalf("%s = %s, want %s", name, b64url(got), want)
		}
	}
	check("ecdh_secret", secret, "kyrL1jIIOHEzg3sM2ZWRHDRB62YACZhhSlknJ672kSs")
	prkKey := hkdfExtract(auth, secret)
	check("PRK_key", prkKey, "Snr3JMxaHVDXHWJn5wdC52WjpCtd2EIEGBykDcZW32k")
	keyInfo := append(append([]byte("WebPush: info\x00"), uaPublic...), asPublic...)
	ikm := hkdfExpand(prkKey, keyInfo, 32)
	check("IKM", ikm, "S4lYMb_L0FxCeq0WhDx813KgSYqU26kOyzWUdsXYyrg")
	prk := hkdfExtract(salt, ikm)
	check("PRK", prk, "09_eUZGrsvxChDCGRCdkLiDXrReGOEVeSCdCcPBSJSc")
	check("CEK", hkdfExpand(prk, []byte("Content-Encoding: aes128gcm\x00"), 16), "oIhVW04MRdy2XN9CiKLxTg")
	check("NONCE", hkdfExpand(prk, []byte("Content-Encoding: nonce\x00"), 12), "4h_95klXJ5E_qnoN")

	message, err := webPushEncrypt(plaintext, uaPublic, auth, asKey, salt)
	if err != nil {
		t.Fatal(err)
	}
	header := mustB64(t, "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8")
	ciphertext := mustB64(t, "8pfeW0KbunFT06SuDKoJH9Ql87S1QUrdirN6GcG7sFz1y1sqLgVi1VhjVkHsUoEsbI_0LpXMuGvnzQ")
	if !bytes.Equal(message, append(header, ciphertext...)) {
		t.Fatalf("message = %s", b64url(message))
	}
	got, err := webPushDecryptForTest(message, uaKey, auth)
	if err != nil || string(got) != string(plaintext) {
		t.Fatalf("round trip = %q, %v", got, err)
	}
}

// TestHKDFRFC5869Case1 is RFC 5869's first SHA-256 test case.
func TestHKDFRFC5869Case1(t *testing.T) {
	ikm := bytes.Repeat([]byte{0x0b}, 22)
	salt := []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c}
	info := []byte{0xf0, 0xf1, 0xf2, 0xf3, 0xf4, 0xf5, 0xf6, 0xf7, 0xf8, 0xf9}
	prk := hkdfExtract(salt, ikm)
	if got := strings.ToLower(hexString(prk)); got != "077709362c2e32df0ddc3f0dc47bba6390b6c73bb50f9c3122ec844ad7c2b3e5" {
		t.Fatalf("PRK = %s", got)
	}
	okm := hkdfExpand(prk, info, 42)
	if got := hexString(okm); got != "3cb25f25faacd57a90434f64d0362f2a2d2d0a90cf1a5a4c5db02d56ecc4c5bf34007208d5b887185865" {
		t.Fatalf("OKM = %s", got)
	}
}

func hexString(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&15])
	}
	return string(out)
}

// webPushDecryptForTest is the receiving browser's side of RFC 8291.
func webPushDecryptForTest(message []byte, uaKey *ecdh.PrivateKey, auth []byte) ([]byte, error) {
	salt := message[:16]
	rs := binary.BigEndian.Uint32(message[16:20])
	idLen := int(message[20])
	asPublic := message[21 : 21+idLen]
	body := message[21+idLen:]
	if rs != webPushRecordSize || len(body) > int(rs) {
		return nil, errBadTestMessage
	}
	asKey, err := ecdh.P256().NewPublicKey(asPublic)
	if err != nil {
		return nil, err
	}
	secret, err := uaKey.ECDH(asKey)
	if err != nil {
		return nil, err
	}
	keyInfo := append(append([]byte("WebPush: info\x00"), uaKey.PublicKey().Bytes()...), asPublic...)
	prk := hkdfExtract(salt, hkdfExpand(hkdfExtract(auth, secret), keyInfo, 32))
	block, _ := aes.NewCipher(hkdfExpand(prk, []byte("Content-Encoding: aes128gcm\x00"), 16))
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, hkdfExpand(prk, []byte("Content-Encoding: nonce\x00"), 12), body, nil)
	if err != nil {
		return nil, err
	}
	plain = bytes.TrimRight(plain, "\x00")
	if len(plain) == 0 || plain[len(plain)-1] != 0x02 {
		return nil, errBadTestMessage
	}
	return plain[:len(plain)-1], nil
}

var errBadTestMessage = &alertFinalError{msg: "bad test message"}

// verifyVAPIDForTest checks an Authorization header the way a push service
// does (RFC 8292) and returns its claims.
func verifyVAPIDForTest(t *testing.T, header string, wantKey string) map[string]any {
	t.Helper()
	rest, ok := strings.CutPrefix(header, "vapid t=")
	if !ok {
		t.Fatalf("authorization = %q", header)
	}
	token, key, ok := strings.Cut(rest, ", k=")
	if !ok {
		t.Fatalf("authorization = %q", header)
	}
	if key != wantKey {
		t.Fatalf("k = %s, want %s", key, wantKey)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt parts = %d", len(parts))
	}
	var head map[string]string
	if err := json.Unmarshal(mustB64(t, parts[0]), &head); err != nil || head["alg"] != "ES256" || head["typ"] != "JWT" {
		t.Fatalf("jwt header = %v, %v", head, err)
	}
	var claims map[string]any
	if err := json.Unmarshal(mustB64(t, parts[1]), &claims); err != nil {
		t.Fatal(err)
	}
	sig := mustB64(t, parts[2])
	if len(sig) != 64 {
		t.Fatalf("signature length = %d", len(sig))
	}
	//lint:ignore SA1019 the test needs only the point, and stays on go1.22
	x, y := elliptic.Unmarshal(elliptic.P256(), mustB64(t, key))
	if x == nil {
		t.Fatal("k is not a P-256 point")
	}
	pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("VAPID signature does not verify")
	}
	return claims
}

func TestVAPIDAuthorization(t *testing.T) {
	store, err := openWebPushStore("")
	if err != nil {
		t.Fatal(err)
	}
	endpoint, _ := url.Parse("https://fcm.googleapis.com/fcm/send/abc:def")
	header, err := vapidAuthorization(store.key, endpoint, "mailto:admin@example.com", alertT0)
	if err != nil {
		t.Fatal(err)
	}
	claims := verifyVAPIDForTest(t, header, store.PublicKey())
	if claims["aud"] != "https://fcm.googleapis.com" || claims["sub"] != "mailto:admin@example.com" || claims["exp"] != float64(alertT0.Add(12*time.Hour).Unix()) {
		t.Fatalf("claims = %v", claims)
	}
	if len(mustB64(t, store.PublicKey())) != 65 {
		t.Fatal("the public key should be an uncompressed P-256 point")
	}
}

func TestWebPushEndpointAllowlist(t *testing.T) {
	hosts, err := normalizeWebPushHosts([]string{"push.example.org"})
	if err != nil {
		t.Fatal(err)
	}
	good := []string{
		"https://fcm.googleapis.com/fcm/send/x",
		"https://updates.push.services.mozilla.com/wpush/v2/x",
		"https://web.push.apple.com/x",
		"https://wns2-par02p.notify.windows.com/w/?token=x",
		"https://push.example.org:443/x",
	}
	for _, raw := range good {
		if _, err := validateWebPushEndpoint(raw, hosts); err != nil {
			t.Errorf("%s refused: %v", raw, err)
		}
	}
	bad := []string{
		"http://fcm.googleapis.com/fcm/send/x",
		"https://fcm.googleapis.com:8443/x",
		"https://notify.windows.com.evil.example/x",
		"https://notify.windows.com/x",
		"https://127.0.0.1/x",
		"https://user@fcm.googleapis.com/x",
		"https://fcm.googleapis.com/x#frag",
		"https://evil.example/x",
		"https://fcm.googleapis.com/" + strings.Repeat("a", maxWebPushEndpoint),
	}
	for _, raw := range bad {
		if _, err := validateWebPushEndpoint(raw, hosts); err == nil {
			t.Errorf("%s accepted", raw)
		}
	}
	for _, entry := range []string{"192.0.2.1", "localhost", "a/b", "*", "push.example.org:8443"} {
		if _, err := normalizeWebPushHosts([]string{entry}); err == nil {
			t.Errorf("allow_hosts %q accepted", entry)
		}
	}
}

func newTestSubscriber(t *testing.T) (*ecdh.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	return key, auth
}

func TestParseWebPushSubscription(t *testing.T) {
	key, auth := newTestSubscriber(t)
	hosts := defaultWebPushHosts
	endpoint := "https://fcm.googleapis.com/fcm/send/x"
	if _, err := parseWebPushSubscription(endpoint, b64url(key.PublicKey().Bytes()), b64url(auth)+"==", hosts); err != nil {
		t.Fatalf("valid subscription refused: %v", err)
	}
	notAPoint := append([]byte{4}, make([]byte, 64)...)
	cases := map[string][3]string{
		"short key":   {endpoint, b64url(key.PublicKey().Bytes()[:33]), b64url(auth)},
		"not a point": {endpoint, b64url(notAPoint), b64url(auth)},
		"short auth":  {endpoint, b64url(key.PublicKey().Bytes()), b64url(auth[:8])},
		"bad host":    {"https://evil.example/x", b64url(key.PublicKey().Bytes()), b64url(auth)},
	}
	for name, c := range cases {
		if _, err := parseWebPushSubscription(c[0], c[1], c[2], hosts); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestWebPushStorePersistsPrivately(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "push")
	store, err := openWebPushStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	key, auth := newTestSubscriber(t)
	sub := webPushSubscription{Endpoint: "https://fcm.googleapis.com/fcm/send/a", P256dh: b64url(key.PublicKey().Bytes()), Auth: b64url(auth), DeviceID: "dev1"}
	if n, err := store.Add(sub); err != nil || n != 1 {
		t.Fatalf("add = %d, %v", n, err)
	}
	sub.DeviceID = "dev2"
	if n, _ := store.Add(sub); n != 1 {
		t.Fatalf("the same endpoint should replace, count %d", n)
	}
	if runtime.GOOS != "windows" {
		for _, name := range []string{webPushKeyFile, webPushSubscriptionFile} {
			info, err := os.Stat(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Errorf("%s mode = %v", name, info.Mode().Perm())
			}
		}
		if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
			t.Errorf("dir mode = %v", info.Mode().Perm())
		}
	}
	again, err := openWebPushStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if again.PublicKey() != store.PublicKey() {
		t.Fatal("the VAPID key changed across a restart")
	}
	if subs := again.List(); len(subs) != 1 || subs[0].DeviceID != "dev2" {
		t.Fatalf("reloaded = %+v", subs)
	}
	for i := 0; i < maxWebPushSubscriptions+3; i++ {
		s := sub
		s.Endpoint = "https://fcm.googleapis.com/fcm/send/" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		if _, err := again.Add(s); err != nil {
			t.Fatal(err)
		}
	}
	if again.Count() != maxWebPushSubscriptions {
		t.Fatalf("count = %d", again.Count())
	}
	if removed, _ := again.RemoveEndpoint("https://fcm.googleapis.com/fcm/send/zz"); removed {
		t.Fatal("removed an endpoint that is not there")
	}
}

// pushServer is a stand-in push service reached through a test dialer, so
// endpoints keep the real https port and an allowlisted name.
type pushServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []*http.Request
	bodies   [][]byte
	status   map[string]int
}

func newPushServer(t *testing.T) (*pushServer, *http.Client) {
	t.Helper()
	p := &pushServer{status: map[string]int{}}
	p.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		p.mu.Lock()
		p.requests = append(p.requests, r)
		p.bodies = append(p.bodies, buf.Bytes())
		status, ok := p.status[r.URL.Path]
		p.mu.Unlock()
		if !ok {
			status = http.StatusCreated
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(p.Close)
	client := newAlertHTTPClient()
	transport := client.Transport.(*http.Transport)
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test server
	addr := p.Listener.Addr().String()
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	return p, client
}

func TestWebPushDeliveryEndToEnd(t *testing.T) {
	srv, client := newPushServer(t)
	srv.status["/gone"] = http.StatusGone
	store, err := openWebPushStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hosts, _ := normalizeWebPushHosts([]string{"push.example.org"})
	key, auth := newTestSubscriber(t)
	add := func(path, device string) {
		sub, err := parseWebPushSubscription("https://push.example.org"+path, b64url(key.PublicKey().Bytes()), b64url(auth), hosts)
		if err != nil {
			t.Fatal(err)
		}
		sub.DeviceID = device
		if _, err := store.Add(sub); err != nil {
			t.Fatal(err)
		}
	}
	add("/live", "dev-ok")
	add("/gone", "dev-ok")
	add("/revoked", "dev-revoked")
	target := &webPushTarget{
		store: store, cfg: webPushConfig{Subject: "mailto:admin@example.com", AllowHosts: hosts},
		client: client, userAgent: "sysmon-agent/test", now: func() time.Time { return alertT0 },
		deviceExists: func(id string) bool { return id != "dev-revoked" },
	}
	d, _ := testDispatcher(target)
	d.deliver(context.Background(), firingNotification())

	if s := d.Status(); s.Sent != 1 || s.Failed != 0 {
		t.Fatalf("status = %+v", s)
	}
	if subs := store.List(); len(subs) != 1 || !strings.HasSuffix(subs[0].Endpoint, "/live") {
		t.Fatalf("gone and revoked subscriptions should be dropped: %+v", subs)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.requests) != 2 {
		t.Fatalf("push requests = %d, want 2 (the revoked device's is never sent)", len(srv.requests))
	}
	for i, r := range srv.requests {
		if r.Header.Get("Content-Encoding") != "aes128gcm" || r.Header.Get("TTL") != "86400" || r.Header.Get("Urgency") != "high" || r.Header.Get("Content-Type") != "application/octet-stream" {
			t.Fatalf("headers = %v", r.Header)
		}
		claims := verifyVAPIDForTest(t, r.Header.Get("Authorization"), store.PublicKey())
		if claims["aud"] != "https://push.example.org" {
			t.Fatalf("aud = %v", claims["aud"])
		}
		plain, err := webPushDecryptForTest(srv.bodies[i], key, auth)
		if err != nil {
			t.Fatalf("decrypt: %v", err)
		}
		var msg webPushMessage
		if err := json.Unmarshal(plain, &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Title != "test-host: Free space" || msg.Body != "Free space on /srv 4% (under 10%)" || msg.Tag != "sysmon-disk_free:/srv" || msg.URL != "/" || msg.Kind != "firing" {
			t.Fatalf("payload = %+v", msg)
		}
	}
	// Two sends must not share an ephemeral key or salt.
	if bytes.Equal(srv.bodies[0][:16], srv.bodies[1][:16]) || bytes.Equal(srv.bodies[0][21:86], srv.bodies[1][21:86]) {
		t.Fatal("salt or ephemeral key reused")
	}
}

func TestWebPushDeliveryErrorsNameOnlyTheHost(t *testing.T) {
	srv, client := newPushServer(t)
	srv.status["/secret-subscription-id"] = http.StatusInternalServerError
	store, _ := openWebPushStore("")
	hosts, _ := normalizeWebPushHosts([]string{"push.example.org"})
	key, auth := newTestSubscriber(t)
	sub, _ := parseWebPushSubscription("https://push.example.org/secret-subscription-id", b64url(key.PublicKey().Bytes()), b64url(auth), hosts)
	_, _ = store.Add(sub)
	target := &webPushTarget{store: store, cfg: webPushConfig{Subject: defaultWebPushSubject, AllowHosts: hosts}, client: client}
	d, slept := testDispatcher(target)
	d.deliver(context.Background(), firingNotification())
	s := d.Status()
	if s.Failed != 1 || len(*slept) != 1 {
		t.Fatalf("status = %+v, sleeps = %v", s, *slept)
	}
	if strings.Contains(s.LastError, "secret-subscription-id") || !strings.Contains(s.LastError, "push.example.org") {
		t.Fatalf("last error = %q", s.LastError)
	}
	if store.Count() != 1 {
		t.Fatal("a 500 must not drop the subscription")
	}
}
