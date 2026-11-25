package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Which apps hold the microphone or camera.
//
// The activity reading says the device is in use; DeviceActivity.Apps says who
// is using it, from evidence the device loop already reads:
//
//   - Linux: the running capture streams in the PipeWire graph (pw-dump) or
//     the PulseAudio source outputs, which carry the client's process binary
//     and PID; the owner_pid of a running ALSA capture substream; and the
//     processes holding a /dev/video* handle, from the fd walk that decides
//     camera activity.
//   - Windows: the process of each active Core Audio capture session, and the
//     app each live CapabilityAccessManager record names.
//
// Only a process name leaves the host, never a command line or a path. When
// the only holder found is the sound server itself (PipeWire, PulseAudio,
// JACK own the ALSA device on a desktop), it is reported as that server with
// SoundServer set, rather than guessed at: the client behind it is unknown.

// DeviceApp is one process holding a capture device.
type DeviceApp struct {
	PID  int    `json:"pid,omitempty"`
	Name string `json:"name"`
	// SoundServer marks a holder that only relays capture for its clients,
	// such as PipeWire owning the ALSA device. Reported only when no client
	// could be named.
	SoundServer bool `json:"sound_server,omitempty"`
}

// deviceAppLimit caps the list; the dashboard shows the first name and a count.
const deviceAppLimit = 5

var soundServerNames = map[string]bool{
	"pipewire":       true,
	"pipewire-pulse": true,
	"pulseaudio":     true,
	"wireplumber":    true,
	"jackd":          true,
	"jackdbus":       true,
}

func isSoundServerName(name string) bool {
	return soundServerNames[strings.ToLower(strings.TrimSpace(name))]
}

// mergeDeviceApps combines holder lists into one: deduplicated by name
// (case-insensitive, keeping the lowest PID), sorted by name so the order
// does not change between samples, and capped at deviceAppLimit. Sound-server
// entries are dropped when any real client is named, since the server sits
// between every client and the device anyway.
func mergeDeviceApps(lists ...[]DeviceApp) []DeviceApp {
	byName := map[string]DeviceApp{}
	clients := false
	for _, list := range lists {
		for _, app := range list {
			app.Name = strings.TrimSpace(app.Name)
			if app.Name == "" {
				continue
			}
			if isSoundServerName(app.Name) {
				app.SoundServer = true
			}
			if !app.SoundServer {
				clients = true
			}
			key := strings.ToLower(app.Name)
			if current, ok := byName[key]; ok {
				if current.PID == 0 || (app.PID > 0 && app.PID < current.PID) {
					current.PID = app.PID
				}
				current.SoundServer = current.SoundServer && app.SoundServer
				byName[key] = current
				continue
			}
			byName[key] = app
		}
	}
	apps := make([]DeviceApp, 0, len(byName))
	for _, app := range byName {
		if clients && app.SoundServer {
			continue
		}
		apps = append(apps, app)
	}
	if len(apps) == 0 {
		return nil
	}
	sort.Slice(apps, func(i, j int) bool {
		a, b := strings.ToLower(apps[i].Name), strings.ToLower(apps[j].Name)
		if a != b {
			return a < b
		}
		return apps[i].Name < apps[j].Name
	})
	if len(apps) > deviceAppLimit {
		apps = apps[:deviceAppLimit]
	}
	return apps
}

// withDeviceApps attaches holders to an active reading only: an idle or
// unknown device has nobody to name, and a leftover list would contradict it.
func withDeviceApps(activity DeviceActivity, apps ...[]DeviceApp) DeviceActivity {
	if activity.State != "active" {
		activity.Apps = nil
		return activity
	}
	activity.Apps = mergeDeviceApps(apps...)
	return activity
}

// alsaCapture is what the running ALSA capture substreams say.
type alsaCapture struct {
	found    bool  // at least one capture substream status exists
	complete bool  // every status could be read
	running  bool  // some substream is RUNNING
	owners   []int // owner_pid of each running substream
}

