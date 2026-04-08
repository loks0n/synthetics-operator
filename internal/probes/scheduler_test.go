package probes

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
)

func TestProbeOffsetIsStable(t *testing.T) {
	interval := 30 * time.Second
	a := ProbeOffset("default", "api-health", interval)
	b := ProbeOffset("default", "api-health", interval)

	if a != b {
		t.Fatalf("expected stable offset, got %v and %v", a, b)
	}
	if a < 0 || a >= interval {
		t.Fatalf("offset %v out of range", a)
	}
}

func TestProbeOffsetDifferentProbes(t *testing.T) {
	interval := 30 * time.Second
	a := ProbeOffset("default", "probe-a", interval)
	b := ProbeOffset("default", "probe-b", interval)
	if a == b {
		t.Fatal("expected different offsets for different probe names")
	}
}

func TestProbeOffsetZeroInterval(t *testing.T) {
	if ProbeOffset("default", "probe", 0) != 0 {
		t.Fatal("expected zero offset for zero interval")
	}
}

func TestInitialDelayWithinInterval(t *testing.T) {
	now := time.Unix(1710000000, 123)
	delay := initialDelay(now, 30*time.Second, 5*time.Second)
	if delay <= 0 || delay > 30*time.Second {
		t.Fatalf("delay %v outside expected bounds", delay)
	}
}

func makeJob(key types.NamespacedName) Job {
	return Job{
		Key:      key,
		Interval: 30 * time.Second,
		Timeout:  time.Second,
		Run:      func(_ context.Context) {},
	}
}

func TestSchedulerRegisterBeforeStartDropsProbe(t *testing.T) {
	pool := NewWorkerPool(logr.Discard(), 1)
	s := NewScheduler(logr.Discard(), pool)

	key := types.NamespacedName{Namespace: "default", Name: "test"}
	s.Register(makeJob(key))

	s.mu.Lock()
	_, ok := s.probes[key]
	s.mu.Unlock()

	if ok {
		t.Fatal("probe should not be registered before Start is called")
	}
}

func TestSchedulerUnregisterRemovesProbe(t *testing.T) {
	pool := NewWorkerPool(logr.Discard(), 1)
	s := NewScheduler(logr.Discard(), pool)

	ctx := t.Context()
	go func() { _ = s.Start(ctx) }()
	waitStarted(t, s)

	key := types.NamespacedName{Namespace: "default", Name: "test"}
	s.Register(makeJob(key))
	s.Unregister(key)

	s.mu.Lock()
	_, ok := s.probes[key]
	s.mu.Unlock()
	if ok {
		t.Fatal("probe should be removed after Unregister")
	}
}

func TestSchedulerReRegisterReplacesExisting(t *testing.T) {
	pool := NewWorkerPool(logr.Discard(), 1)
	s := NewScheduler(logr.Discard(), pool)

	ctx := t.Context()
	go func() { _ = s.Start(ctx) }()
	waitStarted(t, s)

	key := types.NamespacedName{Namespace: "default", Name: "test"}

	s.Register(makeJob(key))
	s.mu.Lock()
	first := s.probes[key]
	s.mu.Unlock()

	s.Register(makeJob(key))
	s.mu.Lock()
	second := s.probes[key]
	s.mu.Unlock()

	if first == second {
		t.Fatal("re-register should create a new scheduledProbe entry")
	}

	select {
	case <-first.stop:
		// expected: old goroutine's stop channel was closed
	default:
		t.Fatal("old stop channel should be closed after re-register")
	}
}

func waitStarted(t *testing.T, s *Scheduler) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		started := s.started
		s.mu.Unlock()
		if started {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("scheduler did not start within 1 second")
}
