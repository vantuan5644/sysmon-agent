package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestUpdateCheckerSkipsDevBuilds(t *testing.T) {
	for _, current := range []string{"", "dev"} {
		checker := newUpdateChecker(UpdateCheckerOptions{
			CurrentVersion: current,
			Enabled:        true,
			StartupDelay:   time.Millisecond,
			Interval:       time.Millisecond,
		})
		calls := 0
		checker.fetchRelease = func(context.Context, string) (ReleaseInfo, error) {
			calls++
			return ReleaseInfo{Tag: "v9.9.9"}, nil
		}
		status, err := checker.CheckNow(context.Background())
		if err != nil || status.Available {
			t.Errorf("%q: CheckNow = %+v, %v; want no update and no error", current, status, err)
		}
		checker.Start()
		time.Sleep(30 * time.Millisecond)
		checker.Stop()
		if calls != 0 {
			t.Errorf("%q: a build without a version asked GitHub %d times", current, calls)
		}
	}
}

func updaterModeChecker(t *testing.T, assets map[string]string) (*UpdateChecker, *[]string, *int) {
	t.Helper()
	checker := newUpdateChecker(UpdateCheckerOptions{CurrentVersion: "v1.0.0", Enabled: true})
	checker.mode = func() string { return updateModeUpdater }
	// Never the host's real result.json.
	checker.lastResult = func() *updaterResult { return nil }
	requested := []string{}
	downloads := 0
	checker.request = func(_ context.Context, tag string) error {
		requested = append(requested, tag)
		return nil
	}
	checker.fetchRelease = func(context.Context, string) (ReleaseInfo, error) {
		return ReleaseInfo{Tag: "v1.1.0", URL: "https://example.com/v1.1.0", Assets: assets}, nil
	}
	checker.downloadAsset = func(context.Context, string) ([]byte, error) {
		downloads++
		return nil, errors.New("the agent must not download in updater mode")
	}
	return checker, &requested, &downloads
}

func TestApplyNowAsksTheRootUpdaterWithoutDownloading(t *testing.T) {
	checker, requested, downloads := updaterModeChecker(t, map[string]string{
		linuxUpdateAssetName(runtime.GOARCH): "https://example.com/linux",
		updateChecksumAssetName:              "https://example.com/sums",
	})
	decision, err := checker.ApplyNow(context.Background())
	if err != nil {
		t.Fatalf("ApplyNow: %v (%+v)", err, decision)
	}
	if !decision.Accepted || decision.Stage != "requested" || decision.Helper != linuxUpdaterUnit {
		t.Errorf("decision = %+v, want accepted, requested, by %s", decision, linuxUpdaterUnit)
	}
	if len(*requested) != 1 || (*requested)[0] != "v1.1.0" {
		t.Errorf("requests = %v, want one for v1.1.0", *requested)
	}
	if *downloads != 0 {
		t.Errorf("the agent downloaded %d assets; the root updater fetches its own", *downloads)
	}
	if checker.Status().Available {
		t.Error("the banner still offers the update after it was requested")
	}
	if _, err := checker.ApplyNow(context.Background()); !errors.Is(err, errUpdateInProgress) {
		t.Errorf("a second ApplyNow while the updater works = %v, want errUpdateInProgress", err)
	}
}

func TestARefusedRootUpdateCanBeRequestedAgain(t *testing.T) {
	checker, requested, _ := updaterModeChecker(t, map[string]string{
		linuxUpdateAssetName(runtime.GOARCH): "https://example.com/linux",
		updateChecksumAssetName:              "https://example.com/sums",
	})
	var result *updaterResult
	checker.lastResult = func() *updaterResult { return result }
	// A result from an earlier run says nothing about this request.
	result = &updaterResult{OK: false, Error: "old", FinishedAt: time.Now().Add(-time.Hour)}
	if _, err := checker.ApplyNow(context.Background()); err != nil {
		t.Fatalf("ApplyNow: %v", err)
	}
	if checker.Status().Available {
		t.Fatal("an old result released the request")
	}
	// The updater refused the release and did not restart the agent.
	result = &updaterResult{OK: false, Error: "checksum mismatch", FinishedAt: time.Now().Add(time.Second)}
	if !checker.Status().Available {
		t.Error("the banner did not offer the update again after the updater refused it")
	}
	if _, err := checker.ApplyNow(context.Background()); err != nil {
		t.Fatalf("ApplyNow after a refused update: %v", err)
	}
	if len(*requested) != 2 {
		t.Errorf("requests = %v, want two", *requested)
	}
}

