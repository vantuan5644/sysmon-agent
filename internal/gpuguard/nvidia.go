package gpuguard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CommandFunc runs a command and returns its standard output. The agent passes one
// that charges the child's CPU to itself (trackedOutput).
type CommandFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

type PowerLimits struct {
	Current, Min, Default, Max float64
}

func parseWatts(field string) (float64, error) {
	field = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(field), "W"))
	return strconv.ParseFloat(strings.TrimSpace(field), 64)
}

// ParsePowerLimits reads one line of
// power.limit,power.min_limit,power.default_limit,power.max_limit.
func ParsePowerLimits(out []byte) (PowerLimits, error) {
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0])
	fields := strings.Split(line, ",")
	if len(fields) != 4 {
		return PowerLimits{}, fmt.Errorf("unexpected power limit output %q", line)
	}
	values := make([]float64, 4)
	for i, field := range fields {
		v, err := parseWatts(field)
		if err != nil || v <= 0 {
			return PowerLimits{}, fmt.Errorf("unexpected power limit field %q", strings.TrimSpace(field))
		}
		values[i] = v
	}
	return PowerLimits{Current: values[0], Min: values[1], Default: values[2], Max: values[3]}, nil
}

// PowerControl lowers the first NVIDIA GPU's power limit to its minimum and
// puts back whatever limit was set before, so a host cap (omarchy's 350 W
// nvidia-power-limit.service) survives an episode.
//
// RecordPath, when set, keeps the limit to put back in a file while it is
// lowered. A restart mid-episode starts a fresh state machine that knows
// nothing of the lowered limit; with the record it can put the limit back
// (Pending, then Restore) instead of leaving the card at the minimum, and
// without one Restore could only fall back to the default, which undoes a
// host cap.
type PowerControl struct {
	Run        CommandFunc
	Binary     string // nvidia-smi
	Index      int
	RecordPath string
	// BootTime, when set, makes a record written before the current boot
	// count for nothing: the driver starts every boot at its default limit,
	// and the host's own cap service runs after that, so an old record could
	// only put back a value someone has since changed.
	BootTime func() time.Time

	mu       sync.Mutex
	previous float64
}

func (p *PowerControl) readRecord() float64 {
	if p.RecordPath == "" {
		return 0
	}
	if p.BootTime != nil {
		info, err := os.Stat(p.RecordPath)
		if err != nil {
			return 0
		}
		if boot := p.BootTime(); !boot.IsZero() && info.ModTime().Before(boot) {
			_ = os.Remove(p.RecordPath)
			return 0
		}
	}
	data, err := os.ReadFile(p.RecordPath)
	if err != nil || len(data) > 64 {
		return 0
	}
	watts, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
	if err != nil || watts <= 0 || watts > 2000 {
		return 0
	}
	return watts
}

func (p *PowerControl) writeRecord(watts float64) error {
	if p.RecordPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p.RecordPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p.RecordPath, []byte(strconv.FormatFloat(watts, 'f', 0, 64)+"\n"), 0o644)
}

// Pending reports whether an earlier run lowered the limit and did not put
// it back.
func (p *PowerControl) Pending() bool { return p.readRecord() > 0 }

func (p *PowerControl) bin() string {
	if p.Binary != "" {
		return p.Binary
	}
	return "nvidia-smi"
}

func (p *PowerControl) Query(ctx context.Context) (PowerLimits, error) {
	out, err := p.Run(ctx, p.bin(), "-i", strconv.Itoa(p.Index),
		"--query-gpu=power.limit,power.min_limit,power.default_limit,power.max_limit", "--format=csv,noheader,nounits")
	if err != nil {
		return PowerLimits{}, fmt.Errorf("query power limit: %w", err)
	}
	return ParsePowerLimits(out)
}

func (p *PowerControl) set(ctx context.Context, watts float64) error {
	_, err := p.Run(ctx, p.bin(), "-i", strconv.Itoa(p.Index), "-pl", strconv.FormatFloat(watts, 'f', 0, 64))
	if err != nil {
		return fmt.Errorf("set power limit %.0f W: %w", watts, err)
	}
	return nil
}

