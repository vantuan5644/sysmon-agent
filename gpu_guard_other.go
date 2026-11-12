//go:build !windows && !linux

package main

// startGPUGuard has nothing to guard on these platforms: the GPU guard needs
// nvidia-smi and a way to read the hotspot, which only the Windows LHM
// bridge and the Linux helper provide.
func startGPUGuard(gpuGuardOptions, MetricsCollector, func(string, ...any)) gpuGuardService {
	return nil
}
