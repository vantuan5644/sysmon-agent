package gpuguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStateFileRoundTripAndChecks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if _, err := ReadStateFile(path, t0); !os.IsNotExist(err) {
		t.Fatalf("missing file: %v", err)
	}
	want := StateFile{WrittenAt: t0, State: State{Mode: ModeAct, Level: LevelCritical, Message: "GPU hotspot 97°C, critical at 95°C"}, AlarmSeq: 3, AlarmLevel: LevelCritical, AlarmMessage: "hot"}
	if err := WriteStateFile(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadStateFile(path, t0.Add(3*time.Second))
	if err != nil || got.AlarmSeq != 3 || got.State.Level != LevelCritical || got.State.Message != want.State.Message {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := ReadStateFile(path, t0.Add(StateFreshFor+time.Second)); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale file accepted: %v", err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat(" ", StateMaxBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadStateFile(path, t0); err == nil || !strings.Contains(err.Error(), "oversized") {
		t.Fatalf("oversized file accepted: %v", err)
	}
}
