//go:build linux

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"sysmon-agent/internal/gpuhotspot"
)

func main() {
	output := flag.String("output", "/run/sysmon-gpu-hotspot/readings.json", "atomic snapshot output path")
	interval := flag.Duration("interval", 2*time.Second, "sensor sampling interval (500ms to 1m)")
	once := flag.Bool("once", false, "read once and print JSON to stdout without writing a file")
	flag.Parse()
	if os.Geteuid() != 0 {
		log.Fatal("GPU hotspot register access requires root")
	}
	if *interval < 500*time.Millisecond || *interval > time.Minute {
		log.Fatal("interval must be between 500ms and 1m")
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(stop)
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		devices, err := gpuhotspot.ReadDevices("/sys", "/dev/mem")
		snapshot := gpuhotspot.Snapshot{Timestamp: time.Now().UTC(), Devices: devices}
		if err != nil {
			snapshot.Error = err.Error()
		}
		data, err := json.Marshal(snapshot)
		if err != nil {
			log.Fatal(err)
		}
		if *once {
			fmt.Println(string(data))
			return
		}
		if err := publish(*output, data); err != nil {
			log.Printf("publish hotspot readings: %v", err)
		}
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
	}
}

func publish(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".readings-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0644); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
