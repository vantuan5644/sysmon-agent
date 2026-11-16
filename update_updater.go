package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The Linux root updater.
//
// A Linux agent runs unprivileged (a system unit with User=, or a desktop's
// systemctl --user unit, both with NoNewPrivileges), and its binary in
// /usr/local/bin belongs to root, so the agent can never replace it. Root
// does the update instead, in one of two ways:
//
//   - by hand: `sudo sysmon-agent -update`;
//   - from the dashboard, once `sudo sysmon-agent -update-setup` has run:
//     the agent writes a file into linuxUpdaterRequestDir, which its own
//     account owns, and sysmon-agent-update.path starts the root oneshot
//     sysmon-agent-update.service, which runs `sysmon-agent -update`.
//
// The request carries nothing the updater trusts. The updater clears it,
// fetches the latest release from GitHub itself, checks the binary against
// SHA256SUMS.txt and against its own -version, then stops the units that run
// this binary, swaps it (applyVerifiedSwap, the same sequence as the Windows
// helper), starts them again and rolls back if one does not come up. Its
// result goes to linuxUpdaterResultFile, in a folder only root can write,
// where the restarted agent reads it for /api/status.
//
// Everything here is free of build tags so the tests run on every OS; the
// /proc scan, systemctl calls and setup live in update_linux.go.

const (
	linuxUpdaterDir         = "/var/lib/sysmon-agent-update"
	linuxUpdaterRequestDir  = linuxUpdaterDir + "/request"
	linuxUpdaterResultFile  = linuxUpdaterDir + "/result.json"
	linuxUpdaterRequestName = "update"
	linuxUpdaterUnit        = "sysmon-agent-update.service"
	linuxUpdaterPathUnit    = "sysmon-agent-update.path"
	linuxUpdaterUnitDir     = "/etc/systemd/system"
	linuxUpdaterLockFile    = "/run/sysmon-agent-update.lock"

	// linuxUpdaterPickupTimeout is how long the agent waits for the updater
	// to take its request before it withdraws it and says the updater is not
	// running. The updater clears the request first thing.
	linuxUpdaterPickupTimeout = 15 * time.Second
	linuxUpdaterPickupPoll    = 250 * time.Millisecond
	// linuxUpdaterSettle is how long a restarted unit must stay active before
	// the update counts. The units' own ExecStartPost=-wait-ready already
	// makes `systemctl start` wait for /readyz; this catches a binary that
	// passes that once and then exits.
	linuxUpdaterSettle = 3 * time.Second
	// linuxUpdaterTimeout bounds one whole -update run.
	linuxUpdaterTimeout = 10 * time.Minute

	linuxUpdaterResultMaxBytes = 16 << 10
	// linuxUpdateBackupSuffix names the rollback copy beside the live binary.
	linuxUpdateBackupSuffix = ".old"
)

// updaterResult is what the root updater writes to result.json and what
// /api/status reports as update.last_apply.
type updaterResult struct {
	OK         bool      `json:"ok"`
	From       string    `json:"from,omitempty"`
	To         string    `json:"to,omitempty"`
	Message    string    `json:"message,omitempty"`
	Error      string    `json:"error,omitempty"`
	Units      []string  `json:"units,omitempty"`
	FinishedAt time.Time `json:"finished_at"`
}

// requestUpdateIn writes an update request into dir and waits up to wait for
// the updater to take it (it deletes everything in dir when it starts). A
// request nobody takes is withdrawn, so a missing or broken updater is an
// error now instead of an update that happens hours later.
func requestUpdateIn(ctx context.Context, dir, tag string, wait, poll time.Duration) error {
	path := filepath.Join(dir, linuxUpdaterRequestName)
	body, err := json.Marshal(struct {
		Tag         string    `json:"tag"`
		RequestedAt time.Time `json:"requested_at"`
	}{tag, time.Now().UTC()})
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("ask the root updater: %w", err)
	}
	deadline := time.Now().Add(wait)
	for {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			_ = os.Remove(path)
			return ctx.Err()
		case <-time.After(poll):
		}
	}
	_ = os.Remove(path)
	return fmt.Errorf("the root updater did not take the request within %s; check `systemctl status %s` on this host", wait, linuxUpdaterPathUnit)
}