func TestAnUnansweredRootUpdateRequestExpires(t *testing.T) {
	checker, _, _ := updaterModeChecker(t, map[string]string{
		linuxUpdateAssetName(runtime.GOARCH): "https://example.com/linux",
		updateChecksumAssetName:              "https://example.com/sums",
	})
	checker.lastResult = func() *updaterResult { return nil }
	if _, err := checker.ApplyNow(context.Background()); err != nil {
		t.Fatalf("ApplyNow: %v", err)
	}
	checker.mu.Lock()
	checker.settleRequestLocked(time.Now().Add(linuxUpdaterTimeout))
	held := checker.applying
	checker.settleRequestLocked(time.Now().Add(linuxUpdaterTimeout + 2*time.Minute))
	released := !checker.applying
	checker.mu.Unlock()
	if !held || !released {
		t.Errorf("held within the updater's limit = %v, released after it = %v; want both", held, released)
	}
}

func TestApplyNowRefusesAReleaseWithoutThisArchitecture(t *testing.T) {
	checker, requested, _ := updaterModeChecker(t, map[string]string{
		"sysmon-agent.exe":      "https://example.com/exe",
		updateChecksumAssetName: "https://example.com/sums",
	})
	_, err := checker.ApplyNow(context.Background())
	if !errors.Is(err, errUpdateMissingAsset) {
		t.Fatalf("ApplyNow err = %v, want errUpdateMissingAsset", err)
	}
	if len(*requested) != 0 {
		t.Error("the root updater was asked for a release it cannot install")
	}
}

func TestApplyNowReportsAnUpdaterThatDidNotAnswer(t *testing.T) {
	checker, _, _ := updaterModeChecker(t, map[string]string{
		linuxUpdateAssetName(runtime.GOARCH): "https://example.com/linux",
		updateChecksumAssetName:              "https://example.com/sums",
	})
	checker.request = func(context.Context, string) error { return errors.New("nobody took it") }
	decision, err := checker.ApplyNow(context.Background())
	if !errors.Is(err, errUpdateSpawn) || !strings.Contains(decision.Error, "nobody took it") {
		t.Fatalf("ApplyNow = %+v, %v; want errUpdateSpawn with the reason", decision, err)
	}
	if checker.applying {
		t.Error("a failed request left the single-flight flag set")
	}
}

func TestApplyNowWithoutAModeIsUnsupported(t *testing.T) {
	checker := newUpdateChecker(UpdateCheckerOptions{CurrentVersion: "v1.0.0", Enabled: true})
	checker.mode = func() string { return updateModeNone }
	fetched := false
	checker.fetchRelease = func(context.Context, string) (ReleaseInfo, error) {
		fetched = true
		return ReleaseInfo{}, nil
	}
	decision, err := checker.ApplyNow(context.Background())
	if !errors.Is(err, errUpdateUnsupported) || decision.Error != updateUnsupportedReason() {
		t.Fatalf("ApplyNow = %+v, %v; want errUpdateUnsupported with this OS's reason", decision, err)
	}
	if fetched {
		t.Error("an unsupported host still asked GitHub")
	}
}

func TestRequestUpdateInReturnsOnceTheUpdaterTakesIt(t *testing.T) {
	dir := t.TempDir()
	go func() {
		path := filepath.Join(dir, linuxUpdaterRequestName)
		for i := 0; i < 200; i++ {
			if _, err := os.Stat(path); err == nil {
				clearUpdateRequests(dir)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	if err := requestUpdateIn(context.Background(), dir, "v1.1.0", 2*time.Second, 5*time.Millisecond); err != nil {
		t.Fatalf("requestUpdateIn: %v", err)
	}
}

func TestRequestUpdateInWithdrawsARequestNobodyTook(t *testing.T) {
	dir := t.TempDir()
	err := requestUpdateIn(context.Background(), dir, "v1.1.0", 30*time.Millisecond, 5*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), linuxUpdaterPathUnit) {
		t.Fatalf("err = %v, want one naming %s", err, linuxUpdaterPathUnit)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the request stayed behind: %v", entries)
	}
}

func TestClearUpdateRequestsRemovesEverything(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "update"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "nested", "deeper"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	keep := filepath.Join(outside, "keep")
	if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
			t.Fatal(err)
		}
	}
	clearUpdateRequests(dir)
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("left %v in the request folder", entries)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("clearing followed a link out of the folder: %v", err)
	}
}

func TestUpdaterResultRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, filepath.Base(linuxUpdaterResultFile))
	if readUpdaterResult(path) != nil {
		t.Fatal("a missing result read as one")
	}
	want := updaterResult{OK: false, From: "v1.0.0", To: "v1.1.0", Error: "rolled back", Units: []string{"sysmon-agent.service"}, FinishedAt: time.Date(2026, 11, 16, 1, 2, 3, 0, time.UTC)}
	if err := writeUpdaterResult(dir, want); err != nil {
		t.Fatal(err)
	}
	got := readUpdaterResult(path)
	if got == nil || got.OK || got.Error != want.Error || got.To != want.To || !got.FinishedAt.Equal(want.FinishedAt) || len(got.Units) != 1 {
		t.Fatalf("read back %+v, want %+v", got, want)
	}
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if readUpdaterResult(path) != nil {
		t.Error("a corrupt result read as one")
	}
}

func TestParseAgentCgroup(t *testing.T) {
	cases := []struct {
		name, content, unit string
		uid                 int
		ok                  bool
	}{
		{"system unit", "0::/system.slice/sysmon-agent.service\n", "sysmon-agent.service", -1, true},
		{"homelab unit", "0::/system.slice/homelab-sysmon-agent.service\n", "homelab-sysmon-agent.service", -1, true},
		{"user unit", "0::/user.slice/user-1000.slice/user@1000.service/app.slice/sysmon-agent.service\n", "sysmon-agent.service", 1000, true},
		{"cgroup v1", "12:pids:/system.slice/x.service\n1:name=systemd:/system.slice/sysmon-agent.service\n", "sysmon-agent.service", -1, true},
		{"terminal scope", "0::/user.slice/user-1000.slice/user@1000.service/app.slice/app-org.gnome.Terminal.slice/vte-spawn-1.scope\n", "", 0, false},
		{"login session", "0::/user.slice/user-1000.slice/session-2.scope\n", "", 0, false},
		{"user manager itself", "0::/user.slice/user-1000.slice/user@1000.service/init.scope\n", "", 0, false},
		{"empty", "", "", 0, false},
	}
	for _, tc := range cases {
		unit, uid, ok := parseAgentCgroup(tc.content)
		if ok != tc.ok || (ok && (unit != tc.unit || uid != tc.uid)) {
			t.Errorf("%s: got %q, %d, %v; want %q, %d, %v", tc.name, unit, uid, ok, tc.unit, tc.uid, tc.ok)
		}
	}
}

func TestProcStatusIDs(t *testing.T) {
	uid, gid := procStatusIDs("Name:\tsysmon-agent\nUid:\t1000\t1000\t1000\t1000\nGid:\t1001\t1001\t1001\t1001\n")
	if uid != 1000 || gid != 1001 {
		t.Errorf("got %d, %d; want 1000, 1001", uid, gid)
	}
}

func TestAgentUnitsOfDeduplicatesAndSkipsTheUpdater(t *testing.T) {
	procs := []agentProcess{
		{PID: 30, InUnit: true, Unit: agentUnit{Name: "sysmon-agent.service", UID: 1000}},
		{PID: 31, InUnit: true, Unit: agentUnit{Name: "sysmon-agent.service", UID: 1000}}, // its -wait-ready
		{PID: 20, InUnit: true, Unit: agentUnit{Name: "sysmon-agent.service", UID: -1}},
		{PID: 40, InUnit: true, Unit: agentUnit{Name: linuxUpdaterUnit, UID: -1}},
		{PID: 50},
	}
	units, loose := agentUnitsOf(procs)
	if len(units) != 2 || units[0].UID != -1 || units[1].UID != 1000 {
		t.Errorf("units = %+v, want the system unit then uid 1000's", units)
	}
	if len(loose) != 1 || loose[0] != 50 {
		t.Errorf("loose = %v, want [50]", loose)
	}
}

