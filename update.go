package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// updateRepo is the GitHub owner/repo the public release is published under and
// the channel both the in-dashboard self-update and `install-windows.ps1
// -Action Update` poll. The agent only reads (GET /releases/latest and asset
// downloads); it never authenticates, so it is bound by GitHub's anonymous
// rate limit (60/hr/IP, irrelevant at the once-per-day cadence).
const updateRepo = "vantuan5644/sysmon-agent"

// updateCheckInterval is the steady-state cadence of the background update
// check. Once per day keeps the agent well under GitHub's anonymous rate limit
// even for many LAN agents behind one NAT IP, and a fresh "update available"
// appears in the dashboard within a day of a release.
const updateCheckInterval = 24 * time.Hour

// updateCheckStartupDelay pushes the first background check a short time after
// the agent starts so a boot-time network blip or DNS cold-cache does not flag
// the check failed before the host has settled.
const updateCheckStartupDelay = 30 * time.Second

// updateRequestTimeout bounds a single GitHub API call. The check runs in the
// background, so this only caps how long a hung request can sit before the
// checker gives up and tries again at the next interval.
const updateRequestTimeout = 15 * time.Second

// updateDownloadTimeout bounds the download of the new binary + checksum during
// an in-dashboard self-update. The binary is ~9 MB; 2 minutes is generous even
// on a slow uplink while still bounding a stuck transfer.
const updateDownloadTimeout = 2 * time.Minute

// updateApplySpawnTimeout bounds the spawn of the detached apply helper. The
// helper itself is fire-and-forget (it stops the service, which kills the
// agent, then runs to completion asynchronously); this only caps how long the
// spawn syscall can sit before the agent returns 202.
const updateApplySpawnTimeout = 10 * time.Second

// updateAssetName is the standalone binary asset name published on each
// release. We swap the binary directly (no installer re-run) so this is the
// file the agent downloads and the file the SHA-256 sum is looked up for.
const updateAssetName = "sysmon-agent.exe"

// updateChecksumAssetName is the release artifact that authenticates the
// download. It is produced by the release flow (sha256sum over the binary and
// the installer) and is the security gate for both the agent self-update and
// the PowerShell `-Action Update` engine: the downloaded exe is verified
// against this file before it is ever executed or swapped in.
const updateChecksumAssetName = "SHA256SUMS.txt"

// ReleaseInfo describes a published release on the update channel.
type ReleaseInfo struct {
	Tag         string    // "vX.Y.Z"
	URL         string    // html_url of the release
	PublishedAt time.Time // published_at, zero if absent
	Assets      map[string]string
}

// UpdateStatus is the cached update-check result surfaced in /api/status. Every
// field is optional except Available so an offline host still serializes a
// well-formed object (available=false) rather than a half-populated one.
type UpdateStatus struct {
	Available     bool       `json:"available"`
	LatestVersion string     `json:"latest_version,omitempty"`
	URL           string     `json:"url,omitempty"`
	PublishedAt   *time.Time `json:"published_at,omitempty"`
	CheckedAt     *time.Time `json:"checked_at,omitempty"`
}

// UpdateDecision is the synchronous result of `UpdateChecker.ApplyNow` and the
// body of the POST /api/update response.
type UpdateDecision struct {
	Accepted       bool   `json:"accepted"`
	LatestVersion  string `json:"latest_version,omitempty"`
	CurrentVersion string `json:"current_version,omitempty"`
	// Stage is "applied" once the detached helper has been spawned. The actual
	// swap+restart happens after the agent returns 202 and is observed by the
	// dashboard via the service-worker staleness reload.
	Stage      string `json:"stage,omitempty"`
	VerifiedTo string `json:"verified_to,omitempty"`
	Helper     string `json:"helper,omitempty"`
	Error      string `json:"error,omitempty"`
}

