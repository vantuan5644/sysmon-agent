package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"sysmon-agent/internal/gpuguard"
)

func TestParseGPUGuardOptions(t *testing.T) {
	opts, err := parseGPUGuardOptions("", 0, 0, 0, " Blender.exe, obs64 ", "")
	if err != nil {
		t.Fatal(err)
	}
	cfg := opts.Config
	if cfg.Mode != gpuguard.ModeWarn || cfg.HotspotWarnC != 90 || cfg.HotspotCritC != 95 || cfg.DeltaWarnC != 20 {
		t.Fatalf("defaults %+v", cfg)
	}
	if strings.Join(opts.Spare, "|") != "Blender.exe|obs64" {
		t.Fatalf("spare %q", opts.Spare)
	}
	opts, err = parseGPUGuardOptions("ACT", 85, 92, 20, "", "C:\\t.json")
	if err != nil || opts.Config.Mode != gpuguard.ModeAct || opts.Config.HotspotWarnC != 85 || opts.Config.HotspotCritC != 92 || opts.Config.DeltaWarnC != 20 || opts.TestFile != "C:\\t.json" {
		t.Fatalf("%+v %v", opts, err)
	}
	for _, bad := range []struct {
		mode              string
		warn, crit, delta int
	}{
		{"kill", 0, 0, 0},
		{"act", 96, 95, 0}, // critical below warn
		{"act", 0, 120, 0},
		{"act", 0, 0, 5},
	} {
		if _, err := parseGPUGuardOptions(bad.mode, bad.warn, bad.crit, bad.delta, "", ""); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

type stubGPUGuard struct{ state *gpuguard.State }

func (*stubGPUGuard) Run(context.Context)      {}
func (g *stubGPUGuard) State() *gpuguard.State { return g.state }

func TestSnapshotsCarryTheGuardState(t *testing.T) {
	s := newSampler(&fakeLaneCollector{hostname: "test-host"}, 0, 0)
	if s.GPUGuardState() != nil {
		t.Fatal("state without a guard")
	}
	guard := &stubGPUGuard{state: &gpuguard.State{Mode: gpuguard.ModeWarn, Level: gpuguard.LevelCritical, Message: "GPU hotspot 96°C, critical at 95°C"}}
	s.SetGPUGuard(guard)
	s.publishWhole(Metrics{Timestamp: time.Now().UTC()})
	got, err := s.Collect(context.Background())
	if err != nil || got.GPUGuard == nil || got.GPUGuard.Level != gpuguard.LevelCritical {
		t.Fatalf("snapshot guard %+v, %v", got.GPUGuard, err)
	}
	if s.GPUGuardState().Message != guard.state.Message {
		t.Fatal("GPUGuardState did not read the guard")
	}
}

func TestGuardFaultsReachTheIssueLists(t *testing.T) {
	shared := make([]string, 1, 4)
	shared[0] = "cpu power unavailable"
	for _, tc := range []struct {
		state         gpuguard.State
		issues, notes []string
	}{
		{gpuguard.State{Source: gpuguard.SourceHotspot}, nil, nil},
		{gpuguard.State{Source: gpuguard.SourceNone, Error: "nvidia-smi not found"}, []string{"gpu guard: nvidia-smi not found"}, nil},
		{gpuguard.State{Source: gpuguard.SourceNone}, []string{"gpu guard: no GPU temperature"}, nil},
		{gpuguard.State{Source: gpuguard.SourceCore, Error: "hotspot file missing"}, nil, []string{"gpu guard judges the core temperature: hotspot file missing"}},
		{gpuguard.State{Source: gpuguard.SourceHotspot, ActionError: "nvidia-smi -pl: Insufficient Permissions"}, []string{"gpu guard action failed: nvidia-smi -pl: Insufficient Permissions"}, nil},
	} {
		m := Metrics{CollectionErrors: shared}
		state := tc.state
		attachGPUGuard(&m, &stubGPUGuard{state: &state})
		if got, want := strings.Join(m.CollectionErrors[1:], "|"), strings.Join(tc.issues, "|"); got != want {
			t.Fatalf("%+v: issues %q, want %q", tc.state, got, want)
		}
		if got, want := strings.Join(m.CollectionNotes, "|"), strings.Join(tc.notes, "|"); got != want {
			t.Fatalf("%+v: notes %q, want %q", tc.state, got, want)
		}
		// Appending must not write into the caller's backing array.
		if extended := shared[:2]; extended[1] != "" {
			t.Fatalf("wrote %q into the shared slice", extended[1])
		}
	}
	m := Metrics{}
	attachGPUGuard(&m, nil)
	if m.GPUGuard != nil || m.CollectionErrors != nil {
		t.Fatal("a missing guard changed the snapshot")
	}
}
