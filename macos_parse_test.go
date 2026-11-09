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
