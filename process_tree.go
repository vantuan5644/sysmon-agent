package main

import (
	"sort"
	"strings"
)

// Process tree: who started what.
//
// Both collectors always read each process's parent PID (Toolhelp's
// th32ParentProcessID on Windows, field 4 of /proc/<pid>/stat on Linux) and
// used to throw it away, so the Apps view grouped by executable name alone.
// Every node.exe landed in one row - several Claude Code sessions, VS Code's
// extension hosts and whatever else runs on Node - while the bash, rg and git
// children a session spawns were scattered into rows of their own. The page
// could say "node is busy" but never "this program, and what it started, is
// busy".
//
// So each process is now counted under its launcher: walking from the process
// up through its parents, the launcher is the first process below the
// "infrastructure" layer - session managers, service hosts, terminals and
// shells (processLauncherBoundaries). That is the program a person started.
// For rg -> bash -> claude -> pwsh -> WindowsTerminal -> explorer the launcher
// is claude; for the LHM daemon's pwsh -> sysmon-agent -> services it is
// sysmon-agent. A process with only infrastructure above it is its own
// launcher, so a bare pwsh still reads as pwsh.
//
// Terminals and shells are boundaries, and an IDE is a launcher: something run
// in VS Code's integrated terminal therefore counts under Code, which is how
// Task Manager's Apps view groups it too.

// processLauncherBoundaries are the lowercased executable names (".exe"
// stripped, see appKey) of processes that start other people's programs rather
// than doing work of their own. Linux comm names are truncated to 15
// characters, so long names appear in that form too.
var processLauncherBoundaries = map[string]bool{
	// Windows session and service infrastructure.
	"system": true, "idle": true, "registry": true, "smss": true, "csrss": true,
	"wininit": true, "winlogon": true, "services": true, "lsass": true,
	"svchost": true, "explorer": true, "sihost": true, "userinit": true,
	"taskhostw": true, "taskeng": true, "conhost": true, "openconsole": true,
	"wslhost": true, "wslservice": true, "wsl": true, "wslrelay": true,
	// Linux init, session and desktop infrastructure.
	"systemd": true, "init": true, "(sd-pam)": true, "kthreadd": true,
	"login": true, "agetty": true, "sddm": true, "sddm-helper": true,
	"gdm": true, "gdm-session-wor": true, "lightdm": true,
	"plasmalogin": true, "plasma-login-ma": true,
	"plasmashell": true, "kwin_wayland": true, "kwin_x11": true,
	"gnome-shell": true, "gnome-session-b": true, "hyprland": true,
	"sway": true, "niri": true, "uwsm": true, "startplasma-way": true,
	"cron": true, "crond": true, "atd": true,
	// Container runtimes and in-container inits: the container's own process
	// is the launcher, not the shim that hosts it.
	"containerd-shim": true, "containerd-shim-runc-v2": true, "conmon": true,
	"tini": true, "dumb-init": true, "docker-init": true, "catatonit": true,
	"s6-svscan": true, "s6-supervise": true,
	// Remote and privilege boundaries.
	"sshd": true, "sshd-session": true, "su": true, "sudo": true, "doas": true,
	"runuser": true,
	// Terminals and multiplexers.
	"windowsterminal": true, "wt": true, "wezterm": true, "wezterm-gui": true,
	"alacritty": true, "kitty": true, "konsole": true, "foot": true,
	"ghostty": true, "xterm": true, "yakuake": true, "tilix": true,
	"terminator": true, "ptyxis": true, "kgx": true, "mintty": true,
	"gnome-terminal-": true, "gnome-terminal-server": true,
	"tmux": true, "tmux: server": true, "screen": true, "zellij": true,
	"herdr": true,
	// Shells.
	"cmd": true, "powershell": true, "pwsh": true, "bash": true, "sh": true,
	"zsh": true, "fish": true, "nu": true, "dash": true, "ksh": true,
	"tcsh": true,
}

// processMaxAncestry bounds a parent walk. Real trees are a dozen deep; the
// bound only matters if PID reuse ever forms a loop the creation-time check
// (Windows) did not catch.
const processMaxAncestry = 64

// maxAppMembers bounds the per-app breakdown of what a launcher started.
const maxAppMembers = 6

func isLauncherBoundary(name string) bool {
	return processLauncherBoundaries[appKey(name)]
}

// annotateProcessTree fills ParentName, Children, Launcher and LauncherPID on
// every row from the PPIDs the collector set. A PPID that does not name a
// process in this same pass is cleared: its parent has exited, and a PID that
// is not here cannot be relied on to still mean that parent.
func annotateProcessTree(raw []ProcessMetric) {
	index := make(map[int]int, len(raw))
	for i := range raw {
		index[raw[i].PID] = i
	}
	for i := range raw {
		if raw[i].PPID == raw[i].PID {
			raw[i].PPID = 0
		}
		if _, ok := index[raw[i].PPID]; !ok {
			raw[i].PPID = 0
		}
	}
	for i := range raw {
		raw[i].Children = 0
	}
	for i := range raw {
		if raw[i].PPID != 0 {
			parent := index[raw[i].PPID]
			raw[parent].Children++
			raw[i].ParentName = raw[parent].Name
		} else {
			raw[i].ParentName = ""
		}
	}
	for i := range raw {
		launcher := launcherIndex(raw, index, i)
		raw[i].Launcher = raw[launcher].Name
		raw[i].LauncherPID = raw[launcher].PID
	}
}

// launcherIndex walks from row i to the top of its ancestry and returns the
// first non-boundary process seen coming back down, or i itself when every
// ancestor (and i) is infrastructure.
func launcherIndex(raw []ProcessMetric, index map[int]int, i int) int {
	chain := []int{i}
	seen := map[int]bool{raw[i].PID: true}
	current := i
	for len(chain) < processMaxAncestry {
		ppid := raw[current].PPID
		if ppid == 0 || seen[ppid] {
			break
		}
		parent, ok := index[ppid]
		if !ok {
			break
		}
		seen[ppid] = true
		chain = append(chain, parent)
		current = parent
	}
	for j := len(chain) - 1; j >= 0; j-- {
		if !isLauncherBoundary(raw[chain[j]].Name) {
			return chain[j]
		}
	}
	return i
}

// appMembers summarizes which executables an app row merged, most numerous
// first, so a launcher row can say what it is carrying ("node x6, bash x5").
func appMembers(procs []ProcessMetric) []AppMember {
	counts := map[string]int{}
	names := map[string]string{}
	for _, p := range procs {
		key := appKey(p.Name)
		counts[key]++
		if _, ok := names[key]; !ok {
			names[key] = stripExe(strings.TrimSpace(p.Name))
		}
	}
	members := make([]AppMember, 0, len(counts))
	for key, count := range counts {
		name := names[key]
		if name == "" {
			name = key
		}
		members = append(members, AppMember{Name: name, Count: count})
	}
	sort.Slice(members, func(i, j int) bool {
		if members[i].Count != members[j].Count {
			return members[i].Count > members[j].Count
		}
		return members[i].Name < members[j].Name
	})
	if len(members) > maxAppMembers {
		members = members[:maxAppMembers]
	}
	return members
}
