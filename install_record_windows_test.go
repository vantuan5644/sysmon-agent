//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

// TestRegistryStringRoundTrip writes and reads a value the way
// syncInstalledVersionRecord does, on a throwaway key under HKCU (HKLM needs
// the service's rights).
func TestRegistryStringRoundTrip(t *testing.T) {
	sub := fmt.Sprintf(`Software\SysmonAgentTest-%d`, os.Getpid())
	if out, err := exec.Command("reg", "add", `HKCU\`+sub, "/v", "InstallLocation", "/t", "REG_SZ", "/d", `C:\Program Files\Sysmon Agent`, "/f").CombinedOutput(); err != nil {
		t.Skipf("reg add: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("reg", "delete", `HKCU\`+sub, "/f").Run() })

	name, _ := syscall.UTF16PtrFromString(sub)
	var key syscall.Handle
	if err := syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, name, 0, syscall.KEY_QUERY_VALUE|syscall.KEY_SET_VALUE, &key); err != nil {
		t.Fatal(err)
	}
	defer syscall.RegCloseKey(key)
	if got := readRegString(key, "InstallLocation"); got != `C:\Program Files\Sysmon Agent` {
		t.Errorf("InstallLocation = %q", got)
	}
	if got := readRegString(key, "DisplayVersion"); got != "" {
		t.Errorf("a missing value read as %q", got)
	}
	if err := writeRegString(key, "DisplayVersion", "0.2.6"); err != nil {
		t.Fatal(err)
	}
	if got := readRegString(key, "DisplayVersion"); got != "0.2.6" {
		t.Errorf("DisplayVersion = %q after writing 0.2.6", got)
	}
}