// clearUpdateRequests deletes everything in the request folder. The folder
// belongs to the agent's account, so nothing in it is opened or followed:
// os.RemoveAll removes a symlink itself, never its target.
func clearUpdateRequests(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, entry := range entries {
		if os.RemoveAll(filepath.Join(dir, entry.Name())) == nil {
			removed++
		}
	}
	return removed
}

// readUpdaterResult reads result.json, or nil when there is none.
func readUpdaterResult(path string) *updaterResult {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, linuxUpdaterResultMaxBytes))
	if err != nil {
		return nil
	}
	var result updaterResult
	if json.Unmarshal(data, &result) != nil || result.FinishedAt.IsZero() {
		return nil
	}
	return &result
}

// writeUpdaterResult replaces result.json in dir. The temp file and rename
// keep a reader from ever seeing half a file.
func writeUpdaterResult(dir string, result updaterResult) error {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".result-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, filepath.Join(dir, filepath.Base(linuxUpdaterResultFile))); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// agentUnit is a systemd unit running the binary being updated. UID is -1
// for a unit of the system manager, else the uid whose user manager runs it.
type agentUnit struct {
	Name string
	UID  int
	GID  int
}

func (u agentUnit) String() string {
	if u.UID < 0 {
		return u.Name
	}
	return fmt.Sprintf("%s (user %d)", u.Name, u.UID)
}

// parseAgentCgroup finds the systemd unit in a /proc/<pid>/cgroup file:
// "0::/system.slice/sysmon-agent.service" is a system unit, and
// "0::/user.slice/user-1000.slice/user@1000.service/app.slice/sysmon-agent.service"
// a unit of uid 1000's manager. A process in a scope (a login shell, a
// terminal) is in no unit, and ok is false.
func parseAgentCgroup(content string) (unit string, uid int, ok bool) {
	path := ""
	for _, line := range strings.Split(content, "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(parts) != 3 {
			continue
		}
		if parts[0] == "0" && parts[1] == "" {
			path = parts[2]
			break
		}
		if parts[1] == "name=systemd" && path == "" {
			path = parts[2]
		}
	}
	if path == "" {
		return "", 0, false
	}
	segments := strings.Split(strings.Trim(path, "/"), "/")
	last := segments[len(segments)-1]
	if !strings.HasSuffix(last, ".service") || isUserManagerSegment(last) {
		return "", 0, false
	}
	uid = -1
	for _, segment := range segments[:len(segments)-1] {
		if isUserManagerSegment(segment) {
			n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(segment, "user@"), ".service"))
			if err != nil {
				return "", 0, false
			}
			uid = n
		}
	}
	return last, uid, true
}

func isUserManagerSegment(segment string) bool {
	if !strings.HasPrefix(segment, "user@") || !strings.HasSuffix(segment, ".service") {
		return false
	}
	_, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(segment, "user@"), ".service"))
	return err == nil
}

// agentProcess is a process running the binary being updated.
type agentProcess struct {
	PID      int
	UID, GID int
	Unit     agentUnit
	InUnit   bool
}

// procStatusIDs reads the real uid and gid from /proc/<pid>/status.
func procStatusIDs(status string) (uid, gid int) {
	uid, gid = -1, -1
	for _, line := range strings.Split(status, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "Uid:":
			uid, _ = strconv.Atoi(fields[1])
		case "Gid:":
			gid, _ = strconv.Atoi(fields[1])
		}
	}
	return uid, gid
}

