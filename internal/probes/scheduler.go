package probes

import (
	"context"
	"hash/fnv"
	"math"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
)

type Scheduler struct {
	logger      logr.Logger
	executor    Executor
	dnsExecutor DNSExecutor
	pool        *WorkerPool
	mu          sync.Mutex
	probes      map[types.NamespacedName]*scheduledProbe
	started     bool
	startCtx    context.Context
}

type scheduledProbe struct {
	stop chan struct{}
}

func NewScheduler(logger logr.Logger, executor Executor, pool *WorkerPool, dnsExecutor DNSExecutor) *Scheduler {
	return &Scheduler{
		logger:      logger,
		executor:    executor,
		dnsExecutor: dnsExecutor,
		pool:        pool,
		probes:      make(map[types.NamespacedName]*scheduledProbe),
	}
}

func (s *Scheduler) Start(ctx context.Context) error {
	go func() {
		_ = s.pool.Start(ctx)
	}()

	s.mu.Lock()
	s.started = true
	s.startCtx = ctx
	s.mu.Unlock()

	<-ctx.Done()
	s.mu.Lock()
	for _, scheduled := range s.probes {
		close(scheduled.stop)
	}
	s.probes = map[types.NamespacedName]*scheduledProbe{}
	s.mu.Unlock()

	return nil
}

func (s *Scheduler) Register(probe *syntheticsv1alpha1.HTTPProbe) {
	name := types.NamespacedName{Namespace: probe.Namespace, Name: probe.Name}
	s.mu.Lock()
	defer s.mu.Unlock()

	if scheduled, ok := s.probes[name]; ok {
		close(scheduled.stop)
		delete(s.probes, name)
	}
	if !s.started {
		return
	}

	scheduled := &scheduledProbe{stop: make(chan struct{})}
	s.probes[name] = scheduled
	go s.runProbe(newStopContext(s.startCtx, scheduled.stop), probe.DeepCopy())
}

func (s *Scheduler) Unregister(name types.NamespacedName) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if scheduled, ok := s.probes[name]; ok {
		close(scheduled.stop)
		delete(s.probes, name)
	}
}

func (s *Scheduler) RegisterDNS(probe *syntheticsv1alpha1.DNSProbe) {
	name := types.NamespacedName{Namespace: probe.Namespace, Name: probe.Name}
	s.mu.Lock()
	defer s.mu.Unlock()

	if scheduled, ok := s.probes[name]; ok {
		close(scheduled.stop)
		delete(s.probes, name)
	}
	if !s.started {
		return
	}

	scheduled := &scheduledProbe{stop: make(chan struct{})}
	s.probes[name] = scheduled
	go s.runDNSProbe(newStopContext(s.startCtx, scheduled.stop), probe.DeepCopy())
}

func (s *Scheduler) runDNSProbe(ctx context.Context, probe *syntheticsv1alpha1.DNSProbe) {
	interval := probe.Spec.Interval.Duration
	offset := ProbeOffset(probe.Namespace, probe.Name, interval)
	timer := time.NewTimer(initialDelay(time.Now(), interval, offset))
	defer timer.Stop()

	job := newDNSProbeJob(probe, s.dnsExecutor)

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.pool.Enqueue(ctx, job)
			timer.Reset(interval)
		}
	}
}

func (s *Scheduler) runProbe(ctx context.Context, probe *syntheticsv1alpha1.HTTPProbe) {
	interval := probe.Spec.Interval.Duration
	offset := ProbeOffset(probe.Namespace, probe.Name, interval)
	timer := time.NewTimer(initialDelay(time.Now(), interval, offset))
	defer timer.Stop()

	job := newHTTPProbeJob(probe, s.executor)

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.pool.Enqueue(ctx, job)
			timer.Reset(interval)
		}
	}
}

func ProbeOffset(namespace, name string, interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(namespace + "/" + name))
	sum := int64(h.Sum64() & uint64(math.MaxInt64))
	return time.Duration(sum % interval.Nanoseconds())
}

func initialDelay(now time.Time, interval, offset time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	elapsed := time.Duration(now.UnixNano()) % interval
	delay := offset - elapsed
	if delay <= 0 {
		delay += interval
	}
	return delay
}

type stopContext struct {
	parent context.Context
	done   chan struct{}
	stop   <-chan struct{}
}

func newStopContext(parent context.Context, stop <-chan struct{}) context.Context {
	ctx := &stopContext{
		parent: parent,
		done:   make(chan struct{}),
		stop:   stop,
	}
	go func() {
		defer close(ctx.done)
		select {
		case <-parent.Done():
		case <-stop:
		}
	}()
	return ctx
}

func (c *stopContext) Deadline() (time.Time, bool) {
	return c.parent.Deadline()
}

func (c *stopContext) Done() <-chan struct{} {
	return c.done
}

func (c *stopContext) Err() error {
	select {
	case <-c.parent.Done():
		return c.parent.Err()
	default:
	}
	select {
	case <-c.stop:
		return context.Canceled
	default:
		return nil
	}
}

func (c *stopContext) Value(key any) any {
	return c.parent.Value(key)
}
