package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
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

func TestRaplRuleOpensTheCounterTheCollectorReads(t *testing.T) {
	collector := readPermissionRuleFile(t, "collector_linux.go")
	rule := readPermissionRuleFile(t, "scripts/udev/99-powercap-rapl.rules")

	if !strings.Contains(collector, `filepath.Join(dir, "energy_uj")`) {
		t.Error("collector_linux.go no longer reads energy_uj; recheck scripts/udev/99-powercap-rapl.rules")
	}
	for _, want := range []string{
		`SUBSYSTEM=="powercap"`,
		`ACTION=="add|change"`,
		"chmod 0444 /sys/class/powercap/intel-rapl:*/energy_uj",
	} {
		if !strings.Contains(rule, want) {
			t.Errorf("99-powercap-rapl.rules is missing %q", want)
		}
	}
}

// TestScriptPathsNamedInSourceExist catches the next upstream message that
// points at an internal-only file: every scripts/... path named in shipped Go
// source must exist here, or users following the message find nothing.
func TestScriptPathsNamedInSourceExist(t *testing.T) {
	ref := regexp.MustCompile(`scripts/[A-Za-z0-9_./-]+\.(?:rules|conf|service|sh)`)
	var found []string
	err := filepath.WalkDir(".", func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name != "." && strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		for _, path := range ref.FindAllString(readPermissionRuleFile(t, name), -1) {
			found = append(found, path)
			if _, err := os.Stat(path); err != nil {
				t.Errorf("%s names %s, which this repo does not ship", name, path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Guard the scan itself: these two references are known to exist today.
	for _, want := range []string{"scripts/udev/70-sysmon-camera.rules", "scripts/udev/99-powercap-rapl.rules"} {
		if !slices.Contains(found, want) {
			t.Errorf("scan did not find %s; the pattern or the source changed", want)
		}
	}
}
