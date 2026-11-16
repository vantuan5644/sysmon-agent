//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// updatePlatformMode reports updateModeUpdater once `sysmon-agent
// -update-setup` has made the request folder this agent's account may write.
// It is checked per call, so the Update button appears without a restart.
func updatePlatformMode() string {
	if syscall.Access(linuxUpdaterRequestDir, 2 /* W_OK */) == nil {
		return updateModeUpdater
	}
	return updateModeNone
}

func updateUnsupportedReason() string {
	exe := linuxUpdateExe()
	return fmt.Sprintf("the dashboard cannot update this agent until the root updater is set up; run `sudo %s -update` to update now, or `sudo %s -update-setup` once to enable the Update button", exe, exe)
}

// updateCommandHint is the command /api/status offers when the dashboard has
// no Update button here.
func updateCommandHint() string {
	return "sudo " + linuxUpdateExe() + " -update"
}

func linuxUpdateExe() string {
	exe, err := os.Executable()
	if err != nil {
		return "sysmon-agent"
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if strings.ContainsAny(exe, " \t'\"$`\\") {
		return "'" + strings.ReplaceAll(exe, "'", `'\''`) + "'"
	}
	return exe
}

func requestRootUpdate(ctx context.Context, tag string) error {
	return requestUpdateIn(ctx, linuxUpdaterRequestDir, tag, linuxUpdaterPickupTimeout, linuxUpdaterPickupPoll)
}

func lastUpdaterResult() *updaterResult {
	return readUpdaterResult(linuxUpdaterResultFile)
}

// findAgentProcesses scans /proc for processes whose executable is exe. A
// process started from a binary that has since been replaced shows its path
// with " (deleted)" appended, and still counts.
func findAgentProcesses(exe string) []agentProcess {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var found []agentProcess
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == self {
			continue
		}
		dir := filepath.Join("/proc", entry.Name())
		target, err := os.Readlink(filepath.Join(dir, "exe"))
		if err != nil {
			continue
		}
		if strings.TrimSuffix(target, " (deleted)") != exe {
			continue
		}
		proc := agentProcess{PID: pid, UID: -1, GID: -1}
		if status, err := os.ReadFile(filepath.Join(dir, "status")); err == nil {
			proc.UID, proc.GID = procStatusIDs(string(status))
		}
		if cgroup, err := os.ReadFile(filepath.Join(dir, "cgroup")); err == nil {
			if name, uid, ok := parseAgentCgroup(string(cgroup)); ok {
				proc.Unit = agentUnit{Name: name, UID: uid, GID: -1}
				if uid >= 0 {
					proc.Unit.GID = proc.GID
				}
				proc.InUnit = true
			}
		}
		found = append(found, proc)
	}
	return found
}

// systemctlFor runs systemctl against the manager that owns unit: the system
// one, or the user's own manager, reached as that user over its bus.
func systemctlFor(unit agentUnit, args ...string) error {
	var cmd *exec.Cmd
	if unit.UID < 0 {
		cmd = exec.Command("systemctl", args...)
	} else {
		cmd = exec.Command("systemctl", append([]string{"--user"}, args...)...)
		runtimeDir := "/run/user/" + strconv.Itoa(unit.UID)
		cmd.Env = []string{
			"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
			"XDG_RUNTIME_DIR=" + runtimeDir,
			"DBUS_SESSION_BUS_ADDRESS=unix:path=" + runtimeDir + "/bus",
		}
		gid := unit.GID
		if gid < 0 {
			gid = unit.UID
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(unit.UID), Gid: uint32(gid)}}
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, msg)
		}
		return fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// runRootUpdateCommand is `sysmon-agent -update`, by hand or from
// sysmon-agent-update.service.
func runRootUpdateCommand(out io.Writer) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("-update replaces the installed binary and restarts its services, so it needs root: sudo %s -update", linuxUpdateExe())
	}
	lock, err := os.OpenFile(linuxUpdaterLockFile, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", linuxUpdaterLockFile, err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("another sysmon-agent -update is already running")
	}
	// Take the request first: the agent that wrote it is waiting to see it
	// gone before it answers the dashboard.
	if n := clearUpdateRequests(linuxUpdaterRequestDir); n > 0 {
		fmt.Fprintf(out, "update requested from the dashboard\n")
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve this binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	ctx, cancel := context.WithTimeout(context.Background(), linuxUpdaterTimeout)
	defer cancel()
	result := runRootUpdate(ctx, hostRootUpdateDeps(ctx, version, exe, out))
	if err := os.MkdirAll(linuxUpdaterDir, 0o755); err == nil {
		if werr := writeUpdaterResult(linuxUpdaterDir, result); werr != nil {
			fmt.Fprintf(out, "could not record the result in %s: %v\n", linuxUpdaterResultFile, werr)
		}
	}
	if !result.OK {
		return errors.New(result.Error)
	}
	return nil
}

