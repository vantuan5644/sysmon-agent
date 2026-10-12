package main

import (
	"errors"
	"reflect"
	"testing"
)

func TestParseGPUEngineInstanceKeepsMultiWordEngineTypes(t *testing.T) {
	cases := []struct {
		name string
		want gpuEngineReading
	}{
		{"pid_9616_luid_0x00000000_0x00017769_phys_0_eng_0_engtype_3D", gpuEngineReading{PID: 9616, LUID: "0x00000000_0x00017769", Engine: "phys_0_eng_0", Type: "3D", Value: 12}},
		{"pid_120_luid_0x00000000_0x0001A5A1_phys_0_eng_3_engtype_Compute 0", gpuEngineReading{PID: 120, LUID: "0x00000000_0x0001a5a1", Engine: "phys_0_eng_3", Type: "Compute 0", Value: 12}},
		{"pid_7_luid_0x00000000_0x00017769_phys_0_eng_12_engtype_ofa_0", gpuEngineReading{PID: 7, LUID: "0x00000000_0x00017769", Engine: "phys_0_eng_12", Type: "ofa_0", Value: 12}},
	}
	for _, tc := range cases {
		got, ok := parseGPUEngineInstance(tc.name, 12)
		if !ok || got != tc.want {
			t.Fatalf("%s = %+v (ok=%t), want %+v", tc.name, got, ok, tc.want)
		}
	}
	for _, bad := range []string{"", "luid_0x0_phys_0_eng_0_engtype_3D", "pid_x_luid_0x0_phys_0_eng_0_engtype_3D", "pid_1_luid_0x0_phys_0_eng_0"} {
		if _, ok := parseGPUEngineInstance(bad, 1); ok {
			t.Fatalf("%q parsed", bad)
		}
	}
	if _, ok := parseGPUEngineInstance("pid_1_luid_0x0_phys_0_eng_0_engtype_3D", -1); ok {
		t.Fatal("a negative utilization parsed")
	}
}

// TestSummarizeGPUEnginesFollowsTaskManager: an engine's load is the sum over
// the processes using it, an engine type's is its busiest engine, and a
// process is labelled with its own busiest engine.
func TestSummarizeGPUEnginesFollowsTaskManager(t *testing.T) {
	const dgpu, igpu = "0x00000000_0x00017769", "0x00000000_0x0001a5a1"
	readings := []gpuEngineReading{
		{PID: 100, LUID: dgpu, Engine: "phys_0_eng_0", Type: "3D", Value: 30},
		{PID: 200, LUID: dgpu, Engine: "phys_0_eng_0", Type: "3D", Value: 25},
		{PID: 200, LUID: dgpu, Engine: "phys_0_eng_14", Type: "VideoEncode", Value: 40},
		{PID: 200, LUID: dgpu, Engine: "phys_0_eng_15", Type: "VideoEncode", Value: 5},
		{PID: 300, LUID: dgpu, Engine: "phys_0_eng_2", Type: "Copy", Value: 80},
		{PID: 300, LUID: dgpu, Engine: "phys_0_eng_2", Type: "Copy", Value: 70},
		{PID: 0, LUID: igpu, Engine: "phys_0_eng_0", Type: "3D", Value: 2},
	}
	summary := summarizeGPUEngines(readings)
	want := []GPUEngine{
		{Name: "Copy", Usage: availableNumber(100, "%")},
		{Name: "3D", Usage: availableNumber(55, "%")},
		{Name: "Video Encode", Usage: availableNumber(40, "%")},
	}
	if got := summary.Adapters[dgpu]; !reflect.DeepEqual(got, want) {
		t.Fatalf("dGPU engines = %+v, want %+v", got, want)
	}
	if got := summary.Processes[200]; got.Percent != 40 || got.Engine != "Video Encode" {
		t.Fatalf("pid 200 = %+v, want its busiest engine, Video Encode at 40%%", got)
	}
	if got := summary.Processes[300]; got.Percent != 80 {
		t.Fatalf("pid 300 = %+v: a process's own value must not include another instance's", got)
	}
	if _, ok := summary.Processes[0]; ok {
		t.Fatal("pid 0 was attributed as a process")
	}
	if summary.Instances[dgpu] != 6 || summary.Instances[igpu] != 1 {
		t.Fatalf("instances = %+v", summary.Instances)
	}
}

