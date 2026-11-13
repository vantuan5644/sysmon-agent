package main

// Device pairing (docs/plans/pairing.md). With -pairing required, every
// /api route and /readyz answers only a paired device: a browser holding the
// HttpOnly cookie the agent set when it paired, or a script sending a bearer
// token minted with -pair-token. A device pairs by typing a 6-digit PIN that
// the owner opened on the host (`sysmon-agent -pair`) or from a dashboard that
// is already paired, so nobody who merely reaches the port can start one.
//
// The store keeps SHA-256 hashes of the tokens, never the tokens. Its files
// live in a private folder beside the settings file, and the folder is the
// authority: whoever can write it (the service's account, or an administrator)
// can open a pairing window or mint a token from the command line, with no
// network call. The running agent and those commands share the files, so every
// write here reloads the file first and the agent rereads it within
// pairingReloadEvery when a command changed it.

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	pairingModeOff      = "off"
	pairingModeRequired = "required"

	pairingDevicesFile = "devices.json"
	pairingWindowFile  = "window.json"

	// pairingWindowTTL is how long a PIN stays valid once opened.
	pairingWindowTTL = 10 * time.Minute
	// pairingMaxFailures wrong PINs close the window, so one window allows at
	// most that many guesses at a 1-in-a-million PIN.
	pairingMaxFailures = 5
	// pairingAttemptGap spaces PIN attempts across all clients.
	pairingAttemptGap = time.Second
	// pairingReloadEvery bounds how often a request may stat devices.json to
	// pick up a token minted or revoked from the command line.
	pairingReloadEvery = 2 * time.Second
	// pairingFlushEvery is how often last-seen times reach the disk; requests
	// themselves never write.
	pairingFlushEvery = 5 * time.Minute

	pairingDeviceNameMax = 64
	pairingMaxDevices    = 64
)

const (
	pairedKindBrowser = "browser"
	pairedKindToken   = "token"
)

var (
	errPairingNoWindow    = errors.New("no pairing window is open; open one on the host with `sysmon-agent -pair`, or from a paired dashboard")
	errPairingWrongPIN    = errors.New("wrong PIN")
	errPairingClosed      = errors.New("too many wrong PINs; the pairing window is closed, open a new one")
	errPairingRateLimited = errors.New("too many attempts; wait a second and try again")
	errPairingUnknown     = errors.New("no such paired device")
	errPairingTooMany     = errors.New("too many paired devices; revoke one first")
)

// parsePairingMode accepts the -pairing flag's values.
func parsePairingMode(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", pairingModeRequired, "on", "true":
		return pairingModeRequired, nil
	case pairingModeOff, "false":
		return pairingModeOff, nil
	}
	return "", fmt.Errorf("-pairing must be required or off, got %q", value)
}

// pairingDirFor is the pairing folder for a settings file: a "pairing"
// folder beside it. An empty settings path means memory-only state.
func pairingDirFor(settingsPath string) string {
	if strings.TrimSpace(settingsPath) == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(settingsPath), "pairing")
}

type pairedDevice struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Kind        string    `json:"kind"`
	TokenSHA256 string    `json:"token_sha256"`
	Created     time.Time `json:"created"`
	LastSeen    time.Time `json:"last_seen,omitempty"`
}

// PairedDeviceInfo is a device as the API and the command line show it,
// without the token hash.
type PairedDeviceInfo struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Kind     string    `json:"kind"`
	Created  time.Time `json:"created"`
	LastSeen time.Time `json:"last_seen,omitempty"`
	Current  bool      `json:"current,omitempty"`
}

func (d pairedDevice) info() PairedDeviceInfo {
	return PairedDeviceInfo{ID: d.ID, Name: d.Name, Kind: d.Kind, Created: d.Created, LastSeen: d.LastSeen}
}

type pairingDevicesDoc struct {
	Version int            `json:"version"`
	Devices []pairedDevice `json:"devices"`
}

