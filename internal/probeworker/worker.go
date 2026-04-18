// Package probeworker is the Phase-14 probe-worker binary role: subscribes
// to NATS for spec updates and probe jobs, executes probes, publishes
// results. Stateless — the in-memory spec cache hydrates from the NATS
// stream at startup.
package probeworker

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
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

	mu    sync.RWMutex
	specs map[specKey]results.SpecUpdate
}

type specKey struct {
	kind      results.Kind
	namespace string
	name      string
}

// NeedLeaderElection tells controller-runtime to run the Worker on every
// replica — probe workers are horizontally scalable via the NATS queue
// group, leader election would defeat the point.
func (*Worker) NeedLeaderElection() bool { return false }

// Start subscribes to specs + jobs and blocks until ctx is cancelled.
func (w *Worker) Start(ctx context.Context) error {
	if w.specs == nil {
		w.specs = map[specKey]results.SpecUpdate{}
	}

	specErr := make(chan error, 1)
	jobErr := make(chan error, 1)

	go func() { specErr <- w.Bus.SubscribeSpecs(ctx, w.onSpec) }()
	go func() { jobErr <- w.Bus.SubscribeProbeJobs(ctx, w.onJob) }()

	select {
	case <-ctx.Done():
		return nil
	case err := <-specErr:
		return err
	case err := <-jobErr:
		return err
	}
}

func (w *Worker) onSpec(_ context.Context, msg results.SpecUpdate) {
	if msg.Kind != results.KindHTTPProbe && msg.Kind != results.KindDNSProbe {
		return // workers only execute probes, not tests
	}
	key := specKey{kind: msg.Kind, namespace: msg.Namespace, name: msg.Name}
	w.mu.Lock()
	defer w.mu.Unlock()
	if msg.Deleted {
		delete(w.specs, key)
		return
	}
	w.specs[key] = msg
}

func (w *Worker) onJob(ctx context.Context, job results.ProbeJob) {
	spec, ok := w.specLookup(job.Kind, job.Namespace, job.Name)
	if !ok {
		// Job arrived before we've cached the spec. This is rare and
		// transient; the next tick will hit a populated cache.
		w.Log.V(1).Info("no spec cached for job", "kind", job.Kind, "namespace", job.Namespace, "name", job.Name)
		return
	}
	if spec.Suspend {
		return
	}

	res := w.execute(ctx, spec, job)
	if err := w.Publisher.PublishProbeResult(ctx, res); err != nil {
		w.Log.Error(err, "publish probe result", "kind", job.Kind, "namespace", job.Namespace, "name", job.Name)
	}
}

func (w *Worker) specLookup(kind results.Kind, namespace, name string) (results.SpecUpdate, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	s, ok := w.specs[specKey{kind: kind, namespace: namespace, name: name}]
	return s, ok
}

func (w *Worker) execute(ctx context.Context, spec results.SpecUpdate, job results.ProbeJob) results.ProbeResult {
	switch spec.Kind {
	case results.KindHTTPProbe:
		return w.executeHTTP(ctx, spec, job)
	case results.KindDNSProbe:
		return w.executeDNS(ctx, spec, job)
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
