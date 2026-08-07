package probes

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

	"github.com/loks0n/synthetics-operator/internal/results"
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

func TestProbeOffsetForKindDistinguishesSameNamedProbes(t *testing.T) {
	interval := 30 * time.Second
	httpOffset := ProbeOffsetForKind(results.KindHTTPProbe, "default", "shared", interval)
	tcpOffset := ProbeOffsetForKind(results.KindTCPProbe, "default", "shared", interval)
	if httpOffset == tcpOffset {
		t.Fatal("expected kind-aware offsets for same-named probes")
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

// fakeJobPublisher captures jobs for assertions. Publisher calls are cheap;
// concurrent-safe via mutex.
type fakeJobPublisher struct {
	mu   sync.Mutex
	jobs []results.ProbeJob
}

func (f *fakeJobPublisher) PublishProbeJob(_ context.Context, msg results.ProbeJob) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs = append(f.jobs, msg)
	return nil
}

func TestSchedulerRegisterBeforeStartDropsProbe(t *testing.T) {
	s := NewScheduler(logr.Discard(), &fakeJobPublisher{})

	key := types.NamespacedName{Namespace: "default", Name: "test"}
	spec := results.SpecUpdate{Kind: results.KindHTTPProbe, Namespace: key.Namespace, Name: key.Name, IntervalMs: (30 * time.Second).Milliseconds()}
	s.Register(spec)

	s.mu.Lock()
	_, ok := s.probes[probeKey{kind: spec.Kind, name: key}]
	s.mu.Unlock()

	if ok {
		t.Fatal("probe should not be registered before Start is called")
	}
}

func TestSchedulerUnregisterRemovesProbe(t *testing.T) {
	s := NewScheduler(logr.Discard(), &fakeJobPublisher{})

	ctx := t.Context()
	go func() { _ = s.Start(ctx) }()
	waitStarted(t, s)

	key := types.NamespacedName{Namespace: "default", Name: "test"}
	spec := results.SpecUpdate{Kind: results.KindHTTPProbe, Namespace: key.Namespace, Name: key.Name, IntervalMs: (30 * time.Second).Milliseconds()}
	s.Register(spec)
	s.Unregister(spec.Kind, key)

	s.mu.Lock()
	_, ok := s.probes[probeKey{kind: spec.Kind, name: key}]
	s.mu.Unlock()
	if ok {
		t.Fatal("probe should be removed after Unregister")
	}
}

func TestSchedulerReRegisterReplacesExisting(t *testing.T) {
	s := NewScheduler(logr.Discard(), &fakeJobPublisher{})

	ctx := t.Context()
	go func() { _ = s.Start(ctx) }()
	waitStarted(t, s)

	key := types.NamespacedName{Namespace: "default", Name: "test"}

	spec := results.SpecUpdate{Kind: results.KindHTTPProbe, Namespace: key.Namespace, Name: key.Name, IntervalMs: (30 * time.Second).Milliseconds()}
	s.Register(spec)
	s.mu.Lock()
	first := s.probes[probeKey{kind: spec.Kind, name: key}]
	s.mu.Unlock()

	s.Register(spec)
	s.mu.Lock()
	second := s.probes[probeKey{kind: spec.Kind, name: key}]
	s.mu.Unlock()

	if first == second {
		t.Fatal("re-register should create a new scheduledProbe entry")
	}

	select {
	case <-first.stop:
	default:
		t.Fatal("old stop channel should be closed after re-register")
	}
}

func TestSchedulerKeepsSameNamedProbeKindsIndependent(t *testing.T) {
	s := NewScheduler(logr.Discard(), &fakeJobPublisher{})
	ctx := t.Context()
	go func() { _ = s.Start(ctx) }()
	waitStarted(t, s)

	name := types.NamespacedName{Namespace: "default", Name: "shared"}
	httpSpec := results.SpecUpdate{Kind: results.KindHTTPProbe, Namespace: name.Namespace, Name: name.Name, IntervalMs: 30000}
	tcpSpec := results.SpecUpdate{Kind: results.KindTCPProbe, Namespace: name.Namespace, Name: name.Name, IntervalMs: 30000}
	s.Register(httpSpec)
	s.Register(tcpSpec)

	s.mu.Lock()
	_, httpOK := s.probes[probeKey{kind: results.KindHTTPProbe, name: name}]
	_, tcpOK := s.probes[probeKey{kind: results.KindTCPProbe, name: name}]
	s.mu.Unlock()
	if !httpOK || !tcpOK {
		t.Fatalf("same-named kinds not independently registered: HTTP=%v TCP=%v", httpOK, tcpOK)
	}

	s.Unregister(results.KindTCPProbe, name)
	s.mu.Lock()
	_, httpOK = s.probes[probeKey{kind: results.KindHTTPProbe, name: name}]
	_, tcpOK = s.probes[probeKey{kind: results.KindTCPProbe, name: name}]
	s.mu.Unlock()
	if !httpOK || tcpOK {
		t.Fatalf("kind-scoped unregister failed: HTTP=%v TCP=%v", httpOK, tcpOK)
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
