//go:build linux

package main

import (
	"path/filepath"
	"testing"
)

const testPCIIDs = `# test pci.ids
1002  Advanced Micro Devices, Inc. [AMD/ATI]
	1681  Rembrandt [Radeon 680M]
	744c  Navi 31 [Radeon RX 7900 XT/7900 XTX/7900 GRE/7900M]
		1043 0510  TUF Gaming Radeon RX 7900 XTX OC
		1462 5320  Some Board Without A Model
	73bf  Navi 21 [Radeon RX 6800/6800 XT / 6900 XT]
1022  Advanced Micro Devices, Inc. [AMD]
	1681  Not a GPU
8086  Intel Corporation
	46a6  Alder Lake-P GT2 [Iris Xe Graphics]
	56a0  DG2 [Arc A770]
`

// usePCIIDsFixture points the pci.ids lookup at a fixture for one test.
func usePCIIDsFixture(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pci.ids")
	if err := writeTestFile(path, testPCIIDs); err != nil {
		t.Fatal(err)
	}
	saved := linuxPCIIDsPaths
	linuxPCIIDsPaths = []string{filepath.Join(t.TempDir(), "missing.ids"), path}
	t.Cleanup(func() { linuxPCIIDsPaths = saved })
}

func writeTestFiles(t *testing.T, files map[string]string) {
	t.Helper()
	for path, value := range files {
		if err := writeTestFile(path, value); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLookupPCIName(t *testing.T) {
	usePCIIDsFixture(t)
	cases := []struct {
		vendor, device, subV, subD, want string
	}{
		{"0x1002", "0x1681", "0x1002", "0x0124", "Radeon 680M"},
		{"0x1002", "0x744c", "0x1043", "0x0510", "Radeon RX 7900 XTX OC"},
		{"0x1002", "0x744c", "0x1462", "0x5320", "Radeon RX 7900 XT/7900 XTX/7900 GRE/7900M"},
		{"0x1002", "0x744c", "", "", "Radeon RX 7900 XT/7900 XTX/7900 GRE/7900M"},
		{"0x1002", "0x73bf", "0x1043", "0x0510", "Radeon RX 6800/6800 XT / 6900 XT"},
		{"0x8086", "0x46a6", "", "", "Iris Xe Graphics"},
		{"0x8086", "0x56a0", "", "", "Arc A770"},
		{"0x1002", "0xffff", "", "", ""},
		{"0x10de", "0x2204", "", "", ""},
		{"bogus", "0x1681", "", "", ""},
	}
	for _, tc := range cases {
		if got := lookupPCIName(tc.vendor, tc.device, tc.subV, tc.subD); got != tc.want {
			t.Errorf("lookupPCIName(%s,%s,%s,%s) = %q, want %q", tc.vendor, tc.device, tc.subV, tc.subD, got, tc.want)
		}
	}
	// A second lookup is served from the cache, misses included.
	pciIDsCache.Lock()
	size := len(pciIDsCache.names)
	pciIDsCache.Unlock()
	if size == 0 || size > pciIDsCacheMax {
		t.Fatalf("cache size = %d", size)
	}
	if got := lookupPCIName("0x1002", "0x1681", "0x1002", "0x0124"); got != "Radeon 680M" {
		t.Fatalf("cached lookup = %q", got)
	}
}

func TestParseDPMCurrentMHz(t *testing.T) {
	cases := map[string]float64{
		"0: 500Mhz\n1: 1800Mhz *\n2: 2500Mhz\n": 1800,
		"0: 400Mhz *\n1: 800Mhz *\n":            400,
		"2: 2200Mhz *\n":                        2200,
		"S: 19Mhz *\n0: 500Mhz\n":               19,
		"0: 1.2GHz *\n":                         1200,
	}
	for text, want := range cases {
		if got, ok := parseDPMCurrentMHz(text); !ok || got != want {
			t.Errorf("parseDPMCurrentMHz(%q) = %v, %v; want %v", text, got, ok, want)
		}
	}
	if _, ok := parseDPMCurrentMHz("0: 500Mhz\n1: 800Mhz\n"); ok {
		t.Fatal("a table without a starred level has no current clock")
	}
}

func TestCollectLinuxDRMGPUAMDDiscreteDetails(t *testing.T) {
	usePCIIDsFixture(t)
	root := t.TempDir()
	device := filepath.Join(root, "class", "drm", "card1", "device")
	hwmon := filepath.Join(device, "hwmon", "hwmon4")
	writeTestFiles(t, map[string]string{
		filepath.Join(device, "vendor"):               "0x1002\n",
		filepath.Join(device, "device"):               "0x744c\n",
		filepath.Join(device, "subsystem_vendor"):     "0x1043\n",
		filepath.Join(device, "subsystem_device"):     "0x0510\n",
		filepath.Join(device, "uevent"):               "DRIVER=amdgpu\nPCI_SLOT_NAME=0000:03:00.0\n",
		filepath.Join(device, "gpu_busy_percent"):     "42\n",
		filepath.Join(device, "mem_info_vram_used"):   "2147483648\n",
		filepath.Join(device, "mem_info_vram_total"):  "25769803776\n",
		filepath.Join(device, "mem_info_gtt_used"):    "1073741824\n",
		filepath.Join(device, "mem_info_gtt_total"):   "8589934592\n",
		filepath.Join(device, "mem_info_vram_vendor"): "samsung\n",
		filepath.Join(device, "pp_dpm_sclk"):          "0: 500Mhz\n1: 2482Mhz *\n2: 2700Mhz\n",
		filepath.Join(hwmon, "temp1_input"):           "51000\n",
		filepath.Join(hwmon, "temp1_label"):           "edge\n",
		filepath.Join(hwmon, "temp2_input"):           "73000\n",
		filepath.Join(hwmon, "temp2_label"):           "junction\n",
		filepath.Join(hwmon, "temp2_crit"):            "110000\n",
		filepath.Join(hwmon, "temp3_input"):           "82000\n",
		filepath.Join(hwmon, "temp3_label"):           "mem\n",
		filepath.Join(hwmon, "temp3_crit"):            "105000\n",
		filepath.Join(hwmon, "power1_input"):          "251000000\n",
		filepath.Join(hwmon, "power1_cap"):            "303000000\n",
		filepath.Join(hwmon, "fan1_input"):            "1250\n",
	})

	got := collectLinuxDRMGPU(root)
	if !got.Available || len(got.Devices) != 1 {
		t.Fatalf("GPU set = %+v", got)
	}
	gpu := got.Devices[0]
	if gpu.Name != "AMD Radeon RX 7900 XTX OC" {
		t.Fatalf("name = %q", gpu.Name)
	}
	if gpu.Integrated || gpu.MemoryKind != "" {
		t.Fatalf("discrete card classified integrated=%v kind=%q", gpu.Integrated, gpu.MemoryKind)
	}
	if !gpu.Memory.Available || gpu.Memory.TotalBytes != 25769803776 {
		t.Fatalf("memory = %+v, want VRAM only", gpu.Memory)
	}
	checks := []struct {
		name   string
		metric NumberMetric
		want   float64
	}{
		{"usage", gpu.Usage, 42},
		{"temperature", gpu.Temperature, 51},
		{"hotspot", gpu.HotspotTemperature, 73},
		{"vram temperature", gpu.VRAMTemperature, 82},
		{"power (power1_input fallback)", gpu.Power, 251},
	}
	for _, c := range checks {
		if !c.metric.Available || c.metric.Value != c.want {
			t.Errorf("%s = %+v, want %v", c.name, c.metric, c.want)
		}
	}
	optional := []struct {
		name   string
		metric *NumberMetric
		want   float64
		unit   string
	}{
		{"power limit", gpu.PowerLimit, 303, "W"},
		{"clock", gpu.Clock, 2482, "MHz"},
		{"fan", gpu.FanRPM, 1250, "RPM"},
		{"hotspot critical", gpu.HotspotCritical, 110, "C"},
		{"vram critical", gpu.VRAMCritical, 105, "C"},
	}
	for _, c := range optional {
		if c.metric == nil || !c.metric.Available || c.metric.Value != c.want || c.metric.Unit != c.unit {
			t.Errorf("%s = %+v, want %v %s", c.name, c.metric, c.want, c.unit)
		}
	}
}

func TestCollectLinuxDRMGPUAPUSharedMemory(t *testing.T) {
	usePCIIDsFixture(t)
	root := t.TempDir()
	device := filepath.Join(root, "class", "drm", "card0", "device")
	hwmon := filepath.Join(device, "hwmon", "hwmon5")
	writeTestFiles(t, map[string]string{
		filepath.Join(device, "vendor"):              "0x1002\n",
		filepath.Join(device, "device"):              "0x1681\n",
		filepath.Join(device, "subsystem_vendor"):    "0x1002\n",
		filepath.Join(device, "subsystem_device"):    "0x0124\n",
		filepath.Join(device, "gpu_busy_percent"):    "3\n",
		filepath.Join(device, "mem_info_vram_used"):  "268435456\n",
		filepath.Join(device, "mem_info_vram_total"): "536870912\n",
		filepath.Join(device, "mem_info_gtt_used"):   "268435456\n",
		filepath.Join(device, "mem_info_gtt_total"):  "7516192768\n",
		filepath.Join(hwmon, "temp1_input"):          "39000\n",
		filepath.Join(hwmon, "temp1_label"):          "edge\n",
		filepath.Join(hwmon, "power1_input"):         "10304000\n",
		filepath.Join(hwmon, "freq1_input"):          "2200000000\n",
		filepath.Join(hwmon, "freq1_label"):          "sclk\n",
	})

	gpu := collectLinuxDRMGPU(root).Devices[0]
	if gpu.Name != "AMD Radeon 680M" {
		t.Fatalf("name = %q", gpu.Name)
	}
	if !gpu.Integrated || gpu.MemoryKind != "shared" {
		t.Fatalf("APU integrated=%v kind=%q", gpu.Integrated, gpu.MemoryKind)
	}
	if gpu.Memory.UsedBytes != 536870912 || gpu.Memory.TotalBytes != 8053063680 {
		t.Fatalf("memory = %+v, want carve-out plus GTT", gpu.Memory)
	}
	if gpu.Clock == nil || gpu.Clock.Value != 2200 {
		t.Fatalf("clock = %+v, want hwmon sclk", gpu.Clock)
	}
	if !gpu.Power.Available || gpu.Power.Value != 10.3 {
		t.Fatalf("power = %+v", gpu.Power)
	}
	if gpu.PowerLimit != nil || gpu.FanRPM != nil || gpu.HotspotCritical != nil {
		t.Fatalf("absent sensors must stay absent: %+v %+v %+v", gpu.PowerLimit, gpu.FanRPM, gpu.HotspotCritical)
	}
	if gpu.HotspotTemperature.Available || gpu.HotspotTemperature.Error == "" {
		t.Fatalf("hotspot = %+v, want unavailable with a reason", gpu.HotspotTemperature)
	}
}

func TestCollectLinuxDRMGPUProductNameAndOrdering(t *testing.T) {
	usePCIIDsFixture(t)
	root := t.TempDir()
	apu := filepath.Join(root, "class", "drm", "card0", "device")
	dgpu := filepath.Join(root, "class", "drm", "card1", "device")
	twin := filepath.Join(root, "class", "drm", "card2", "device")
	writeTestFiles(t, map[string]string{
		filepath.Join(apu, "vendor"):                         "0x1002\n",
		filepath.Join(apu, "device"):                         "0x1681\n",
		filepath.Join(dgpu, "vendor"):                        "0x1002\n",
		filepath.Join(dgpu, "product_name"):                  "  AMD  Radeon RX 7900 XTX \n",
		filepath.Join(dgpu, "hwmon", "hwmon1", "fan1_input"): "900\n",
		filepath.Join(twin, "vendor"):                        "0x1002\n",
		filepath.Join(twin, "product_name"):                  "AMD Radeon RX 7900 XTX\n",
		filepath.Join(twin, "mem_info_vram_vendor"):          "hynix\n",
	})

	got := collectLinuxDRMGPU(root)
	if len(got.Devices) != 3 {
		t.Fatalf("devices = %+v", got.Devices)
	}
	want := []string{"AMD Radeon RX 7900 XTX (card1)", "AMD Radeon RX 7900 XTX (card2)", "AMD Radeon 680M"}
	for i, name := range want {
		if got.Devices[i].Name != name {
			t.Fatalf("order = %q, %q, %q; want %v", got.Devices[0].Name, got.Devices[1].Name, got.Devices[2].Name, want)
		}
	}
	if got.Devices[0].Integrated || !got.Devices[2].Integrated {
		t.Fatalf("integrated flags = %v %v", got.Devices[0].Integrated, got.Devices[2].Integrated)
	}
}

func TestCollectLinuxDRMGPUIntelUsageFromFdinfo(t *testing.T) {
	usePCIIDsFixture(t)
	root := t.TempDir()
	card := filepath.Join(root, "class", "drm", "card0")
	device := filepath.Join(card, "device")
	writeTestFiles(t, map[string]string{
		filepath.Join(device, "vendor"):        "0x8086\n",
		filepath.Join(device, "device"):        "0x46a6\n",
		filepath.Join(device, "uevent"):        "DRIVER=i915\nPCI_SLOT_NAME=0000:00:02.0\n",
		filepath.Join(card, "gt_cur_freq_mhz"): "1300\n",
	})
	engines := []GPUEngine{
		{Name: "3D", Usage: availableNumber(37.5, "%")},
		{Name: "Video", Usage: availableNumber(12, "%")},
	}
	summary := drmUsageSummary{Available: true, Devices: map[string]drmDeviceUsage{
		"0000:00:02.0": {Load: availableNumber(37.5, "%"), Engines: engines},
	}}

	got := collectLinuxDRMGPUWithUsage(root, func() drmUsageSummary { return summary })
	gpu := got.Devices[0]
	if gpu.Name != "Intel Iris Xe Graphics" || !gpu.Integrated {
		t.Fatalf("name = %q integrated = %v", gpu.Name, gpu.Integrated)
	}
	if !gpu.Usage.Available || gpu.Usage.Value != 37.5 {
		t.Fatalf("usage = %+v, want the fdinfo load", gpu.Usage)
	}
	if len(gpu.Engines) != 2 || gpu.Engines[0].Name != "3D" {
		t.Fatalf("engines = %+v", gpu.Engines)
	}
	if gpu.Clock == nil || gpu.Clock.Value != 1300 {
		t.Fatalf("clock = %+v", gpu.Clock)
	}

	warming := drmUsageSummary{Available: true, Devices: map[string]drmDeviceUsage{
		"0000:00:02.0": {Load: unavailableNumber("%", "GPU usage sampler is warming up")},
	}}
	gpu = collectLinuxDRMGPUWithUsage(root, func() drmUsageSummary { return warming }).Devices[0]
	if gpu.Usage.Available || gpu.Usage.Error != "GPU usage sampler is warming up" {
		t.Fatalf("usage = %+v, want the sampler's reason", gpu.Usage)
	}

	// An Arc card is discrete.
	writeTestFiles(t, map[string]string{filepath.Join(device, "device"): "0x56a0\n"})
	gpu = collectLinuxDRMGPU(root).Devices[0]
	if gpu.Name != "Intel Arc A770" || gpu.Integrated {
		t.Fatalf("Arc name = %q integrated = %v", gpu.Name, gpu.Integrated)
	}
}

func TestCollectLinuxDRMGPURuntimeSuspended(t *testing.T) {
	usePCIIDsFixture(t)
	root := t.TempDir()
	device := filepath.Join(root, "class", "drm", "card1", "device")
	writeTestFiles(t, map[string]string{
		filepath.Join(device, "vendor"):                          "0x1002\n",
		filepath.Join(device, "power", "runtime_status"):         "suspended\n",
		filepath.Join(device, "gpu_busy_percent"):                "99\n",
		filepath.Join(device, "hwmon", "hwmon2", "temp1_input"):  "60000\n",
		filepath.Join(device, "hwmon", "hwmon2", "power1_input"): "5000000\n",
	})
	gpu := collectLinuxDRMGPU(root).Devices[0]
	if !gpu.Usage.Available || gpu.Usage.Value != 0 {
		t.Fatalf("usage = %+v, want an idle 0", gpu.Usage)
	}
	if gpu.Temperature.Available || gpu.Power.Available || gpu.Memory.Available {
		t.Fatalf("a suspended card's sensors must be left unread: %+v %+v %+v", gpu.Temperature, gpu.Power, gpu.Memory)
	}
	if gpu.Temperature.Error != "GPU is runtime-suspended" {
		t.Fatalf("temperature error = %q", gpu.Temperature.Error)
	}
}

func TestDRMEngineDisplayName(t *testing.T) {
	cases := map[string]string{
		"gfx": "3D", "render": "3D", "rcs": "3D", "compute": "Compute", "ccs": "Compute",
		"dma": "Copy", "copy": "Copy", "bcs": "Copy", "video": "Video", "vcs": "Video",
		"video-enhance": "Video Enhance", "vecs": "Video Enhance", "dec": "Video Decode",
		"enc": "Video Encode", "enc_1": "Video Encode 1", "jpeg": "JPEG", "vpe": "VPE",
		"ofa": "OFA", "mystery_engine": "Mystery Engine",
	}
	for class, want := range cases {
		if got := drmEngineDisplayName(class); got != want {
			t.Errorf("drmEngineDisplayName(%q) = %q, want %q", class, got, want)
		}
	}
}