// UpdateChecker runs the background update-check loop, caches the latest result,
// and performs the synchronous half of an in-dashboard self-update (download +
// verify), handing the verified file to the detached apply-update helper.
//
// It runs on the serve path only — never under -self-check — and only when the
// update check is enabled (setting default true, hard-off via -no-update-check
// flag / SYSMON_UPDATE_CHECK=0). Offline/error is non-fatal: the last known
// result is kept and no log spam is emitted.
type UpdateChecker struct {
	enabled       bool
	current       string
	repo          string
	interval      time.Duration
	startupDelay  time.Duration
	httpClient    *http.Client
	apply         updateApplier // Windows-only swap helper; no-op stub elsewhere
	logf          func(format string, args ...any)
	fetchRelease  func(ctx context.Context, repo string) (ReleaseInfo, error)
	downloadAsset func(ctx context.Context, url string) ([]byte, error)

	mu     sync.RWMutex
	cached UpdateStatus
	// applying is the single-flight guard for ApplyNow. Without it, a
	// double-click or a second dashboard tab starts a second 9 MB download and
	// spawns a second detached helper, and the two race on the swap — the
	// loser's os.Remove(backup) deletes the winner's rollback copy. It is set
	// for the rest of the process lifetime once a helper is spawned: the agent
	// is about to be stopped by that helper, so there is no "done" to reset on.
	applying bool

	stop    chan struct{}
	stopped chan struct{}
	started bool
}

// UpdateCheckerOptions captures the inputs main.go resolves once (the agent
// version, the effective enabled flag after merging setting + flag/env) so the
// checker does not need to re-resolve them on every check.
type UpdateCheckerOptions struct {
	CurrentVersion string
	Enabled        bool
	Repo           string
	Interval       time.Duration
	StartupDelay   time.Duration
	HTTPClient     *http.Client
	Logf           func(format string, args ...any)
}

// newUpdateChecker builds a checker with sensible defaults. Passing Enabled
// false yields a valid checker that surfaces "no update" in /api/status and
// refuses ApplyNow; the background loop is still a no-op so callers do not have
// to branch on it.
func newUpdateChecker(opts UpdateCheckerOptions) *UpdateChecker {
	if opts.Repo == "" {
		opts.Repo = updateRepo
	}
	if opts.Interval <= 0 {
		opts.Interval = updateCheckInterval
	}
	if opts.StartupDelay < 0 {
		opts.StartupDelay = updateCheckStartupDelay
	}
	if opts.HTTPClient == nil {
		// Deliberately no client-level Timeout: the same client serves the
		// quick API call and the ~9 MB asset download, which need very
		// different budgets. Each call scopes its own deadline onto the
		// context instead (updateRequestTimeout / updateDownloadTimeout).
		opts.HTTPClient = &http.Client{}
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	return &UpdateChecker{
		enabled:      opts.Enabled,
		current:      normalizeVersionTag(opts.CurrentVersion),
		repo:         opts.Repo,
		interval:     opts.Interval,
		startupDelay: opts.StartupDelay,
		httpClient:   opts.HTTPClient,
		apply:        defaultUpdateApplier{},
		logf:         opts.Logf,
		// Bind the resolved client into the default implementations so an
		// injected HTTPClient (tests, or a future proxy-aware transport) is
		// actually used rather than silently ignored.
		fetchRelease: func(ctx context.Context, repo string) (ReleaseInfo, error) {
			return fetchLatestReleaseWithClient(ctx, opts.HTTPClient, repo)
		},
		downloadAsset: func(ctx context.Context, url string) ([]byte, error) {
			return downloadReleaseAssetWithClient(ctx, opts.HTTPClient, url)
		},
	}
}

// Start launches the background check loop. It is safe to call when disabled:
// the loop parks without making any network calls so the agent never phones
// home for a host that has opted out. Start is idempotent.
func (c *UpdateChecker) Start() {
	c.mu.Lock()
	if c.started || !c.enabled {
		c.mu.Unlock()
		return
	}
	c.started = true
	c.stop = make(chan struct{})
	c.stopped = make(chan struct{})
	c.mu.Unlock()

	go c.loop()
}

// Stop signals the background loop to exit and blocks until it has. A stopped
// checker cannot be restarted; main treats it as a one-shot resource.
func (c *UpdateChecker) Stop() {
	c.mu.Lock()
	if !c.started {
		c.mu.Unlock()
		return
	}
	stop := c.stop
	stopped := c.stopped
	c.started = false
	c.mu.Unlock()

	close(stop)
	<-stopped
}

// Enabled reports whether the checker is permitted to make outbound calls.
// /api/status surfaces this so the dashboard can explain "no checks are being
// made" rather than showing an empty update block.
func (c *UpdateChecker) Enabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.enabled
}

