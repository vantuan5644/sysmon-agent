//go:build linux

package main

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"sysmon-agent/internal/gpuguard"
)

// smiStream keeps one `nvidia-smi ... -lms` running and holds its newest
// line. A fresh nvidia-smi every two seconds costs far more than one that
// stays loaded and prints on its own timer (measured in WSL: one line a
// second, flushed through the pipe as it is printed).
type smiStream struct {
	binary string
	index  int
	every  time.Duration

	mu      sync.Mutex
	latest  gpuguard.SMIReading
	at      time.Time
	err     error
	childID int
}

func (s *smiStream) Run(ctx context.Context) {
	for ctx.Err() == nil {
		err := s.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		s.err = fmt.Errorf("nvidia-smi stopped: %v", err)
		s.childID = 0
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func (s *smiStream) runOnce(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, s.binary, "-i", strconv.Itoa(s.index), gpuguard.SMIQuery,
		"--format=csv,noheader,nounits", "-lms", strconv.Itoa(int(s.every/time.Millisecond)))
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	s.mu.Lock()
	s.childID = cmd.Process.Pid
	s.mu.Unlock()
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 4096), 4096)
	for scanner.Scan() {
		reading, parseErr := gpuguard.ParseSMILine(scanner.Text())
		s.mu.Lock()
		if parseErr != nil {
			s.err = parseErr
		} else {
			s.latest, s.at, s.err = reading, time.Now(), nil
		}
		s.mu.Unlock()
	}
	waitErr := cmd.Wait()
	if waitErr == nil {
		waitErr = scanner.Err()
	}
	if waitErr == nil {
		waitErr = fmt.Errorf("exited")
	}
	return waitErr
}

// Latest is the newest reading, or an error once nvidia-smi has gone quiet
// for three intervals.
func (s *smiStream) Latest(now time.Time) (gpuguard.SMIReading, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.at.IsZero() {
		if s.err != nil {
			return gpuguard.SMIReading{}, s.err
		}
		return gpuguard.SMIReading{}, fmt.Errorf("nvidia-smi has not reported yet")
	}
	if age := now.Sub(s.at); age > 3*s.every+time.Second {
		if s.err != nil {
			return s.latest, fmt.Errorf("nvidia-smi silent for %s: %v", age.Round(time.Second), s.err)
		}
		return s.latest, fmt.Errorf("nvidia-smi silent for %s", age.Round(time.Second))
	}
	return s.latest, nil
}

// ChildPID is the running nvidia-smi, which the guard must never end.
func (s *smiStream) ChildPID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.childID
}
