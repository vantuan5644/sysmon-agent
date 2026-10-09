//go:build windows

package main

import (
	"errors"
	"strings"
	"testing"
)

func TestMediaSessionResult(t *testing.T) {
	for _, tc := range []struct {
		name, output, wantError string
		wantKey                 bool
	}{
		{"accepted", `{"handled":true,"applied":true}`, "", false},
		{"legacy player", `{"handled":false,"applied":false}`, "", true},
		{"rejected", `{"handled":true,"applied":false,"error":"player rejected"}`, "player rejected", false},
		{"timed out", `{"handled":true,"applied":false,"error":"timed out"}`, "timed out", false},
		{"invalid output", `invalid`, "parse Windows media session result", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keyCalled := false
			err := applyMediaSessionResult([]byte(tc.output), func() error { keyCalled = true; return nil })
			if keyCalled != tc.wantKey {
				t.Fatalf("keyboard fallback=%v, want %v", keyCalled, tc.wantKey)
			}
			if tc.wantError == "" && err != nil || tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError)) {
				t.Fatalf("error=%v, want %q", err, tc.wantError)
			}
		})
	}
	keyError := errors.New("input unavailable")
	if err := applyMediaSessionResult([]byte(`{"handled":false}`), func() error { return keyError }); !errors.Is(err, keyError) {
		t.Fatalf("keyboard failure lost: %v", err)
	}
}