func TestGPUEngineDisplayName(t *testing.T) {
	for raw, want := range map[string]string{
		"3D": "3D", "3d": "3D", "VideoDecode": "Video Decode", "videoencode": "Video Encode",
		"Compute 0": "Compute 0", "video codec 0": "Video Codec 0", "ofa_0": "OFA 0",
		"security_1": "Security 1", "LegacyOverlay": "Legacy Overlay", "": "Engine",
	} {
		if got := gpuEngineDisplayName(raw); got != want {
			t.Fatalf("gpuEngineDisplayName(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestPickAdapterLUIDPrefersTheLUIDWithReadings: DXGI lists BBLWIN's RTX 4090
// twice and only one of the two LUIDs has engine counters.
func TestPickAdapterLUIDPrefersTheLUIDWithReadings(t *testing.T) {
	adapters := []dxgiAdapterInfo{
		{Name: "NVIDIA GeForce RTX 4090", LUID: "0x00000000_0x00017769"},
		{Name: "AMD Radeon(TM) Graphics", LUID: "0x00000000_0x0001a5a1"},
		{Name: "NVIDIA GeForce RTX 4090", LUID: "0x00000000_0x00026eaf"},
		{Name: "Microsoft Basic Render Driver", LUID: "0x00000000_0x0001a550", Software: true},
	}
	summary := gpuEngineSummary{Available: true, Instances: map[string]int{"0x00000000_0x00017769": 600, "0x00000000_0x0001a5a1": 88, "0x00000000_0x0001a550": 198}}
	if got := pickAdapterLUID("NVIDIA GeForce RTX 4090", 2, adapters, summary); got != "0x00000000_0x00017769" {
		t.Fatalf("RTX 4090 LUID = %q", got)
	}
	if got := pickAdapterLUID("amd radeon(tm) graphics ", 2, adapters, summary); got != "0x00000000_0x0001a5a1" {
		t.Fatalf("Radeon LUID = %q", got)
	}
	// Two hardware adapters with readings: an unnamed match is ambiguous.
	if got := pickAdapterLUID("GPU 0", 1, adapters, summary); got != "" {
		t.Fatalf("ambiguous lone device matched %q", got)
	}
	lone := gpuEngineSummary{Available: true, Instances: map[string]int{"0x00000000_0x00017769": 10, "0x00000000_0x0001a550": 5}}
	if got := pickAdapterLUID("NVIDIA GeForce RTX 4090 (WDDM)", 1, adapters, lone); got != "0x00000000_0x00017769" {
		t.Fatalf("lone device with a differently spelled name = %q", got)
	}
}

func TestAttachGPUEnginesAndProcessUsage(t *testing.T) {
	summary := summarizeGPUEngines([]gpuEngineReading{
		{PID: 42, LUID: "0x00000000_0x00017769", Engine: "phys_0_eng_14", Type: "VideoEncode", Value: 12.5},
	})
	gpu := GPUSet{Available: true, Devices: []GPUMetric{{Name: "NVIDIA GeForce RTX 4090", Engines: []GPUEngine{{Name: "Video Decode", Usage: availableNumber(1, "%")}}}}}
	adapters := []dxgiAdapterInfo{{Name: "NVIDIA GeForce RTX 4090", LUID: "0x00000000_0x00017769"}}
	got := attachGPUEngines(gpu, adapters, summary)
	if len(got.Devices[0].Engines) != 1 || got.Devices[0].Engines[0].Name != "Video Encode" {
		t.Fatalf("engines = %+v, want the PDH list in place of nvidia-smi's", got.Devices[0].Engines)
	}
	if gpu.Devices[0].Engines[0].Name != "Video Decode" {
		t.Fatal("attachGPUEngines modified the caller's (cached) device")
	}
	kept := attachGPUEngines(gpu, adapters, unavailableGPUEngineSummary("warming up"))
	if kept.Devices[0].Engines[0].Name != "Video Decode" {
		t.Fatal("an unavailable summary dropped the device's existing engines")
	}

	raw := []ProcessMetric{{PID: 42, Name: "sunshine.exe"}, {PID: 43, Name: "idle.exe"}}
	applyProcessGPUUsage(raw, summary)
	if raw[0].GPU.Value != 12.5 || raw[0].GPUEngine != "Video Encode" || !raw[1].GPU.Available || raw[1].GPU.Value != 0 {
		t.Fatalf("process usage = %+v / %+v", raw[0], raw[1])
	}
	applyProcessGPUUsage(raw, unavailableGPUEngineSummary("GPU engine counters are warming up"))
	if raw[0].GPU.Available || raw[0].GPU.Error != "GPU engine counters are warming up" || raw[0].GPUEngine != "" {
		t.Fatalf("warming process usage = %+v", raw[0])
	}
}

// TestAggregateByAppSumsGPU: two processes of one app on the same engine add
// up, as they do inside the engine.
func TestAggregateByAppSumsGPU(t *testing.T) {
	a := treeProcess(10, 0, "game", 1)
	a.GPU = availableNumber(70, "%")
	b := treeProcess(11, 10, "game-helper", 1)
	b.GPU = availableNumber(45, "%")
	apps := aggregateByApp(func() []ProcessMetric { raw := []ProcessMetric{a, b}; annotateProcessTree(raw); return raw }())
	if len(apps) != 1 || apps[0].GPU.Value != 100 {
		t.Fatalf("apps = %+v, want one row capped at 100%%", apps)
	}
}

func TestParseNVIDIAGPUCSVReadsVideoEngines(t *testing.T) {
	set := parseNVIDIAGPUCSV("NVIDIA GeForce RTX 4090, 28, 7358, 24564, 51, 112.50, 00000000:01:00.0, 5, [N/A]\n")
	if !set.Available || len(set.Devices) != 1 {
		t.Fatalf("set = %+v", set)
	}
	want := []GPUEngine{{Name: "Video Encode", Usage: availableNumber(5, "%")}}
	if got := set.Devices[0].Engines; !reflect.DeepEqual(got, want) {
		t.Fatalf("engines = %+v, want %+v ([N/A] decoder skipped)", got, want)
	}
	old := parseNVIDIAGPUCSV("NVIDIA GeForce RTX 3090, 3, 1000, 24576, 40, 30.0, 00000000:01:00.0\n")
	if !old.Available || old.Devices[0].Engines != nil {
		t.Fatalf("seven-column row = %+v", old)
	}
}

func TestNVIDIARejectedQueryFieldOnlyMatchesAnUnknownField(t *testing.T) {
	failed := errors.New("exit status 2")
	if !nvidiaRejectedQueryField([]byte(`Field "utilization.encoder" is not a valid field to query.`), failed) {
		t.Fatal("unknown field not recognized")
	}
	if nvidiaRejectedQueryField([]byte("NVIDIA-SMI has failed because it couldn't communicate with the NVIDIA driver."), failed) {
		t.Fatal("a driver outage would latch the video engine fields off")
	}
	if nvidiaRejectedQueryField([]byte(`not a valid field`), nil) {
		t.Fatal("a successful query was treated as a rejection")
	}
}
