//go:build linux

package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"sysmon-agent/internal/gpuguard"
)

type recordedAlarm struct {
	level   gpuguard.Level
	message string
}

func newTestGuardReader(t *testing.T) (*linuxGPUGuardReader, *[]recordedAlarm) {
	t.Helper()
	var alarms []recordedAlarm
	r := &linuxGPUGuardReader{
		path: filepath.Join(t.TempDir(), "state.json"),
		logf: func(string, ...any) {},
		notify: func(_ context.Context, level gpuguard.Level, message string) error {
			alarms = append(alarms, recordedAlarm{level, message})
			return nil
		},
	}
	return r, &alarms
}

func writeGuardState(t *testing.T, r *linuxGPUGuardReader, at time.Time, instance string, seq uint64, level gpuguard.Level, message string) {
	t.Helper()
	err := gpuguard.WriteStateFile(r.path, gpuguard.StateFile{
		WrittenAt: at, Instance: instance, AlarmSeq: seq, AlarmLevel: level, AlarmMessage: message,
		State: gpuguard.State{Mode: gpuguard.ModeAct, Level: level, Message: message},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLinuxGuardReaderRaisesEachAlarmOnce(t *testing.T) {
	r, alarms := newTestGuardReader(t)
	now := time.Now()

	// No helper: nothing published, nothing raised.
	r.poll(context.Background(), now)
	if r.State() != nil || len(*alarms) != 0 {
		t.Fatalf("state %+v alarms %v", r.State(), *alarms)
	}

	// The agent starts while the helper has already raised two alarms and
	// the card is fine: those are old news.
	writeGuardState(t, r, now, "a", 2, gpuguard.LevelOK, "")
	r.poll(context.Background(), now)
	if len(*alarms) != 0 || r.State() == nil || r.State().Level != gpuguard.LevelOK {
		t.Fatalf("old alarms raised: %v", *alarms)
	}

	// A new alarm is raised once, however often it is read.
	writeGuardState(t, r, now, "a", 3, gpuguard.LevelCritical, "GPU hotspot 97°C")
	r.poll(context.Background(), now)
	r.poll(context.Background(), now)
	if len(*alarms) != 1 || (*alarms)[0].level != gpuguard.LevelCritical {
		t.Fatalf("alarms %v", *alarms)
	}

	// The helper restarts and counts from zero: its first alarm still counts.
	writeGuardState(t, r, now, "b", 1, gpuguard.LevelWarn, "GPU hotspot 91°C")
	r.poll(context.Background(), now)
	if len(*alarms) != 2 || (*alarms)[1].message != "GPU hotspot 91°C" {
		t.Fatalf("alarm after a helper restart lost: %v", *alarms)
	}

	// A helper that stopped writing is reported, not trusted.
	r.poll(context.Background(), now.Add(gpuguard.StateFreshFor+time.Second))
	if state := r.State(); state == nil || state.Level != gpuguard.LevelUnknown || state.Error == "" {
		t.Fatalf("stale state: %+v", state)
	}
}

func TestLinuxGuardReaderRaisesAnEpisodeInProgressAtStart(t *testing.T) {
	r, alarms := newTestGuardReader(t)
	now := time.Now()
	writeGuardState(t, r, now, "a", 4, gpuguard.LevelCritical, "GPU hotspot 98°C")
	r.poll(context.Background(), now)
	if len(*alarms) != 1 || (*alarms)[0].message != "GPU hotspot 98°C" {
		t.Fatalf("alarms %v", *alarms)
	}
	r.poll(context.Background(), now)
	if len(*alarms) != 1 {
		t.Fatalf("raised twice: %v", *alarms)
	}
}
