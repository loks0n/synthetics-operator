package probes

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
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

func TestSchedulerRegisterBeforeStartDropsProbe(t *testing.T) {
	pool := NewWorkerPool(logr.Discard(), 1, fixedExecutor{}, nil, nil)
	s := NewScheduler(logr.Discard(), pool)

	probe := &syntheticsv1alpha1.HttpProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec:       syntheticsv1alpha1.HttpProbeSpec{Interval: metav1.Duration{Duration: 30 * time.Second}},
	}
	s.Register(probe)

	s.mu.Lock()
	_, ok := s.probes[types.NamespacedName{Namespace: "default", Name: "test"}]
	s.mu.Unlock()

	if ok {
		t.Fatal("probe should not be registered before Start is called")
	}
}

func TestSchedulerUnregisterRemovesProbe(t *testing.T) {
	pool := NewWorkerPool(logr.Discard(), 1, fixedExecutor{}, nil, nil)
	s := NewScheduler(logr.Discard(), pool)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Start(ctx) }()
	waitStarted(t, s)

	key := types.NamespacedName{Namespace: "default", Name: "test"}
	probe := &syntheticsv1alpha1.HttpProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec:       syntheticsv1alpha1.HttpProbeSpec{Interval: metav1.Duration{Duration: 30 * time.Second}},
	}
	s.Register(probe)
	s.Unregister(key)

	s.mu.Lock()
	_, ok := s.probes[key]
	s.mu.Unlock()
	if ok {
		t.Fatal("probe should be removed after Unregister")
	}
}

func TestSchedulerReRegisterReplacesExisting(t *testing.T) {
	pool := NewWorkerPool(logr.Discard(), 1, fixedExecutor{}, nil, nil)
	s := NewScheduler(logr.Discard(), pool)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Start(ctx) }()
	waitStarted(t, s)

	key := types.NamespacedName{Namespace: "default", Name: "test"}
	probe := &syntheticsv1alpha1.HttpProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec:       syntheticsv1alpha1.HttpProbeSpec{Interval: metav1.Duration{Duration: 30 * time.Second}},
	}

	s.Register(probe)
	s.mu.Lock()
	first := s.probes[key]
	s.mu.Unlock()

	s.Register(probe)
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
