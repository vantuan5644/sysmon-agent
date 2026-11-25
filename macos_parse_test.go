package main

import "testing"

func TestMacCPUUsesInterval(t *testing.T) {
	output := "CPU usage: 1.0% user, 2.0% sys, 97.0% idle\nCPU usage: 20.0% user, 5.5% sys, 74.5% idle\n"
	metric := parseMacCPU(output)
	if !metric.Available || metric.Value != 25.5 {
		t.Fatalf("unexpected CPU: %+v", metric)
	}
	if parseMacCPU("CPU usage: 1% user, 2% sys, 97% idle").Available {
		t.Fatal("since-boot sample must be unavailable")
	}
}

// Captured on a 16 GB M1 Pro under normal use. Total minus free reads 99.6% here.
const macVMStatSample = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                     4137.
Pages active:                                 264092.
Pages inactive:                               261962.
Pages speculative:                              3395.
Pages throttled:                                   0.
Pages wired down:                             134789.
Pages purgeable:                                7480.
"Translation faults":                     1457224170.
Pages copy-on-write:                       115621320.
File-backed pages:                            214002.
Anonymous pages:                              315447.
Pages stored in compressor:                   734069.
Pages occupied by compressor:                 328031.
`

func macVMStat(pageSize, anonymous, purgeable, wired, compressor string) string {
	return "Mach Virtual Memory Statistics: (page size of " + pageSize + " bytes)\n" +
		"Anonymous pages: " + anonymous + ".\n" +
		"Pages purgeable: " + purgeable + ".\n" +
		"Pages wired down: " + wired + ".\n" +
		"Pages occupied by compressor: " + compressor + ".\n"
}

func TestMacMemoryLeavesOutFileCache(t *testing.T) {
	m := parseMacMemory(macVMStatSample, 17179869184)
	want := uint64(315447-7480+134789+328031) * 16384
	if !m.Available || m.UsedBytes != want {
		t.Fatalf("used = %d, want %d: %+v", m.UsedBytes, want, m)
	}
	if m.Percent < 73 || m.Percent > 74 {
		t.Fatalf("percent = %v, want about 73.5", m.Percent)
	}
}

func TestMacMemoryPageSizes(t *testing.T) {
	for _, input := range []struct {
		pageSize string
		used     uint64
	}{
		{"4096", 8 * 4096},
		{"16384", 8 * 16384},
	} {
		m := parseMacMemory(macVMStat(input.pageSize, "4", "1", "2", "3"), 1<<20)
		if !m.Available || m.UsedBytes != input.used {
			t.Fatalf("page size %s: unexpected memory: %+v", input.pageSize, m)
		}
	}
}

func TestMacMemoryPurgeableAboveAnonymousDoesNotWrap(t *testing.T) {
	m := parseMacMemory(macVMStat("4096", "1", "3", "2", "3"), 65536)
	if !m.Available || m.UsedBytes != 5*4096 {
		t.Fatalf("unexpected memory: %+v", m)
	}
}

func TestMacMemoryRejectsBadCounters(t *testing.T) {
	for name, text := range map[string]string{
		"no page size":   "Anonymous pages: 4.\nPages purgeable: 1.\nPages wired down: 2.\nPages occupied by compressor: 3.\n",
		"missing row":    "page size of 4096 bytes\nAnonymous pages: 4.\nPages wired down: 2.\n",
		"repeated row":   "page size of 4096 bytes\nAnonymous pages: 4.\nAnonymous pages: 4.\nPages purgeable: 1.\nPages wired down: 2.\n",
		"invalid row":    macVMStat("4096", "invalid", "1", "2", "3"),
		"row over total": macVMStat("4096", "9999", "1", "2", "3"),
		"sum over total": macVMStat("4096", "10", "0", "10", "10"),
	} {
		if m := parseMacMemory(text, 65536); m.Available {
			t.Fatalf("%s: accepted: %+v", name, m)
		}
	}
}

func TestMacNetworkLinkRowsOnly(t *testing.T) {
	text := "Name Mtu Network Address Ipkts Ierrs Ibytes Opkts Oerrs Obytes Coll\nen0 1500 <Link#4> aa:bb 10 0 1000 12 0 2000 0\nen0 1500 192.0.2 192.0.2.2 10 - 1000 12 - 2000 -\nlo0 16384 <Link#1> loopback 1 0 44 1 0 44 0\n"
	n, err := parseMacNetwork(text)
	if err != nil || len(n) != 1 || n["en0"].rx != 1000 || n["en0"].tx != 2000 {
		t.Fatalf("unexpected counters: %v %v", n, err)
	}
	if _, err := parseMacNetwork("unrecognized output"); err == nil {
		t.Fatal("missing byte columns accepted")
	}
}

// A hand-written `ioreg -r -d 1 -w 0 -c IOAccelerator` reply in the shape an
// Apple silicon Mac gives: one AGX accelerator, its PerformanceStatistics on
// one line, and nested values that must not split the dictionary.
const macIORegAGXFixture = `+-o AGXAcceleratorG13X  <class AGXAcceleratorG13X, id 0x1000004b2, registered, matched, active, busy 0 (0 ms), retain 49>
    {
      "IOClass" = "AGXAcceleratorG13X"
      "CFBundleIdentifier" = "com.apple.AGXG13X"
      "IOMatchedAtBoot" = Yes
      "model" = "Apple M1 Pro"
      "gpu-core-count" = 16
      "AGXParameterBufferMaxSize" = 1073741824
      "PerformanceStatistics" = {"In use system memory (driver)"=0,"Alloc system memory"=1610612736,"Tiler Utilization %"=31,"recoveryCount"=0,"lastRecoveryTime"=0,"Renderer Utilization %"=44,"TiledSceneBytes"=2359296,"SplitSceneCount"=0,"Allocated PB Size"=2621440,"In use system memory"=583925760,"Device Utilization %"=47}
      "IOReportLegend" = ({"IOReportGroupName"="GPU Stats","IOReportChannels"=((5764607523034234880,1,"GPU Busy"),(5764607523034234881,1,"GPU Idle"))})
      "AGXTraceCodeVersion" = "3.38.10"
    }
