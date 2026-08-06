// Package prober subscribes to NATS for probe jobs, executes them, and
// publishes results. Genuinely stateless: each job carries the spec it needs,
// so a freshly started replica can execute the very first job it receives.
package prober

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
	"github.com/loks0n/synthetics-operator/internal/natsbus"
	internalprobes "github.com/loks0n/synthetics-operator/internal/probes"
	"github.com/loks0n/synthetics-operator/internal/results"
)

// ResultPublisher publishes probe results back onto the bus.
type ResultPublisher interface {
	PublishProbeResult(ctx context.Context, msg results.ProbeResult) error
}

// Worker implements controller-runtime's Runnable. Configure with a NATS
// bus client and Start returns when the context is cancelled.
type Worker struct {
	Log       logr.Logger
	Bus       *natsbus.Client
	Publisher ResultPublisher
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
	spec := job.Spec
	if spec.Kind != results.KindHTTPProbe && spec.Kind != results.KindDNSProbe && spec.Kind != results.KindTCPProbe {
		return // workers only execute probes, not tests
	}
	// The scheduler unregisters suspended probes, so this should not arrive;
	// it covers a suspend landing between publish and delivery.
	if spec.Suspend {
		return
	}

	res := w.execute(ctx, spec, job)
	w.Log.Info("probed",
		"kind", spec.Kind, "namespace", spec.Namespace, "name", spec.Name,
		"result", res.Result, "failed_assertion", res.FailedAssertion,
		"duration_ms", res.DurationMs)
	if err := w.Publisher.PublishProbeResult(ctx, res); err != nil {
		w.Log.Error(err, "publish probe result", "kind", spec.Kind, "namespace", spec.Namespace, "name", spec.Name)
	}
}

func (w *Worker) execute(ctx context.Context, spec results.SpecUpdate, job results.ProbeJob) results.ProbeResult {
	switch spec.Kind {
	case results.KindHTTPProbe:
		return w.executeHTTP(ctx, spec, job)
	case results.KindDNSProbe:
		return w.executeDNS(ctx, spec, job)
	case results.KindTCPProbe:
		return w.executeTCP(ctx, spec, job)
	case results.KindK6Test, results.KindPlaywrightTest:
		// Workers don't execute test kinds — CronJob pods do.
	}
	return results.ProbeResult{
		Kind:      spec.Kind,
		Name:      spec.Name,
		Namespace: spec.Namespace,
		Timestamp: time.Now(),
		Result:    "config_error",
	}
}

