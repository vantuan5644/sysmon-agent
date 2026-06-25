package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

// readTailscaleStatus queries the local Tailscale daemon via
// `tailscale status --json` and distills it to the two states the dashboard
// cares about: whether this node is online to the coordination server and
// whether it is currently routing traffic through an exit node. Tailscale is
// optional -- a missing CLI or an unreachable daemon degrades to
// Available:false instead of failing the whole sample, exactly like nvidia-smi.
func readTailscaleStatus(ctx context.Context) TailscaleStatus {
	if _, err := exec.LookPath("tailscale"); err != nil {
		return TailscaleStatus{Available: false, Error: "tailscale CLI not found"}
	}

	callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	out, err := exec.CommandContext(callCtx, "tailscale", "status", "--json").Output()
	if err != nil {
		return TailscaleStatus{Available: false, Error: fmt.Sprintf("tailscale status failed: %v", err)}
	}

	var status tailscaleStatusJSON
	if err := json.Unmarshal(out, &status); err != nil {
		return TailscaleStatus{Available: false, Error: fmt.Sprintf("tailscale status JSON parse failed: %v", err)}
	}

	exit := status.ExitNodeStatus != nil && status.ExitNodeStatus.Online
	return TailscaleStatus{Available: true, Online: status.Self.Online, ExitNodeEnabled: exit}
}

// tailscaleStatusJSON is the minimal slice of `tailscale status --json` we
// decode; the rest (peers, DNS, capabilities) is ignored. Keys are PascalCase
// as emitted by the Tailscale CLI.
type tailscaleStatusJSON struct {
	Self struct {
		Online bool `json:"Online"`
	} `json:"Self"`
	ExitNodeStatus *struct {
		Online bool `json:"Online"`
	} `json:"ExitNodeStatus"`
}