type pairingWindow struct {
	ID        string    `json:"id"`
	Salt      string    `json:"salt"`
	PINSHA256 string    `json:"pin_sha256"`
	Created   time.Time `json:"created"`
	Expires   time.Time `json:"expires"`
}

type fileStamp struct {
	modTime time.Time
	size    int64
	exists  bool
}

type pairingStore struct {
	dir string // empty: memory only
	now func() time.Time

	mu        sync.Mutex
	devices   map[string]*pairedDevice
	alive     map[string]chan struct{} // closed when a device goes away
	stamp     fileStamp
	lastStat  time.Time
	seenDirty bool
	window    *pairingWindow // memory-only mode
	failures  map[string]int // by window id
	lastTry   time.Time
}

// newPairingStore opens the store in dir, creating the private folder, or a
// memory-only store when dir is empty.
func newPairingStore(dir string) (*pairingStore, error) {
	s := &pairingStore{
		dir:      dir,
		now:      func() time.Time { return time.Now().UTC() },
		devices:  map[string]*pairedDevice{},
		alive:    map[string]chan struct{}{},
		failures: map[string]int{},
	}
	if dir == "" {
		return s, nil
	}
	if err := ensurePrivateDir(dir); err != nil {
		return nil, fmt.Errorf("pairing folder %s: %w", dir, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(true); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *pairingStore) persistent() bool { return s.dir != "" }

func (s *pairingStore) path(name string) string { return filepath.Join(s.dir, name) }

// reloadLocked rereads devices.json when it changed (or always, with force),
// keeping newer in-memory last-seen times and closing the alive channel of
// every device that is gone.
func (s *pairingStore) reloadLocked(force bool) error {
	if !s.persistent() {
		return nil
	}
	s.lastStat = s.now()
	stamp := statStamp(s.path(pairingDevicesFile))
	if !force && stamp == s.stamp {
		return nil
	}
	doc, err := readPairingDevices(s.path(pairingDevicesFile))
	if err != nil {
		return err
	}
	next := make(map[string]*pairedDevice, len(doc.Devices))
	for i := range doc.Devices {
		d := doc.Devices[i]
		if d.ID == "" || d.TokenSHA256 == "" {
			continue
		}
		if old, ok := s.devices[d.ID]; ok && old.LastSeen.After(d.LastSeen) {
			d.LastSeen = old.LastSeen
		}
		next[d.ID] = &d
	}
	s.replaceDevicesLocked(next)
	s.stamp = stamp
	return nil
}

func (s *pairingStore) replaceDevicesLocked(next map[string]*pairedDevice) {
	for id, ch := range s.alive {
		if _, ok := next[id]; !ok {
			close(ch)
			delete(s.alive, id)
		}
	}
	for id := range next {
		if _, ok := s.alive[id]; !ok {
			s.alive[id] = make(chan struct{})
		}
	}
	s.devices = next
}

func (s *pairingStore) maybeReloadLocked() {
	if !s.persistent() {
		return
	}
	// now() is wall time (UTC drops the monotonic reading), so a host clock
	// stepped back makes this span negative. Reread then as well: waiting for
	// the clock to catch up would hold off a revoke from the command line.
	if since := s.now().Sub(s.lastStat); since >= 0 && since < pairingReloadEvery {
		return
	}
	// A failed reread keeps the devices already loaded: a half-written file
	// from another process must not unpair everyone for two seconds.
	_ = s.reloadLocked(false)
}

// writeDevicesLocked writes the in-memory devices back to devices.json.
func (s *pairingStore) writeDevicesLocked() error {
	if !s.persistent() {
		return nil
	}
	doc := pairingDevicesDoc{Version: 1, Devices: make([]pairedDevice, 0, len(s.devices))}
	for _, d := range s.devices {
		doc.Devices = append(doc.Devices, *d)
	}
	sort.Slice(doc.Devices, func(i, j int) bool { return doc.Devices[i].Created.Before(doc.Devices[j].Created) })
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := writePrivateFile(s.dir, pairingDevicesFile, append(data, '\n')); err != nil {
		return err
	}
	s.stamp = statStamp(s.path(pairingDevicesFile))
	s.seenDirty = false
	return nil
}

// Authenticate checks a token and returns its device and a channel that is
// closed if the device is revoked later.
func (s *pairingStore) Authenticate(token string) (PairedDeviceInfo, <-chan struct{}, bool) {
	id, _, ok := splitPairingToken(token)
	if !ok {
		return PairedDeviceInfo{}, nil, false
	}
	sum := hashPairingToken(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maybeReloadLocked()
	d, ok := s.devices[id]
	if !ok || subtle.ConstantTimeCompare([]byte(d.TokenSHA256), []byte(sum)) != 1 {
		return PairedDeviceInfo{}, nil, false
	}
	now := s.now()
	if now.Sub(d.LastSeen) >= time.Minute {
		d.LastSeen = now
		s.seenDirty = true
	}
	return d.info(), s.alive[id], true
}

// Flush writes pending last-seen times. The agent calls it on a timer and at
// shutdown.
func (s *pairingStore) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.seenDirty || !s.persistent() {
		return nil
	}
	seen := make(map[string]time.Time, len(s.devices))
	for id, d := range s.devices {
		seen[id] = d.LastSeen
	}
	if err := s.reloadLocked(true); err != nil {
		return err
	}
	for id, t := range seen {
		if d, ok := s.devices[id]; ok && t.After(d.LastSeen) {
			d.LastSeen = t
		}
	}
	return s.writeDevicesLocked()
}

// Start runs the store's upkeep until stop is called: it rereads
// devices.json every pairingReloadEvery, so a device revoked from the command
// line loses its open streams even when it sends no new request, and writes
// last-seen times every pairingFlushEvery and once more at stop.
func (s *pairingStore) Start(logf logFunc) (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(pairingFlushEvery)
		defer ticker.Stop()
		reload := time.NewTicker(pairingReloadEvery)
		defer reload.Stop()
		for {
			select {
			case <-done:
				return
			case <-reload.C:
				s.mu.Lock()
				if s.persistent() {
					_ = s.reloadLocked(false)
				}
				s.mu.Unlock()
			case <-ticker.C:
				if err := s.Flush(); err != nil && logf != nil {
					logf("pairing: saving last-seen times: %v", err)
				}
			}
		}
	}()
	return func() {
		close(done)
		<-finished
		if err := s.Flush(); err != nil && logf != nil {
			logf("pairing: saving last-seen times: %v", err)
		}
	}
}

// Devices lists the paired devices, oldest first.
func (s *pairingStore) Devices() []PairedDeviceInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maybeReloadLocked()
	out := make([]PairedDeviceInfo, 0, len(s.devices))
	for _, d := range s.devices {
		out = append(out, d.info())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Created.Equal(out[j].Created) {
			return out[i].ID < out[j].ID
		}
		return out[i].Created.Before(out[j].Created)
	})
	return out
}

