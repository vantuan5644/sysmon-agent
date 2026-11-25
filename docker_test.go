package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDocker is an Engine API double on a unix socket in a temp directory. It
// records every request so tests can prove the monitor only ever GETs.
type fakeDocker struct {
	mu        sync.Mutex
	list      []map[string]any
	stats     map[string]map[string]any // by full container id
	methods   []string
	paths     []string
	listCalls atomic.Int32
	status    int // list endpoint status, 0 = 200
	socket    string
}

func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// sun_path is 104 bytes on macOS and 108 on Linux.
	if len(filepath.Join(dir, "docker.sock")) < 100 {
		return dir
	}
	short, err := os.MkdirTemp("", "sd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })
	return short
}

func startFakeDocker(t *testing.T) *fakeDocker {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets are not used for Docker on Windows")
	}
	f := &fakeDocker{stats: map[string]map[string]any{}}
	f.socket = filepath.Join(shortSocketDir(t), "docker.sock")
	ln, err := net.Listen("unix", f.socket)
	if err != nil {
		t.Skipf("unix socket listener unavailable: %v", err)
	}
	server := &http.Server{Handler: http.HandlerFunc(f.serve)}
	go server.Serve(ln)
	t.Cleanup(func() { server.Close() })
	return f
}

func (f *fakeDocker) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.methods = append(f.methods, r.Method)
	f.paths = append(f.paths, r.URL.RequestURI())
	list, status := f.list, f.status
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/containers/json":
		f.listCalls.Add(1)
		if status != 0 {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"message":"engine says no"}`)
			return
		}
		json.NewEncoder(w).Encode(list)
	case strings.HasPrefix(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/stats"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/containers/"), "/stats")
		f.mu.Lock()
		stats, ok := f.stats[id]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, `{"message":"No such container: %s"}`, id)
			return
		}
		json.NewEncoder(w).Encode(stats)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeDocker) setStats(id string, total, system, usage, limit, inactive uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Shaped like a real one-shot answer: precpu_stats is empty.
	f.stats[id] = map[string]any{
		"read":         "2026-11-25T10:00:00Z",
		"preread":      "0001-01-01T00:00:00Z",
		"cpu_stats":    map[string]any{"cpu_usage": map[string]any{"total_usage": total}, "system_cpu_usage": system, "online_cpus": 8},
		"precpu_stats": map[string]any{"cpu_usage": map[string]any{"total_usage": 0}},
		"memory_stats": map[string]any{"usage": usage, "limit": limit, "stats": map[string]any{"inactive_file": inactive}},
	}
}

func containerEntry(id, name, image, state, status, health string) map[string]any {
	entry := map[string]any{
		"Id": id, "Names": []string{"/" + name}, "Image": image,
		"State": state, "Status": status, "Created": 1700000000,
	}
	if health != "" {
		entry["Health"] = map[string]any{"Status": health, "FailingStreak": 0}
	}
	return entry
}

func testDockerMonitor(t *testing.T, socket string) *dockerMonitor {
	t.Helper()
	m, err := newDockerMonitor("unix://" + socket)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestDockerCollectListsContainersAndComputesCPUFromOwnDeltas(t *testing.T) {
	f := startFakeDocker(t)
	idWeb := strings.Repeat("a", 64)
	idDB := strings.Repeat("b", 64)
	idOld := strings.Repeat("c", 64)
	f.list = []map[string]any{
		containerEntry(idWeb, "web", "nginx:latest", "running", "Up 6 days (healthy)", "healthy"),
		containerEntry(idOld, "old", "busybox", "exited", "Exited (0) 2 hours ago", ""),
		containerEntry(idDB, "db", "postgres:17", "running", "Up About a minute (health: starting)", ""),
	}
	f.setStats(idWeb, 1_000, 100_000, 300<<20, 1<<30, 100<<20)
	f.setStats(idDB, 5_000, 100_000, 50<<20, 2<<30, 0)

	clock := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	m := testDockerMonitor(t, f.socket)
	m.now = func() time.Time { return clock }

	set, warming := m.collect(context.Background())
	if !set.Available || set.Error != "" {
		t.Fatalf("first pass unavailable: %+v", set)
	}
	if !warming {
		t.Fatal("first pass should report CPU warming up")
	}
	if set.Total != 3 || set.Running != 2 || len(set.Containers) != 3 {
		t.Fatalf("counts: total=%d running=%d rows=%d", set.Total, set.Running, len(set.Containers))
	}
	// Running first, then by name.
	if got := []string{set.Containers[0].Name, set.Containers[1].Name, set.Containers[2].Name}; strings.Join(got, ",") != "db,web,old" {
		t.Fatalf("order = %v", got)
	}
	web := set.Containers[1]
	if web.ID != idWeb[:12] || web.Image != "nginx:latest" || web.Health != "healthy" || web.State != "running" {
		t.Fatalf("web row = %+v", web)
	}
	if web.UptimeSeconds == nil || *web.UptimeSeconds != 6*86400 {
		t.Fatalf("web uptime = %v", web.UptimeSeconds)
	}
	if web.CPU.Available || !strings.Contains(web.CPU.Error, "warming up") {
		t.Fatalf("first-pass CPU should be warming up: %+v", web.CPU)
	}
	if !web.Memory.Available || web.Memory.UsedBytes != 200<<20 || web.Memory.TotalBytes != 1<<30 {
		t.Fatalf("web memory = %+v (want usage less inactive_file against the limit)", web.Memory)
	}
	db := set.Containers[0]
	if db.Health != "starting" || db.UptimeSeconds == nil || *db.UptimeSeconds != 60 {
		t.Fatalf("db row = %+v", db)
	}
	old := set.Containers[2]
	if old.UptimeSeconds != nil || old.CPU.Available || old.Memory.Available || !strings.Contains(old.CPU.Error, "exited") {
		t.Fatalf("exited row = %+v", old)
	}

	// Second pass 3 s later: CPU is the container's share of host CPU time.
	clock = clock.Add(3 * time.Second)
	f.setStats(idWeb, 1_000+25_000, 100_000+100_000, 300<<20, 1<<30, 100<<20)
	f.setStats(idDB, 5_000+2_000, 100_000+100_000, 50<<20, 2<<30, 0)
	set, warming = m.collect(context.Background())
	if warming {
		t.Fatal("second pass should not be warming")
	}
	web, db = set.Containers[1], set.Containers[0]
	if !web.CPU.Available || web.CPU.Value != 25 || web.CPU.Unit != "%" {
		t.Fatalf("web CPU = %+v, want 25%%", web.CPU)
	}
	if !db.CPU.Available || db.CPU.Value != 2 {
		t.Fatalf("db CPU = %+v, want 2%%", db.CPU)
	}

	// Only GETs, only the two documented endpoints.
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, method := range f.methods {
		if method != http.MethodGet {
			t.Fatalf("request %d used %s %s", i, method, f.paths[i])
		}
		path := f.paths[i]
		if path != "/containers/json?all=1" && !(strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/stats?stream=false&one-shot=true")) {
			t.Fatalf("unexpected request %s", path)
		}
		if strings.Contains(path, idOld) {
			t.Fatalf("stats requested for a stopped container: %s", path)
		}
	}
}

func TestDockerCPUStaleSampleWarmsUpAgain(t *testing.T) {
	start := time.Unix(1000, 0)
	prev := dockerCPUSample{total: 100, system: 1000, at: start}
	cur := dockerCPUSample{total: 200, system: 2000, at: start.Add(dockerCPUSampleMaxAge + time.Second)}
	if got, warming := dockerCPUPercent(prev, cur); got.Available || !warming {
		t.Fatalf("stale previous sample should warm up: %+v %v", got, warming)
	}
	restarted := dockerCPUSample{total: 50, system: 2000, at: start.Add(time.Second)}
	if got, warming := dockerCPUPercent(prev, restarted); got.Available || !warming {
		t.Fatalf("a counter that went back should warm up: %+v %v", got, warming)
	}
	ok := dockerCPUSample{total: 600, system: 2000, at: start.Add(time.Second)}
	if got, warming := dockerCPUPercent(prev, ok); !got.Available || got.Value != 50 || warming {
		t.Fatalf("cpu = %+v %v, want 50%%", got, warming)
	}
}

func TestDockerSkipsStatsWithTooManyRunningContainers(t *testing.T) {
	f := startFakeDocker(t)
	for i := 0; i < dockerMaxStats+dockerMaxRows; i++ {
		id := fmt.Sprintf("%064d", i)
		f.list = append(f.list, containerEntry(id, fmt.Sprintf("c%03d", i), "img", "running", "Up 2 hours", ""))
	}
	m := testDockerMonitor(t, f.socket)
	set, _ := m.collect(context.Background())
	if !set.Available || set.Total != dockerMaxStats+dockerMaxRows || set.Running != set.Total {
		t.Fatalf("set = available %v total %d running %d", set.Available, set.Total, set.Running)
	}
	if len(set.Containers) != dockerMaxRows {
		t.Fatalf("rows = %d, want the cap %d", len(set.Containers), dockerMaxRows)
	}
	if !strings.Contains(set.Note, "skipped") {
		t.Fatalf("note = %q", set.Note)
	}
	if set.Containers[0].CPU.Available || !strings.Contains(set.Containers[0].CPU.Error, "skipped") {
		t.Fatalf("row CPU = %+v", set.Containers[0].CPU)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.paths) != 1 {
		t.Fatalf("made %d requests, want only the list: %v", len(f.paths), f.paths)
	}

	var metrics Metrics
	m.store(set)
	m.Attach(&metrics)
	if len(metrics.CollectionNotes) != 1 || !strings.HasPrefix(metrics.CollectionNotes[0], "docker: ") || len(metrics.CollectionErrors) != 0 {
		t.Fatalf("notes=%v errors=%v", metrics.CollectionNotes, metrics.CollectionErrors)
	}
}

func TestDockerMissingSocketIsAClearIssue(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets")
	}
	socket := filepath.Join(shortSocketDir(t), "absent.sock")
	m := testDockerMonitor(t, socket)

	var before Metrics
	m.Attach(&before)
	if before.Docker == nil || before.Docker.Available || before.Docker.Error != dockerWaitingMessage || len(before.CollectionErrors) != 0 {
		t.Fatalf("before the first pass: %+v errors=%v", before.Docker, before.CollectionErrors)
	}

	set, _ := m.collect(context.Background())
	if set.Available || !strings.Contains(set.Error, "does not exist") || !strings.Contains(set.Error, socket) {
		t.Fatalf("set = %+v", set)
	}
	m.store(set)
	shared := []string{"cpu: something"}
	metrics := Metrics{CollectionErrors: shared[:1:1]}
	m.Attach(&metrics)
	if len(metrics.CollectionErrors) != 2 || !strings.HasPrefix(metrics.CollectionErrors[1], "docker: ") {
		t.Fatalf("errors = %v", metrics.CollectionErrors)
	}
	if b, _ := json.Marshal(metrics.Docker); !strings.Contains(string(b), `"available":false`) {
		t.Fatalf("json = %s", b)
	}
}

func TestDockerPermissionDeniedNamesTheDockerGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores socket permissions")
	}
	f := startFakeDocker(t)
	if err := os.Chmod(f.socket, 0); err != nil {
		t.Skipf("chmod: %v", err)
	}
	m := testDockerMonitor(t, f.socket)
	set, _ := m.collect(context.Background())
	if set.Available || !strings.Contains(set.Error, "permission denied") || !strings.Contains(set.Error, "root-equivalent") {
		t.Fatalf("set = %+v", set)
	}
}

func TestDockerHTTPErrorCarriesTheEngineMessage(t *testing.T) {
	f := startFakeDocker(t)
	f.status = http.StatusInternalServerError
	m := testDockerMonitor(t, f.socket)
	set, _ := m.collect(context.Background())
	if set.Available || !strings.Contains(set.Error, "HTTP 500") || !strings.Contains(set.Error, "engine says no") {
		t.Fatalf("set = %+v", set)
	}
}

func TestDockerRunIsDemandGated(t *testing.T) {
	f := startFakeDocker(t)
	f.list = []map[string]any{}
	m := testDockerMonitor(t, f.socket)
	var demand atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(ctx, demand.Load)
	}()
	time.Sleep(1500 * time.Millisecond)
	if n := f.listCalls.Load(); n != 0 {
		cancel()
		<-done
		t.Fatalf("parked monitor read Docker %d times", n)
	}
	demand.Store(true)
	deadline := time.Now().Add(3 * time.Second)
	for f.listCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if f.listCalls.Load() == 0 {
		t.Fatal("monitor never read Docker once there was demand")
	}
	var metrics Metrics
	m.Attach(&metrics)
	if metrics.Docker == nil || !metrics.Docker.Available || metrics.Docker.Total != 0 {
		t.Fatalf("docker = %+v", metrics.Docker)
	}
}

func TestParseDockerHost(t *testing.T) {
	cases := []struct {
		in, goos, want string
		wantErr        string
	}{
		{"", "linux", "unix:///var/run/docker.sock", ""},
		{"unix:///run/user/1000/docker.sock", "linux", "unix:///run/user/1000/docker.sock", ""},
		{"/tmp/d.sock", "linux", "unix:///tmp/d.sock", ""},
		{"tcp://127.0.0.1:2375", "windows", "tcp://127.0.0.1:2375", ""},
		{"tcp://127.0.0.1", "linux", "", "tcp://host:port"},
		{"npipe:////./pipe/docker_engine", "windows", "", "named pipes"},
		{"", "windows", "", "named pipe"},
		{"docker.sock", "linux", "", "must be"},
		{"unix://", "linux", "", "no socket path"},
	}
	for _, tc := range cases {
		got, err := parseDockerHost(tc.in, tc.goos)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("parseDockerHost(%q, %s) err = %v, want %q", tc.in, tc.goos, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got.String() != tc.want {
			t.Errorf("parseDockerHost(%q, %s) = %s, %v; want %s", tc.in, tc.goos, got, err, tc.want)
		}
	}
}

func TestDockerWindowsDefaultIsReportedNotFatal(t *testing.T) {
	m := &dockerMonitor{configErr: "Docker Desktop on Windows listens on a named pipe"}
	var metrics Metrics
	m.Attach(&metrics)
	if metrics.Docker == nil || metrics.Docker.Available || len(metrics.CollectionErrors) != 1 {
		t.Fatalf("docker=%+v errors=%v", metrics.Docker, metrics.CollectionErrors)
	}
}

func TestDockerUptimeAndHealthParsing(t *testing.T) {
	uptimes := map[string]int64{
		"Up Less than a second":       0,
		"Up 5 seconds":                5,
		"Up About a minute (healthy)": 60,
		"Up 12 minutes":               720,
		"Up About an hour":            3600,
		"Up 16 hours (unhealthy)":     16 * 3600,
		"Up 1 day":                    86400,
		"Up 6 days (healthy)":         6 * 86400,
		"Up 2 weeks":                  14 * 86400,
		"Up 3 months":                 90 * 86400,
		"Up 2 years (Paused)":         730 * 86400,
	}
	for status, want := range uptimes {
		got, ok := dockerUptimeSeconds(status)
		if !ok || got != want {
			t.Errorf("dockerUptimeSeconds(%q) = %d, %v; want %d", status, got, ok, want)
		}
	}
	for _, status := range []string{"Exited (0) 2 hours ago", "Created", "Up", "Up many moons"} {
		if _, ok := dockerUptimeSeconds(status); ok {
			t.Errorf("dockerUptimeSeconds(%q) parsed", status)
		}
	}
	health := []struct {
		status, listHealth, want string
	}{
		{"Up 1 hour (healthy)", "", "healthy"},
		{"Up 1 hour (unhealthy)", "", "unhealthy"},
		{"Up 3 seconds (health: starting)", "", "starting"},
		{"Up 2 weeks", "none", ""},
		{"Up 2 weeks", "unhealthy", "unhealthy"},
		{"Up 2 weeks", "", ""},
	}
	for _, tc := range health {
		entry := dockerListEntry{Status: tc.status}
		if tc.listHealth != "" {
			entry.Health = &struct {
				Status string `json:"Status"`
			}{Status: tc.listHealth}
		}
		if got := dockerHealth(entry); got != tc.want {
			t.Errorf("dockerHealth(%q, %q) = %q, want %q", tc.status, tc.listHealth, got, tc.want)
		}
	}
}
