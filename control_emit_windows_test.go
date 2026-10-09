//go:build windows

package main

import (
	"errors"
	"testing"
	"unsafe"
)

func TestWindowsInputLayout(t *testing.T) {
	var input windowsInput
	wantSize, wantOffset := uintptr(28), uintptr(4)
	if unsafe.Sizeof(uintptr(0)) == 8 {
		wantSize, wantOffset = 40, 8
	}
	if unsafe.Sizeof(input) != wantSize || unsafe.Offsetof(input.Keyboard) != wantOffset {
		t.Fatalf("INPUT ABI: size=%d keyboard offset=%d, want %d and %d",
			unsafe.Sizeof(input), unsafe.Offsetof(input.Keyboard), wantSize, wantOffset)
	}
}

func TestEmitMediaKeyDelivery(t *testing.T) {
	for _, sent := range []uint32{0, 1, 2} {
		t.Run(string(rune('0'+sent)), func(t *testing.T) {
			var calls [][]windowsInput
			err := emitMediaKey(func(inputs []windowsInput) (uint32, error) {
				calls = append(calls, append([]windowsInput(nil), inputs...))
				if len(calls) > 1 {
					return 1, nil
				}
				return sent, errors.New("input blocked")
			})
			if (err == nil) != (sent == 2) {
				t.Fatalf("sent=%d error=%v", sent, err)
			}
			if len(calls[0]) != 2 {
				t.Fatal("media toggle must send one press/release pair")
			}
			for i, input := range calls[0] {
				if input.Type != 1 || input.Keyboard.VirtualKey != 0xB3 || input.Keyboard.ScanCode != 0x22 ||
					input.Keyboard.Flags != uint32(1+2*i) {
					t.Fatalf("incorrect media input: %+v", input)
				}
			}
			if sent == 1 {
				if len(calls) != 2 || len(calls[1]) != 1 || calls[1][0].Keyboard.Flags != 3 {
					t.Fatal("partial delivery must release the key without repeating the toggle")
				}
			} else if len(calls) != 1 {
				t.Fatal("media toggle was sent more than once")
			}
		})
	}
}
