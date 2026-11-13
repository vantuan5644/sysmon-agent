package gpuguard

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// The Linux root helper (cmd/gpu-guard) publishes its state here and the
// unprivileged agent reads it, the same bounded-file pattern as the hotspot
// helper's readings.json.
const (
	StatePath      = "/run/sysmon-gpu-guard/state.json"
	StateMaxBytes  = 16 * 1024
	StateFreshFor  = 10 * time.Second
	stateFileFloor = -2 * time.Second // clock skew allowance
)

// StateFile is the helper's state plus its alarm counter. AlarmSeq goes up by
// one for every host alarm the guard raised, so the agent, which shows the
// desktop notification, raises each one once however often it reads.
// Instance is random per helper process: a restarted helper counts from zero
// again, and a new Instance tells the agent so.
type StateFile struct {
	WrittenAt    time.Time `json:"written_at"`
	Instance     string    `json:"instance"`
	State        State     `json:"state"`
	AlarmSeq     uint64    `json:"alarm_seq"`
	AlarmLevel   Level     `json:"alarm_level,omitempty"`
	AlarmMessage string    `json:"alarm_message,omitempty"`
}

// WriteStateFile replaces path atomically with a world-readable copy.
func WriteStateFile(path string, file StateFile) error {
	data, err := json.Marshal(file)
	if err != nil {
		return err
	}
	if len(data) > StateMaxBytes {
		return fmt.Errorf("GPU guard state is %d bytes, over %d", len(data), StateMaxBytes)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ReadStateFile reads and checks the helper's file. A missing file returns an
// error satisfying os.IsNotExist, which the agent treats as "no guard here".
func ReadStateFile(path string, now time.Time) (StateFile, error) {
	var file StateFile
	f, err := os.Open(path)
	if err != nil {
		return file, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, StateMaxBytes+1))
	if err != nil || len(data) > StateMaxBytes {
		return file, fmt.Errorf("GPU guard state file unreadable or oversized")
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return file, fmt.Errorf("invalid GPU guard state file: %w", err)
	}
	age := now.Sub(file.WrittenAt)
	if file.WrittenAt.IsZero() || age < stateFileFloor || age > StateFreshFor {
		return file, fmt.Errorf("GPU guard helper state is stale (written %s)", file.WrittenAt.Format(time.RFC3339))
	}
	return file, nil
}
