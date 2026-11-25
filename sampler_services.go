package main

import (
	"context"
	"time"
)

// snapshotService is a side collector the resident sampler runs beside its
// lanes: the Docker container monitor (docker.go) and the internet latency
// prober (net_latency.go). Each one owns its own cadence, slower than any
// lane, and reads the sampler's demand so it stops talking to Docker or the
// network while nobody is watching. Attach folds the service's newest state
// into a snapshot about to be published; it runs on every publish (five times
// a second), so it must only copy state the service already holds.
type snapshotService interface {
	Run(ctx context.Context, demand func() bool)
	Attach(m *Metrics)
}

// serviceDemandPoll is how often a parked service checks for demand again. A
// dashboard that opens waits at most this long for the first fresh reading.
const serviceDemandPoll = time.Second

// AddService registers a side collector. Call it before Start: the sampler
// starts every registered service once, with its own loops, and stops them
// with them.
func (s *sampler) AddService(service snapshotService) {
	if service == nil {
		return
	}
	s.mu.Lock()
	s.services = append(s.services, service)
	s.mu.Unlock()
}

func (s *sampler) startServices(ctx context.Context) {
	s.mu.Lock()
	services := append([]snapshotService(nil), s.services...)
	s.mu.Unlock()
	for _, service := range services {
		s.wg.Add(1)
		go func(service snapshotService) {
			defer s.wg.Done()
			service.Run(ctx, s.hasDemand)
		}(service)
	}
}

// attachServices lets every service fold its state into a snapshot. Services
// that add issues copy CollectionErrors/CollectionNotes before appending, as
// attachGPUGuard does, because the slices can share a backing array with the
// sampler's working snapshot.
func attachServices(m *Metrics, services []snapshotService) {
	for _, service := range services {
		service.Attach(m)
	}
}

// serviceDue reports whether a demand-gated service should run now: there is
// demand, and its last run (zero for never) is at least every ago.
func serviceDue(demand func() bool, last time.Time, every time.Duration, now time.Time) bool {
	if demand != nil && !demand() {
		return false
	}
	return last.IsZero() || now.Sub(last) >= every
}

// appendIssue and appendNote add one entry to a snapshot's issue lists
// without writing into a backing array another snapshot may share.
func appendIssue(m *Metrics, message string) {
	m.CollectionErrors = append(append([]string(nil), m.CollectionErrors...), message)
}

func appendNote(m *Metrics, message string) {
	m.CollectionNotes = append(append([]string(nil), m.CollectionNotes...), message)
}

// serviceWait is how long a service loop sleeps before checking again: until
// its next run is due, but never longer than serviceDemandPoll, so a parked
// service notices new demand quickly.
func serviceWait(last time.Time, every time.Duration, now time.Time) time.Duration {
	if last.IsZero() {
		return serviceDemandPoll
	}
	wait := every - now.Sub(last)
	if wait <= 0 || wait > serviceDemandPoll {
		return serviceDemandPoll
	}
	return wait
}
