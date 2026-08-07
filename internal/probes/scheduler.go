package probes

import (
	"context"
	"hash/fnv"
	"math"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

	"github.com/loks0n/synthetics-operator/internal/results"
)

// JobPublisher is the subset of the NATS bus the scheduler needs to publish
// probe jobs. Defined as an interface so tests can inject a fake.
type JobPublisher interface {
	PublishProbeJob(ctx context.Context, msg results.ProbeJob) error
}

// Scheduler maintains a timer per registered probe and publishes a ProbeJob
// to NATS each tick. Probe-workers pick the job up via a queue-group
// subscription, so each tick is handled exactly once regardless of worker
// count.
type Scheduler struct {
	logger    logr.Logger
	publisher JobPublisher
	mu        sync.Mutex
	probes    map[probeKey]*scheduledProbe
	started   bool
	startCtx  context.Context
}

type probeKey struct {
	kind results.Kind
	name types.NamespacedName
}

type scheduledProbe struct {
	spec     results.SpecUpdate
	interval time.Duration
	stop     chan struct{}
}

func NewScheduler(logger logr.Logger, publisher JobPublisher) *Scheduler {
	return &Scheduler{
		logger:    logger,
		publisher: publisher,
		probes:    make(map[probeKey]*scheduledProbe),
	}
}

func (s *Scheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	s.started = true
	s.startCtx = ctx
	s.mu.Unlock()

	<-ctx.Done()
	s.mu.Lock()
	for _, scheduled := range s.probes {
		close(scheduled.stop)
	}
	s.probes = map[probeKey]*scheduledProbe{}
	s.mu.Unlock()

	return nil
}

// Register adds (or replaces) a scheduled probe. Re-registering with the
// same key resets the timer. Called by reconcilers whenever a probe's spec
// changes; both the spec and its interval may change across calls. The spec
// is held so every published job can carry it.
func (s *Scheduler) Register(spec results.SpecUpdate) {
	key := probeKey{
		kind: spec.Kind,
		name: types.NamespacedName{Namespace: spec.Namespace, Name: spec.Name},
	}
	interval := time.Duration(spec.IntervalMs) * time.Millisecond
	s.mu.Lock()
	defer s.mu.Unlock()

	if scheduled, ok := s.probes[key]; ok {
		close(scheduled.stop)
		delete(s.probes, key)
	}
	if !s.started {
		return
	}

	scheduled := &scheduledProbe{
		spec:     spec,
		interval: interval,
		stop:     make(chan struct{}),
	}
	s.probes[key] = scheduled
	go s.runLoop(newStopContext(s.startCtx, scheduled.stop), key, scheduled)
}

// Unregister stops the scheduled probe if present.
func (s *Scheduler) Unregister(kind results.Kind, name types.NamespacedName) {
	key := probeKey{kind: kind, name: name}
	s.mu.Lock()
	defer s.mu.Unlock()
	if scheduled, ok := s.probes[key]; ok {
		close(scheduled.stop)
		delete(s.probes, key)
	}
}

func (s *Scheduler) runLoop(ctx context.Context, key probeKey, scheduled *scheduledProbe) {
	offset := ProbeOffsetForKind(key.kind, key.name.Namespace, key.name.Name, scheduled.interval)
	timer := time.NewTimer(initialDelay(time.Now(), scheduled.interval, offset))
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-timer.C:
			msg := results.ProbeJob{
				Spec:        scheduled.spec,
				ScheduledAt: now,
			}
			if err := s.publisher.PublishProbeJob(ctx, msg); err != nil {
				s.logger.Error(err, "publish probe job", "kind", scheduled.spec.Kind, "namespace", key.name.Namespace, "name", key.name.Name)
			}
			timer.Reset(scheduled.interval)
		}
	}
}

// ProbeOffset retains the stable namespace/name offset used by CronJob-backed
// tests.
func ProbeOffset(namespace, name string, interval time.Duration) time.Duration {
	return probeOffsetHash(namespace+"/"+name, interval)
}

// ProbeOffsetForKind includes the probe kind so same-named lightweight probes
// do not share an execution offset.
func ProbeOffsetForKind(kind results.Kind, namespace, name string, interval time.Duration) time.Duration {
	return probeOffsetHash(string(kind)+"/"+namespace+"/"+name, interval)
}

func probeOffsetHash(identity string, interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(identity))
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

func (c *stopContext) Deadline() (time.Time, bool) { return c.parent.Deadline() }
func (c *stopContext) Done() <-chan struct{}       { return c.done }
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
func (c *stopContext) Value(key any) any { return c.parent.Value(key) }
