package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type stubSnapshotService struct {
	started atomic.Bool
	stopped atomic.Bool
}

func (s *stubSnapshotService) Run(ctx context.Context, demand func() bool) {
	s.started.Store(true)
	<-ctx.Done()
	s.stopped.Store(true)
}

func (s *stubSnapshotService) Attach(m *Metrics) {
	m.Network.Latency = &NetworkLatency{Available: true, Target: "192.0.2.1:443", Method: "tcp-connect"}
	m.Docker = &DockerSet{Available: true, Total: 2, Running: 1}
	appendNote(m, "stub: attached")
}

func TestSnapshotsCarryServiceState(t *testing.T) {
	s := newSampler(&fakeLaneCollector{hostname: "test-host"}, 0, 0)
	service := &stubSnapshotService{}
	s.AddService(service)
	s.AddService(nil)
	s.publishWhole(Metrics{Timestamp: time.Now().UTC()})
	got, err := s.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Docker == nil || got.Docker.Total != 2 || got.Network.Latency == nil || got.Network.Latency.Target != "192.0.2.1:443" {
		t.Fatalf("snapshot docker=%+v latency=%+v", got.Docker, got.Network.Latency)
	}
	if len(got.CollectionNotes) == 0 || got.CollectionNotes[len(got.CollectionNotes)-1] != "stub: attached" {
		t.Fatalf("notes = %v", got.CollectionNotes)
	}
}

func TestServicesStartAndStopWithTheSampler(t *testing.T) {
	s := newSampler(&fakeLaneCollector{hostname: "test-host"}, 0, 0)
	service := &stubSnapshotService{}
	s.AddService(service)
	s.Start()
	waitForCond(t, 2*time.Second, service.started.Load)
	s.Stop()
	if !service.stopped.Load() {
		t.Fatal("Stop returned before the service stopped")
	}
}

func TestServiceWaitAndDue(t *testing.T) {
	now := time.Unix(1000, 0)
	if serviceDue(func() bool { return false }, time.Time{}, time.Second, now) {
		t.Fatal("due without demand")
	}
	if !serviceDue(func() bool { return true }, time.Time{}, time.Second, now) {
		t.Fatal("never-run service not due")
	}
	if serviceDue(func() bool { return true }, now.Add(-4*time.Second), 5*time.Second, now) {
		t.Fatal("due early")
	}
	if got := serviceWait(now.Add(-4800*time.Millisecond), 5*time.Second, now); got != 200*time.Millisecond {
		t.Fatalf("wait = %s", got)
	}
	if got := serviceWait(now, 15*time.Second, now); got != serviceDemandPoll {
		t.Fatalf("wait = %s, want the demand poll", got)
	}
}
