package gpuhotspot

import "testing"

func TestPCIBusID(t *testing.T) {
	for _, raw := range []string{"00000000:01:00.0", "0000:01:00.0"} {
		got, err := NormalizePCIBusID(raw)
		if err != nil || got != "0000:01:00.0" {
			t.Fatalf("normalize %q: %q, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"", "../resource", "0000:100:00.0", "0000:01:20.0", "0000:01:00.8", "-1:01:00.0"} {
		if _, err := NormalizePCIBusID(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}

func TestDecode(t *testing.T) {
	got, err := Decode(0x000052a1)
	if err != nil || got != 82 {
		t.Fatalf("decode: %v, %v", got, err)
	}
	for _, raw := range []uint32{0, 0xffffffff, 150 << 8, 255 << 8} {
		if _, err := Decode(raw); err == nil {
			t.Fatalf("accepted register %x", raw)
		}
	}
}
