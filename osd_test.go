package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"
)

type fakeOSDCollector struct {
	fakeLaneCollector
	osdCalls int64
}

func (f *fakeOSDCollector) CollectOSDHardware(context.Context) osdHardware {
	atomic.AddInt64(&f.osdCalls, 1)
	return osdHardware{CPUTemperature: availableNumber(61, "C"), TemperatureName: "CPU Package",
		GPU: GPUSet{Available: true, Devices: []GPUMetric{{Name: "fixture GPU", Usage: availableNumber(72, "%"), Temperature: availableNumber(54, "C")}}}}
}

func TestOSDRequestsDoNotWakeFullSlowCollector(t *testing.T) {
	collector := &fakeOSDCollector{}
	s := newSampler(collector, 100*time.Millisecond, 500*time.Millisecond)
	s.idleAfter = 100 * time.Millisecond
	s.Start()
	defer s.Stop()
	started := time.Now()
	deadline := time.Now().Add(2 * time.Second)
	for {
		metrics := s.OSDSnapshot()
		if metrics.GPU.Available && metrics.CPU.Available {
			if metrics.CPUTemperature.Value != 61 || metrics.GPU.Devices[0].Usage.Value != 72 {
				t.Fatalf("OSD readings: %+v", metrics)
			}
			if time.Since(started) >= 1200*time.Millisecond {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("OSD hardware did not warm up")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if calls := atomic.LoadInt64(&collector.slow); calls != 1 {
		t.Fatalf("full slow calls = %d, want only eager startup pass", calls)
	}
	if atomic.LoadInt64(&collector.fallbackCalls) != 0 || s.hasDemand() {
		t.Fatal("OSD requests activated full collection demand")
	}
	s.mu.Lock()
	s.osdHardware.Timestamp = time.Now().Add(-4 * time.Second)
	s.mu.Unlock()
	metrics := s.OSDSnapshot()
	if !metrics.CPU.Available || metrics.CPUTemperature.Available || metrics.GPU.Available {
		t.Fatalf("expired hardware must degrade independently of CPU: %+v", metrics)
	}
}

func TestOSDHTTPReadsCachedStateWithoutColdCollection(t *testing.T) {
	collector := &fakeOSDCollector{}
	s := newSampler(collector, 0, 0)
	s.snapshot.Timestamp = time.Now()
	s.snapshot.CPU = availableNumber(12, "%")
	handler, err := newHTTPHandler(s, fstest.MapFS{"static/index.html": {Data: []byte("fixture")}})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/osd", nil))
	var metrics osdMetrics
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &metrics) != nil {
		t.Fatalf("OSD response: %d %s", recorder.Code, recorder.Body.String())
	}
	if !metrics.CPU.Available || metrics.CPU.Value != 12 || metrics.GPU.Available || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("OSD cached response: %+v", metrics)
	}
	if atomic.LoadInt64(&collector.fallbackCalls) != 0 || s.hasDemand() {
		t.Fatal("HTTP request performed or requested full collection")
	}
}
