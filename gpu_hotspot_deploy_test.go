package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// The GPU hotspot installer and unit exist only in this repo, so a change to the
// helper or the agent synced from upstream would leave them pointing at stale
// paths or device IDs with nothing else noticing.

const gpuHotspotReadingsPath = "/run/sysmon-gpu-hotspot/readings.json"

func readGPUHotspotDeployFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestGPUHotspotDeployAgreesWithHelperAndAgent(t *testing.T) {
	unit := readGPUHotspotDeployFile(t, "deploy/sysmon-gpu-hotspot.service")
	installer := readGPUHotspotDeployFile(t, "install-gpu-hotspot.sh")

	for name, text := range map[string]string{
		"helper default -output": readGPUHotspotDeployFile(t, "cmd/gpu-hotspot/main_linux.go"),
		"agent reader":           readGPUHotspotDeployFile(t, "gpu_linux.go"),
		"installer":              installer,
	} {
		if !strings.Contains(text, gpuHotspotReadingsPath) {
			t.Errorf("%s no longer uses %s", name, gpuHotspotReadingsPath)
		}
	}
	// The unit owns the readings directory and runs the binary the installer places.
	for _, needle := range []string{
		"RuntimeDirectory=" + filepath.Base(filepath.Dir(gpuHotspotReadingsPath)) + "\n",
		"ExecStart=/usr/local/bin/sysmon-gpu-hotspot ",
		"DeviceAllow=/dev/mem r\n",
	} {
		if !strings.Contains(unit, needle) {
			t.Errorf("deploy/sysmon-gpu-hotspot.service is missing %q", needle)
		}
	}
	for _, needle := range []string{
		"INSTALL_PATH=/usr/local/bin/sysmon-gpu-hotspot\n",
		`UNIT_SOURCE="$SCRIPT_DIR/deploy/$UNIT_NAME"`,
		"go build -trimpath -o sysmon-gpu-hotspot ./cmd/gpu-hotspot",
	} {
		if !strings.Contains(installer, needle) {
			t.Errorf("install-gpu-hotspot.sh is missing %q", needle)
		}
	}
}

func TestGPUHotspotInstallerKnowsEverySupportedDevice(t *testing.T) {
	reader := readGPUHotspotDeployFile(t, "internal/gpuhotspot/reader_linux.go")
	installer := readGPUHotspotDeployFile(t, "install-gpu-hotspot.sh")

	var readerIDs []string
	for _, m := range regexp.MustCompile(`device\) == "0x([0-9a-f]{4})"`).FindAllStringSubmatch(reader, -1) {
		readerIDs = append(readerIDs, m[1])
	}
	m := regexp.MustCompile(`(?m)^SUPPORTED_IDS=\(([^)]*)\)`).FindStringSubmatch(installer)
	if m == nil {
		t.Fatal("install-gpu-hotspot.sh has no SUPPORTED_IDS array")
	}
	installerIDs := strings.Fields(m[1])
	slices.Sort(readerIDs)
	slices.Sort(installerIDs)
	if len(readerIDs) == 0 || !slices.Equal(readerIDs, installerIDs) {
		t.Fatalf("installer SUPPORTED_IDS %v != helper supportedDevice %v", installerIDs, readerIDs)
	}
}

func TestGPUHotspotInstallerDryRunsByDefault(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("the installer only runs on Linux x86_64")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}
	helper := filepath.Join(t.TempDir(), "sysmon-gpu-hotspot")
	if err := os.WriteFile(helper, []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--binary", helper},
		{"--uninstall"},
	} {
		out, err := exec.Command("bash", append([]string{"install-gpu-hotspot.sh"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("dry-run %v failed: %v\n%s", args, err, out)
		}
		if !strings.Contains(string(out), "Dry-run complete") {
			t.Fatalf("dry-run %v did not stop before applying:\n%s", args, out)
		}
	}

	out, err := exec.Command("bash", "install-gpu-hotspot.sh", "--binary", filepath.Join(t.TempDir(), "missing")).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "go build -trimpath -o sysmon-gpu-hotspot ./cmd/gpu-hotspot") {
		t.Fatalf("a missing helper should fail with the build command, got err=%v:\n%s", err, out)
	}
}
