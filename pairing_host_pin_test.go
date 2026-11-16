package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

type fakeHostPINDisplay struct {
	ready  bool
	why    string
	err    error
	titles []string
	bodies []string
}

func (f *fakeHostPINDisplay) Ready() (bool, string) { return f.ready, f.why }

func (f *fakeHostPINDisplay) Show(title, body string) error {
	if f.err != nil {
		return f.err
	}
	f.titles = append(f.titles, title)
	f.bodies = append(f.bodies, body)
	return nil
}

type hostPINFixture struct {
	dir     string
	store   *pairingStore
	display *fakeHostPINDisplay
	logs    []string
}

func newHostPINFixture(t *testing.T) *hostPINFixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pairing")
	store, err := newPairingStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return &hostPINFixture{dir: dir, store: store, display: &fakeHostPINDisplay{ready: true}}
}

func (f *hostPINFixture) run() bool {
	return maybeShowHostPIN(hostPINInput{
		Store:       f.store,
		Dir:         f.dir,
		FromDefault: true,
		Command:     "sysmon-agent -pair -settings /etc/sysmon/settings.json",
		Display:     f.display,
		Logf:        func(format string, args ...any) { f.logs = append(f.logs, fmt.Sprintf(format, args...)) },
	})
}

var shownPIN = regexp.MustCompile(`PIN: (\d{3}) (\d{3})`)

func TestHostPINShowsAWorkingPINOnce(t *testing.T) {
	f := newHostPINFixture(t)
	if !f.run() {
		t.Fatalf("no PIN shown; logs %v", f.logs)
	}
	if len(f.display.bodies) != 1 {
		t.Fatalf("shown %d times, want once", len(f.display.bodies))
	}
	body := f.display.bodies[0]
	m := shownPIN.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no PIN in %q", body)
	}
	if !strings.Contains(body, "sysmon-agent -pair -settings") || !strings.Contains(body, "10 minutes") {
		t.Errorf("body lacks the -pair command or the time limit: %q", body)
	}
	for _, line := range f.logs {
		if strings.Contains(line, m[1]+m[2]) || strings.Contains(line, m[1]+" "+m[2]) {
			t.Errorf("the PIN reached the log: %q", line)
		}
	}
	if _, _, _, err := f.store.Pair(m[1]+m[2], "phone", pairedKindBrowser); err != nil {
		t.Fatalf("the shown PIN does not pair: %v", err)
	}
}

func TestHostPINIsShownOnlyOnce(t *testing.T) {
	f := newHostPINFixture(t)
	if !f.run() {
		t.Fatalf("no PIN shown; logs %v", f.logs)
	}
	if _, err := os.Stat(filepath.Join(f.dir, hostPINMarkerName)); err != nil {
		t.Fatalf("no marker written: %v", err)
	}
	// A later start with nothing paired and the window gone (it expired).
	restart := func() bool {
		t.Helper()
		if err := os.Remove(filepath.Join(f.dir, pairingWindowFile)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		store, err := newPairingStore(f.dir)
		if err != nil {
			t.Fatal(err)
		}
		f.store = store
		return f.run()
	}
	if restart() {
		t.Error("showed a second PIN after the marker was written")
	}
	if err := os.Remove(filepath.Join(f.dir, hostPINMarkerName)); err != nil {
		t.Fatal(err)
	}
	if !restart() {
		t.Error("without the marker the restart showed nothing, so the marker was not what stopped it")
	}
}

func TestHostPINStaysQuiet(t *testing.T) {
	cases := map[string]func(*hostPINFixture) hostPINInput{
		"pairing chosen explicitly": func(f *hostPINFixture) hostPINInput {
			return hostPINInput{Store: f.store, Dir: f.dir, FromDefault: false, Display: f.display}
		},
		"memory-only store": func(f *hostPINFixture) hostPINInput {
			return hostPINInput{Store: f.store, Dir: "", FromDefault: true, Display: f.display}
		},
		"a device is paired already": func(f *hostPINFixture) hostPINInput {
			pin, _, err := f.store.OpenWindow()
			if err != nil {
				panic(err)
			}
			if _, _, _, err := f.store.Pair(pin, "phone", pairedKindBrowser); err != nil {
				panic(err)
			}
			return hostPINInput{Store: f.store, Dir: f.dir, FromDefault: true, Display: f.display}
		},
		"a pairing window is open": func(f *hostPINFixture) hostPINInput {
			if _, _, err := f.store.OpenWindow(); err != nil {
				panic(err)
			}
			return hostPINInput{Store: f.store, Dir: f.dir, FromDefault: true, Display: f.display}
		},
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			f := newHostPINFixture(t)
			if maybeShowHostPIN(input(f)) || len(f.display.bodies) != 0 {
				t.Error("showed a PIN")
			}
		})
	}
}

func TestHostPINWithoutAScreenOnlyLogsTheCommand(t *testing.T) {
	f := newHostPINFixture(t)
	f.display.ready, f.display.why = false, "no desktop session"
	if f.run() {
		t.Fatal("reported a PIN shown with no screen")
	}
	if f.store.WindowOpen() {
		t.Error("opened a pairing window nobody can see")
	}
	if len(f.logs) != 1 || !strings.Contains(f.logs[0], "no desktop session") || !strings.Contains(f.logs[0], "-pair -settings") {
		t.Errorf("logs = %v, want one line with the reason and the -pair command", f.logs)
	}
	if _, err := os.Stat(filepath.Join(f.dir, hostPINMarkerName)); err == nil {
		t.Error("wrote the marker without showing anything; a later start with a screen would skip it")
	}
}

func TestHostPINThatFailsToShowIsTriedAgainNextStart(t *testing.T) {
	f := newHostPINFixture(t)
	f.display.err = errors.New("WTSSendMessageW: access denied")
	if f.run() {
		t.Fatal("reported a PIN shown although Show failed")
	}
	if _, err := os.Stat(filepath.Join(f.dir, hostPINMarkerName)); err == nil {
		t.Error("wrote the marker for a PIN nobody saw")
	}
}

func TestHostPairCommandQuotesForTheHostShell(t *testing.T) {
	if got := posixQuoteArg("/usr/local/bin/sysmon-agent"); got != "/usr/local/bin/sysmon-agent" {
		t.Errorf("plain path quoted: %q", got)
	}
	if got := posixQuoteArg("/home/a b/it's"); got != `'/home/a b/it'\''s'` {
		t.Errorf("posix quoting = %q", got)
	}
	if got := windowsQuoteArg(`C:\Program Files\Sysmon Agent\sysmon-agent.exe`); got != `"C:\Program Files\Sysmon Agent\sysmon-agent.exe"` {
		t.Errorf("windows quoting = %q", got)
	}
	command := hostPairCommand("/usr/local/bin/sysmon-agent", "settings.json")
	if !strings.Contains(command, " -pair -settings ") || strings.Contains(command, " settings.json") {
		t.Errorf("command = %q, want -pair with an absolute settings path", command)
	}
}