// agentUnitsOf splits the processes into the units to restart, deduplicated
// (a unit's ExecStartPost=-wait-ready runs the same binary), and the PIDs
// outside any unit. The updater's own unit is never one of them.
func agentUnitsOf(procs []agentProcess) (units []agentUnit, loose []int) {
	seen := map[string]bool{}
	for _, p := range procs {
		if !p.InUnit {
			loose = append(loose, p.PID)
			continue
		}
		if p.Unit.Name == linuxUpdaterUnit && p.Unit.UID < 0 {
			continue
		}
		key := p.Unit.Name + "/" + strconv.Itoa(p.Unit.UID)
		if seen[key] {
			continue
		}
		seen[key] = true
		units = append(units, p.Unit)
	}
	sort.Slice(units, func(i, j int) bool {
		if units[i].UID != units[j].UID {
			return units[i].UID < units[j].UID
		}
		return units[i].Name < units[j].Name
	})
	sort.Ints(loose)
	return units, loose
}

// rootUpdateDeps are the root updater's side effects, injected so every path
// (and every rollback) runs in tests with fakes.
type rootUpdateDeps struct {
	Current  string // the running binary's version tag
	Exe      string // the live binary
	Arch     string // GOARCH, which picks the release asset
	Fetch    func(ctx context.Context) (ReleaseInfo, error)
	Download func(ctx context.Context, url string) ([]byte, error)
	// BinaryVersion runs a binary with -version.
	BinaryVersion func(path string) (string, error)
	Units         []agentUnit
	// Loose are PIDs running the binary outside any unit; they keep the old
	// version until restarted by hand.
	Loose  []int
	Stop   func(agentUnit) error
	Start  func(agentUnit) error
	Active func(agentUnit) error
	Settle time.Duration
	Logf   func(format string, args ...any)
}

// runRootUpdate installs the latest release over d.Exe when it is newer.
func runRootUpdate(ctx context.Context, d rootUpdateDeps) updaterResult {
	logf := d.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	result := updaterResult{From: normalizeVersionTag(d.Current)}
	for _, unit := range d.Units {
		result.Units = append(result.Units, unit.String())
	}
	finish := func(err error) updaterResult {
		if err != nil {
			result.OK = false
			result.Error = err.Error()
		} else {
			result.OK = true
		}
		result.FinishedAt = time.Now().UTC()
		return result
	}

	current := normalizeVersionTag(d.Current)
	if current == "" || current == "dev" {
		return finish(errors.New("this binary has no version (a development build), so no release can be compared with it"))
	}
	release, err := d.Fetch(ctx)
	if err != nil {
		return finish(fmt.Errorf("fetch the latest release: %w", err))
	}
	result.To = release.Tag
	if !isVersionNewer(release.Tag, current) {
		result.Message = fmt.Sprintf("%s is already the latest release", current)
		logf("%s", result.Message)
		return finish(nil)
	}
	asset := linuxUpdateAssetName(d.Arch)
	assetURL := release.Assets[asset]
	if assetURL == "" {
		return finish(fmt.Errorf("release %s has no %s asset", release.Tag, asset))
	}
	sumsURL := release.Assets[updateChecksumAssetName]
	if sumsURL == "" {
		return finish(fmt.Errorf("release %s has no %s asset", release.Tag, updateChecksumAssetName))
	}

	logf("downloading %s %s", release.Tag, asset)
	downloadCtx, cancel := context.WithTimeout(ctx, updateDownloadTimeout)
	defer cancel()
	binary, err := d.Download(downloadCtx, assetURL)
	if err != nil {
		return finish(fmt.Errorf("download %s: %w", asset, err))
	}
	sums, err := d.Download(downloadCtx, sumsURL)
	if err != nil {
		return finish(fmt.Errorf("download %s: %w", updateChecksumAssetName, err))
	}
	expected, err := parseChecksumForAsset(sums, asset)
	if err != nil {
		return finish(err)
	}
	if actual := sha256Hex(binary); !strings.EqualFold(actual, expected) {
		return finish(fmt.Errorf("checksum mismatch for %s: expected %s, got %s", asset, expected, actual))
	}
	logf("checksum verified against %s", updateChecksumAssetName)

	staged, err := stageRootUpdate(binary, filepath.Dir(d.Exe), release.Tag)
	if err != nil {
		return finish(fmt.Errorf("stage the new binary: %w", err))
	}
	defer os.Remove(staged)
	reported, err := d.BinaryVersion(staged)
	if err != nil {
		return finish(fmt.Errorf("run the new binary: %w", err))
	}
	if normalizeVersionTag(reported) != normalizeVersionTag(release.Tag) {
		return finish(fmt.Errorf("the new binary reports version %q, not %s", strings.TrimSpace(reported), release.Tag))
	}

	ops := updateSwapOps{
		StopAndWait: func(string) error {
			for _, unit := range d.Units {
				logf("stopping %s", unit)
				if err := d.Stop(unit); err != nil {
					return fmt.Errorf("%s: %w", unit, err)
				}
			}
			return nil
		},
		Start: func(string) error {
			var first error
			for _, unit := range d.Units {
				logf("starting %s", unit)
				if err := d.Start(unit); err != nil && first == nil {
					first = fmt.Errorf("%s: %w", unit, err)
				}
			}
			return first
		},
		WaitReady: func(string, time.Duration) error {
			if len(d.Units) == 0 {
				return nil
			}
			if d.Settle > 0 {
				time.Sleep(d.Settle)
			}
			for _, unit := range d.Units {
				if err := d.Active(unit); err != nil {
					return fmt.Errorf("%s: %w", unit, err)
				}
			}
			return nil
		},
		BackupSuffix: linuxUpdateBackupSuffix,
	}
	if err := applyVerifiedSwap("sysmon-agent units", d.Exe, staged, "", 0, ops); err != nil {
		return finish(err)
	}
	result.Message = fmt.Sprintf("updated %s from %s to %s", d.Exe, current, release.Tag)
	switch {
	case len(d.Units) == 0 && len(d.Loose) == 0:
		result.Message += "; no running agent was found, so nothing was restarted"
	case len(d.Loose) > 0:
		result.Message += fmt.Sprintf("; restart the agent running outside systemd (pid %s) yourself", joinInts(d.Loose))
	}
	logf("%s", result.Message)
	return finish(nil)
}

