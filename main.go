package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

//go:embed static/*
var staticFS embed.FS

// version is the agent build version. It is overridden at release-build time via
// -ldflags="-X main.version=<version>"; "dev" when built without a stamp.
var version = "dev"

func main() {
	defaultBind := envString("SYSMON_BIND", "0.0.0.0")
	defaultPort := envInt("SYSMON_PORT", 9099)

	bind := flag.String("bind", defaultBind, "HTTP bind address")
	port := flag.Int("port", defaultPort, "HTTP listen port")
	tlsEnabled := flag.Bool("tls", envBool("SYSMON_TLS", false), "enable direct TLS")
	certFile := flag.String("cert", envString("SYSMON_CERT", ""), "TLS certificate file path")
	keyFile := flag.String("key", envString("SYSMON_KEY", ""), "TLS private key file path")
	settingsPath := flag.String("settings", envString("SYSMON_SETTINGS", ""), "optional JSON file for persisted dashboard settings")
	fastMS := flag.Int("fast-ms", envInt("SYSMON_FAST_MS", 0), "resident sampler fast-lane interval in ms for CPU/RAM (min 100); 0 uses the default 200ms")
	slowMS := flag.Int("slow-ms", envInt("SYSMON_SLOW_MS", 0), "resident sampler slow-lane interval in ms for power/temps/disk/net/GPU (min 500); 0 uses the default 1500ms")
	refreshMS := flag.Int("refresh-ms", envInt("SYSMON_REFRESH_MS", 0), "dashboard refresh interval in ms (250, 500, 1000, or 2000); 0 keeps the saved/default value")
	cpuWarn := flag.Int("cpu-warn", envInt("SYSMON_CPU_WARN", 0), "CPU utilization warn threshold percent (50-90); 0 keeps the saved/default value")
	memWarn := flag.Int("mem-warn", envInt("SYSMON_MEM_WARN", 0), "memory utilization warn threshold percent (50-90); 0 keeps the saved/default value")
	diskWarn := flag.Int("disk-warn", envInt("SYSMON_DISK_WARN", 0), "disk utilization warn threshold percent (50-90); 0 keeps the saved/default value")
	gpuWarn := flag.Int("gpu-warn", envInt("SYSMON_GPU_WARN", 0), "GPU utilization warn threshold percent (50-90); 0 keeps the saved/default value")
	tempWarn := flag.Int("temp-warn", envInt("SYSMON_TEMP_WARN", 0), "temperature warn threshold in Celsius (50-90); 0 keeps the saved/default value")
	showVersion := flag.Bool("version", false, "print the build version and exit")
	selfCheck := flag.Bool("self-check", envBool("SYSMON_SELF_CHECK", false), "run in-process endpoint checks and exit")
	waitHealth := flag.Bool("wait-health", envBool("SYSMON_WAIT_HEALTH", false), "wait until the configured /healthz endpoint responds and exit")
	waitHealthTimeout := flag.Duration("wait-health-timeout", envDuration("SYSMON_WAIT_HEALTH_TIMEOUT", 10*time.Second), "maximum time to wait with -wait-health")
	waitReady := flag.Bool("wait-ready", envBool("SYSMON_WAIT_READY", false), "wait until the configured /readyz endpoint responds and exit")
	waitReadyTimeout := flag.Duration("wait-ready-timeout", envDuration("SYSMON_WAIT_READY_TIMEOUT", 15*time.Second), "maximum time to wait with -wait-ready")
	flag.Parse()

	if *showVersion {
		fmt.Printf("sysmon-agent %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
		return
	}

	listen := listenConfig{
		bind:       *bind,
		port:       *port,
		tlsEnabled: *tlsEnabled,
		certFile:   *certFile,
		keyFile:    *keyFile,
	}
	if *waitHealth {
		if err := waitForConfiguredHealth(listen, *waitHealthTimeout); err != nil {
			log.Fatalf("health check failed: %v", err)
		}
		log.Printf("health check ok: %s", healthCheckURL(listen))
		return
	}
	if *waitReady {
		if err := waitForConfiguredReady(listen, *waitReadyTimeout); err != nil {
			log.Fatalf("readiness check failed: %v", err)
		}
		log.Printf("readiness check ok: %s", readinessCheckURL(listen))
		return
	}

	collector := NewSystemCollector()
	// The resident sampler keeps a warm in-memory snapshot (fast lane: CPU/RAM;
	// slow lane: everything else) and exposes an SSE stream, so the HTTP layer
	// serves metrics instantly instead of spawning a collection per request. It
	// implements both MetricsCollector and metricsStreamer, so passing it to the
	// handler enables the /api/stream route automatically.
	metricsSampler := newSampler(collector, time.Duration(*fastMS)*time.Millisecond, time.Duration(*slowMS)*time.Millisecond)
	state, err := NewRuntimeState(*settingsPath)
	if err != nil {
		log.Fatalf("settings: %v", err)
	}
	hostUpdate := hostSettingsUpdate(*refreshMS, *cpuWarn, *memWarn, *diskWarn, *gpuWarn, *tempWarn)
	if !hostSettingsUpdateEmpty(hostUpdate) {
		if _, err := state.UpdateSettings(hostUpdate); err != nil {
			log.Fatalf("host settings: %v", err)
		}
	}
	handler, err := newHTTPHandlerWithState(metricsSampler, staticFS, state)
	if err != nil {
		log.Fatalf("static assets: %v", err)
	}
	if *selfCheck {
		// The sampler is intentionally left unstarted here: its Collect() falls
		// back to a direct platform collection when there is no warm snapshot yet,
		// so the in-process checks exercise the real collector without background
		// goroutines.
		if err := runSelfCheck(handler); err != nil {
			log.Fatalf("self-check failed: %v", err)
		}
		log.Println("self-check ok")
		return
	}

	if err := listen.validate(); err != nil {
		log.Fatal(err)
	}

	metricsSampler.Start()
	defer metricsSampler.Stop()

	serve := func(stop <-chan struct{}, ready func()) error {
		return serveAgent(listen, handler, stop, ready)
	}

	// On Windows, when launched by the Service Control Manager, run as a real
	// service (report status, handle STOP/SHUTDOWN). When run from a console -
	// or on any other OS - runAsService reports handled=false and we fall
	// through to the signal-driven console path below.
	if handled, err := runAsService(serviceName, serve); handled {
		if err != nil {
			log.Fatalf("service error: %v", err)
		}
		return
	}

	stop := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		close(stop)
	}()

	if err := serve(stop, nil); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// serviceName is the Windows service name registered by install-windows.ps1. It
// is also passed to the SCM dispatcher and control-handler registration on
// Windows (ignored on other platforms).
const serviceName = "SysmonAgent"

// serviceRunFunc runs the agent until stop is closed, invoking ready once the
// listener is bound. It is shared by the console path and the Windows service
// control handler so both drive the same graceful shutdown.
type serviceRunFunc func(stop <-chan struct{}, ready func()) error

// serveAgent starts the HTTP server, calls ready (if set) once it is listening,
// then blocks until stop is closed or the server errors, performing a graceful
// shutdown on stop.
func serveAgent(listen listenConfig, handler http.Handler, stop <-chan struct{}, ready func()) error {
	server := &http.Server{
		Addr:              listen.addr(),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		err := serveConfiguredHTTP(server, listen, net.Listen, log.Printf)
		if err != nil && err != http.ErrServerClosed {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	if ready != nil {
		ready()
	}

	select {
	case err := <-serveErr:
		return err
	case <-stop:
	}

	log.Println("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown error: %w", err)
	}
	return nil
}

// hostSettingsUpdate builds a settings update from host-level CLI flags / env
// vars. Each value is applied only when non-zero (0 means "unset, keep the
// saved/default value"). These are the dashboard controls deliberately moved
// off the touch UI and onto the host: refresh interval and warn thresholds.
func hostSettingsUpdate(refreshMS, cpuWarn, memWarn, diskWarn, gpuWarn, tempWarn int) DashboardSettingsUpdate {
	update := DashboardSettingsUpdate{}
	if refreshMS != 0 {
		v := refreshMS
		update.RefreshMS = &v
	}
	thresholds := DashboardThresholdsUpdate{}
	if cpuWarn != 0 {
		v := cpuWarn
		thresholds.CPUWarn = &v
	}
	if memWarn != 0 {
		v := memWarn
		thresholds.MemoryWarn = &v
	}
	if diskWarn != 0 {
		v := diskWarn
		thresholds.DiskWarn = &v
	}
	if gpuWarn != 0 {
		v := gpuWarn
		thresholds.GPUWarn = &v
	}
	if tempWarn != 0 {
		v := tempWarn
		thresholds.TempWarnC = &v
	}
	if thresholds != (DashboardThresholdsUpdate{}) {
		update.Thresholds = &thresholds
	}
	return update
}

func hostSettingsUpdateEmpty(update DashboardSettingsUpdate) bool {
	return update.RefreshMS == nil && update.Thresholds == nil
}

func envString(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envBool(name string, fallback bool) bool {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ignoring invalid %s=%q\n", name, value)
		return fallback
	}
	return parsed
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		fmt.Fprintf(os.Stderr, "ignoring invalid %s=%q\n", name, value)
		return fallback
	}
	return parsed
}

func envInt(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		fmt.Fprintf(os.Stderr, "ignoring invalid %s=%q\n", name, value)
		return fallback
	}
	return parsed
}