// SetEnabled toggles the runtime-enabled flag. This is driven by the persisted
// DashboardSettings.UpdateCheckEnabled toggle (POST /api/settings). A flag/env
// hard-off (-no-update-check / SYSMON_UPDATE_CHECK=0) wins over the setting and
// is enforced at construction time (Enabled() stays false regardless of calls
// here); the dashboard surfaces the effective state, not the requested one.
func (c *UpdateChecker) SetEnabled(enabled bool) {
	c.mu.Lock()
	c.enabled = enabled
	c.mu.Unlock()
}

// beginApply claims the single-flight apply slot, returning false when another
// apply is already running (or has already spawned a helper).
func (c *UpdateChecker) beginApply() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.applying {
		return false
	}
	c.applying = true
	return true
}

// endApply releases the apply slot. It is called only when the attempt failed
// before spawning a helper — once the helper is running, the slot stays claimed
// because the service (and this process) is about to be stopped.
func (c *UpdateChecker) endApply() {
	c.mu.Lock()
	c.applying = false
	c.mu.Unlock()
}

// Status returns the cached update state for /api/status.
func (c *UpdateChecker) Status() UpdateStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cached
}

// CheckNow forces a single check on-demand and returns the fresh status. Used
// by the apply path (re-verify right before downloading) and could back a
// future "check now" dashboard affordance. It does not require the background
// loop to be running.
func (c *UpdateChecker) CheckNow(ctx context.Context) (UpdateStatus, error) {
	status, err := c.checkOnce(ctx)
	c.cacheStatus(status)
	return status, err
}

func (c *UpdateChecker) loop() {
	defer close(c.stopped)
	select {
	case <-c.stop:
		return
	case <-time.After(c.startupDelay):
	}
	c.checkAndCache(context.Background())
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			c.checkAndCache(context.Background())
		}
	}
}

func (c *UpdateChecker) checkAndCache(ctx context.Context) {
	status, err := c.checkOnce(ctx)
	if err != nil {
		c.logf("update check failed: %v", err)
		return
	}
	c.cacheStatus(status)
}

func (c *UpdateChecker) cacheStatus(status UpdateStatus) {
	c.mu.Lock()
	c.cached = status
	c.mu.Unlock()
}

func (c *UpdateChecker) checkOnce(ctx context.Context) (UpdateStatus, error) {
	if !c.Enabled() {
		return UpdateStatus{Available: false, CheckedAt: timeNowUTCPtr()}, nil
	}
	release, err := c.fetchRelease(ctx, c.repo)
	if err != nil {
		return UpdateStatus{}, err
	}
	status := UpdateStatus{
		LatestVersion: release.Tag,
		URL:           release.URL,
		CheckedAt:     timeNowUTCPtr(),
	}
	if !release.PublishedAt.IsZero() {
		t := release.PublishedAt.UTC()
		status.PublishedAt = &t
	}
	if isVersionNewer(release.Tag, c.current) {
		status.Available = true
	}
	return status, nil
}