// Revoke removes a device; its token stops working at once in this process
// and within pairingReloadEvery in the running agent when called from the
// command line.
func (s *pairingStore) Revoke(id string) (PairedDeviceInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(true); err != nil {
		return PairedDeviceInfo{}, err
	}
	d, ok := s.devices[id]
	if !ok {
		return PairedDeviceInfo{}, errPairingUnknown
	}
	next := make(map[string]*pairedDevice, len(s.devices))
	for k, v := range s.devices {
		if k != id {
			next[k] = v
		}
	}
	s.replaceDevicesLocked(next)
	if err := s.writeDevicesLocked(); err != nil {
		return PairedDeviceInfo{}, err
	}
	return d.info(), nil
}

// AddDevice creates a device and returns its token, which is shown once and
// never stored.
func (s *pairingStore) AddDevice(name, kind string) (string, PairedDeviceInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addDeviceLocked(name, kind)
}

func (s *pairingStore) addDeviceLocked(name, kind string) (string, PairedDeviceInfo, error) {
	if err := s.reloadLocked(true); err != nil {
		return "", PairedDeviceInfo{}, err
	}
	if len(s.devices) >= pairingMaxDevices {
		return "", PairedDeviceInfo{}, errPairingTooMany
	}
	if kind != pairedKindToken {
		kind = pairedKindBrowser
	}
	token, id, err := newPairingToken()
	if err != nil {
		return "", PairedDeviceInfo{}, err
	}
	now := s.now()
	d := &pairedDevice{
		ID:          id,
		Name:        cleanDeviceName(name, kind),
		Kind:        kind,
		TokenSHA256: hashPairingToken(token),
		Created:     now,
		LastSeen:    now,
	}
	next := make(map[string]*pairedDevice, len(s.devices)+1)
	for k, v := range s.devices {
		next[k] = v
	}
	next[id] = d
	s.replaceDevicesLocked(next)
	if err := s.writeDevicesLocked(); err != nil {
		delete(next, id)
		s.replaceDevicesLocked(next)
		return "", PairedDeviceInfo{}, err
	}
	return token, d.info(), nil
}

