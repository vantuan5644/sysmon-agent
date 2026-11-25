package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The Docker container list is opt-in (-docker / SYSMON_DOCKER=1) and
// read-only. The agent speaks the Engine API over the Docker socket with plain
// net/http and issues GET requests only, to exactly two endpoints:
// /containers/json?all=1 for the list, and, for each running container,
// /containers/{id}/stats?stream=false&one-shot=true. It never creates, starts,
// stops, execs into or removes anything; dockerMonitor.get is the only request
// it makes and hardcodes the method.
//
// Reading the socket is still a privilege worth naming: on Linux the socket is
// root:docker 0660, and any process that can open it can ask Docker to start a
// privileged container, so membership in the docker group is root-equivalent.
// Giving the agent's account that access trusts the agent binary as root; the
// GET-only rule above is what this agent does with it, not something the
// socket enforces.
//
// Collection is demand-gated like the slow lane: a pass every
// dockerCollectEvery (15 s) while a dashboard is open or /api/metrics was
// fetched recently, nothing at all otherwise. The list is one request; the
// stats are one request per running container, at most dockerStatsWorkers at
// a time, each bounded by dockerRequestTimeout, and skipped (with a note) for a
// host running more than dockerMaxStats containers.
//
// CPU is computed from two of the agent's own passes rather than Docker's
// precpu_stats: one-shot stats return immediately with precpu empty, where a
// plain stream=false read waits a second or two per container for Docker to
// take its second sample. So a container's first pass reads "warming up" and
// the next pass follows after dockerWarmupRetry instead of the full interval.

const (
	dockerCollectEvery   = 15 * time.Second
	dockerWarmupRetry    = 3 * time.Second
	dockerRequestTimeout = 4 * time.Second
	dockerPassTimeout    = 12 * time.Second
	dockerStatsWorkers   = 4
	dockerMaxStats       = 32
	dockerMaxRows        = 50
	dockerListLimit      = 8 << 20
	dockerStatsLimit     = 1 << 20
	// dockerCPUSampleMaxAge is how old the previous CPU reading may be and
	// still make a rate. An older one (the monitor was parked) would average
	// the CPU over however long nobody watched.
	dockerCPUSampleMaxAge = 3 * dockerCollectEvery
	dockerWaitingMessage  = "waiting for the first Docker read"
)

// DockerSet is the container list published as metrics.docker. Total and
// Running count every container Docker reported; Containers holds at most
// dockerMaxRows of them, running ones first. Note explains an expected gap
// (stats skipped on a host with many containers) and goes to
// collection_notes; Error is a failed read and goes to collection_errors,
// since the feature is opt-in and a host that asked for it wants to know.
type DockerSet struct {
	Available  bool              `json:"available"`
	Endpoint   string            `json:"endpoint,omitempty"`
	Total      int               `json:"total"`
	Running    int               `json:"running"`
	Containers []DockerContainer `json:"containers,omitempty"`
	Note       string            `json:"note,omitempty"`
	UpdatedAt  *time.Time        `json:"updated_at,omitempty"`
	Error      string            `json:"error,omitempty"`
}

// DockerContainer is one container. Status is Docker's own phrase ("Up 6
// days (healthy)", "Exited (0) 2 hours ago"); UptimeSeconds is parsed from it
// for a running container, so it is only as precise as that phrase. Health is
// healthy, unhealthy or starting, and empty without a healthcheck. CPU is the
// share of the whole host (or of Docker Desktop's VM) the container used
// since the previous pass, 0-100 like the process page, not docker stats'
// per-core figure. Memory is usage less inactive file cache, as docker stats
// shows it, against the container's limit (the host's memory when unlimited).
type DockerContainer struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Image         string         `json:"image"`
	State         string         `json:"state"`
	Status        string         `json:"status"`
	Health        string         `json:"health,omitempty"`
	UptimeSeconds *int64         `json:"uptime_seconds,omitempty"`
	CPU           NumberMetric   `json:"cpu_percent"`
	Memory        CapacityMetric `json:"memory"`
}

// dockerEndpoint is where the Engine API listens.
type dockerEndpoint struct {
	network string // "unix" or "tcp"
	address string // socket path or host:port
}

func (e dockerEndpoint) String() string {
	return e.network + "://" + e.address
}

