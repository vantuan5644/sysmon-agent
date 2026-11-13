package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The GPU guard's Linux installer and unit exist only in this repo (upstream
// keeps its copies in a directory the release sync never copies), so a change
// on either side would leave the other pointing at stale paths with nothing
// else noticing.

func readGPUGuardDeployFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestGPUGuardInstallerAndUnitAgree(t *testing.T) {
	unit := readGPUGuardDeployFile(t, "scripts/systemd/sysmon-gpu-guard.service")
	installer := readGPUGuardDeployFile(t, "scripts/install-gpu-guard.sh")

	// The unit runs the binary the installer places, reads the mode file the
	// installer writes, and owns the directory the installer's record lives in.
	for _, needle := range []string{
		"ExecStart=/usr/local/bin/sysmon-gpu-guard\n",
		"EnvironmentFile=-/etc/default/sysmon-gpu-guard\n",
		"RuntimeDirectory=sysmon-gpu-guard\n",
		"RuntimeDirectoryPreserve=yes\n",
	} {
		if !strings.Contains(unit, needle) {
			t.Errorf("scripts/systemd/sysmon-gpu-guard.service is missing %q", needle)
		}
	}
	for _, needle := range []string{
		"install_path=/usr/local/bin/sysmon-gpu-guard\n",
		"env_file=/etc/default/sysmon-gpu-guard\n",
		"record=/run/sysmon-gpu-guard/power-limit\n",
		`unit_source="$script_dir/systemd/$unit_name"`,
		"go build -trimpath -o sysmon-gpu-guard ./cmd/gpu-guard",
	} {
		if !strings.Contains(installer, needle) {
			t.Errorf("scripts/install-gpu-guard.sh is missing %q", needle)
		}
	}
}

func TestGPUGuardInstallerDryRunsByDefault(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("the installer only runs on Linux x86_64")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}
	// The installer refuses to install without nvidia-smi; a stub stands in.
	stubs := t.TempDir()
	if err := os.WriteFile(filepath.Join(stubs, "nvidia-smi"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"))
	run := func(args ...string) (string, error) {
		cmd := exec.Command("bash", append([]string{"scripts/install-gpu-guard.sh"}, args...)...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	helper := filepath.Join(t.TempDir(), "sysmon-gpu-guard")
	if err := os.WriteFile(helper, []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--binary", helper},
		{"--binary", helper, "--mode", "act"},
		{"--uninstall"},
	} {
		out, err := run(args...)
		if err != nil {
			t.Fatalf("dry-run %v failed: %v\n%s", args, err, out)
		}
		if !strings.Contains(out, "Dry-run complete") {
			t.Fatalf("dry-run %v did not stop before applying:\n%s", args, out)
		}
	}
	if out, _ := run("--binary", helper, "--mode", "act"); !strings.Contains(out, "SYSMON_GPU_GUARD=act") {
		t.Errorf("--mode act should plan SYSMON_GPU_GUARD=act:\n%s", out)
	}

	if out, err := run("--binary", helper, "--mode", "kill"); err == nil || !strings.Contains(out, "--mode must be warn or act") {
		t.Errorf("an unknown mode should be refused, got err=%v:\n%s", err, out)
	}
	out, err := run("--binary", filepath.Join(t.TempDir(), "missing"))
	if err == nil || !strings.Contains(out, "go build -trimpath -o sysmon-gpu-guard ./cmd/gpu-guard") {
		t.Fatalf("a missing helper should fail with the build command, got err=%v:\n%s", err, out)
	}
}