// hostRootUpdateDeps are the real side effects for updating the binary at exe:
// GitHub, the units running it on this host, and systemctl.
func hostRootUpdateDeps(ctx context.Context, current, exe string, out io.Writer) rootUpdateDeps {
	units, loose := agentUnitsOf(findAgentProcesses(exe))
	client := &http.Client{}
	return rootUpdateDeps{
		Current: current,
		Exe:     exe,
		Arch:    runtime.GOARCH,
		Fetch: func(ctx context.Context) (ReleaseInfo, error) {
			return fetchLatestReleaseWithClient(ctx, client, updateRepo)
		},
		Download: func(ctx context.Context, url string) ([]byte, error) {
			return downloadReleaseAssetWithClient(ctx, client, url)
		},
		BinaryVersion: func(path string) (string, error) {
			vctx, vcancel := context.WithTimeout(ctx, 10*time.Second)
			defer vcancel()
			got, err := exec.CommandContext(vctx, path, "-version").Output()
			return strings.TrimSpace(string(got)), err
		},
		Units:  units,
		Loose:  loose,
		Stop:   func(u agentUnit) error { return systemctlFor(u, "stop", u.Name) },
		Start:  func(u agentUnit) error { return systemctlFor(u, "start", u.Name) },
		Active: func(u agentUnit) error { return systemctlFor(u, "is-active", "--quiet", u.Name) },
		Settle: linuxUpdaterSettle,
		Logf: func(format string, args ...any) {
			fmt.Fprintf(out, format+"\n", args...)
		},
	}
}

// runUpdaterSetup is `sysmon-agent -update-setup [-update-user NAME]`: it
// lets the agent's account ask for updates and installs the root units that
// answer it.
func runUpdaterSetup(account string, out io.Writer) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("-update-setup writes systemd units, so it needs root: sudo %s -update-setup", linuxUpdateExe())
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve this binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	uid, gid, name, err := updaterAccount(account, exe)
	if err != nil {
		return err
	}
	if uid == 0 {
		return errors.New("the agent runs as root here, which the updater does not need; run `sudo sysmon-agent -update` instead")
	}

	if err := ensureRootDir(linuxUpdaterDir); err != nil {
		return err
	}
	if info, err := os.Lstat(linuxUpdaterRequestDir); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		if err := os.RemoveAll(linuxUpdaterRequestDir); err != nil {
			return fmt.Errorf("replace %s: %w", linuxUpdaterRequestDir, err)
		}
	}
	if err := os.Mkdir(linuxUpdaterRequestDir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create %s: %w", linuxUpdaterRequestDir, err)
	}
	if err := os.Chown(linuxUpdaterRequestDir, uid, gid); err != nil {
		return fmt.Errorf("give %s to %s: %w", linuxUpdaterRequestDir, name, err)
	}
	if err := os.Chmod(linuxUpdaterRequestDir, 0o700); err != nil {
		return err
	}
	clearUpdateRequests(linuxUpdaterRequestDir)

	pathUnit, serviceUnit := linuxUpdaterUnits(exe)
	if err := os.WriteFile(filepath.Join(linuxUpdaterUnitDir, linuxUpdaterPathUnit), []byte(pathUnit), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(linuxUpdaterUnitDir, linuxUpdaterUnit), []byte(serviceUnit), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", "--now", linuxUpdaterPathUnit}} {
		if err := systemctlFor(agentUnit{UID: -1}, args...); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "The dashboard's Update button now installs new releases on this host.\n")
	fmt.Fprintf(out, "  account allowed to ask: %s (uid %d), through %s\n", name, uid, linuxUpdaterRequestDir)
	fmt.Fprintf(out, "  updater: %s and %s in %s, running %s -update as root\n", linuxUpdaterPathUnit, linuxUpdaterUnit, linuxUpdaterUnitDir, exe)
	fmt.Fprintf(out, "The updater takes nothing from the request: it fetches the latest release itself and checks it against SHA256SUMS.txt.\n")
	fmt.Fprintf(out, "To undo: sudo systemctl disable --now %s && sudo rm %s/%s %s/%s && sudo rm -r %s\n",
		linuxUpdaterPathUnit, linuxUpdaterUnitDir, linuxUpdaterPathUnit, linuxUpdaterUnitDir, linuxUpdaterUnit, linuxUpdaterDir)
	return nil
}

// updaterAccount picks the account allowed to ask for updates: the one
// named, else the one account running this binary, else the user who ran sudo.
func updaterAccount(account, exe string) (uid, gid int, name string, err error) {
	if account != "" {
		u, err := user.Lookup(account)
		if err != nil {
			if _, numErr := strconv.Atoi(account); numErr == nil {
				u, err = user.LookupId(account)
			}
		}
		if err != nil {
			return 0, 0, "", fmt.Errorf("-update-user %s: %w", account, err)
		}
		return lookedUpIDs(u)
	}
	ids := map[[2]int]bool{}
	for _, p := range findAgentProcesses(exe) {
		if p.UID >= 0 && p.InUnit {
			ids[[2]int{p.UID, p.GID}] = true
		}
	}
	if len(ids) == 1 {
		for id := range ids {
			name := strconv.Itoa(id[0])
			if u, err := user.LookupId(name); err == nil {
				name = u.Username
			}
			return id[0], id[1], name, nil
		}
	}
	if len(ids) > 1 {
		return 0, 0, "", errors.New("agents run under more than one account here; name the one the dashboard's Update button should work for with -update-user")
	}
	if sudoUID := os.Getenv("SUDO_UID"); sudoUID != "" {
		if u, err := user.LookupId(sudoUID); err == nil {
			return lookedUpIDs(u)
		}
	}
	return 0, 0, "", errors.New("no running agent found to take its account from; start the agent first, or name its account with -update-user")
}

func lookedUpIDs(u *user.User) (int, int, string, error) {
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, "", err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return 0, 0, "", err
	}
	return uid, gid, u.Username, nil
}

// ensureRootDir makes dir a real directory owned by root with mode 0755.
func ensureRootDir(dir string) error {
	info, err := os.Lstat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.Mkdir(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	case err != nil:
		return err
	case !info.IsDir() || info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("%s exists and is not a directory; remove it and run -update-setup again", dir)
	}
	if err := os.Chown(dir, 0, 0); err != nil {
		return err
	}
	return os.Chmod(dir, 0o755)
}
