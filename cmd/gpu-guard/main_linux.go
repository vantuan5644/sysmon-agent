//go:build linux

// sysmon-gpu-guard is the root half of the GPU overheat guard on Linux. The
// agent runs unprivileged with NoNewPrivileges, so it can neither set the
// power limit nor end another user's process. This helper runs the same
// state machine the Windows agent runs in process (internal/gpuguard),
// carries out its actions, and publishes its state for the agent, which shows
// it on the dashboard and raises the desktop notification
// (docs/plans/gpu-guard.md).
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"sysmon-agent/internal/gpuguard"
)

func envString(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	if value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name))); err == nil {
		return value
	}
	return fallback
}

func main() {
	mode := flag.String("mode", envString("SYSMON_GPU_GUARD", "warn"), "warn (alarms only, actions logged as \"would\") or act (alarms, then lower the power limit, then end GPU apps)")
	warnC := flag.Int("hotspot-warn", envInt("SYSMON_GPU_HOTSPOT_WARN", 0), "hotspot warning in Celsius (70-105); 0 uses 90")
	critC := flag.Int("hotspot-crit", envInt("SYSMON_GPU_HOTSPOT_CRIT", 0), "hotspot critical level in Celsius (80-110); 0 uses 95")
	deltaC := flag.Int("delta-warn", envInt("SYSMON_GPU_DELTA_WARN", 0), "hotspot above core under load, in Celsius (10-40); 0 uses 20")
	spare := flag.String("spare", envString("SYSMON_GPU_GUARD_SPARE", ""), "process names never ended, comma-separated, besides the built-in desktop, terminal, sysmon and streaming list")
	output := flag.String("output", gpuguard.StatePath, "state file the agent reads")
	hotspotPath := flag.String("hotspot-file", "/run/sysmon-gpu-hotspot/readings.json", "readings of the sysmon-gpu-hotspot helper")
	record := flag.String("record", "/run/sysmon-gpu-guard/power-limit", "keeps the limit to put back while it is lowered")
	index := flag.Int("index", 0, "nvidia-smi index of the guarded GPU")
	testFile := flag.String("test-file", envString("SYSMON_GPU_GUARD_TEST_FILE", ""), "testing: a JSON file such as {\"hotspot\":96} whose values replace the readings while it exists")
	flag.Parse()

	cfg := gpuguard.DefaultConfig()
	m, err := gpuguard.ParseMode(*mode)
	if err != nil {
		log.Fatal(err)
	}
	if m == gpuguard.ModeOff {
		log.Print("gpu guard: off")
		return
	}
	cfg.Mode = m
	if *warnC != 0 {
		cfg.HotspotWarnC = float64(*warnC)
	}
	if *critC != 0 {
		cfg.HotspotCritC = float64(*critC)
	}
	if *deltaC != 0 {
		cfg.DeltaWarnC = float64(*deltaC)
	}
	if err := cfg.Validate(); err != nil {
		log.Fatal(err)
	}
	if cfg.Mode == gpuguard.ModeAct && os.Geteuid() != 0 {
		log.Fatal("act mode sets the power limit and ends other users' processes: run as root")
	}
	binary, err := exec.LookPath("nvidia-smi")
	if err != nil {
		log.Fatal("nvidia-smi not found")
	}
	if err := os.MkdirAll(filepath.Dir(*output), 0o755); err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	smi := &smiStream{binary: binary, index: *index, every: gpuguard.Interval}
	go smi.Run(ctx)

	act := &actor{
		power: &gpuguard.PowerControl{Run: command, Binary: binary, Index: *index, RecordPath: *record, BootTime: bootTime},
		run:   command,
		spare: gpuguard.ParseSpareList(*spare),
		self:  os.Getpid(),
		smi:   smi,
		proc:  "/proc",
	}
	var src gpuguard.Source = &source{smi: smi, hotspotPath: *hotspotPath}
	if *testFile != "" {
		src = gpuguard.TestSource{Inner: src, Path: filepath.Clean(*testFile)}
		log.Printf("gpu guard: readings may be overridden by %s", *testFile)
	}
	loop := gpuguard.NewLoop(cfg, src, act, log.Printf)
	instance := newInstanceID()
	var lastWriteErr string
	loop.OnStep = func(state gpuguard.State) {
		seq, level, message := act.alarm()
		err := gpuguard.WriteStateFile(*output, gpuguard.StateFile{
			WrittenAt: time.Now().UTC(), Instance: instance, State: state,
			AlarmSeq: seq, AlarmLevel: level, AlarmMessage: message,
		})
		text := ""
		if err != nil {
			text = err.Error()
		}
		if text != lastWriteErr {
			if text != "" {
				log.Printf("gpu guard: write state: %v", err)
			} else {
				log.Printf("gpu guard: state file written again")
			}
			lastWriteErr = text
		}
	}
	// Give nvidia-smi a moment to print its first line, so the first sample
	// is not reported as missing core temperature.
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Second):
	}
	loop.Run(ctx)
}

func newInstanceID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}
