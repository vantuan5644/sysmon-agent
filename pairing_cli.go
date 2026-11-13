package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

// The pairing commands run on the host and exit. They work on the pairing
// folder directly, so their authority is file access to it: the service's
// account or an administrator. They never talk to the running agent, which
// picks up their changes from the files.

type pairingCommand struct {
	open   bool
	token  string
	list   bool
	revoke string
}

func (c pairingCommand) requested() bool {
	return c.open || c.token != "" || c.list || c.revoke != ""
}

const pairingSettingsHint = "pass the agent's -settings path, the same one its service uses " +
	`(the installer's default is %ProgramData%\SysmonAgent\settings.json on Windows; ` +
	"Linux units use /var/lib/sysmon-agent/settings.json or ~/.local/state/sysmon-agent/settings.json)"

func runPairingCommand(cmd pairingCommand, settingsPath string, listen listenConfig, out io.Writer) error {
	dir := pairingDirFor(settingsPath)
	if dir == "" {
		return errors.New("no pairing folder: " + pairingSettingsHint)
	}
	store, err := newPairingStore(dir)
	if err != nil {
		return fmt.Errorf("%w (run this as the agent's own account or as an administrator)", err)
	}
	switch {
	case cmd.revoke != "":
		removed, err := store.Revoke(strings.TrimSpace(cmd.revoke))
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Revoked %s (%s, %s). It stops working within a few seconds.\n", removed.ID, removed.Name, removed.Kind)
		return nil
	case cmd.list:
		printPairedDevices(out, store.Devices())
		return nil
	case cmd.token != "":
		token, device, err := store.AddDevice(cmd.token, pairedKindToken)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Token for %q (id %s). It is shown once and not stored:\n\n  %s\n\n", device.Name, device.ID, token)
		fmt.Fprintln(out, "Send it on each request as:  Authorization: Bearer <token>")
		fmt.Fprintln(out, "A running agent accepts it within a few seconds, when it next rereads its paired devices.")
		fmt.Fprintf(out, "Revoke it with:  sysmon-agent -pair-revoke %s -settings <path>\n", device.ID)
		// A client with no browser to click through the warning, such as the
		// Moonlight OSD, pins the agent's own certificate by this instead.
		if fingerprint := readAgentCertificateFingerprint(dir); fingerprint != "" && listen.networkTLS {
			fmt.Fprintf(out, "\nNetwork clients reach the agent over HTTPS with its own certificate. Its SHA-256 is\n  %s\n", fingerprint)
		}
		return nil
	}
	pin, expires, err := store.OpenWindow()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Pairing PIN:  %s\n\n", formatPIN(pin))
	fmt.Fprintf(out, "Valid until %s (%d minutes) for one device; %d wrong tries close it.\n",
		expires.Local().Format("15:04"), int(pairingWindowTTL/time.Minute), pairingMaxFailures)
	if urls := accessURLCandidates(listen); len(urls) > 0 {
		fmt.Fprintln(out, "Open the dashboard on the device and enter the PIN:")
		for _, url := range urls {
			fmt.Fprintf(out, "  %s\n", url)
		}
	} else {
		fmt.Fprintln(out, "Open the dashboard on the device and enter the PIN.")
	}
	if fingerprint := readAgentCertificateFingerprint(dir); fingerprint != "" && listen.networkTLS {
		fmt.Fprintf(out, "\nThe browser will warn about the agent's own certificate. Its SHA-256 is\n  %s\n", fingerprint)
	}
	return nil
}

func printPairedDevices(out io.Writer, devices []PairedDeviceInfo) {
	if len(devices) == 0 {
		fmt.Fprintln(out, "No paired devices.")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tKIND\tNAME\tPAIRED\tLAST SEEN")
	for _, d := range devices {
		lastSeen := "-"
		if !d.LastSeen.IsZero() {
			lastSeen = d.LastSeen.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", d.ID, d.Kind, d.Name, d.Created.Local().Format("2006-01-02 15:04"), lastSeen)
	}
	_ = tw.Flush()
}
