package main

import "testing"

func TestInstallRecordUpdate(t *testing.T) {
	dir := `C:\Program Files\Sysmon Agent`
	cases := []struct {
		name, running, exeDir, location, display, want string
		ok                                             bool
	}{
		{"after a dashboard update", "0.2.6", dir, dir, "0.2.5", "0.2.6", true},
		{"tag with a v", "v0.2.6", dir, dir, "0.2.5", "0.2.6", true},
		{"already right", "0.2.6", dir, dir, "0.2.6", "", false},
		{"location quoted with a trailing slash, other case", "0.2.6", dir, `"c:\program files\sysmon agent\"`, "0.2.5", "0.2.6", true},
		{"a dev build", "dev", dir, dir, "0.2.5", "", false},
		{"no version", "", dir, dir, "0.2.5", "", false},
		{"another copy of the binary", "0.2.6", `C:\Users\me\sysmon`, dir, "0.2.5", "", false},
		{"entry without a location", "0.2.6", dir, "", "0.2.5", "", false},
	}
	for _, tc := range cases {
		got, ok := installRecordUpdate(tc.running, tc.exeDir, tc.location, tc.display)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: got %q, %v; want %q, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}