// ApplyNow performs the synchronous half of an in-dashboard self-update. It
// re-checks the channel, downloads the standalone binary + checksum, verifies
// the SHA-256 against the published sum, and spawns the detached apply-update
// helper. It returns 202-worthy metadata on success; the dashboard then waits
// for the staleness reload that fires once the new binary is up.
//
// The apply helper itself does no network and no verification — it is handed
// the already-vetted file — so the trust surface is concentrated here, in the
// agent process that already has the network and the keys.
func (c *UpdateChecker) ApplyNow(ctx context.Context) (UpdateDecision, error) {
	if !c.Enabled() {
		return UpdateDecision{Error: "update check is disabled"}, errUpdateDisabled
	}
	if c.current == "" || c.current == "dev" {
		return UpdateDecision{Error: "current agent version is unknown; cannot self-update a dev build"}, errUpdateUnsupported
	}
	if !selfUpdateSupported() {
		return UpdateDecision{Error: "self-update is supported only for the Windows service; use install-windows.ps1 -Action Update on this host"}, errUpdateUnsupported
	}
	// Single-flight: two concurrent applies would download twice and spawn two
	// detached helpers that race the same swap, with the second clobbering the
	// first's rollback copy.
	if !c.beginApply() {
		return UpdateDecision{CurrentVersion: c.current, Error: "an update is already in progress"}, errUpdateInProgress
	}
	applied := false
	defer func() {
		if !applied {
			c.endApply()
		}
	}()

	release, err := c.fetchRelease(ctx, c.repo)
	if err != nil {
		return UpdateDecision{Error: "fetch latest release: " + err.Error()}, errUpdateNetwork
	}
	if !isVersionNewer(release.Tag, c.current) {
		return UpdateDecision{
			CurrentVersion: c.current,
			LatestVersion:  release.Tag,
			Error:          fmt.Sprintf("latest release %s is not newer than current %s", release.Tag, c.current),
		}, errUpdateNotNewer
	}
	assetURL, ok := release.Assets[updateAssetName]
	if !ok || assetURL == "" {
		return UpdateDecision{LatestVersion: release.Tag, Error: "release is missing the " + updateAssetName + " asset"}, errUpdateMissingAsset
	}
	checksumURL, ok := release.Assets[updateChecksumAssetName]
	if !ok || checksumURL == "" {
		return UpdateDecision{LatestVersion: release.Tag, Error: "release is missing the " + updateChecksumAssetName + " asset"}, errUpdateMissingAsset
	}

	downloadCtx, cancel := context.WithTimeout(ctx, updateDownloadTimeout)
	defer cancel()
	binary, err := c.downloadAsset(downloadCtx, assetURL)
	if err != nil {
		return UpdateDecision{LatestVersion: release.Tag, Error: "download binary: " + err.Error()}, errUpdateNetwork
	}
	checksums, err := c.downloadAsset(downloadCtx, checksumURL)
	if err != nil {
		return UpdateDecision{LatestVersion: release.Tag, Error: "download checksums: " + err.Error()}, errUpdateNetwork
	}
	expected, err := parseChecksumForAsset(checksums, updateAssetName)
	if err != nil {
		return UpdateDecision{LatestVersion: release.Tag, Error: "parse checksum: " + err.Error()}, errUpdateChecksum
	}
	actual := sha256Hex(binary)
	if !strings.EqualFold(actual, expected) {
		return UpdateDecision{LatestVersion: release.Tag, Error: fmt.Sprintf("checksum mismatch: expected %s, got %s", expected, actual)}, errUpdateChecksum
	}
	verifiedPath, err := stageVerifiedBinary(binary, release.Tag)
	if err != nil {
		return UpdateDecision{LatestVersion: release.Tag, Error: "stage verified binary: " + err.Error()}, errUpdateStage
	}
	applyCtx, applyCancel := context.WithTimeout(context.Background(), updateApplySpawnTimeout)
	defer applyCancel()
	if err := c.apply.Spawn(applyCtx, release.Tag, verifiedPath); err != nil {
		_ = os.Remove(verifiedPath)
		return UpdateDecision{LatestVersion: release.Tag, VerifiedTo: verifiedPath, Error: "spawn apply helper: " + err.Error()}, errUpdateSpawn
	}
	applied = true
	c.cacheStatus(UpdateStatus{
		Available:     false,
		LatestVersion: release.Tag,
		URL:           release.URL,
		CheckedAt:     timeNowUTCPtr(),
	})
	return UpdateDecision{
		Accepted:       true,
		CurrentVersion: c.current,
		LatestVersion:  release.Tag,
		Stage:          "applied",
		VerifiedTo:     verifiedPath,
		Helper:         applyHelperName(),
	}, nil
}

