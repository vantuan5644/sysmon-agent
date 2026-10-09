package main

import (
	"encoding/json"
	"errors"
	"fmt"
)

func parsePulseActivity(outputs, sources []byte) (bool, error) {
	var streams []struct {
		Source     int               `json:"source"`
		Corked     bool              `json:"corked"`
		Properties map[string]string `json:"properties"`
	}
	var devices []struct {
		Index         int               `json:"index"`
		MonitorOfSink any               `json:"monitor_of_sink"`
		Properties    map[string]string `json:"properties"`
	}
	if err := json.Unmarshal(outputs, &streams); err != nil {
		return false, err
	}
	if err := json.Unmarshal(sources, &devices); err != nil {
		return false, err
	}
	monitors := map[int]bool{}
	for _, device := range devices {
		value := fmt.Sprint(device.MonitorOfSink)
		monitors[device.Index] = (device.MonitorOfSink != nil && value != "n/a" && value != "4294967295" && value != "-1") || device.Properties["device.class"] == "monitor"
	}
	for _, stream := range streams {
		if stream.Corked || stream.Properties["stream.capture.sink"] == "true" {
			continue
		}
		monitor, known := monitors[stream.Source]
		if !known {
			return false, errors.New("recording source could not be identified")
		}
		if !monitor {
			return true, nil
		}
	}
	return false, nil
}

type pipewireActivityObject struct {
	ID   int    `json:"id"`
	Type string `json:"type"`
	Info struct {
		InputNode  int            `json:"input-node-id"`
		OutputNode int            `json:"output-node-id"`
		State      string         `json:"state"`
		Props      map[string]any `json:"props"`
	} `json:"info"`
}

func parsePipewireActivity(data []byte) (bool, bool, error) {
	var objects []pipewireActivityObject
	if err := json.Unmarshal(data, &objects); err != nil {
		return false, false, err
	}
	mic, camera := false, false
	classes := map[int]string{}
	for _, object := range objects {
		if object.Type == "PipeWire:Interface:Node" {
			classes[object.ID], _ = object.Info.Props["media.class"].(string)
		}
	}
	for _, object := range objects {
		if object.Type != "PipeWire:Interface:Node" || object.Info.State != "running" {
			continue
		}
		class, _ := object.Info.Props["media.class"].(string)
		if class == "Stream/Input/Video" {
			camera = true
		}
		if class == "Stream/Input/Audio" {
			monitor := fmt.Sprint(object.Info.Props["stream.capture.sink"])
			fromSink := false
			for _, link := range objects {
				if link.Type == "PipeWire:Interface:Link" && link.Info.InputNode == object.ID && classes[link.Info.OutputNode] == "Audio/Sink" {
					fromSink = true
				}
			}
			if monitor != "true" && monitor != "1" && !fromSink {
				mic = true
			}
		}
	}
	return mic, camera, nil
}
