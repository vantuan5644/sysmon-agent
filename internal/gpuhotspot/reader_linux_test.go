//go:build linux

package gpuhotspot

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestReadOnlyRegisterAndDeviceAllowlist(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "bus", "pci", "devices", "0000:01:00.0")
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"vendor": "0x10de\n", "device": "0x2204\n", "resource": fmt.Sprintf("%x %x 200\n", 4096, 4096+hotspotOffset+3)} {
		if err := os.WriteFile(filepath.Join(path, name), []byte(value), 0644); err != nil {
			t.Fatal(err)
		}
	}
	memory := filepath.Join(root, "memory")
	data := make([]byte, 4096+hotspotOffset+4)
	binary.LittleEndian.PutUint32(data[4096+hotspotOffset:], 82<<8)
	if err := os.WriteFile(memory, data, 0444); err != nil {
		t.Fatal(err)
	}
	got, err := ReadDevices(root, memory)
	if err != nil || len(got) != 1 || got[0].Celsius == nil || *got[0].Celsius != 82 {
		t.Fatalf("read: %+v, %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(path, "device"), []byte("0x9999"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err = ReadDevices(root, memory)
	if err != nil || len(got) != 0 {
		t.Fatalf("unsupported GPU read: %+v, %v", got, err)
	}
}

func TestBARBounds(t *testing.T) {
	for _, resource := range []string{"", "0 ffffff 200", "1000 1003 200", "1000 ffffff 100", "ffffffffffffffff ffffffffffffffff 200", "1000 0000 200"} {
		if _, err := registerAddress(resource); err == nil {
			t.Fatalf("accepted %q", resource)
		}
	}
}
