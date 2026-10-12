package main

import (
	"sort"
	"strconv"
	"strings"
)

// GPU engines and per-process GPU utilization.
//
// A GPU is several engines - 3D, copy, video encode and decode, compute - and
// the single utilization figure on the GPU card hides which one is busy. On
// these hosts that matters: game streaming (Sunshine, Apollo) lives on the
// video encoder and Jellyfin transcoding on the decoder, while the 3D engine
// can sit idle. The App details page also showed per-process GPU memory only,
// which says nothing for a process that renders without allocating much.
//
// Windows keeps exactly this in the "GPU Engine" performance counter set that
// Task Manager reads: one instance per process per engine,
//
//	pid_9616_luid_0x00000000_0x00017769_phys_0_eng_0_engtype_3D
//
// with its utilization over the interval between two collections. The agent
// holds one PDH query open (gpu_engines_windows.go) and collects it once per
// slow pass. Measured on BBLWIN with 946 instances across three adapters: about
// 1 ms of wall time per collection, below the process CPU timer's resolution.
//
// Aggregation follows Task Manager. An engine's utilization is the sum over
// the processes using it; an engine type's is the busiest engine of that type
// on the adapter; a process's GPU figure is its busiest engine, which is also
// what it is labelled with.
//
// On Linux, NVIDIA's encoder and decoder utilization come from the nvidia-smi
// query the slow lane already runs. Per-process utilization there would need
// NVML (cgo) or a walk of every process's DRM file descriptors, so it is
// reported as unavailable.

// GPUEngine is one engine type's utilization on an adapter.
type GPUEngine struct {
	Name  string       `json:"name"`
	Usage NumberMetric `json:"usage_percent"`
}

// gpuEngineReading is one parsed GPU Engine counter instance.
type gpuEngineReading struct {
	PID    int
	LUID   string
	Engine string // "phys_0_eng_3": the physical engine, unique per adapter
	Type   string // raw engtype, e.g. "3D", "VideoEncode", "Compute 0"
	Value  float64
}

// parseGPUEngineInstance splits a GPU Engine instance name. The engine type
// runs to the end of the name and may itself contain spaces or underscores.
func parseGPUEngineInstance(name string, value float64) (gpuEngineReading, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(name), "pid_")
	if !ok {
		return gpuEngineReading{}, false
	}
	pidText, rest, ok := strings.Cut(rest, "_luid_")
	if !ok {
		return gpuEngineReading{}, false
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid < 0 {
		return gpuEngineReading{}, false
	}
	luid, rest, ok := strings.Cut(rest, "_phys_")
	if !ok || luid == "" {
		return gpuEngineReading{}, false
	}
	physEng, engType, ok := strings.Cut(rest, "_engtype_")
	if !ok || engType == "" {
		return gpuEngineReading{}, false
	}
	if !isFinite(value) || value < 0 {
		return gpuEngineReading{}, false
	}
	return gpuEngineReading{
		PID:    pid,
		LUID:   strings.ToLower(luid),
		Engine: "phys_" + physEng,
		Type:   engType,
		Value:  value,
	}, true
}

// processGPUUsage is one process's busiest engine.
type processGPUUsage struct {
	Percent float64
	Engine  string
}

// gpuEngineSummary is one collection, aggregated.
type gpuEngineSummary struct {
	Available bool
	Error     string
	// Adapters maps LUID to its engine types, busiest first.
	Adapters map[string][]GPUEngine
	// Instances counts readings per LUID, used to pick between two adapters
	// that share a name (DXGI lists the RTX 4090 on BBLWIN twice; only one of
	// the two LUIDs carries engine counters).
	Instances map[string]int
	Processes map[int]processGPUUsage
}

func unavailableGPUEngineSummary(message string) gpuEngineSummary {
	return gpuEngineSummary{Available: false, Error: message}
}

func summarizeGPUEngines(readings []gpuEngineReading) gpuEngineSummary {
	type engineKey struct{ luid, engine string }
	engineTotal := map[engineKey]float64{}
	engineType := map[engineKey]string{}
	instances := map[string]int{}
	processes := map[int]processGPUUsage{}
	for _, r := range readings {
		key := engineKey{r.LUID, r.Engine}
		engineTotal[key] += r.Value
		engineType[key] = r.Type
		instances[r.LUID]++
		if r.PID == 0 {
			continue
		}
		value := clampEnginePercent(r.Value)
		if current, ok := processes[r.PID]; !ok || value > current.Percent {
			processes[r.PID] = processGPUUsage{Percent: value, Engine: gpuEngineDisplayName(r.Type)}
		}
	}
	byType := map[string]map[string]float64{}
	for key, total := range engineTotal {
		name := gpuEngineDisplayName(engineType[key])
		if byType[key.luid] == nil {
			byType[key.luid] = map[string]float64{}
		}
		if value := clampEnginePercent(total); value >= byType[key.luid][name] {
			byType[key.luid][name] = value
		}
	}
	adapters := map[string][]GPUEngine{}
	for luid, types := range byType {
		engines := make([]GPUEngine, 0, len(types))
		for name, value := range types {
			engines = append(engines, GPUEngine{Name: name, Usage: availableNumber(value, "%")})
		}
		sortGPUEngines(engines)
		adapters[luid] = engines
	}
	return gpuEngineSummary{Available: true, Adapters: adapters, Instances: instances, Processes: processes}
}