// rootUpdateFixture is a live binary, a release that replaces it and fake
// systemctl calls that record their order.
type rootUpdateFixture struct {
	exe     string
	calls   []string
	deps    rootUpdateDeps
	newBody []byte
}

func newRootUpdateFixture(t *testing.T) *rootUpdateFixture {
	t.Helper()
	dir := t.TempDir()
	f := &rootUpdateFixture{exe: filepath.Join(dir, "sysmon-agent"), newBody: []byte("NEW BINARY")}
	if err := os.WriteFile(f.exe, []byte("OLD BINARY"), 0o755); err != nil {
		t.Fatal(err)
	}
	asset := linuxUpdateAssetName("amd64")
	sum := sha256.Sum256(f.newBody)
	sums := []byte(hex.EncodeToString(sum[:]) + "  " + asset + "\n0000  sysmon-agent.exe\n")
	unit := agentUnit{Name: "sysmon-agent.service", UID: 1000, GID: 1000}
	f.deps = rootUpdateDeps{
		Current: "0.2.5",
		Exe:     f.exe,
		Arch:    "amd64",
		Fetch: func(context.Context) (ReleaseInfo, error) {
			return ReleaseInfo{Tag: "v0.2.6", Assets: map[string]string{asset: "bin", updateChecksumAssetName: "sums"}}, nil
		},
		Download: func(_ context.Context, url string) ([]byte, error) {
			if url == "bin" {
				return f.newBody, nil
			}
			return sums, nil
		},
		BinaryVersion: func(path string) (string, error) {
			data, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}
			if string(data) != string(f.newBody) {
				return "0.0.0", nil
			}
			return "0.2.6", nil
		},
		Units:  []agentUnit{unit},
		Stop:   func(u agentUnit) error { f.calls = append(f.calls, "stop "+u.Name); return nil },
		Start:  func(u agentUnit) error { f.calls = append(f.calls, "start "+u.Name); return nil },
		Active: func(u agentUnit) error { f.calls = append(f.calls, "active "+u.Name); return nil },
	}
	return f
}

func (f *rootUpdateFixture) live(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.exe)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (f *rootUpdateFixture) leftovers(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(f.exe))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if e.Name() != filepath.Base(f.exe) {
			names = append(names, e.Name())
		}
	}
	return names
}

func TestRunRootUpdateInstallsAVerifiedRelease(t *testing.T) {
	f := newRootUpdateFixture(t)
	result := runRootUpdate(context.Background(), f.deps)
	if !result.OK || result.From != "v0.2.5" || result.To != "v0.2.6" || result.FinishedAt.IsZero() {
		t.Fatalf("result = %+v, want ok from v0.2.5 to v0.2.6", result)
	}
	if got := f.live(t); got != "NEW BINARY" {
		t.Errorf("live binary = %q, want the release", got)
	}
	want := "stop sysmon-agent.service,start sysmon-agent.service,active sysmon-agent.service"
	if got := strings.Join(f.calls, ","); got != want {
		t.Errorf("calls = %s, want %s", got, want)
	}
	if left := f.leftovers(t); len(left) != 0 {
		t.Errorf("left %v beside the binary", left)
	}
	if len(result.Units) != 1 || !strings.Contains(result.Units[0], "user 1000") {
		t.Errorf("units = %v", result.Units)
	}
}

func TestRunRootUpdateLeavesTheLatestAlone(t *testing.T) {
	f := newRootUpdateFixture(t)
	f.deps.Current = "0.2.6"
	result := runRootUpdate(context.Background(), f.deps)
	if !result.OK || !strings.Contains(result.Message, "already the latest") {
		t.Fatalf("result = %+v, want ok and already the latest", result)
	}
	if len(f.calls) != 0 || f.live(t) != "OLD BINARY" {
		t.Errorf("touched the install: calls %v, live %q", f.calls, f.live(t))
	}
}