`

func TestMacGPUReadsAppleAcceleratorStatistics(t *testing.T) {
	const memsize = 16 << 30
	set := parseMacGPU(macIORegAGXFixture, memsize)
	if !set.Available || set.Error != "" || len(set.Devices) != 1 {
		t.Fatalf("set = %+v", set)
	}
	gpu := set.Devices[0]
	if gpu.Name != "Apple M1 Pro" {
		t.Fatalf("name = %q", gpu.Name)
	}
	if !gpu.Usage.Available || gpu.Usage.Value != 47 || gpu.Usage.Unit != "%" {
		t.Fatalf("usage = %+v", gpu.Usage)
	}
	if !gpu.Memory.Available || gpu.Memory.UsedBytes != 583925760 || gpu.Memory.TotalBytes != memsize {
		t.Fatalf("memory = %+v", gpu.Memory)
	}
	if len(gpu.Engines) != 2 || gpu.Engines[0].Name != "Renderer" || gpu.Engines[0].Usage.Value != 44 ||
		gpu.Engines[1].Name != "Tiler" || gpu.Engines[1].Usage.Value != 31 {
		t.Fatalf("engines = %+v", gpu.Engines)
	}
	for name, metric := range map[string]NumberMetric{"power": gpu.Power, "temperature": gpu.Temperature, "hotspot": gpu.HotspotTemperature, "vram": gpu.VRAMTemperature} {
		if metric.Available || metric.Error == "" {
			t.Errorf("%s must stay unavailable with a reason: %+v", name, metric)
		}
	}
}

func TestMacGPUDegradesPerField(t *testing.T) {
	// No model: an AGX class is still the Apple GPU. Only the allocation is
	// reported, and the busy share is missing.
	output := "+-o AGXAcceleratorG14G  <class AGXAcceleratorG14G, id 0x1, registered>\n" +
		"  {\n    \"PerformanceStatistics\" = {\"Alloc system memory\"=2048,\"Tiler Utilization %\"=150}\n  }\n" +
		"+-o IntelAccelerator  <class IntelAccelerator, id 0x2, registered>\n  {\n    \"IOClass\" = \"IntelAccelerator\"\n  }\n"
	set := parseMacGPU(output, 0)
	if !set.Available || len(set.Devices) != 2 {
		t.Fatalf("set = %+v", set)
	}
	apple, other := set.Devices[0], set.Devices[1]
	if apple.Name != "Apple GPU" || apple.Usage.Available || apple.Usage.Error == "" {
		t.Fatalf("apple = %+v", apple)
	}
	if apple.Memory.Available || apple.Memory.Error == "" {
		t.Fatalf("memory without hw.memsize must be unavailable: %+v", apple.Memory)
	}
	if len(apple.Engines) != 1 || apple.Engines[0].Usage.Value != 100 {
		t.Fatalf("engine percent must clamp: %+v", apple.Engines)
	}
	if other.Name != "IntelAccelerator" || other.Usage.Available || other.Memory.Available {
		t.Fatalf("accelerator without statistics = %+v", other)
	}

	if set := parseMacGPU("", 1); set.Available || set.Error == "" {
		t.Fatalf("no accelerator must be an error: %+v", set)
	}
}

func TestMacIORegDictKeepsNestedValuesWhole(t *testing.T) {
	got := parseMacIORegDict(`{"a"=1,"b,c"="x=y,z","d"=({"e"=1,"f"=2}),"g"=<0a0b>}`)
	want := map[string]string{"a": "1", "b,c": `"x=y,z"`, "d": `({"e"=1,"f"=2})`, "g": "<0a0b>"}
	if len(got) != len(want) {
		t.Fatalf("dict = %v", got)
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%q = %q, want %q", key, got[key], value)
		}
	}
	if parseMacIORegDict("12") != nil {
		t.Fatal("a scalar is not a dictionary")
	}
}