// parseDockerHost reads -docker-host: unix:///path, a bare absolute socket
// path, or tcp://host:port. An empty value is the platform default. Windows
// has no default, because Docker Desktop listens on a named pipe, which the
// standard library cannot dial; the returned error says what to pass instead.
func parseDockerHost(value, goos string) (dockerEndpoint, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultDockerEndpoint(goos)
	}
	switch {
	case strings.HasPrefix(value, "unix://"):
		path := strings.TrimPrefix(value, "unix://")
		if path == "" {
			return dockerEndpoint{}, fmt.Errorf("docker host %q names no socket path", value)
		}
		return dockerEndpoint{network: "unix", address: path}, nil
	case strings.HasPrefix(value, "tcp://"):
		address := strings.TrimSuffix(strings.TrimPrefix(value, "tcp://"), "/")
		host, port, err := net.SplitHostPort(address)
		if err != nil || host == "" || port == "" {
			return dockerEndpoint{}, fmt.Errorf("docker host %q must be tcp://host:port", value)
		}
		return dockerEndpoint{network: "tcp", address: address}, nil
	case strings.HasPrefix(value, "npipe:"):
		return dockerEndpoint{}, fmt.Errorf("docker host %q: named pipes are not supported; expose the engine on tcp://127.0.0.1:2375 and pass that", value)
	case filepath.IsAbs(value) || strings.HasPrefix(value, "/"):
		return dockerEndpoint{network: "unix", address: value}, nil
	}
	return dockerEndpoint{}, fmt.Errorf("docker host %q must be unix:///path, an absolute socket path or tcp://host:port", value)
}

func defaultDockerEndpoint(goos string) (dockerEndpoint, error) {
	switch goos {
	case "windows":
		return dockerEndpoint{}, errors.New("Docker Desktop on Windows listens on a named pipe, which this agent cannot read; expose the engine on tcp://127.0.0.1:2375 and pass -docker-host tcp://127.0.0.1:2375")
	case "darwin":
		// Docker Desktop links /var/run/docker.sock only when allowed to;
		// its own socket lives in the user's home.
		const system = "/var/run/docker.sock"
		if _, err := os.Stat(system); err != nil {
			if home, herr := os.UserHomeDir(); herr == nil {
				user := filepath.Join(home, ".docker", "run", "docker.sock")
				if _, uerr := os.Stat(user); uerr == nil {
					return dockerEndpoint{network: "unix", address: user}, nil
				}
			}
		}
		return dockerEndpoint{network: "unix", address: system}, nil
	}
	return dockerEndpoint{network: "unix", address: "/var/run/docker.sock"}, nil
}

type dockerCPUSample struct {
	total  uint64
	system uint64
	at     time.Time
}

type dockerMonitor struct {
	endpoint  dockerEndpoint
	configErr string
	client    *http.Client
	every     time.Duration
	retry     time.Duration
	now       func() time.Time

	mu  sync.Mutex
	set *DockerSet // newest finished pass, never mutated once stored

	// prev is touched only by the goroutine running passes.
	prev map[string]dockerCPUSample
}

// newDockerMonitor builds the monitor for -docker-host. A host the agent
// cannot use (Windows without -docker-host) is not fatal: the monitor reports
// it as the docker issue instead of starting.
func newDockerMonitor(host string) (*dockerMonitor, error) {
	endpoint, err := parseDockerHost(host, runtime.GOOS)
	m := &dockerMonitor{
		endpoint: endpoint,
		every:    dockerCollectEvery,
		retry:    dockerWarmupRetry,
		now:      time.Now,
		prev:     map[string]dockerCPUSample{},
	}
	if err != nil {
		if strings.TrimSpace(host) != "" {
			return nil, err
		}
		m.configErr = err.Error()
		return m, nil
	}
	m.client = newDockerClient(endpoint)
	return m, nil
}

func newDockerClient(endpoint dockerEndpoint) *http.Client {
	transport := &http.Transport{
		// Never through HTTP(S)_PROXY: the socket is local.
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, endpoint.network, endpoint.address)
		},
		MaxIdleConns:          dockerStatsWorkers,
		MaxIdleConnsPerHost:   dockerStatsWorkers,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: dockerRequestTimeout,
		DisableCompression:    true,
	}
	return &http.Client{Transport: transport, Timeout: dockerRequestTimeout}
}