// OpenWindow starts a new pairing window, replacing any open one, and returns
// its PIN. The PIN itself is never stored, only a salted hash of it.
func (s *pairingStore) OpenWindow() (string, time.Time, error) {
	pin, err := randomPIN()
	if err != nil {
		return "", time.Time{}, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", time.Time{}, err
	}
	id, err := randomHex(8)
	if err != nil {
		return "", time.Time{}, err
	}
	now := s.now()
	w := &pairingWindow{
		ID:        id,
		Salt:      hex.EncodeToString(salt),
		PINSHA256: hashPIN(salt, pin),
		Created:   now,
		Expires:   now.Add(pairingWindowTTL),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.persistent() {
		data, err := json.MarshalIndent(w, "", "  ")
		if err != nil {
			return "", time.Time{}, err
		}
		if err := writePrivateFile(s.dir, pairingWindowFile, append(data, '\n')); err != nil {
			return "", time.Time{}, err
		}
	} else {
		s.window = w
	}
	return pin, w.Expires, nil
}

// WindowOpen reports whether a PIN can be entered now.
func (s *pairingStore) WindowOpen() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.loadWindowLocked()
	return w != nil
}

func (s *pairingStore) loadWindowLocked() *pairingWindow {
	var w *pairingWindow
	if s.persistent() {
		data, err := os.ReadFile(s.path(pairingWindowFile))
		if err != nil {
			return nil
		}
		var parsed pairingWindow
		if json.Unmarshal(data, &parsed) != nil {
			return nil
		}
		w = &parsed
	} else {
		w = s.window
	}
	if w == nil || w.ID == "" || !s.now().Before(w.Expires) || s.failures[w.ID] >= pairingMaxFailures {
		return nil
	}
	return w
}

func (s *pairingStore) closeWindowLocked() {
	if s.persistent() {
		_ = os.Remove(s.path(pairingWindowFile))
	} else {
		s.window = nil
	}
}

