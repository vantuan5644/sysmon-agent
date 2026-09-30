//go:build windows

package main

import "context"

func (c *systemCollector) CollectOSDHardware(ctx context.Context) osdHardware {
	c.lhmMu.Lock()
	defer c.lhmMu.Unlock()
	result := osdHardware{CPUTemperature: unavailableNumber("C", "OSD requires the resident sensor bridge"),
		GPU: GPUSet{Available: false, Error: "OSD requires the resident sensor bridge"}}
	// Never use the one-shot interpreter fallback for OSD requests.
	if !c.useDaemon || c.daemon == nil {
		return result
	}
	bridge, err := c.daemon.readRequest(ctx, "osd")
	if err != nil || !bridge.Available {
		return result
	}
	temperatures := TemperatureSet{Available: true, Sensors: lhmTemperatureMetrics(bridge.Temperatures, nil)}
	result.CPUTemperature, result.TemperatureName = pickCPUTemperatureSensor(temperatures)
	result.GPU = GPUSet{Available: len(bridge.GPU) > 0, Devices: bridge.GPU}
	return result
}