// Run collects while there is demand, every m.every (m.retry while a running
// container's CPU is still warming up), and parks otherwise.
func (m *dockerMonitor) Run(ctx context.Context, demand func() bool) {
	if m.configErr != "" || m.client == nil {
		<-ctx.Done()
		return
	}
	defer m.client.CloseIdleConnections()
	var last time.Time
	every := m.every
	for {
		if serviceDue(demand, last, every, m.now()) {
			last = m.now()
			set, warming := m.collect(ctx)
			if ctx.Err() != nil {
				return
			}
			m.store(set)
			every = m.every
			if warming {
				every = m.retry
			}
		}
		if sleepCanceled(ctx, serviceWait(last, every, m.now())) {
			return
		}
	}
}

func (m *dockerMonitor) store(set DockerSet) {
	m.mu.Lock()
	m.set = &set
	m.mu.Unlock()
}

// Attach publishes the newest pass. Before the first one finishes the set
// says so without raising an issue; a failed read is an issue and a skipped
// stats pass a note.
func (m *dockerMonitor) Attach(metrics *Metrics) {
	m.mu.Lock()
	set := m.set
	m.mu.Unlock()
	if set == nil {
		if m.configErr != "" {
			metrics.Docker = &DockerSet{Available: false, Error: m.configErr}
			appendIssue(metrics, "docker: "+m.configErr)
			return
		}
		metrics.Docker = &DockerSet{Available: false, Endpoint: m.endpoint.String(), Error: dockerWaitingMessage}
		return
	}
	metrics.Docker = set
	if !set.Available {
		appendIssue(metrics, "docker: "+firstNonEmpty(set.Error, "unavailable"))
	}
	if set.Note != "" {
		appendNote(metrics, "docker: "+set.Note)
	}
}

type dockerListEntry struct {
	ID     string   `json:"Id"`
	Names  []string `json:"Names"`
	Image  string   `json:"Image"`
	State  string   `json:"State"`
	Status string   `json:"Status"`
	// Health is in the list from Engine API 1.52 on; older engines only put
	// it in Status, which dockerHealth falls back to.
	Health *struct {
		Status string `json:"Status"`
	} `json:"Health"`
}

type dockerStatsResponse struct {
	CPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage uint64 `json:"system_cpu_usage"`
	} `json:"cpu_stats"`
	MemoryStats struct {
		Usage uint64 `json:"usage"`
		Limit uint64 `json:"limit"`
		Stats struct {
			InactiveFile      uint64 `json:"inactive_file"`       // cgroup v2
			TotalInactiveFile uint64 `json:"total_inactive_file"` // cgroup v1
		} `json:"stats"`
	} `json:"memory_stats"`
}

// collect runs one pass. warming is true when a running container's CPU had
// no recent previous reading to make a rate from.
func (m *dockerMonitor) collect(parent context.Context) (DockerSet, bool) {
	ctx, cancel := context.WithTimeout(parent, dockerPassTimeout)
	defer cancel()
	endpoint := m.endpoint.String()
	var list []dockerListEntry
	if err := m.get(ctx, "/containers/json?all=1", dockerListLimit, &list); err != nil {
		now := m.now().UTC()
		return DockerSet{Available: false, Endpoint: endpoint, UpdatedAt: &now, Error: err.Error()}, false
	}

	rows := make([]DockerContainer, len(list))
	var running []int
	for i, entry := range list {
		rows[i] = dockerRow(entry)
		if rows[i].State == "running" {
			running = append(running, i)
		}
	}

	note := ""
	warming := false
	if len(running) > dockerMaxStats {
		note = fmt.Sprintf("CPU and memory skipped: %d running containers is more than %d", len(running), dockerMaxStats)
		for _, i := range running {
			rows[i].CPU = unavailableNumber("%", "stats skipped: too many running containers")
			rows[i].Memory = unavailableCapacity("stats skipped: too many running containers")
		}
		m.prev = map[string]dockerCPUSample{}
	} else {
		warming = m.collectStats(ctx, list, rows, running)
	}

	sort.SliceStable(rows, func(a, b int) bool {
		ra, rb := rows[a].State == "running", rows[b].State == "running"
		if ra != rb {
			return ra
		}
		return strings.ToLower(rows[a].Name) < strings.ToLower(rows[b].Name)
	})
	if len(rows) > dockerMaxRows {
		rows = rows[:dockerMaxRows]
	}
	now := m.now().UTC()
	return DockerSet{
		Available:  true,
		Endpoint:   endpoint,
		Total:      len(list),
		Running:    len(running),
		Containers: rows,
		Note:       note,
		UpdatedAt:  &now,
	}, warming
}

