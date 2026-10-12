package main

import (
	"sync"
	"time"
)

// metricsHistory keeps the last historyRetention of host metrics on the agent,
// one point per historyStep, so a dashboard that was not watching can still
// show what happened.
//
// The dashboard used to be the only thing holding a trend, 24 samples in the
// browser, and the phone PWA spends most of its life backgrounded between
// glances. Reopening it showed only what arrived after resume, so the spike
// that made someone pick the phone up was already gone from every sparkline.
// The sampler publishes every snapshot anyway; folding them into 5 s buckets
// here costs a few comparisons per publish and ~180 small structs.
//
// Each bucket holds the PEAK of every snapshot published inside it. A glance
// dashboard asks "did anything spike", and a mean of 25 fast-lane samples would
// flatten exactly that.
//
// A nil field means no reading in that bucket: the metric was unavailable, the
// agent was warming up, or - for the slow-lane fields (GPU, network, disk) -
// the slow lane was parked because nobody was watching (samplerIdleAfter). The
// gap is reported as a gap. CPU and RAM come from the fast lane, which never
// parks, so their history is complete for as long as the agent has run.
type metricsHistory struct {
	mu     sync.Mutex
	step   time.Duration
	keep   time.Duration
	points []HistoryPoint
}

const (
	historyStep      = 5 * time.Second
	historyRetention = 15 * time.Minute
)

// HistoryPoint is one historyStep bucket. T is the bucket start in Unix
// milliseconds on the host's clock. Rates are bytes per second; percentages are
// 0-100.
type HistoryPoint struct {
	T      int64    `json:"t"`
	CPU    *float64 `json:"cpu"`
	Memory *float64 `json:"mem"`
	GPU    *float64 `json:"gpu"`
	NetRX  *float64 `json:"net_rx"`
	NetTX  *float64 `json:"net_tx"`
	// Disk is the peak of all drives' read+write throughput together.
	Disk *float64 `json:"disk"`
}

// HistoryResponse is the GET /api/history body. Points are oldest first and
// may have holes where the agent published nothing (it was stopped, or the
// host slept); a client places them by T rather than by index.
type HistoryResponse struct {
	StepSeconds      int            `json:"step_seconds"`
	RetentionSeconds int            `json:"retention_seconds"`
	Points           []HistoryPoint `json:"points"`
}

// historySource is implemented by collectors that keep a metricsHistory (the
// resident sampler). The route is registered only for those, like /api/stream.
type historySource interface {
	History() HistoryResponse
}

func newMetricsHistory(step, keep time.Duration) *metricsHistory {
	return &metricsHistory{step: step, keep: keep}
}

// observe folds one published snapshot into its bucket. slowFresh says whether
// the slow-lane fields in m were refreshed recently enough to count: a parked
// slow lane leaves its last values in the snapshot, and recording those would
// draw a flat line through a period nobody measured.
func (h *metricsHistory) observe(m Metrics, slowFresh bool) {
	if m.Timestamp.IsZero() {
		return
	}
	stepMS := h.step.Milliseconds()
	ms := m.Timestamp.UnixMilli()
	bucket := ms - ms%stepMS

	h.mu.Lock()
	defer h.mu.Unlock()
	n := len(h.points)
	switch {
	case n == 0 || h.points[n-1].T < bucket:
		h.points = append(h.points, HistoryPoint{T: bucket})
		h.trimLocked(bucket)
	case h.points[n-1].T > bucket:
		// The host clock stepped backwards. Points ahead of "now" would sit at
		// the end of every chart until real time caught up with them.
		h.points = h.points[:0]
		h.points = append(h.points, HistoryPoint{T: bucket})
	}
	point := &h.points[len(h.points)-1]

	if m.CPU.Available {
		peak(&point.CPU, m.CPU.Value)
	}
	if m.Memory.Available {
		peak(&point.Memory, m.Memory.Percent)
	}
	if !slowFresh {
		return
	}
	if usage, ok := primaryGPUUsage(m.GPU); ok {
		peak(&point.GPU, usage)
	}
	if rx, tx, ok := networkRateTotals(m.Network); ok {
		peak(&point.NetRX, rx)
		peak(&point.NetTX, tx)
	}
	if read, write, ok := storageRateTotals(m.Storage); ok {
		peak(&point.Disk, read+write)
	}
}

func (h *metricsHistory) trimLocked(newest int64) {
	oldest := newest - h.keep.Milliseconds()
	drop := 0
	for drop < len(h.points) && h.points[drop].T <= oldest {
		drop++
	}
	if drop > 0 {
		h.points = append(h.points[:0], h.points[drop:]...)
	}
}

// snapshot copies the points; the pointer fields are never mutated after a
// bucket closes, but the open bucket is, so it gets fresh values.
func (h *metricsHistory) snapshot() HistoryResponse {
	h.mu.Lock()
	defer h.mu.Unlock()
	points := make([]HistoryPoint, len(h.points))
	for i, p := range h.points {
		points[i] = HistoryPoint{
			T:      p.T,
			CPU:    copyFloat(p.CPU),
			Memory: copyFloat(p.Memory),
			GPU:    copyFloat(p.GPU),
			NetRX:  copyFloat(p.NetRX),
			NetTX:  copyFloat(p.NetTX),
			Disk:   copyFloat(p.Disk),
		}
	}
	return HistoryResponse{
		StepSeconds:      int(h.step / time.Second),
		RetentionSeconds: int(h.keep / time.Second),
		Points:           points,
	}
}

func peak(slot **float64, value float64) {
	if !isFinite(value) {
		return
	}
	value = round(value, 2)
	if *slot == nil {
		*slot = &value
		return
	}
	if value > **slot {
		**slot = value
	}
}

func copyFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	v := *value
	return &v
}

// primaryGPUUsage mirrors firstAvailableGPU in static/app.js: the first device
// reporting usage, which is the one the GPU gauge shows.
func primaryGPUUsage(gpu GPUSet) (float64, bool) {
	if !gpu.Available {
		return 0, false
	}
	for _, device := range gpu.Devices {
		if device.Usage.Available {
			return device.Usage.Value, true
		}
	}
	return 0, false
}

// networkRateTotals sums interface rates the way networkTotals in app.js does
// for the NET gauge. ok is false until at least one interface has a rate.
func networkRateTotals(network NetworkSet) (rx, tx float64, ok bool) {
	if !network.Available {
		return 0, 0, false
	}
	for _, iface := range network.Interfaces {
		if iface.RXBytesPerSecond.Available {
			rx += iface.RXBytesPerSecond.Value
			ok = true
		}
		if iface.TXBytesPerSecond.Available {
			tx += iface.TXBytesPerSecond.Value
			ok = true
		}
	}
	return rx, tx, ok
}