// readALSACapture reads <procRoot>/asound/card*/pcm*c/sub*/status. Direct
// ALSA capture bypasses session servers, so a running substream is evidence of
// use on its own; its owner_pid is the process that opened it.
func readALSACapture(procRoot string) alsaCapture {
	statuses, _ := filepath.Glob(filepath.Join(procRoot, "asound", "card*", "pcm*c", "sub*", "status"))
	result := alsaCapture{found: len(statuses) > 0, complete: len(statuses) > 0}
	for _, path := range statuses {
		data, err := os.ReadFile(path)
		if err != nil {
			result.complete = false
			continue
		}
		running, owner := parseALSAStatus(string(data))
		if running {
			result.running = true
			if owner > 0 {
				result.owners = append(result.owners, owner)
			}
		}
	}
	return result
}

// parseALSAStatus reads one substream status file:
//
//	state: RUNNING
//	owner_pid   : 4242
//	trigger_time: 1234.567
//
// A closed substream reads just "closed".
func parseALSAStatus(text string) (running bool, owner int) {
	running = strings.Contains(text, "RUNNING")
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "owner_pid" {
			continue
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && pid > 0 {
			owner = pid
		}
	}
	return running, owner
}

// procAppName names a process from <procRoot>/<pid>: comm, or the executable's
// base name when comm is empty or is the kernel's 15-character truncation of
// it. Reading exe needs ptrace access, so another user's process keeps comm.
func procAppName(procRoot string, pid int) string {
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	comm := readTrimmedFile(filepath.Join(dir, "comm"))
	if comm != "" && len(comm) < 15 {
		return comm
	}
	if target, err := os.Readlink(filepath.Join(dir, "exe")); err == nil {
		base := strings.TrimSuffix(filepath.Base(target), " (deleted)")
		if base != "" && base != "." && base != "/" && (comm == "" || strings.HasPrefix(base, comm)) {
			return base
		}
	}
	return comm
}

func readTrimmedFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// streamClientApp names the client of a PipeWire node or PulseAudio source
// output from its properties. The process binary comes first because
// application.name is often a library's label ("WEBRTC VoiceEngine" for
// Discord and Chromium) rather than the app's.
func streamClientApp(props map[string]any) (DeviceApp, bool) {
	text := func(key string) string {
		value, ok := props[key]
		if !ok || value == nil {
			return ""
		}
		return strings.TrimSpace(fmt.Sprint(value))
	}
	name := text("application.process.binary")
	if name == "" {
		name = text("application.name")
	}
	if name == "" {
		return DeviceApp{}, false
	}
	pid, _ := strconv.Atoi(text("application.process.id"))
	if pid < 0 {
		pid = 0
	}
	return DeviceApp{PID: pid, Name: name}, true
}

// privacyRecordApp names the app a CapabilityAccessManager record belongs to:
// the image file name without ".exe" for NonPackaged\<path with # for \>, and
// the package name for a packaged app ("Microsoft.WindowsCamera_8wekyb3d8bbwe"
// gives "WindowsCamera").
func privacyRecordApp(key []string) (DeviceApp, bool) {
	switch {
	case len(key) == 2 && strings.EqualFold(key[0], "NonPackaged"):
		name := key[1]
		if i := strings.LastIndex(name, "#"); i >= 0 {
			name = name[i+1:]
		}
		if strings.HasSuffix(strings.ToLower(name), ".exe") {
			name = name[:len(name)-4]
		}
		name = strings.TrimSpace(name)
		return DeviceApp{Name: name}, name != ""
	case len(key) == 1 && !strings.EqualFold(key[0], "NonPackaged"):
		name, _, _ := strings.Cut(key[0], "_")
		if i := strings.LastIndex(name, "."); i >= 0 && i < len(name)-1 {
			name = name[i+1:]
		}
		name = strings.TrimSpace(name)
		return DeviceApp{Name: name}, name != ""
	}
	return DeviceApp{}, false
}

// windowsImageAppName turns a full image path into the name the dashboard
// shows: its file name without ".exe". The path itself never leaves the host.
func windowsImageAppName(path string) string {
	if i := strings.LastIndexAny(path, `\/`); i >= 0 {
		path = path[i+1:]
	}
	if strings.HasSuffix(strings.ToLower(path), ".exe") {
		path = path[:len(path)-4]
	}
	return strings.TrimSpace(path)
}