// collectStats fills CPU and memory for the running rows, dockerStatsWorkers
// requests at a time.
func (m *dockerMonitor) collectStats(ctx context.Context, list []dockerListEntry, rows []DockerContainer, running []int) bool {
	type result struct {
		index int
		stats dockerStatsResponse
		err   error
		at    time.Time
	}
	results := make([]result, len(running))
	sem := make(chan struct{}, dockerStatsWorkers)
	var wg sync.WaitGroup
	for slot, index := range running {
		wg.Add(1)
		go func(slot, index int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results[slot] = result{index: index, err: ctx.Err()}
				return
			}
			defer func() { <-sem }()
			reqCtx, cancel := context.WithTimeout(ctx, dockerRequestTimeout)
			defer cancel()
			var stats dockerStatsResponse
			path := "/containers/" + url.PathEscape(list[index].ID) + "/stats?stream=false&one-shot=true"
			err := m.get(reqCtx, path, dockerStatsLimit, &stats)
			results[slot] = result{index: index, stats: stats, err: err, at: m.now()}
		}(slot, index)
	}
	wg.Wait()

	warming := false
	next := make(map[string]dockerCPUSample, len(running))
	for _, r := range results {
		row := &rows[r.index]
		id := list[r.index].ID
		if r.err != nil {
			row.CPU = unavailableNumber("%", r.err.Error())
			row.Memory = unavailableCapacity(r.err.Error())
			continue
		}
		cur := dockerCPUSample{total: r.stats.CPUStats.CPUUsage.TotalUsage, system: r.stats.CPUStats.SystemCPUUsage, at: r.at}
		if cur.system > 0 {
			next[id] = cur
		}
		var cpuWarming bool
		row.CPU, cpuWarming = dockerCPUPercent(m.prev[id], cur)
		warming = warming || cpuWarming
		row.Memory = dockerMemory(r.stats)
	}
	m.prev = next
	return warming
}

// dockerCPUPercent is the container's share of all host CPU time between two
// readings. warming is true when there is no usable previous reading yet.
func dockerCPUPercent(prev, cur dockerCPUSample) (NumberMetric, bool) {
	if cur.system == 0 {
		return unavailableNumber("%", "Docker reports no host CPU time for this container"), false
	}
	if prev.at.IsZero() || cur.at.Sub(prev.at) > dockerCPUSampleMaxAge || cur.at.Before(prev.at) {
		return unavailableNumber("%", "CPU is warming up"), true
	}
	if cur.system <= prev.system || cur.total < prev.total {
		// The container restarted (its counter went back) or no host time
		// passed: start over from this reading.
		return unavailableNumber("%", "CPU is warming up"), true
	}
	percent := float64(cur.total-prev.total) / float64(cur.system-prev.system) * 100
	if percent > 100 {
		percent = 100
	}
	return availableNumber(percent, "%"), false
}

func dockerMemory(stats dockerStatsResponse) CapacityMetric {
	mem := stats.MemoryStats
	if mem.Limit == 0 {
		return unavailableCapacity("Docker reports no memory figures for this container")
	}
	cache := mem.Stats.InactiveFile
	if cache == 0 {
		cache = mem.Stats.TotalInactiveFile
	}
	used := mem.Usage
	if cache < used {
		used -= cache
	}
	return availableCapacity(used, mem.Limit)
}