// Plan describes what Limit would do, for a dry run.
func (p *PowerControl) Plan(ctx context.Context) (string, error) {
	limits, err := p.Query(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("power limit %.0f W -> %.0f W", limits.Current, limits.Min), nil
}

func (p *PowerControl) Limit(ctx context.Context) (string, error) {
	limits, err := p.Query(ctx)
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	if p.previous == 0 {
		p.previous = p.readRecord()
	}
	if p.previous == 0 {
		p.previous = limits.Current
	}
	previous := p.previous
	p.mu.Unlock()
	// Record before lowering: a crash between the two must not lose it.
	recordErr := p.writeRecord(previous)
	if err := p.set(ctx, limits.Min); err != nil {
		return "", err
	}
	what := fmt.Sprintf("power limit %.0f W -> %.0f W", previous, limits.Min)
	if recordErr != nil {
		what += fmt.Sprintf(" (not recorded: %v)", recordErr)
	}
	return what, nil
}

func (p *PowerControl) Restore(ctx context.Context) (string, error) {
	p.mu.Lock()
	previous := p.previous
	p.mu.Unlock()
	if previous == 0 {
		previous = p.readRecord()
	}
	if previous == 0 {
		limits, err := p.Query(ctx)
		if err != nil {
			return "", err
		}
		previous = limits.Default
	}
	if err := p.set(ctx, previous); err != nil {
		return "", err
	}
	p.mu.Lock()
	p.previous = 0
	p.mu.Unlock()
	if p.RecordPath != "" {
		if err := os.Remove(p.RecordPath); err != nil && !os.IsNotExist(err) {
			return fmt.Sprintf("power limit restored to %.0f W", previous), fmt.Errorf("remove %s: %w", p.RecordPath, err)
		}
	}
	return fmt.Sprintf("power limit restored to %.0f W", previous), nil
}

type App struct {
	PID  int
	Name string
}

func (a App) String() string { return fmt.Sprintf("%s (%d)", a.Name, a.PID) }

// ParseComputeApps reads nvidia-smi --query-compute-apps=pid,process_name
// --format=csv,noheader output.
func ParseComputeApps(out []byte) []App {
	var apps []App
	for _, line := range strings.Split(string(out), "\n") {
		pidText, name, ok := strings.Cut(strings.TrimSpace(line), ",")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(pidText))
		if err != nil || pid <= 0 {
			continue
		}
		apps = append(apps, App{PID: pid, Name: strings.TrimSpace(name)})
	}
	return apps
}

// ParsePmon reads `nvidia-smi pmon -c 1`: comment lines start with '#', then
// "gpu pid type sm mem enc dec ... command", with "-" for an idle value. It
// keeps compute clients (type C or C+G) and any process whose SM use is at
// least minSM: every desktop program holds a graphics context, and one that
// is not drawing adds no heat.
func ParsePmon(out []byte, minSM float64) []App {
	var apps []App
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		pid, err := strconv.Atoi(fields[1])
		if err != nil || pid <= 0 {
			continue
		}
		sm, smErr := strconv.ParseFloat(fields[3], 64)
		if !strings.Contains(fields[2], "C") && (smErr != nil || sm < minSM) {
			continue
		}
		apps = append(apps, App{PID: pid, Name: fields[len(fields)-1]})
	}
	return apps
}

// sparedNames are never ended: the desktop itself, the monitor, terminals
// and the streaming host, so the machine stays usable and reachable while
// everything else on the GPU is stopped.
var sparedNames = []string{
	// Windows session and shell
	"system", "idle", "registry", "smss", "csrss", "wininit", "winlogon", "services", "lsass",
	"svchost", "dwm", "explorer", "fontdrvhost", "sihost", "ctfmon", "taskhostw",
	"startmenuexperiencehost", "shellexperiencehost", "searchhost", "textinputhost",
	"applicationframehost", "lockapp", "logonui", "nvcontainer", "nvdisplay.container",
	// Linux session and shell
	"systemd", "hyprland", "xwayland", "xorg", "quickshell", "waybar", "mako", "swayosd-server",
	"walker", "gnome-shell", "kwin_wayland", "kwin_x11", "plasmashell", "plasmalogin",
	"hyprlock", "swaybg", "hyprpaper", "sddm", "gdm",
	// the monitor and the tool it acts through
	"sysmon-agent", "sysmon-gpu-guard", "sysmon-gpu-hotspot", "pwsh", "powershell", "conhost",
	"nvidia-smi",
	// terminals
	"wezterm-gui", "wezterm", "windowsterminal", "openconsole", "alacritty", "kitty", "ghostty",
	"foot",
	// streaming hosts: remote access must survive the episode
	"sunshine", "apollo",
}

// Spared reports whether a process name is protected. name may be a path and
// may end in .exe; matching ignores case.
func Spared(name string, extra []string) bool {
	base := strings.ToLower(filepath.Base(strings.ReplaceAll(strings.TrimSpace(name), `\`, "/")))
	base = strings.TrimSuffix(base, ".exe")
	if base == "" || base == "." {
		return true // nothing to identify it by: leave it alone
	}
	for _, list := range [][]string{sparedNames, extra} {
		for _, spared := range list {
			if strings.TrimSuffix(strings.ToLower(strings.TrimSpace(spared)), ".exe") == base {
				return true
			}
		}
	}
	return false
}

// ParseSpareList splits the -gpu-guard-spare flag.
func ParseSpareList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// Targets merges app lists, drops duplicates, protected names and the given
// PIDs (the caller itself), and returns the apps to stop plus the ones spared.
func Targets(lists [][]App, extra []string, skipPIDs ...int) (stop, spared []App) {
	seen := map[int]bool{}
	for _, pid := range skipPIDs {
		seen[pid] = true
	}
	for _, list := range lists {
		for _, app := range list {
			if app.PID <= 4 || seen[app.PID] {
				continue
			}
			seen[app.PID] = true
			if Spared(app.Name, extra) {
				spared = append(spared, app)
				continue
			}
			stop = append(stop, app)
		}
	}
	return stop, spared
}

func AppList(apps []App) string {
	if len(apps) == 0 {
		return "none"
	}
	parts := make([]string, len(apps))
	for i, app := range apps {
		parts[i] = app.String()
	}
	return strings.Join(parts, ", ")
}
