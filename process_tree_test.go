package main

import (
	"reflect"
	"testing"
)

func treeProcess(pid, ppid int, name string, cpu float64) ProcessMetric {
	return ProcessMetric{
		PID:       pid,
		PPID:      ppid,
		Name:      name,
		CPU:       availableNumber(cpu, "%"),
		Memory:    availableNumber(1024, "B"),
		GPUMemory: unavailableNumber("B", "not a CUDA/compute process"),
		DiskRead:  unavailableNumber("B/s", "warming up"),
		DiskWrite: unavailableNumber("B/s", "warming up"),
	}
}

func byPID(raw []ProcessMetric) map[int]ProcessMetric {
	out := map[int]ProcessMetric{}
	for _, p := range raw {
		out[p.PID] = p
	}
	return out
}

// TestAnnotateProcessTreeCountsWorkUnderItsLauncher pins the shape that
// motivated the tree: a Claude Code session in Windows Terminal spawns bash and
// rg, and the agent's LHM daemon is a pwsh under sysmon-agent. Each child must
// be counted under the program that started it, while terminals and shells,
// which only start other people's programs, never become the launcher.
func TestAnnotateProcessTreeCountsWorkUnderItsLauncher(t *testing.T) {
	raw := []ProcessMetric{
		treeProcess(4, 0, "System", 0),
		treeProcess(700, 600, "services.exe", 0),
		treeProcess(1000, 900, "explorer.exe", 1),
		treeProcess(2000, 1000, "WindowsTerminal.exe", 1),
		treeProcess(2100, 2000, "pwsh.exe", 0),
		treeProcess(2200, 2100, "claude.exe", 6),
		treeProcess(2300, 2200, "bash.exe", 1),
		treeProcess(2400, 2300, "rg.exe", 9),
		treeProcess(3000, 700, "sysmon-agent.exe", 1),
		treeProcess(3100, 3000, "pwsh.exe", 2),
		treeProcess(5000, 2000, "pwsh.exe", 0),
	}
	annotateProcessTree(raw)
	got := byPID(raw)

	for pid, want := range map[int]string{
		2200: "claude.exe", 2300: "claude.exe", 2400: "claude.exe",
		3000: "sysmon-agent.exe", 3100: "sysmon-agent.exe",
		// Only infrastructure above and itself a shell: its own launcher.
		5000: "pwsh.exe", 2100: "pwsh.exe", 2000: "WindowsTerminal.exe",
	} {
		if got[pid].Launcher != want {
			t.Fatalf("pid %d (%s) launcher = %q, want %q", pid, got[pid].Name, got[pid].Launcher, want)
		}
	}
	if got[2400].LauncherPID != 2200 {
		t.Fatalf("rg launcher pid = %d, want 2200", got[2400].LauncherPID)
	}
	if got[2300].ParentName != "claude.exe" || got[2200].Children != 1 || got[2000].Children != 2 {
		t.Fatalf("parent/children not annotated: bash parent=%q claude children=%d terminal children=%d",
			got[2300].ParentName, got[2200].Children, got[2000].Children)
	}
	// services.exe's parent (wininit, 600) is not in this pass: the link is
	// dropped instead of naming a PID that may mean something else now.
	if got[700].PPID != 0 || got[700].ParentName != "" {
		t.Fatalf("dangling parent kept: %+v", got[700])
	}
}

// TestAggregateByAppGroupsByLauncherAndListsMembers: the Apps row for a
// session carries everything it started, says what that was, and leaves no
// orphaned bash/rg rows behind.
func TestAggregateByAppGroupsByLauncherAndListsMembers(t *testing.T) {
	raw := []ProcessMetric{
		treeProcess(1, 0, "systemd", 0),
		treeProcess(10, 1, "kitty", 1),
		treeProcess(11, 10, "zsh", 0),
		treeProcess(12, 11, "claude", 5),
		treeProcess(13, 12, "bash", 1),
		treeProcess(14, 12, "bash", 1),
		treeProcess(15, 13, "rg", 20),
		treeProcess(20, 1, "containerd-shim", 0),
		treeProcess(21, 20, "tini", 0),
		treeProcess(22, 21, "immich-server", 30),
		treeProcess(23, 22, "ffmpeg", 40),
	}
	set := buildProcessSet(raw, len(raw))
	apps := map[string]AppMetric{}
	for _, app := range set.Apps {
		apps[app.Name] = app
	}
	claude, ok := apps["claude"]
	if !ok {
		t.Fatalf("no claude app row in %+v", set.Apps)
	}
	if claude.Count != 4 || claude.CPU.Value != 27 {
		t.Fatalf("claude row = count %d cpu %v, want 4 processes at 27%%", claude.Count, claude.CPU.Value)
	}
	wantMembers := []AppMember{{Name: "bash", Count: 2}, {Name: "claude", Count: 1}, {Name: "rg", Count: 1}}
	if !reflect.DeepEqual(claude.Members, wantMembers) {
		t.Fatalf("claude members = %+v, want %+v", claude.Members, wantMembers)
	}
	for _, gone := range []string{"bash", "rg", "ffmpeg"} {
		if _, ok := apps[gone]; ok {
			t.Fatalf("%s still has its own app row", gone)
		}
	}
	// In a container the workload is the launcher, not the shim or the init.
	if immich := apps["immich-server"]; immich.Count != 2 || immich.CPU.Value != 70 {
		t.Fatalf("immich-server row = %+v, want the server and its ffmpeg", immich)
	}
}

// TestAnnotateProcessTreeSurvivesALoop: PID reuse the collector failed to
// catch could, in principle, link two processes to each other. The walk must
// terminate and still pick a launcher.
func TestAnnotateProcessTreeSurvivesALoop(t *testing.T) {
	raw := []ProcessMetric{
		treeProcess(100, 200, "node", 1),
		treeProcess(200, 100, "python", 1),
		treeProcess(300, 300, "self-parented", 1),
	}
	annotateProcessTree(raw)
	got := byPID(raw)
	if got[100].Launcher == "" || got[200].Launcher == "" {
		t.Fatalf("loop left a process without a launcher: %+v", raw)
	}
	if got[300].PPID != 0 || got[300].Launcher != "self-parented" {
		t.Fatalf("self-parented process = %+v", got[300])
	}
}

// TestLauncherBoundariesMatchCollectorNames: Windows names carry .exe and
// arbitrary case; Linux comm is truncated to 15 characters.
func TestLauncherBoundariesMatchCollectorNames(t *testing.T) {
	for _, name := range []string{"PWSH.EXE", "svchost.exe", "WindowsTerminal.exe", "gnome-terminal-", "containerd-shim", "tmux: server", "Hyprland"} {
		if !isLauncherBoundary(name) {
			t.Fatalf("%q is not treated as a boundary", name)
		}
	}
	for _, name := range []string{"claude", "node.exe", "Code.exe", "chrome", "sysmon-agent"} {
		if isLauncherBoundary(name) {
			t.Fatalf("%q is wrongly treated as a boundary", name)
		}
	}
}
