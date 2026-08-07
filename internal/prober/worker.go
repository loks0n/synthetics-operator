// Package prober subscribes to NATS for probe jobs, executes them, and
// publishes results. Genuinely stateless: each job carries the spec it needs,
// so a freshly started replica can execute the very first job it receives.
package prober

import (
	"context"

	"github.com/go-logr/logr"

	"github.com/loks0n/synthetics-operator/internal/natsbus"
	internalprobes "github.com/loks0n/synthetics-operator/internal/probes"
	"github.com/loks0n/synthetics-operator/internal/results"
)

// ResultPublisher publishes probe results back onto the bus.
type ResultPublisher interface {
	PublishProbeResult(ctx context.Context, msg results.ProbeResult) error
}

// Worker implements controller-runtime's Runnable. Per-kind executors own the
// complete job-to-result policy; Worker owns only dispatch and publication.
type Worker struct {
	Log       logr.Logger
	Bus       *natsbus.Client
	Publisher ResultPublisher
	executors map[results.Kind]internalprobes.Executor
}

// NewWorker wires the production executors behind the Worker dispatch seam.
func NewWorker(log logr.Logger, bus *natsbus.Client, publisher ResultPublisher) *Worker {
	return &Worker{
		Log:       log,
		Bus:       bus,
		Publisher: publisher,
		executors: defaultExecutors(),
	}
}

func defaultExecutors() map[results.Kind]internalprobes.Executor {
	return map[results.Kind]internalprobes.Executor{
		results.KindHTTPProbe:      internalprobes.HTTPExecutor{},
		results.KindDNSProbe:       internalprobes.DNSExecutor{},
		results.KindTCPProbe:       internalprobes.TCPExecutor{},
		results.KindK6Test:         nil,
		results.KindPlaywrightTest: nil,
	}
}

// NeedLeaderElection tells controller-runtime to run the Worker on every
// replica — probe workers are horizontally scalable via the NATS queue
// group, leader election would defeat the point.
func (*Worker) NeedLeaderElection() bool { return false }

// Start subscribes to probe jobs and blocks until ctx is cancelled.
func (w *Worker) Start(ctx context.Context) error {
	jobErr := make(chan error, 1)
	go func() { jobErr <- w.Bus.SubscribeProbeJobs(ctx, w.onJob) }()

	select {
	case <-ctx.Done():
		return nil
	case err := <-jobErr:
		return err
	}
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
