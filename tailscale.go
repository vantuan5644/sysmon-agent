package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"
)

var errTailscaleCLIMissing = errors.New("tailscale CLI not found")

// tailscaleStatusOutput runs `tailscale status --json` with a short deadline.
// Shared by the NET card's status read and peer discovery, so both report a
// missing CLI or an unreachable daemon in the same words.
func tailscaleStatusOutput(ctx context.Context) ([]byte, error) {
	if _, err := exec.LookPath("tailscale"); err != nil {
		return nil, errTailscaleCLIMissing
	}

	callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	out, err := exec.CommandContext(callCtx, "tailscale", "status", "--json").Output()
	if err != nil {
		return nil, fmt.Errorf("tailscale status failed: %w", err)
	}
	return out, nil
}

// readTailscaleStatus queries the local Tailscale daemon via
// `tailscale status --json` and distills it to the two states the dashboard
// cares about: whether this node is online to the coordination server and
// whether it is currently routing traffic through an exit node. Tailscale is
// optional -- a missing CLI or an unreachable daemon degrades to
// Available:false instead of failing the whole sample, exactly like nvidia-smi.
func readTailscaleStatus(ctx context.Context) TailscaleStatus {
	out, err := tailscaleStatusOutput(ctx)
	if err != nil {
		return TailscaleStatus{Available: false, Error: err.Error()}
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

// tailscalePeer is one tailnet machine that peer discovery may ask for an
// agent's /api/status.
type tailscalePeer struct {
	// Name is the first label of the MagicDNS name: the machine name shown by
	// `tailscale status`, unique within the tailnet, which makes it the host
	// picker's label. OS hostnames can repeat across machines.
	Name    string
	DNSName string
	IPv4    string
	OS      string
}

type tailscalePeersJSON struct {
	MagicDNSSuffix string                       `json:"MagicDNSSuffix"`
	Self           *tailscaleNodeJSON           `json:"Self"`
	Peer           map[string]tailscaleNodeJSON `json:"Peer"`
}

type tailscaleNodeJSON struct {
	HostName     string   `json:"HostName"`
	DNSName      string   `json:"DNSName"`
	OS           string   `json:"OS"`
	TailscaleIPs []string `json:"TailscaleIPs"`
	Online       bool     `json:"Online"`
	ShareeNode   bool     `json:"ShareeNode"`
}

// readTailscalePeers returns this node's machine name and the online peers
// worth checking for an agent.
func readTailscalePeers(ctx context.Context) (string, []tailscalePeer, error) {
	out, err := tailscaleStatusOutput(ctx)
	if err != nil {
		return "", nil, err
	}
	return parseTailscalePeers(out)
}

// parseTailscalePeers keeps only peers that could plausibly run an agent and
// that belong to this tailnet. Phones and TVs are skipped outright. Nodes
// shared in from another tailnet are skipped too: their MagicDNS suffix is the
// other tailnet's, and someone else's machines are out of scope.
func parseTailscalePeers(data []byte) (string, []tailscalePeer, error) {
	var status tailscalePeersJSON
	if err := json.Unmarshal(data, &status); err != nil {
		return "", nil, fmt.Errorf("tailscale status JSON parse failed: %w", err)
	}
	suffix := strings.ToLower(strings.Trim(status.MagicDNSSuffix, "."))
	selfName := ""
	if status.Self != nil {
		selfName = tailnetMachineName(*status.Self)
	}

	peers := make([]tailscalePeer, 0, len(status.Peer))
	for _, node := range status.Peer {
		if !node.Online || node.ShareeNode {
			continue
		}
		switch strings.ToLower(node.OS) {
		case "ios", "android", "tvos":
			continue
		}
		dnsName := strings.ToLower(strings.TrimSuffix(node.DNSName, "."))
		if suffix != "" && dnsName != "" && !strings.HasSuffix(dnsName, "."+suffix) {
			continue
		}
		peer := tailscalePeer{Name: tailnetMachineName(node), DNSName: dnsName, OS: node.OS}
		for _, raw := range node.TailscaleIPs {
			if ip := net.ParseIP(raw); ip != nil && ip.To4() != nil {
				peer.IPv4 = ip.String()
				break
			}
		}
		if peer.Name == "" || (peer.IPv4 == "" && peer.DNSName == "") {
			continue
		}
		peers = append(peers, peer)
	}
	return selfName, peers, nil
}

func tailnetMachineName(node tailscaleNodeJSON) string {
	dnsName := strings.ToLower(strings.TrimSuffix(node.DNSName, "."))
	if label, _, _ := strings.Cut(dnsName, "."); label != "" {
		return label
	}
	return strings.ToLower(strings.TrimSpace(node.HostName))
}
