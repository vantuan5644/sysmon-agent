package main

import (
	"os"
	"strings"
	"testing"
)

// The Linux permission rules under scripts/ exist only in this repo. Upstream
// keeps its copies in a directory the release sync never copies, yet the agent's
// own error messages (synced from upstream) send users to these exact paths. A
// rule that drifts from the code it unlocks would fail nothing else, so these
// tests tie each rule to the strings in the agent that it has to satisfy.

func readPermissionRuleFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestCameraRulesGrantWhatTheAgentChecks(t *testing.T) {
	agent := readPermissionRuleFile(t, "device_activity_linux.go")
	rule := readPermissionRuleFile(t, "scripts/udev/70-sysmon-camera.rules")
	tmpfiles := readPermissionRuleFile(t, "scripts/tmpfiles/sysmon-camera.conf")

	// What the agent probes, writes and names in its error message.
	for _, want := range []string{
		`"../drivers_probe"`,
		`"authorized"`,
		`"bInterfaceClass")) != "0e"`,
		"sysmon-camera group (scripts/udev/70-sysmon-camera.rules)",
	} {
		if !strings.Contains(agent, want) {
			t.Errorf("device_activity_linux.go no longer contains %q; recheck scripts/udev/70-sysmon-camera.rules and scripts/tmpfiles/sysmon-camera.conf", want)
		}
	}
	// The udev rule must reach the same interfaces and grant the same group.
	for _, want := range []string{
		`ATTR{bInterfaceClass}=="0e"`,
		`ACTION=="add|change"`,
		"chgrp sysmon-camera /sys%p/authorized",
	} {
		if !strings.Contains(rule, want) {
			t.Errorf("70-sysmon-camera.rules is missing %q", want)
		}
	}
	// probeWritable gates all camera control on drivers_probe being writable.
	if !strings.Contains(tmpfiles, "/sys/bus/usb/drivers_probe 0220 root sysmon-camera") {
		t.Error("sysmon-camera.conf no longer makes /sys/bus/usb/drivers_probe group-writable for sysmon-camera")
	}
}