func (w *Worker) executeTCP(ctx context.Context, spec results.SpecUpdate, job results.ProbeJob) results.ProbeResult {
	payload := spec.TCPProbe
	if payload == nil {
		return results.ProbeResult{
			Kind: spec.Kind, Name: spec.Name, Namespace: spec.Namespace,
			Generation: spec.Generation, Timestamp: time.Now(), Result: "config_error",
		}
	}

	timeout := time.Duration(payload.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	probe := w.tcpProbeFromPayload(spec, payload)
	r := (internalprobes.TCPExecutor{}).Execute(runCtx, probe)
	out := results.ProbeResult{
		Kind: spec.Kind, Name: spec.Name, Namespace: spec.Namespace,
		Generation: spec.Generation, Timestamp: job.ScheduledAt,
		DurationMs: r.Duration.Milliseconds(), TCPHost: payload.Host, TCPPort: payload.Port,
	}
	switch {
	case r.ConfigError:
		out.Result = "config_error"
	case r.ConnectErr != nil:
		out.Result = internalprobes.ClassifyTCPConnect(r.ConnectErr)
	case len(payload.Assertions) > 0:
		out.Result, out.FailedAssertion, out.AssertionResults = internalprobes.EvalTCPAssertions(r, toInternalAssertions(payload.Assertions))
	default:
		out.Result = "ok"
	}
	return out
}

func (w *Worker) executeHTTP(ctx context.Context, spec results.SpecUpdate, job results.ProbeJob) results.ProbeResult {
	payload := spec.HTTPProbe
	if payload == nil {
		return results.ProbeResult{
			Kind:       spec.Kind,
			Name:       spec.Name,
			Namespace:  spec.Namespace,
			Generation: spec.Generation,
			Timestamp:  time.Now(),
			Result:     "config_error",
		}
	}

	timeout := time.Duration(payload.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	probe := w.httpProbeFromPayload(spec, payload)
	exec := internalprobes.HTTPExecutor{}
	r := exec.Execute(runCtx, probe)

	out := results.ProbeResult{
		Kind:                  spec.Kind,
		Name:                  spec.Name,
		Namespace:             spec.Namespace,
		Generation:            spec.Generation,
		Timestamp:             job.ScheduledAt,
		DurationMs:            r.Duration.Milliseconds(),
		URL:                   payload.URL,
		Method:                strings.ToUpper(payload.Method),
		HTTPStatusCode:        r.StatusCode,
		HTTPVersion:           r.HTTPVersion,
		HTTPPhaseDNSMs:        r.PhaseDNSMs,
		HTTPPhaseConnectMs:    r.PhaseConnectMs,
		HTTPPhaseTLSMs:        r.PhaseTLSMs,
		HTTPPhaseProcessingMs: r.PhaseProcessingMs,
		HTTPPhaseTransferMs:   r.PhaseTransferMs,
	}
	if r.CertExpiryTime != nil {
		out.TLSCertExpiryUnix = r.CertExpiryTime.Unix()
	}

	switch {
	case r.ConfigError:
		out.Result = "config_error"
	case r.TransportErr != nil:
		out.Result = internalprobes.ClassifyHTTPTransport(r.TransportErr)
	case len(payload.Assertions) > 0:
		out.Result, out.FailedAssertion, out.AssertionResults = internalprobes.EvalHTTPAssertions(r, toInternalAssertions(payload.Assertions))
	default:
		out.Result = "ok"
	}
	return out
}

func (w *Worker) executeDNS(ctx context.Context, spec results.SpecUpdate, job results.ProbeJob) results.ProbeResult {
	payload := spec.DNSProbe
	if payload == nil {
		return results.ProbeResult{
			Kind:       spec.Kind,
			Name:       spec.Name,
			Namespace:  spec.Namespace,
			Generation: spec.Generation,
			Timestamp:  time.Now(),
			Result:     "config_error",
		}
	}

	timeout := time.Duration(payload.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	probe := w.dnsProbeFromPayload(spec, payload)
	r := internalprobes.DNSExecutor{}.Execute(runCtx, probe)

	out := results.ProbeResult{
		Kind:                spec.Kind,
		Name:                spec.Name,
		Namespace:           spec.Namespace,
		Generation:          spec.Generation,
		Timestamp:           job.ScheduledAt,
		DurationMs:          r.Duration.Milliseconds(),
		DNSFirstAnswerValue: r.FirstAnswerValue,
		DNSFirstAnswerType:  r.FirstAnswerType,
		DNSAnswerCount:      r.AnswerCount,
		DNSAuthorityCount:   r.AuthorityCount,
		DNSAdditionalCount:  r.AdditionalCount,
	}

	switch {
	case r.ConfigError:
		out.Result = "config_error"
	case r.ResolverErr != nil:
		out.Result = "dns_failed"
	case len(payload.Assertions) > 0:
		out.Result, out.FailedAssertion, out.AssertionResults = internalprobes.EvalDNSAssertions(r, toInternalAssertions(payload.Assertions))
	default:
		out.Result = "ok"
	}
	return out
}

func (w *Worker) httpProbeFromPayload(spec results.SpecUpdate, payload *results.HTTPProbeSpecPayload) *syntheticsv1alpha1.HTTPProbe {
	probe := &syntheticsv1alpha1.HTTPProbe{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.Name,
			Namespace: spec.Namespace,
		},
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Timeout: metav1.Duration{Duration: time.Duration(payload.TimeoutMs) * time.Millisecond},
			Request: syntheticsv1alpha1.HTTPRequestSpec{
				URL:     payload.URL,
				Method:  payload.Method,
				Headers: payload.Headers,
				Body:    payload.Body,
			},
		},
	}
	if payload.TLS != nil {
		probe.Spec.TLS = &syntheticsv1alpha1.TLSConfig{
			InsecureSkipVerify: payload.TLS.InsecureSkipVerify,
			CACert:             payload.TLS.CACert,
		}
	}
	return probe
}

func (w *Worker) dnsProbeFromPayload(spec results.SpecUpdate, payload *results.DNSProbeSpecPayload) *syntheticsv1alpha1.DNSProbe {
	return &syntheticsv1alpha1.DNSProbe{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.Name,
			Namespace: spec.Namespace,
		},
		Spec: syntheticsv1alpha1.DNSProbeSpec{
			Timeout: metav1.Duration{Duration: time.Duration(payload.TimeoutMs) * time.Millisecond},
			Query: syntheticsv1alpha1.DNSQuery{
				Name:     payload.Name,
				Type:     payload.Type,
				Resolver: payload.Resolver,
			},
		},
	}
}

func (w *Worker) tcpProbeFromPayload(spec results.SpecUpdate, payload *results.TCPProbeSpecPayload) *syntheticsv1alpha1.TCPProbe {
	return &syntheticsv1alpha1.TCPProbe{
		ObjectMeta: metav1.ObjectMeta{Name: spec.Name, Namespace: spec.Namespace},
		Spec: syntheticsv1alpha1.TCPProbeSpec{
			Timeout: metav1.Duration{Duration: time.Duration(payload.TimeoutMs) * time.Millisecond},
			Target:  syntheticsv1alpha1.TCPTarget{Host: payload.Host, Port: payload.Port},
		},
	}
}

func toInternalAssertions(in []results.Assertion) []syntheticsv1alpha1.Assertion {
	out := make([]syntheticsv1alpha1.Assertion, len(in))
	for i, a := range in {
		out[i] = syntheticsv1alpha1.Assertion{Name: a.Name, Expr: a.Expr}
	}
	return out
}

// silence unused; exposed so linter doesn't complain if these helpers go
// unused in subsequent refactors.
var (
	_ = http.MethodGet
	_ = errors.New
	_ types.NamespacedName
)
