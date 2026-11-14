// Package prober subscribes to NATS for probe jobs, executes them, and
// publishes results. Genuinely stateless: each job carries the spec it needs,
// so a freshly started replica can execute the very first job it receives.
package prober

import (
	"context"
	"sync"

	"github.com/go-logr/logr"

	"github.com/loks0n/synthetics-operator/internal/natsbus"
	internalprobes "github.com/loks0n/synthetics-operator/internal/probes"
	"github.com/loks0n/synthetics-operator/internal/results"
)

// ResultPublisher publishes probe results back onto the bus.
type ResultPublisher interface {
	PublishProbeResult(ctx context.Context, msg results.ProbeResult) error
}

// DefaultConcurrency is how many probes one replica runs at once. Probes
// spend their time waiting on the network, so one at a time left a replica
// idle behind every slow target.
const DefaultConcurrency = 16

// Worker implements controller-runtime's Runnable. Per-kind executors own the
// complete job-to-result policy; Worker owns only dispatch and publication.
type Worker struct {
	Log       logr.Logger
	Bus       *natsbus.Client
	Publisher ResultPublisher
	// Concurrency caps the probes running at once; below 1 means 1.
	Concurrency int
	executors   map[results.Kind]internalprobes.Executor
	slots       chan struct{}
	running     sync.WaitGroup
}

// NewWorker wires the production executors behind the Worker dispatch seam.
func NewWorker(log logr.Logger, bus *natsbus.Client, publisher ResultPublisher, concurrency int) *Worker {
	return &Worker{
		Log:         log,
		Bus:         bus,
		Publisher:   publisher,
		Concurrency: concurrency,
		executors:   defaultExecutors(),
	}
}

func defaultExecutors() map[results.Kind]internalprobes.Executor {
	return map[results.Kind]internalprobes.Executor{
		results.KindHTTPProbe: internalprobes.HTTPExecutor{},
		results.KindDNSProbe:  internalprobes.DNSExecutor{},
		results.KindTCPProbe:  internalprobes.TCPExecutor{},
		// Tests execute in CronJob pods, not here. Heartbeats are inbound and
		// execute nowhere at all. All three are listed so that adding a kind
		// without deciding how the prober treats it fails the exhaustive lint.
		results.KindK6Test:         nil,
		results.KindPlaywrightTest: nil,
		results.KindHeartbeat:      nil,
	}
}

// NeedLeaderElection tells controller-runtime to run the Worker on every
// replica — probe workers are horizontally scalable via the NATS queue
// group, leader election would defeat the point.
func (*Worker) NeedLeaderElection() bool { return false }

// Start subscribes to probe jobs and blocks until ctx is cancelled and every
// job already delivered to this replica has been executed and published.
// The subscription is drained before returning, so no dispatch can start a
// probe after the wait for running probes begins.
func (w *Worker) Start(ctx context.Context) error {
	w.slots = make(chan struct{}, max(1, w.Concurrency))
	err := w.Bus.SubscribeProbeJobs(ctx, w.dispatch)
	w.running.Wait()
	return err
}

// dispatch runs a job in its own goroutine once a slot is free. While every
// slot is taken it blocks, and further jobs delivered to this replica wait in
// its NATS client buffer; they are not handed back to other replicas.
func (w *Worker) dispatch(ctx context.Context, job results.ProbeJob) {
	w.slots <- struct{}{}
	w.running.Go(func() {
		defer func() { <-w.slots }()
		w.onJob(ctx, job)
	})
}

func (w *Worker) onJob(ctx context.Context, job results.ProbeJob) {
	executors := w.executors
	if executors == nil {
		// Preserve direct struct construction for embedders while NewWorker
		// remains the production path.
		executors = defaultExecutors()
	}
	executor, ok := executors[job.Spec.Kind]
	if !ok || executor == nil {
		return // workers only execute probes, not tests
	}
	// The scheduler unregisters suspended probes, so this should not arrive;
	// it covers a suspend landing between publish and delivery.
	if job.Spec.Suspend {
		return
	}

	result := executor.Execute(ctx, job)
	w.Log.Info("probed",
		"kind", result.Kind, "namespace", result.Namespace, "name", result.Name,
		"result", result.Result, "failed_assertion", result.FailedAssertion,
		"duration_ms", result.DurationMs)
	if err := w.Publisher.PublishProbeResult(ctx, result); err != nil {
		w.Log.Error(err, "publish probe result", "kind", result.Kind, "namespace", result.Namespace, "name", result.Name)
	}
}