// Pair checks a PIN against the open window and, when it matches, closes the
// window and creates a device. remaining is the number of wrong PINs the
// window still accepts after a wrong one.
func (s *pairingStore) Pair(pin, name, kind string) (token string, device PairedDeviceInfo, remaining int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	// A negative span is a clock stepped back, not a fast retry; refusing it
	// would refuse every PIN until the clock caught up.
	if since := now.Sub(s.lastTry); !s.lastTry.IsZero() && since >= 0 && since < pairingAttemptGap {
		return "", PairedDeviceInfo{}, 0, errPairingRateLimited
	}
	s.lastTry = now
	w := s.loadWindowLocked()
	if w == nil {
		return "", PairedDeviceInfo{}, 0, errPairingNoWindow
	}
	salt, decodeErr := hex.DecodeString(w.Salt)
	want := []byte(w.PINSHA256)
	got := []byte(hashPIN(salt, normalizePIN(pin)))
	if decodeErr != nil || subtle.ConstantTimeCompare(want, got) != 1 {
		s.failures[w.ID]++
		left := pairingMaxFailures - s.failures[w.ID]
		if left <= 0 {
			s.closeWindowLocked()
			return "", PairedDeviceInfo{}, 0, errPairingClosed
		}
		return "", PairedDeviceInfo{}, left, errPairingWrongPIN
	}
	token, device, err = s.addDeviceLocked(name, kind)
	if err != nil {
		return "", PairedDeviceInfo{}, 0, err
	}
	// Single use: the window closes on the first success.
	s.closeWindowLocked()
	delete(s.failures, w.ID)
	return token, device, 0, nil
}

func readPairingDevices(path string) (pairingDevicesDoc, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return pairingDevicesDoc{Version: 1}, nil
	}
	if err != nil {
		return pairingDevicesDoc{}, fmt.Errorf("read %s: %w", path, err)
	}
	var doc pairingDevicesDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return pairingDevicesDoc{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return doc, nil
}

func statStamp(path string) fileStamp {
	info, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{modTime: info.ModTime(), size: info.Size(), exists: true}
}

// writePrivateFile replaces dir/name atomically with an owner-only file.
func writePrivateFile(dir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return fmt.Errorf("create temporary %s: %w", name, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	// Windows ignores the mode; the folder's ACL is what protects it there.
	_ = tmp.Chmod(0o600)
	matchDirOwner(tmp, dir)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temporary %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary %s: %w", name, err)
	}
	if err := replaceFile(tmpName, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("replace %s: %w", name, err)
	}
	return nil
}

// newPairingToken returns "<id>.<secret>": a 16-hex-digit device id and 32
// random bytes, base64url. Both halves are cookie-safe.
func newPairingToken() (token, id string, err error) {
	id, err = randomHex(8)
	if err != nil {
		return "", "", err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", "", err
	}
	return id + "." + base64.RawURLEncoding.EncodeToString(secret), id, nil
}

func splitPairingToken(token string) (id, secret string, ok bool) {
	token = strings.TrimSpace(token)
	if len(token) > 128 {
		return "", "", false
	}
	id, secret, ok = strings.Cut(token, ".")
	if !ok || len(id) != 16 || len(secret) < 32 {
		return "", "", false
	}
	if _, err := hex.DecodeString(id); err != nil {
		return "", "", false
	}
	return id, secret, true
}

func hashPairingToken(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

func hashPIN(salt []byte, pin string) string {
	h := sha256.New()
	h.Write([]byte("sysmon-pairing-pin\x00"))
	h.Write(salt)
	h.Write([]byte(pin))
	return hex.EncodeToString(h.Sum(nil))
}

// normalizePIN drops the spaces and dashes people type between digit groups.
func normalizePIN(pin string) string {
	var b strings.Builder
	for _, r := range pin {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func randomPIN() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// formatPIN shows a PIN as two groups of three digits.
func formatPIN(pin string) string {
	if len(pin) != 6 {
		return pin
	}
	return pin[:3] + " " + pin[3:]
}

func cleanDeviceName(name, kind string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(name) {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	cleaned := strings.TrimSpace(b.String())
	if runes := []rune(cleaned); len(runes) > pairingDeviceNameMax {
		cleaned = strings.TrimSpace(string(runes[:pairingDeviceNameMax]))
	}
	if cleaned == "" {
		if kind == pairedKindToken {
			return "Script"
		}
		return "Browser"
	}
	return cleaned
}
