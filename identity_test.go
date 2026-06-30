package main

import "testing"

func TestCleanCPUModelName(t *testing.T) {
	cases := map[string]string{
		"  AMD Ryzen 9 7950X 16-Core Processor  ":  "AMD Ryzen 9 7950X",
		"Intel(R) Core(TM) i7-9750H CPU @ 2.60GHz": "Intel Core i7-9750H",
		"AMD Ryzen 7 5800X 8-Core Processor":       "AMD Ryzen 7 5800X",
		"":                                         "",
	}
	for raw, want := range cases {
		if got := cleanCPUModelName(raw); got != want {
			t.Errorf("cleanCPUModelName(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestComposeMemoryName(t *testing.T) {
	cases := []struct {
		memType, speed, channel, want string
	}{
		{"DDR5", "6000 MT/s", "Dual Channel", "DDR5 · 6000 MT/s · Dual Channel"},
		{"DDR4", "3200 MT/s", "", "DDR4 · 3200 MT/s"},
		{"DDR5", "", "", "DDR5"},
		{"", "6000 MT/s", "", "6000 MT/s"},
		{"", "", "", ""},
		{"  DDR5  ", "  6000 MT/s  ", "", "DDR5 · 6000 MT/s"},
	}
	for _, c := range cases {
		if got := composeMemoryName(c.memType, c.speed, c.channel); got != c.want {
			t.Errorf("composeMemoryName(%q,%q,%q) = %q, want %q", c.memType, c.speed, c.channel, got, c.want)
		}
	}
}

func TestSmbiosMemoryTypeLabel(t *testing.T) {
	cases := map[int]string{
		26: "DDR4",
		34: "DDR5",
		24: "DDR3",
		35: "LPDDR5",
		0:  "",
		99: "",
	}
	for code, want := range cases {
		if got := smbiosMemoryTypeLabel(code); got != want {
			t.Errorf("smbiosMemoryTypeLabel(%d) = %q, want %q", code, got, want)
		}
	}
}

func TestMemorySpeedLabel(t *testing.T) {
	cases := map[int]string{
		6000: "6000 MT/s",
		3200: "3200 MT/s",
		0:    "",
		-1:   "",
	}
	for mtps, want := range cases {
		if got := memorySpeedLabel(mtps); got != want {
			t.Errorf("memorySpeedLabel(%d) = %q, want %q", mtps, got, want)
		}
	}
}

func TestChannelLabel(t *testing.T) {
	cases := []struct {
		distinct, populated int
		want                string
	}{
		{1, 1, "Single Channel"},
		{2, 2, "Dual Channel"},
		{3, 4, "Dual Channel"},
		{4, 4, "Quad Channel"},
		{0, 1, "Single Channel"}, // count fallback
		{0, 2, "Dual Channel"},   // count fallback (2-DIMM desktop)
		{0, 4, "Dual Channel"},   // count fallback (4-DIMM consumer board)
		{0, 0, ""},
	}
	for _, c := range cases {
		if got := channelLabel(c.distinct, c.populated); got != c.want {
			t.Errorf("channelLabel(%d,%d) = %q, want %q", c.distinct, c.populated, got, c.want)
		}
	}
}

func TestMemoryChannelKey(t *testing.T) {
	cases := map[string]string{
		"Controller0-ChannelA-DIMM0": "a",
		"ChannelB-DIMM1":             "b",
		"P0 CHANNEL A":               "a",
		"DIMM_A1":                    "a",
		"DIMM B2":                    "b",
		"DIMM0":                      "", // numeric DIMM, no channel letter
		"BANK 0":                     "",
		"":                           "",
	}
	for locator, want := range cases {
		if got := memoryChannelKey(locator); got != want {
			t.Errorf("memoryChannelKey(%q) = %q, want %q", locator, got, want)
		}
	}
}

func TestBuildMemoryName(t *testing.T) {
	cases := []struct {
		name    string
		modules []memoryModuleInfo
		want    string
	}{
		{
			name: "dual channel DDR5 by locator",
			modules: []memoryModuleInfo{
				{CapacityBytes: 16 << 30, SpeedMTps: 6000, SMBIOSType: 34, Locator: "Controller0-ChannelA-DIMM0"},
				{CapacityBytes: 16 << 30, SpeedMTps: 6000, SMBIOSType: 34, Locator: "Controller1-ChannelB-DIMM0"},
			},
			want: "DDR5 · 6000 MT/s · Dual Channel",
		},
		{
			name: "two sticks no channel letters falls back to count",
			modules: []memoryModuleInfo{
				{CapacityBytes: 8 << 30, SpeedMTps: 3200, SMBIOSType: 26, Locator: "DIMM0"},
				{CapacityBytes: 8 << 30, SpeedMTps: 3200, SMBIOSType: 26, Locator: "DIMM1"},
			},
			want: "DDR4 · 3200 MT/s · Dual Channel",
		},
		{
			name: "single populated stick among empty slots",
			modules: []memoryModuleInfo{
				{CapacityBytes: 16 << 30, SpeedMTps: 5600, SMBIOSType: 34, Locator: "DIMM_A1"},
				{CapacityBytes: 0, SpeedMTps: 0, SMBIOSType: 0, Locator: "DIMM_B1"},
			},
			want: "DDR5 · 5600 MT/s · Single Channel",
		},
		{
			name: "unknown type and speed still reports channel",
			modules: []memoryModuleInfo{
				{CapacityBytes: 8 << 30, SpeedMTps: 0, SMBIOSType: 0, Locator: "ChannelA-DIMM0"},
				{CapacityBytes: 8 << 30, SpeedMTps: 0, SMBIOSType: 0, Locator: "ChannelB-DIMM0"},
			},
			want: "Dual Channel",
		},
		{
			name:    "nothing populated",
			modules: []memoryModuleInfo{{CapacityBytes: 0}},
			want:    "",
		},
		{
			name:    "no modules",
			modules: nil,
			want:    "",
		},
		{
			name: "first module missing speed, second reports it",
			modules: []memoryModuleInfo{
				{CapacityBytes: 16 << 30, SpeedMTps: 0, SMBIOSType: 34, Locator: "ChannelA-DIMM0"},
				{CapacityBytes: 16 << 30, SpeedMTps: 6400, SMBIOSType: 34, Locator: "ChannelB-DIMM0"},
			},
			want: "DDR5 · 6400 MT/s · Dual Channel",
		},
	}
	for _, c := range cases {
		if got := buildMemoryName(c.modules); got != c.want {
			t.Errorf("%s: buildMemoryName(...) = %q, want %q", c.name, got, c.want)
		}
	}
}
