package main

import (
	"os/exec"
	"reflect"
	"testing"
	"time"
)

// TestAgentCostSumsTheAgentsSubtree: the agent's own cost is the agent plus
// every live descendant (the LHM pwsh and its conhost, an nvidia-smi query),
// and nothing else - a sibling under services.exe is not ours.
func TestAgentCostSumsTheAgentsSubtree(t *testing.T) {
	raw := []ProcessMetric{
		treeProcess(700, 0, "services.exe", 0),
		treeProcess(3000, 700, "sysmon-agent.exe", 0.25),
		treeProcess(3100, 3000, "pwsh.exe", 0.5),
		treeProcess(3200, 3100, "conhost.exe", 0.25),
		treeProcess(3300, 3000, "nvidia-smi.exe", 1),
		treeProcess(4000, 700, "svchost.exe", 40),
	}
	annotateProcessTree(raw)
	var tracker agentCostTracker
	now := time.Unix(1_760_000_000, 0)
	cost := tracker.measure(raw, 3000, now, 32)

	if !cost.Available || cost.PID != 3000 || cost.Processes != 4 {
		t.Fatalf("cost = %+v, want the agent and its three descendants", cost)
	}
	if cost.CPU.Value != 2 {
		t.Fatalf("cpu = %v%%, want 2%% (0.25+0.5+0.25+1)", cost.CPU.Value)
	}
	if cost.CPUCores.Value != 0.64 || cost.CPUCores.Unit != "cores" {
		t.Fatalf("cores = %+v, want 0.64 cores on a 32-thread host", cost.CPUCores)
	}
	if cost.Memory.Value != 4096 {
		t.Fatalf("memory = %v, want 4 x 1024 B", cost.Memory.Value)
	}
	wantMembers := []AppMember{{Name: "conhost", Count: 1}, {Name: "nvidia-smi", Count: 1}, {Name: "pwsh", Count: 1}, {Name: "sysmon-agent", Count: 1}}
	if !reflect.DeepEqual(cost.Members, wantMembers) {
		t.Fatalf("members = %+v, want %+v", cost.Members, wantMembers)
	}
}

// TestAgentCostChargesChildrenThatExitedBetweenPasses is the regression the
// cost exists to catch: a per-pass PowerShell spawn lives for a second and is
// gone before the next enumeration, so only noteChildExit's counter sees it.
func TestAgentCostChargesChildrenThatExitedBetweenPasses(t *testing.T) {
	raw := []ProcessMetric{treeProcess(3000, 0, "sysmon-agent", 0.5)}
	annotateProcessTree(raw)
	var childNanos int64
	tracker := agentCostTracker{exited: func() int64 { return childNanos }}
	start := time.Unix(1_760_000_000, 0)
	first := tracker.measure(raw, 3000, start, 4)
	if first.CPU.Value != 0.5 {
		t.Fatalf("first pass cpu = %v, want 0.5 (nothing exited yet)", first.CPU.Value)
	}

	// 0.8 s of child CPU over a 2 s pass on 4 cores is 10% of the host.
	childNanos += int64(800 * time.Millisecond)
	second := tracker.measure(raw, 3000, start.Add(2*time.Second), 4)
	if second.CPU.Value != 10.5 {
		t.Fatalf("second pass cpu = %v, want 10.5 (0.5 live + 10 exited)", second.CPU.Value)
	}
	if second.CPUCores.Value != 0.42 {
		t.Fatalf("second pass cores = %v, want 0.42", second.CPUCores.Value)
	}
	third := tracker.measure(raw, 3000, start.Add(4*time.Second), 4)
	if third.CPU.Value != 0.5 {
		t.Fatalf("third pass cpu = %v, want 0.5: an exited child must be charged once", third.CPU.Value)
	}
}

// TestAgentCostDegradesPerField: before the second enumeration the agent's
// own CPU is still warming up, and a pass that cannot see the agent at all
// reports that instead of a zero.
func TestAgentCostDegradesPerField(t *testing.T) {
	raw := []ProcessMetric{treeProcess(3000, 0, "sysmon-agent", 0)}
	raw[0].CPU = unavailableNumber("%", "process sampler is warming up")
	var tracker agentCostTracker
	warming := tracker.measure(raw, 3000, time.Now(), 8)
	if !warming.Available || warming.CPU.Available || warming.CPU.Error != "process sampler is warming up" || !warming.Memory.Available {
		t.Fatalf("warming cost = %+v", warming)
	}
	missing := tracker.measure(raw, 9999, time.Now(), 8)
	if missing.Available || missing.CPU.Available || missing.Error == "" {
		t.Fatalf("missing agent cost = %+v", missing)
	}
}

func TestNoteChildExitIgnoresAChildThatNeverRan(t *testing.T) {
	before := exitedChildCPUNanos.Load()
	noteChildExit(nil)
	noteChildExit(exec.Command("sysmon-agent-no-such-binary"))
	if exitedChildCPUNanos.Load() != before {
		t.Fatal("a child that never ran was charged CPU time")
	}
}