// stageRootUpdate writes the verified binary beside the live one, executable.
func stageRootUpdate(data []byte, dir, tag string) (string, error) {
	safeTag := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-':
			return r
		}
		return '_'
	}, tag)
	tmp, err := os.CreateTemp(dir, "."+updateStagedExePrefix+"-"+safeTag+"-*")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

func joinInts(values []int) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, ", ")
}

// linuxUpdaterUnits returns the path and service units -update-setup installs
// for the binary at exe.
func linuxUpdaterUnits(exe string) (pathUnit, serviceUnit string) {
	pathUnit = `# Written by sysmon-agent -update-setup. Starts the root updater when the
# agent asks for an update from the dashboard, by writing a file into the
# request folder, which its account owns.
[Unit]
Description=Sysmon Agent update requests from the dashboard

[Path]
DirectoryNotEmpty=` + linuxUpdaterRequestDir + `
Unit=` + linuxUpdaterUnit + `

[Install]
WantedBy=multi-user.target
`
	serviceUnit = `# Written by sysmon-agent -update-setup. Installs the latest release when it
# is newer: fetched from GitHub and checked against SHA256SUMS.txt here, never
# taken from the request. Its result is in ` + linuxUpdaterResultFile + `.
[Unit]
Description=Sysmon Agent updater (installs the latest verified release)

[Service]
Type=oneshot
ExecStart=` + systemdQuoteArg(exe) + ` -update
TimeoutStartSec=` + strconv.Itoa(int(linuxUpdaterTimeout/time.Minute)+1) + `min
`
	return pathUnit, serviceUnit
}

// systemdQuoteArg quotes a path for an ExecStart= line when it needs it.
func systemdQuoteArg(arg string) string {
	if !strings.ContainsAny(arg, " \t\"'\\$%;") {
		return arg
	}
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `$$`, `%`, `%%`).Replace(arg)
	return `"` + escaped + `"`
}
