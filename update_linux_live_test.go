//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestLiveRootUpdateRestartsAUserUnit drives the root updater's real host side
// against a user unit shaped like scripts/systemd/sysmon-agent.user.service:
// the /proc scan, systemctl --user as that user, the swap, the restart, and a
// rollback. Only GitHub is replaced. It needs root and that user's running
// manager, so it runs only when asked:
//
//	sudo env "PATH=$PATH" SYSMON_UPDATER_LIVE_UID=$(id -u) go test -count=1 -run TestLiveRootUpdate .
func TestLiveRootUpdateRestartsAUserUnit(t *testing.T) {
	uidText := os.Getenv("SYSMON_UPDATER_LIVE_UID")
	if uidText == "" {
		t.Skip("set SYSMON_UPDATER_LIVE_UID and run as root to drive a real user unit")
	}
	if os.Geteuid() != 0 {
		t.Fatal("SYSMON_UPDATER_LIVE_UID is set but the test is not running as root")
	}
	account, err := user.LookupId(uidText)
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := strconv.Atoi(account.Uid)
	gid, _ := strconv.Atoi(account.Gid)
	if _, err := os.Stat(fmt.Sprintf("/run/user/%d/bus", uid)); err != nil {
		t.Skipf("uid %d has no running user manager: %v", uid, err)
	}

	root, err := os.MkdirTemp("", "sysmon-update-live-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(root, "sysmon-agent")
	liveBuild(t, exe, "0.0.1")
	newer := filepath.Join(root, "next")
	liveBuild(t, newer, "0.0.2")
	state := filepath.Join(root, "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(state, uid, gid); err != nil {
		t.Fatal(err)
	}

	port := liveFreePort(t)
	unit := agentUnit{Name: fmt.Sprintf("sysmon-update-live-%d.service", os.Getpid()), UID: uid, GID: gid}
	unitDir := "/run/systemd/user"
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	unitPath := filepath.Join(unitDir, unit.Name)
	unitBody := fmt.Sprintf(`[Service]
Type=simple
ExecStart=%[1]s -bind 127.0.0.1 -port %[2]d -pairing off -no-update-check -settings %[3]s/settings.json
ExecStartPost=%[1]s -wait-ready -bind 127.0.0.1 -port %[2]d -wait-ready-timeout 20s
TimeoutStartSec=60
Restart=on-failure
RestartSec=3
NoNewPrivileges=true
`, exe, port, state)
	if err := os.WriteFile(unitPath, []byte(unitBody), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = systemctlFor(unit, "stop", unit.Name)
		_ = os.Remove(unitPath)
		_ = systemctlFor(unit, "daemon-reload")
		_ = systemctlFor(unit, "reset-failed", unit.Name)
	})
	if err := systemctlFor(unit, "daemon-reload"); err != nil {
		t.Fatal(err)
	}
	if err := systemctlFor(unit, "start", unit.Name); err != nil {
		t.Fatal(err)
	}
	liveWaitForUnitProcess(t, exe, unit)

	var log strings.Builder
	release := func(tag string) func(context.Context) (ReleaseInfo, error) {
		return func(context.Context) (ReleaseInfo, error) {
			return ReleaseInfo{Tag: tag, Assets: map[string]string{linuxUpdateAssetName(runtime.GOARCH): "bin", updateChecksumAssetName: "sums"}}, nil
		}
	}
	serve := func(binary []byte) func(context.Context, string) ([]byte, error) {
		sum := sha256.Sum256(binary)
		sums := []byte(hex.EncodeToString(sum[:]) + "  " + linuxUpdateAssetName(runtime.GOARCH) + "\n")
		return func(_ context.Context, url string) ([]byte, error) {
			if url == "bin" {
				return binary, nil
			}
			return sums, nil
		}
	}

	// 1. A real update: found by the /proc scan, stopped, swapped, started.
	nextBytes, err := os.ReadFile(newer)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	deps := hostRootUpdateDeps(ctx, "0.0.1", exe, &log)
	if len(deps.Units) != 1 || deps.Units[0] != unit || len(deps.Loose) != 0 {
		t.Fatalf("found units %+v and loose %v, want only %+v", deps.Units, deps.Loose, unit)
	}
	deps.Fetch, deps.Download, deps.Settle = release("v0.0.2"), serve(nextBytes), time.Second
	result := runRootUpdate(ctx, deps)
	if !result.OK {
		t.Fatalf("update failed: %+v\n%s", result, log.String())
	}
	if got := liveStatusVersion(t, port); got != "0.0.2" {
		t.Fatalf("the restarted unit serves version %q, want 0.0.2\n%s", got, log.String())
	}
	liveAssertRunsTheNewImage(t, exe, unit)
	if _, err := os.Stat(exe + linuxUpdateBackupSuffix); !os.IsNotExist(err) {
		t.Errorf("the rollback copy stayed behind: %v", err)
	}

	// 2. A release that passes -version but cannot serve: rolled back, and the
	// unit runs the previous binary again.
	broken := []byte("#!/bin/sh\nif [ \"$1\" = -version ]; then echo 0.0.3; exit 0; fi\nexit 1\n")
	deps = hostRootUpdateDeps(ctx, "0.0.2", exe, &log)
	deps.Fetch, deps.Download, deps.Settle = release("v0.0.3"), serve(broken), time.Second
	result = runRootUpdate(ctx, deps)
	if result.OK || !strings.Contains(result.Error, "rolled back to the previous binary") {
		t.Fatalf("result = %+v, want a rollback\n%s", result, log.String())
	}
	if got := liveStatusVersion(t, port); got != "0.0.2" {
		t.Fatalf("after the rollback the unit serves version %q, want 0.0.2\n%s", got, log.String())
	}
	liveAssertRunsTheNewImage(t, exe, unit)
}

func liveBuild(t *testing.T, out, version string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", out, "-ldflags", "-X main.version="+version, ".")
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", version, err, msg)
	}
	if err := os.Chmod(out, 0o755); err != nil {
		t.Fatal(err)
	}
}

func liveFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func liveWaitForUnitProcess(t *testing.T, exe string, unit agentUnit) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, p := range findAgentProcesses(exe) {
			if p.InUnit && p.Unit == unit {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("no process of %s found running %s", unit, exe)
}

// liveAssertRunsTheNewImage checks the unit's process runs the file now at
// exe, not the replaced one ("(deleted)" in its /proc exe link).
func liveAssertRunsTheNewImage(t *testing.T, exe string, unit agentUnit) {
	t.Helper()
	for _, p := range findAgentProcesses(exe) {
		if !p.InUnit || p.Unit != unit {
			continue
		}
		target, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", p.PID))
		if err == nil && target == exe {
			return
		}
	}
	t.Errorf("no process of %s runs the current %s", unit, exe)
}

func liveStatusVersion(t *testing.T, port int) string {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/status", port))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var status struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatalf("/api/status: %v: %s", err, body)
	}
	return status.Version
}
