package probes

import (
	"context"
	"time"

	"github.com/loks0n/synthetics-operator/internal/results"
)

const defaultProbeTimeout = 10 * time.Second

// Executor is the complete execution surface used by the prober. Each
// per-kind implementation owns payload validation, timeout policy, transport,
// assertions, result classification, and telemetry projection.
type Executor interface {
	Execute(context.Context, results.ProbeJob) results.ProbeResult
}

func runContext(ctx context.Context, timeoutMs int64) (context.Context, context.CancelFunc) {
	timeout := time.Duration(timeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

func probeResult(job results.ProbeJob) results.ProbeResult {
	spec := job.Spec
	return results.ProbeResult{
		Kind:       spec.Kind,
		Name:       spec.Name,
		Namespace:  spec.Namespace,
		Generation: spec.Generation,
		Timestamp:  job.ScheduledAt,
	}
}

func configErrorResult(job results.ProbeJob) results.ProbeResult {
	result := probeResult(job)
	result.Result = "config_error"
	return result
}
