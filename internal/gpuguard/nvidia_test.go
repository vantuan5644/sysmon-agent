package gpuguard

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParsePowerLimits(t *testing.T) {
	got, err := ParsePowerLimits([]byte("350.00, 150.00, 450.00, 450.00\r\n"))
	if err != nil || got != (PowerLimits{Current: 350, Min: 150, Default: 450, Max: 450}) {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := ParsePowerLimits([]byte("[N/A], 150.00, 450.00, 450.00")); err == nil {
		t.Fatal("accepted N/A")
	}
}

type fakeSMI struct {
	limit float64
	calls []string
}

func (f *fakeSMI) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	for i, a := range args {
		if a == "-pl" {
			var w float64
			for _, c := range args[i+1] {
				w = w*10 + float64(c-'0')
			}
			f.limit = w
			return nil, nil
		}
	}
	return []byte(strings.TrimSpace(strings.Join([]string{ftoa(f.limit), "150.00", "450.00", "450.00"}, ", "))), nil
}

func ftoa(v float64) string {
	s := ""
	n := int(v)
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s + ".00"
}

func TestPowerControlRestoresTheHostCap(t *testing.T) {
	smi := &fakeSMI{limit: 350}
	p := &PowerControl{Run: smi.run}
	msg, err := p.Limit(context.Background())
	if err != nil || smi.limit != 150 || msg != "power limit 350 W -> 150 W" {
		t.Fatalf("limit: %q %v, now %v W", msg, err, smi.limit)
	}
	// A second limit in the same episode must not record 150 as "previous".
	if _, err := p.Limit(context.Background()); err != nil {
		t.Fatal(err)
	}
	msg, err = p.Restore(context.Background())
	if err != nil || smi.limit != 350 || msg != "power limit restored to 350 W" {
		t.Fatalf("restore: %q %v, now %v W", msg, err, smi.limit)
	}
	// Restore with nothing remembered (agent restarted mid-episode): default.
	smi.limit = 150
	if _, err := p.Restore(context.Background()); err != nil || smi.limit != 450 {
		t.Fatalf("restore without memory: %v, now %v W", err, smi.limit)
	}
}

func TestParseApps(t *testing.T) {
	compute := ParseComputeApps([]byte("4120, C:\\Python\\python.exe\r\n\r\n77, llama-server\nbad line\n"))
	if AppList(compute) != `C:\Python\python.exe (4120), llama-server (77)` {
		t.Fatalf("%v", compute)
	}
	pmon := ParsePmon([]byte("# gpu        pid  type    sm   mem   enc   dec   command\n# Idx          #   C/G     %     %     %     %   name\n    0       2210     G     3     1     -     -   Hyprland\n    0       2400     G     -     -     -     -   chromium\n    0       9001     G    90    40     -     -   Palworld-Win64-\n    0       7000     C     -     -     -     -   llama-server\n    0          -     -     -     -     -     -   -\n"), 1)
	if AppList(pmon) != "Hyprland (2210), Palworld-Win64- (9001), llama-server (7000)" {
		t.Fatalf("%v", pmon)
	}
}

func TestParseSMILine(t *testing.T) {
	got, err := ParseSMILine("NVIDIA GeForce RTX 4090, 00000000:01:00.0, 55, 65.48\n")
	if err != nil || got.Name != "NVIDIA GeForce RTX 4090" || got.PCIBusID != "00000000:01:00.0" || *got.Core != 55 || *got.Power != 65.48 {
		t.Fatalf("%+v %v", got, err)
	}
	got, err = ParseSMILine("Some, Odd Name, 00000000:02:00.0, 61, [N/A]")
	if err != nil || got.Name != "Some, Odd Name" || got.Power != nil || *got.Core != 61 {
		t.Fatalf("comma in name or N/A power: %+v %v", got, err)
	}
	if _, err := ParseSMILine("NVIDIA, 00000000:01:00.0, [Unknown Error], [N/A]"); err == nil {
		t.Fatal("a line with no readings was accepted")
	}
	if _, err := ParseSMILine("No devices were found"); err == nil {
		t.Fatal("an error line was accepted")
	}
}

func TestPowerRecordFromAnEarlierBootIsIgnored(t *testing.T) {
	record := t.TempDir() + "/limit"
	smi := &fakeSMI{limit: 350}
	boot := time.Now().Add(-time.Hour)
	first := &PowerControl{Run: smi.run, RecordPath: record, BootTime: func() time.Time { return boot }}
	if _, err := first.Limit(context.Background()); err != nil || !first.Pending() {
		t.Fatalf("limit: %v", err)
	}
	// A reboot after the record was written: the driver is back at its
	// default and the record must not be put back.
	boot = time.Now().Add(time.Minute)
	second := &PowerControl{Run: smi.run, RecordPath: record, BootTime: func() time.Time { return boot }}
	if second.Pending() {
		t.Fatal("a record from before the boot counted")
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatalf("old record left behind: %v", err)
	}
}

func TestPowerRecordSurvivesARestart(t *testing.T) {
	record := t.TempDir() + "/limit"
	smi := &fakeSMI{limit: 350}
	first := &PowerControl{Run: smi.run, RecordPath: record}
	if _, err := first.Limit(context.Background()); err != nil || !first.Pending() {
		t.Fatalf("limit: %v, pending %v", err, first.Pending())
	}
	// A new process: no memory of the episode, only the record.
	second := &PowerControl{Run: smi.run, RecordPath: record}
	if !second.Pending() {
		t.Fatal("record lost")
	}
	if msg, err := second.Restore(context.Background()); err != nil || smi.limit != 350 || msg != "power limit restored to 350 W" {
		t.Fatalf("restore: %q %v, now %v W", msg, err, smi.limit)
	}
	if second.Pending() {
		t.Fatal("record left behind after restore")
	}
	// Lowering twice across a restart keeps the host cap, not the minimum.
	if _, err := first.Limit(context.Background()); err != nil {
		t.Fatal(err)
	}
	third := &PowerControl{Run: smi.run, RecordPath: record}
	if _, err := third.Limit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := third.Restore(context.Background()); err != nil || smi.limit != 350 {
		t.Fatalf("after two limits: %v, now %v W", err, smi.limit)
	}
}

func TestTargetsSpareTheDesktopAndTheCaller(t *testing.T) {
	stop, spared := Targets([][]App{
		{{PID: 10, Name: `C:\Windows\explorer.exe`}, {PID: 11, Name: "Game.exe"}, {PID: 12, Name: "sysmon-agent.exe"}},
		{{PID: 11, Name: "Game.exe"}, {PID: 13, Name: "python"}, {PID: 14, Name: "Hyprland"}, {PID: 15, Name: "blender"}, {PID: 4, Name: "System"}},
	}, []string{"Blender.exe"}, 13)
	if AppList(stop) != "Game.exe (11)" {
		t.Fatalf("stop %v", stop)
	}
	if AppList(spared) != `C:\Windows\explorer.exe (10), sysmon-agent.exe (12), Hyprland (14), blender (15)` {
		t.Fatalf("spared %v", spared)
	}
	if !Spared("", nil) || Spared("python.exe", nil) {
		t.Fatal("Spared edge cases")
	}
	if got := ParseSpareList(" obs64 , ,steam"); strings.Join(got, "|") != "obs64|steam" {
		t.Fatalf("%v", got)
	}
}