// fetchLatestRelease queries GitHub's releases/latest endpoint. It requires a
// User-Agent header (GitHub rejects requests without one) and follows the
// standard schema {tag_name, html_url, published_at, assets[]}.
func fetchLatestRelease(ctx context.Context, repo string) (ReleaseInfo, error) {
	return fetchLatestReleaseWithClient(ctx, http.DefaultClient, repo)
}

// fetchLatestReleaseWithClient is fetchLatestRelease with an explicit client so
// the checker's configured transport is honoured. The per-call deadline is
// applied to the context here rather than as a client Timeout, because the same
// client is shared with the much longer asset download.
func fetchLatestReleaseWithClient(ctx context.Context, client *http.Client, repo string) (ReleaseInfo, error) {
	if client == nil {
		client = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, updateRequestTimeout)
	defer cancel()
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ReleaseInfo{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "sysmon-agent/"+version)
	resp, err := client.Do(req)
	if err != nil {
		return ReleaseInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return ReleaseInfo{}, fmt.Errorf("github releases/latest returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		TagName     string    `json:"tag_name"`
		HTMLURL     string    `json:"html_url"`
		PublishedAt time.Time `json:"published_at"`
		Assets      []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return ReleaseInfo{}, fmt.Errorf("decode releases/latest JSON: %w", err)
	}
	if strings.TrimSpace(payload.TagName) == "" {
		return ReleaseInfo{}, errors.New("release has no tag_name")
	}
	assets := make(map[string]string, len(payload.Assets))
	for _, asset := range payload.Assets {
		if asset.Name != "" && asset.BrowserDownloadURL != "" {
			assets[asset.Name] = asset.BrowserDownloadURL
		}
	}
	return ReleaseInfo{
		Tag:         strings.TrimSpace(payload.TagName),
		URL:         payload.HTMLURL,
		PublishedAt: payload.PublishedAt,
		Assets:      assets,
	}, nil
}

// downloadReleaseAsset fetches a release asset body. Assets are served from
// GitHub's CDN; the User-Agent is included for parity with the API call.
func downloadReleaseAsset(ctx context.Context, url string) ([]byte, error) {
	return downloadReleaseAssetWithClient(ctx, http.DefaultClient, url)
}

// downloadReleaseAssetWithClient is downloadReleaseAsset with an explicit
// client. The caller bounds the transfer via the context (updateDownloadTimeout).
func downloadReleaseAssetWithClient(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "sysmon-agent/"+version)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("asset download returned HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, updateMaxAssetBytes))
}

// updateMaxAssetBytes caps a downloaded release asset. The standalone binary is
// ~9 MB and the checksum file is tiny; 128 MB is a generous ceiling that still
// rejects a runaway or hostile response before it exhausts memory.
const updateMaxAssetBytes = 128 << 20

// parseChecksumForAsset parses a sha256sum-style checksum file and returns the
// hex digest for the named asset. Lines are `<hex>  <name>` (two spaces,
// per coreutils), but a single space and `*name` (binary mode) are tolerated.
func parseChecksumForAsset(data []byte, assetName string) (string, error) {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		digest := fields[0]
		name := strings.TrimPrefix(strings.TrimSpace(strings.Join(fields[1:], " ")), "*")
		if name == assetName {
			if !checksumHexPattern.MatchString(digest) {
				return "", fmt.Errorf("checksum line for %s is not a valid SHA-256 hex: %q", assetName, digest)
			}
			return strings.ToLower(digest), nil
		}
	}
	return "", fmt.Errorf("checksum file has no entry for %s", assetName)
}

var checksumHexPattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// sha256Hex returns the lowercase hex SHA-256 digest of data.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// exeInstallDir returns the directory containing the running agent's exe. Used
// to stage the verified update binary adjacent to the live exe so the detached
// helper can find it after the agent process is gone. It tolerates a missing
// resolution (rare) by falling back to the OS temp dir via CreateTemp.
func exeInstallDir(selfExe string) string {
	if selfExe == "" {
		return os.TempDir()
	}
	dir := filepath.Dir(selfExe)
	if dir == "" || dir == "." {
		return os.TempDir()
	}
	return dir
}

// stageVerifiedBinary writes the downloaded, verified binary to a stable temp
// path adjacent to the agent's own exe so the apply helper can find it after
// the agent process is gone. The path is returned; the caller (or the helper
// on success) is responsible for removing it.
func stageVerifiedBinary(data []byte, tag string) (string, error) {
	selfExe, err := os.Executable()
	if err != nil {
		return "", err
	}
	dir := exeInstallDir(selfExe)
	pattern := updateStagedExePrefix
	if tag != "" {
		safeTag := strings.Trim(strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
				return r
			}
			return '_'
		}, tag), ".-_")
		if safeTag != "" {
			pattern = pattern + "-" + safeTag
		}
	}
	pattern = pattern + "-*.exe"
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		// CreateTemp in the install dir can fail under restricted service
		// tokens; fall back to the system temp dir. The apply helper resolves
		// the file by absolute path either way.
		tmp, err = os.CreateTemp("", updateStagedExePrefix+"-*.exe")
		if err != nil {
			return "", err
		}
	}
	path := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

// updateStagedExePrefix is the prefix used for the staged verified binary; the
// apply helper reuses it so it can identify staged files for cleanup on a
// successful swap.
const updateStagedExePrefix = "sysmon-agent.update"

// normalizeVersionTag returns the version as a leading-"v" tag, the form GitHub
// releases are tagged in and the form isVersionNewer compares. A bare "dev" or
// empty string is returned as-is so callers can detect "unknown version".
func normalizeVersionTag(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || v == "dev" {
		return v
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return v
}

// applyHelperName returns the human-readable helper identifier surfaced in the
// /api/update response so the dashboard can describe what is happening.
func applyHelperName() string {
	return "sysmon-agent --apply-update"
}

// timeNowUTCPtr returns a pointer to the current UTC time, or nil if time is
// the zero value. Used for the optional CheckedAt / PublishedAt pointers.
func timeNowUTCPtr() *time.Time {
	now := time.Now().UTC()
	return &now
}

// selfUpdateSupported reports whether the in-dashboard self-update is allowed
// on this host. It is confined to the Windows service: console runs and any
// non-Windows host return false (and /api/update reports 501) so the highest-
// risk surface (a SYSTEM-level binary swap) is only reachable from the vetted
// deployment shape.
func selfUpdateSupported() bool {
	return updatePlatformSelfUpdateSupported()
}

// updateApplier is the platform-injected helper-spawn interface. The Windows
// implementation stops the service, swaps the binary, restarts the service,
// and rolls back on /readyz failure; the stub on other platforms returns
// errUpdateUnsupported so the gating in ApplyNow is the single source of truth.
type updateApplier interface {
	Spawn(ctx context.Context, tag, verifiedExe string) error
}

// defaultUpdateApplier delegates to the build-tagged platform implementation.
type defaultUpdateApplier struct{}

func (defaultUpdateApplier) Spawn(ctx context.Context, tag, verifiedExe string) error {
	return spawnApplyHelper(ctx, tag, verifiedExe)
}

// errUpdate* sentinel errors let the HTTP layer distinguish user-visible
// "unsupported / disabled" outcomes (501/409) from genuine network/checksum
// failures (502/500) without inspecting error strings.
var (
	errUpdateDisabled     = errors.New("update check disabled")
	errUpdateUnsupported  = errors.New("self-update unsupported on this host")
	errUpdateNetwork      = errors.New("update network error")
	errUpdateChecksum     = errors.New("update checksum verification failed")
	errUpdateMissingAsset = errors.New("release asset missing")
	errUpdateStage        = errors.New("could not stage verified binary")
	errUpdateSpawn        = errors.New("could not spawn apply helper")
	errUpdateNotNewer     = errors.New("latest release is not newer")
	errUpdateInProgress   = errors.New("an update is already in progress")
)

// isExecutableOnPath reports whether the named binary is resolvable via PATH.
// Used to gate sc.exe / pwsh usage with a useful error when missing.
func isExecutableOnPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// --- version comparison -----------------------------------------------------

// semver is the parsed form of an "X.Y.Z" version with an optional pre-release
// suffix. It is the minimal subset needed to order release tags; build metadata
// (after a "+") is ignored per semver and not stored.
type semver struct {
	major, minor, patch int
	pre                 string
}

func parseSemver(tag string) (semver, bool) {
	s := strings.TrimSpace(tag)
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i] // drop build metadata
	}
	pre := ""
	if i := strings.IndexByte(s, '-'); i >= 0 {
		pre = s[i+1:]
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return semver{}, false
	}
	var nums [3]int
	for i, p := range parts {
		if p == "" {
			return semver{}, false
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return semver{}, false
		}
		nums[i] = n
	}
	return semver{major: nums[0], minor: nums[1], patch: nums[2], pre: pre}, true
}

