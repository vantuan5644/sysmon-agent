package main

import (
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// The agent's own cost.
//
// A monitor that is always watching can change what it measures. On
// 2026-09-10 an SSE-connected Windows dashboard kept the slow lane busy, and
// the process collector's per-pass PowerShell + Get-Counter spawns lined up
// with 23-45% whole-host CPU peaks. That was found indirectly, from the host's
// CPU graph; nothing reported the agent's own share. AgentCost does: the agent
// process plus every live descendant (the LHM pwsh daemon, nvidia-smi, a
// codex app-server lookup), from the same process enumeration the App details
// page uses, plus children that started and exited between two enumerations.
//
// That last part matters because the regression class is exactly short-lived
// spawns, which a process scan only sees if one happens to be alive at that
// instant. Every periodic spawn site calls noteChildExit after the child is
// reaped, which adds the child's user+kernel time from its ProcessState. A
// long-lived child that exits (a restarted LHM daemon) gets its whole lifetime
// added at once, so the pass where that happens can read high; that is rare
// and errs on the side of reporting cost.

var exitedChildCPUNanos atomic.Int64

// noteChildExit adds a reaped child's CPU time to the agent's own cost. Call
// it after Run, Output, CombinedOutput or Wait returns, error or not; a child
// that never started has no ProcessState and adds nothing.
func noteChildExit(cmd *exec.Cmd) {
	if cmd == nil || cmd.ProcessState == nil {
		return
	}
	cpu := cmd.ProcessState.UserTime() + cmd.ProcessState.SystemTime()
	if cpu > 0 {
		exitedChildCPUNanos.Add(int64(cpu))
	}
}

// trackedOutput is cmd.Output that charges the child's CPU to the agent.
func trackedOutput(cmd *exec.Cmd) ([]byte, error) {
	out, err := cmd.Output()
	noteChildExit(cmd)
	return out, err
}

// trackedCombinedOutput is cmd.CombinedOutput that charges the child's CPU
// to the agent.
func trackedCombinedOutput(cmd *exec.Cmd) ([]byte, error) {
	out, err := cmd.CombinedOutput()
	noteChildExit(cmd)
	return out, err
}

// AgentCost is the agent's own footprint, reported under processes.agent.
// CPU is whole-host percent like every process row; CPUCores is the same
// figure in cores (1.0 = one logical core fully busy), which is the number
// that compares across hosts with different core counts. Processes counts the
// agent and its live descendants; Members names them.
type AgentCost struct {
	Available bool         `json:"available"`
	PID       int          `json:"pid,omitempty"`
	CPU       NumberMetric `json:"cpu_percent"`
	CPUCores  NumberMetric `json:"cpu_cores"`
	Memory    NumberMetric `json:"memory_bytes"`
	Processes int          `json:"processes"`
	Members   []AppMember  `json:"members,omitempty"`
	Error     string       `json:"error,omitempty"`
}

// agentCostTracker remembers the exited-children counter between passes so
// each pass charges only what exited since the last one.
type agentCostTracker struct {
	mu         sync.Mutex
	lastExited int64
	lastAt     time.Time
	// exited reads the exited-children counter; nil means the process-wide
	// exitedChildCPUNanos. Tests substitute their own so a child spawned by
	// another test cannot land in their arithmetic.
	exited func() int64
}

// measure sums the agent's subtree in an annotated pass (see
// annotateProcessTree; buildProcessSet runs it) and adds children that exited
// since the previous call.
func (t *agentCostTracker) measure(raw []ProcessMetric, selfPID int, now time.Time, numCPU int) AgentCost {
	read := t.exited
	if read == nil {
		read = exitedChildCPUNanos.Load
	}
	exited := read()
	t.mu.Lock()
	prevExited, prevAt := t.lastExited, t.lastAt
	t.lastExited, t.lastAt = exited, now
	t.mu.Unlock()

	tree := agentSubtree(raw, selfPID)
	if len(tree) == 0 {
		return AgentCost{
			Available: false,
			CPU:       unavailableNumber("%", "agent process not found in this pass"),
			CPUCores:  unavailableNumber("cores", "agent process not found in this pass"),
			Memory:    unavailableNumber("B", "agent process not found in this pass"),
			Error:     "agent process not found in this pass",
		}
	}
	if numCPU < 1 {
		numCPU = 1
	}
	cost := AgentCost{
		Available: true,
		PID:       selfPID,
		Processes: len(tree),
		Members:   appMembers(tree),
		Memory:    sumProcessField(tree, func(p ProcessMetric) NumberMetric { return p.Memory }, "B"),
	}
	self := tree[0]
	if !self.CPU.Available {
		reason := firstNonEmpty(self.CPU.Error, "process sampler is warming up")
		cost.CPU = unavailableNumber("%", reason)
		cost.CPUCores = unavailableNumber("cores", reason)
		return cost
	}
	percent := sumProcessField(tree, func(p ProcessMetric) NumberMetric { return p.CPU }, "%").Value
	if elapsed := now.Sub(prevAt).Seconds(); !prevAt.IsZero() && elapsed > 0 && exited > prevExited {
		percent += float64(exited-prevExited) / 1e9 / elapsed / float64(numCPU) * 100
	}
	if percent > 100 {
		percent = 100
	}
	cost.CPU = availableNumber(percent, "%")
	cost.CPUCores = availableNumber(percent*float64(numCPU)/100, "cores")
	return cost
}

// agentSubtree returns the agent's own row first, then every live descendant.
func agentSubtree(raw []ProcessMetric, selfPID int) []ProcessMetric {
	children := map[int][]int{}
	selfIndex := -1
	for i, p := range raw {
		if p.PID == selfPID {
			selfIndex = i
		}
		if p.PPID != 0 {
			children[p.PPID] = append(children[p.PPID], i)
		}
	}
	if selfIndex < 0 {
		return nil
	}
	tree := []ProcessMetric{raw[selfIndex]}
	seen := map[int]bool{selfPID: true}
	queue := []int{selfPID}
	for len(queue) > 0 && len(tree) < len(raw) {
		pid := queue[0]
		queue = queue[1:]
		for _, i := range children[pid] {
			if seen[raw[i].PID] {
				continue
			}
			seen[raw[i].PID] = true
			tree = append(tree, raw[i])
			queue = append(queue, raw[i].PID)
		}
	}
	return tree
}