func TestRunRootUpdateRefusesBadReleases(t *testing.T) {
	cases := map[string]func(*rootUpdateFixture){
		// The release serves other bytes than SHA256SUMS.txt names.
		"checksum mismatch": func(f *rootUpdateFixture) { f.newBody = []byte("TAMPERED") },
		"wrong version inside": func(f *rootUpdateFixture) {
			f.deps.BinaryVersion = func(string) (string, error) { return "0.2.5", nil }
		},
		"binary will not run": func(f *rootUpdateFixture) {
			f.deps.BinaryVersion = func(string) (string, error) { return "", errors.New("exec format error") }
		},
		"no asset for this architecture": func(f *rootUpdateFixture) { f.deps.Arch = "riscv64" },
		"dev build":                      func(f *rootUpdateFixture) { f.deps.Current = "dev" },
		"offline": func(f *rootUpdateFixture) {
			f.deps.Fetch = func(context.Context) (ReleaseInfo, error) { return ReleaseInfo{}, errors.New("no route") }
		},
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRootUpdateFixture(t)
			breakIt(f)
			result := runRootUpdate(context.Background(), f.deps)
			if result.OK || result.Error == "" {
				t.Fatalf("result = %+v, want a failure", result)
			}
			if len(f.calls) != 0 {
				t.Errorf("stopped or started units for a refused release: %v", f.calls)
			}
			if got := f.live(t); got != "OLD BINARY" {
				t.Errorf("live binary = %q, want it untouched", got)
			}
			if left := f.leftovers(t); len(left) != 0 {
				t.Errorf("left %v beside the binary", left)
			}
		})
	}
}

func TestRunRootUpdateRollsBackAUnitThatDoesNotStay(t *testing.T) {
	f := newRootUpdateFixture(t)
	f.deps.Active = func(u agentUnit) error {
		f.calls = append(f.calls, "active "+u.Name)
		return errors.New("inactive")
	}
	result := runRootUpdate(context.Background(), f.deps)
	if result.OK || !strings.Contains(result.Error, "rolled back") {
		t.Fatalf("result = %+v, want a rollback", result)
	}
	if got := f.live(t); got != "OLD BINARY" {
		t.Errorf("live binary = %q, want the previous one restored", got)
	}
	want := "stop sysmon-agent.service,start sysmon-agent.service,active sysmon-agent.service,stop sysmon-agent.service,start sysmon-agent.service"
	if got := strings.Join(f.calls, ","); got != want {
		t.Errorf("calls = %s, want %s", got, want)
	}
}

func TestRunRootUpdateSaysWhatItCouldNotRestart(t *testing.T) {
	f := newRootUpdateFixture(t)
	f.deps.Units = nil
	f.deps.Loose = []int{4242}
	result := runRootUpdate(context.Background(), f.deps)
	if !result.OK || !strings.Contains(result.Message, "4242") {
		t.Fatalf("result = %+v, want ok naming pid 4242", result)
	}
	f = newRootUpdateFixture(t)
	f.deps.Units = nil
	result = runRootUpdate(context.Background(), f.deps)
	if !result.OK || !strings.Contains(result.Message, "nothing was restarted") {
		t.Fatalf("result = %+v, want ok saying nothing was restarted", result)
	}
}

func TestLinuxUpdaterUnits(t *testing.T) {
	pathUnit, serviceUnit := linuxUpdaterUnits("/usr/local/bin/sysmon-agent")
	for _, want := range []string{"DirectoryNotEmpty=" + linuxUpdaterRequestDir, "Unit=" + linuxUpdaterUnit, "WantedBy=multi-user.target"} {
		if !strings.Contains(pathUnit, want) {
			t.Errorf("path unit lacks %q:\n%s", want, pathUnit)
		}
	}
	for _, want := range []string{"Type=oneshot", "ExecStart=/usr/local/bin/sysmon-agent -update\n", "TimeoutStartSec="} {
		if !strings.Contains(serviceUnit, want) {
			t.Errorf("service unit lacks %q:\n%s", want, serviceUnit)
		}
	}
	_, quoted := linuxUpdaterUnits("/opt/sysmon agent/50%/sysmon-agent")
	if !strings.Contains(quoted, `ExecStart="/opt/sysmon agent/50%%/sysmon-agent" -update`) {
		t.Errorf("a path with a space and a %% was not quoted for systemd:\n%s", quoted)
	}
}