func clampEnginePercent(value float64) float64 {
	if value > 100 {
		return 100
	}
	return value
}

// sortGPUEngines orders busiest first, then by name, so the dashboard can show
// the leaders and a stable tail.
func sortGPUEngines(engines []GPUEngine) {
	sort.SliceStable(engines, func(i, j int) bool {
		a, b := engines[i].Usage.Value, engines[j].Usage.Value
		if a != b {
			return a > b
		}
		return engines[i].Name < engines[j].Name
	})
}

// gpuEngineDisplayName turns a driver's engtype into a label: "3D",
// "VideoEncode" -> "Video Encode", "video decode 1" -> "Video Decode 1",
// "ofa_0" -> "OFA 0". Drivers name engines freely, so anything unrecognized
// keeps its words, capitalized.
func gpuEngineDisplayName(raw string) string {
	words := strings.Fields(strings.ReplaceAll(strings.TrimSpace(raw), "_", " "))
	known := map[string]string{
		"3d": "3D", "vr": "VR", "ofa": "OFA", "jpeg": "JPEG",
		"videodecode": "Video Decode", "videoencode": "Video Encode",
		"videoprocessing": "Video Processing", "legacyoverlay": "Legacy Overlay",
	}
	for i, word := range words {
		lower := strings.ToLower(word)
		if mapped, ok := known[lower]; ok {
			words[i] = mapped
			continue
		}
		words[i] = strings.ToUpper(lower[:1]) + lower[1:]
	}
	if len(words) == 0 {
		return "Engine"
	}
	return strings.Join(words, " ")
}

// pickAdapterLUID chooses the LUID for a GPU device named name among the
// DXGI adapters, preferring the one with engine readings when several share
// the name. A lone hardware adapter with readings is used for a lone device
// whatever its name (the fallback GPU path names devices from WMI, which can
// differ from DXGI's description).
func pickAdapterLUID(name string, deviceCount int, adapters []dxgiAdapterInfo, summary gpuEngineSummary) string {
	want := strings.ToLower(strings.TrimSpace(name))
	best, bestInstances := "", -1
	for _, adapter := range adapters {
		if adapter.Software || strings.ToLower(strings.TrimSpace(adapter.Name)) != want {
			continue
		}
		if n := summary.Instances[adapter.LUID]; n > bestInstances {
			best, bestInstances = adapter.LUID, n
		}
	}
	if best != "" {
		return best
	}
	if deviceCount != 1 {
		return ""
	}
	only := ""
	for _, adapter := range adapters {
		if adapter.Software || summary.Instances[adapter.LUID] == 0 {
			continue
		}
		if only != "" {
			return ""
		}
		only = adapter.LUID
	}
	return only
}

// dxgiAdapterInfo is one adapter as DXGI describes it. LUID uses the GPU
// Engine counter's spelling, "0x<high>_0x<low>" in lowercase hex.
type dxgiAdapterInfo struct {
	Name     string
	LUID     string
	Software bool
}

// attachGPUEngines sets each device's engine list from a summary.
func attachGPUEngines(gpu GPUSet, adapters []dxgiAdapterInfo, summary gpuEngineSummary) GPUSet {
	if !gpu.Available || !summary.Available {
		return gpu
	}
	devices := append([]GPUMetric(nil), gpu.Devices...)
	for i := range devices {
		luid := pickAdapterLUID(devices[i].Name, len(devices), adapters, summary)
		if engines := summary.Adapters[luid]; luid != "" && len(engines) > 0 {
			devices[i].Engines = engines
		}
	}
	gpu.Devices = devices
	return gpu
}

// applyProcessGPUUsage fills each process row's GPU utilization from a
// summary. A process with no engine instance used no GPU engine in the
// interval, which is a real 0%.
func applyProcessGPUUsage(raw []ProcessMetric, summary gpuEngineSummary) {
	for i := range raw {
		if !summary.Available {
			raw[i].GPU = unavailableNumber("%", firstNonEmpty(summary.Error, "GPU engine counters unavailable"))
			raw[i].GPUEngine = ""
			continue
		}
		usage := summary.Processes[raw[i].PID]
		raw[i].GPU = availableNumber(usage.Percent, "%")
		raw[i].GPUEngine = usage.Engine
	}
}

// nvidiaVideoEngines turns nvidia-smi's utilization.encoder/decoder columns
// into engine entries, skipping a column the driver reported as [N/A].
func nvidiaVideoEngines(encoder, decoder string) []GPUEngine {
	var engines []GPUEngine
	if value, ok := parseFloat(encoder); ok && value >= 0 {
		engines = append(engines, GPUEngine{Name: "Video Encode", Usage: availableNumber(clampEnginePercent(value), "%")})
	}
	if value, ok := parseFloat(decoder); ok && value >= 0 {
		engines = append(engines, GPUEngine{Name: "Video Decode", Usage: availableNumber(clampEnginePercent(value), "%")})
	}
	sortGPUEngines(engines)
	return engines
}
