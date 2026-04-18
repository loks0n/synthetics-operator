package probes

import (
	"context"
	"hash/fnv"
	"math"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
)

type Scheduler struct {
	logger   logr.Logger
	pool     *WorkerPool
	mu       sync.Mutex
	probes   map[types.NamespacedName]*scheduledProbe
	started  bool
	startCtx context.Context
}

type scheduledProbe struct {
	stop chan struct{}
}

func NewScheduler(logger logr.Logger, pool *WorkerPool) *Scheduler {
	return &Scheduler{
		logger: logger,
		pool:   pool,
		probes: make(map[types.NamespacedName]*scheduledProbe),
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

func (s *Scheduler) Register(job Job) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if scheduled, ok := s.probes[job.Key]; ok {
		close(scheduled.stop)
		delete(s.probes, job.Key)
	}
	if !s.started {
		return
	}

	scheduled := &scheduledProbe{stop: make(chan struct{})}
	s.probes[job.Key] = scheduled
	go s.runLoop(newStopContext(s.startCtx, scheduled.stop), job)
}

func (s *Scheduler) Unregister(name types.NamespacedName) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if scheduled, ok := s.probes[name]; ok {
		close(scheduled.stop)
		delete(s.probes, name)
	}
}

func (s *Scheduler) runLoop(ctx context.Context, job Job) {
	offset := ProbeOffset(job.Key.Namespace, job.Key.Name, job.Interval)
	timer := time.NewTimer(initialDelay(time.Now(), job.Interval, offset))
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.pool.Enqueue(ctx, job)
			timer.Reset(job.Interval)
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