func dockerRow(entry dockerListEntry) DockerContainer {
	id := entry.ID
	if len(id) > 12 {
		id = id[:12]
	}
	name := ""
	if len(entry.Names) > 0 {
		name = strings.TrimPrefix(entry.Names[0], "/")
	}
	image := strings.TrimSpace(entry.Image)
	if strings.HasPrefix(image, "sha256:") && len(image) > len("sha256:")+12 {
		image = image[:len("sha256:")+12]
	}
	state := strings.ToLower(strings.TrimSpace(entry.State))
	row := DockerContainer{
		ID:     id,
		Name:   firstNonEmpty(name, id),
		Image:  image,
		State:  state,
		Status: strings.TrimSpace(entry.Status),
		Health: dockerHealth(entry),
	}
	if state == "running" {
		if seconds, ok := dockerUptimeSeconds(entry.Status); ok {
			row.UptimeSeconds = &seconds
		}
	} else {
		reason := "container is " + firstNonEmpty(state, "not running")
		row.CPU = unavailableNumber("%", reason)
		row.Memory = unavailableCapacity(reason)
	}
	return row
}

func dockerHealth(entry dockerListEntry) string {
	if entry.Health != nil {
		switch status := strings.ToLower(strings.TrimSpace(entry.Health.Status)); status {
		case "healthy", "unhealthy", "starting":
			return status
		}
	}
	status := strings.ToLower(entry.Status)
	switch {
	case strings.Contains(status, "(unhealthy)"):
		return "unhealthy"
	case strings.Contains(status, "(healthy)"):
		return "healthy"
	case strings.Contains(status, "(health: starting)"):
		return "starting"
	}
	return ""
}

// dockerUptimeSeconds reads the duration out of Docker's "Up ..." status,
// which Docker writes with go-units' HumanDuration: "Less than a second",
// "About a minute", "5 minutes", "About an hour", "16 hours", "6 days",
// "2 weeks", "3 months", "2 years".
func dockerUptimeSeconds(status string) (int64, bool) {
	s := strings.TrimSpace(status)
	if !strings.HasPrefix(s, "Up ") {
		return 0, false
	}
	s = strings.TrimSpace(strings.TrimPrefix(s, "Up "))
	if i := strings.Index(s, " ("); i >= 0 {
		s = s[:i]
	}
	switch strings.ToLower(s) {
	case "less than a second":
		return 0, true
	case "about a minute":
		return 60, true
	case "about an hour":
		return 3600, true
	}
	fields := strings.Fields(strings.ToLower(s))
	if len(fields) != 2 {
		return 0, false
	}
	n, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	unit := strings.TrimSuffix(fields[1], "s")
	scale := map[string]int64{
		"second": 1,
		"minute": 60,
		"hour":   3600,
		"day":    86400,
		"week":   7 * 86400,
		"month":  30 * 86400,
		"year":   365 * 86400,
	}[unit]
	if scale == 0 {
		return 0, false
	}
	return n * scale, true
}

// get is the only request the monitor makes: a GET, read to at most limit
// bytes and decoded into out.
func (m *dockerMonitor) get(ctx context.Context, path string, limit int64, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return m.describeError(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return m.describeError(err)
	}
	if int64(len(body)) > limit {
		return fmt.Errorf("Docker API answer to %s is larger than %d bytes", dockerPathLabel(path), limit)
	}
	if resp.StatusCode != http.StatusOK {
		var apiErr struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &apiErr)
		message := firstNonEmpty(apiErr.Message, strings.TrimSpace(string(body)), resp.Status)
		if len(message) > 200 {
			message = message[:200]
		}
		return fmt.Errorf("Docker API %s: HTTP %d: %s", dockerPathLabel(path), resp.StatusCode, message)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("Docker API %s: unreadable answer: %v", dockerPathLabel(path), err)
	}
	return nil
}

func dockerPathLabel(path string) string {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	return path
}

// describeError turns a transport failure into the reason a person can act
// on: the socket is missing, the agent may not open it, or nothing answers.
func (m *dockerMonitor) describeError(err error) error {
	where := m.endpoint.address
	var netErr net.Error
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("Docker socket %s does not exist; is Docker running?", where)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("permission denied on %s: the agent's account needs read access to the Docker socket (the docker group, which is root-equivalent)", where)
	case errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Errorf("nothing answers on %s; is Docker running?", where)
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
		return fmt.Errorf("Docker did not answer on %s within %s", where, dockerRequestTimeout)
	case errors.Is(err, context.Canceled):
		return err
	}
	return fmt.Errorf("Docker API on %s: %v", where, err)
}