func compareSemver(a, b semver) int {
	if a.major != b.major {
		return cmpInt(a.major, b.major)
	}
	if a.minor != b.minor {
		return cmpInt(a.minor, b.minor)
	}
	if a.patch != b.patch {
		return cmpInt(a.patch, b.patch)
	}
	// A release without a pre-release ranks higher than one with (1.0.0 > 1.0.0-rc1).
	if a.pre == "" && b.pre == "" {
		return 0
	}
	if a.pre == "" {
		return 1
	}
	if b.pre == "" {
		return -1
	}
	return comparePrerelease(a.pre, b.pre)
}

// comparePrerelease orders two pre-release strings per the semver spec's
// dot-separated identifier rules: all-digit identifiers compare numerically,
// mixed/alphabetic identifiers compare lexically, and a numeric identifier
// always ranks *lower* than an alphanumeric one.
func comparePrerelease(a, b string) int {
	aParts := strings.Split(a, ".")
	bParts := strings.Split(b, ".")
	for i := 0; i < len(aParts) && i < len(bParts); i++ {
		ai, aNum := parsePreIdentifier(aParts[i])
		bi, bNum := parsePreIdentifier(bParts[i])
		if aNum != bNum {
			if aNum {
				return -1
			}
			return 1
		}
		if aNum {
			if ai != bi {
				return cmpInt(ai, bi)
			}
			continue
		}
		if aParts[i] != bParts[i] {
			if aParts[i] < bParts[i] {
				return -1
			}
			return 1
		}
	}
	return cmpInt(len(aParts), len(bParts))
}

func parsePreIdentifier(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || strings.HasPrefix(s, "0") && len(s) > 1 {
		return 0, false
	}
	return n, true
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// isVersionNewer reports whether candidate is a strictly newer release than
// current. Unparseable tags degrade conservatively to false so a malformed
// release never claims "newer" than the running agent.
//
// An unknown current version ("" or the "dev" placeholder left by an untagged
// `go build`) also yields false. It is tempting to treat a dev build as older
// than every release, but ApplyNow refuses to self-update a dev build anyway —
// reporting "newer" there would surface an update banner whose button can only
// ever fail. Both halves agree on "unknown means no offer".
func isVersionNewer(candidate, current string) bool {
	candidate = strings.TrimSpace(candidate)
	current = strings.TrimSpace(current)
	if candidate == "" {
		return false
	}
	if current == "" || current == "dev" {
		return false
	}
	cand, okC := parseSemver(candidate)
	curr, okC2 := parseSemver(current)
	if !okC || !okC2 {
		return false
	}
	return compareSemver(cand, curr) > 0
}
