// Package metricsconsumer is the Phase-14 metrics-consumer binary role:
// subscribes to NATS for spec updates + probe results + test results,
// populates an in-memory Store, and serves /metrics. Stateless with respect
// to the Kubernetes API — all state flows through NATS.
package metricsconsumer

import (
	"context"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
	"github.com/loks0n/synthetics-operator/internal/metrics"
	"github.com/loks0n/synthetics-operator/internal/natsbus"
	"github.com/loks0n/synthetics-operator/internal/results"
)

// Consumer binds a NATS client to an in-memory metrics Store. Implements
// controller-runtime's Runnable; Start returns when ctx is cancelled.
type Consumer struct {
	Log   logr.Logger
	Bus   *natsbus.Client
	Store *metrics.Store
}

// NeedLeaderElection runs on every replica; each consumer subscribes
// independently to the NATS streams and maintains identical state. Metrics
// scrapes hit any replica and get the same answer.
func (*Consumer) NeedLeaderElection() bool { return false }

func (c *Consumer) Start(ctx context.Context) error {
	specErr := make(chan error, 1)
	probeErr := make(chan error, 1)
	testErr := make(chan error, 1)
	pingErr := make(chan error, 1)

	specReady := make(chan struct{})
	go func() { specErr <- c.Bus.SubscribeSpecs(ctx, c.onSpec, natsbus.WithReady(specReady)) }()
	go func() { probeErr <- c.Bus.SubscribeProbeResults(ctx, c.onProbeResult) }()
	go func() { testErr <- c.Bus.SubscribeTestResults(ctx, c.onTestResult) }()
	go func() { pingErr <- c.Bus.SubscribeHeartbeatPings(ctx, c.onHeartbeatPing) }()

	// Ask for a resync rather than waiting out the controller's interval. A
	// consumer that restarts alone has an empty store, and every heartbeat in
	// it reads as pending until its spec arrives with the last-ping seed.
	//
	// Strictly after the spec subscription exists. The controller answers
	// within milliseconds and core NATS drops a message nobody is subscribed
	// to, so requesting first loses the very reply being asked for and falls
	// silently through to the periodic tick.
	select {
	case <-specReady:
		if err := c.Bus.RequestSpecResync(ctx); err != nil {
			c.Log.Error(err, "requesting spec resync")
		}
	case <-ctx.Done():
		return nil
	}

	select {
	case <-ctx.Done():
		return nil
	case err := <-specErr:
		return err
	case err := <-probeErr:
		return err
	case err := <-testErr:
		return err
	case err := <-pingErr:
		return err
	}
}

func (c *Consumer) onSpec(_ context.Context, msg results.SpecUpdate) {
	name := types.NamespacedName{Namespace: msg.Namespace, Name: msg.Name}
	kind := string(msg.Kind)
	if msg.Deleted {
		c.Store.Delete(kind, name)
		c.Store.ClearDepends(kind, name)
		c.Store.ClearMetricLabels(kind, name)
		return
	}
	c.Store.SetDepends(kind, name, toAPIDepends(msg.Depends))
	c.Store.SetMetricLabels(kind, name, msg.MetricLabels)

	if msg.Kind == results.KindHeartbeat && msg.Heartbeat != nil {
		c.Store.SetHeartbeatSpec(
			name,
			float64(msg.Heartbeat.PeriodMs)/1000,
			float64(msg.Heartbeat.GraceMs)/1000,
			msg.Suspend,
			float64(msg.Heartbeat.LastPingUnix),
			msg.Heartbeat.LastResult == syntheticsv1alpha1.HeartbeatResultFailed,
		)
	}
}

func (c *Consumer) onHeartbeatPing(ctx context.Context, msg results.HeartbeatPing) {
	name := types.NamespacedName{Namespace: msg.Namespace, Name: msg.Name}
	c.Store.RecordHeartbeatPing(ctx, name, msg.Failed, msg.ExitCode, float64(msg.ReceivedAt.Unix()))
}

func (c *Consumer) onProbeResult(_ context.Context, msg results.ProbeResult) {
	name := types.NamespacedName{Namespace: msg.Namespace, Name: msg.Name}
	state := metrics.ProbeState{
		Kind:                  string(msg.Kind),
		Result:                metrics.Result(msg.Result),
		FailedAssertion:       msg.FailedAssertion,
		DurationMilliseconds:  float64(msg.DurationMs),
		LastRunTimestamp:      float64(msg.Timestamp.Unix()),
		HTTPStatusCode:        float64(msg.HTTPStatusCode),
		HTTPVersion:           msg.HTTPVersion,
		URL:                   msg.URL,
		Method:                msg.Method,
		HTTPPhaseDNSMs:        msg.HTTPPhaseDNSMs,
		HTTPPhaseConnectMs:    msg.HTTPPhaseConnectMs,
		HTTPPhaseTLSMs:        msg.HTTPPhaseTLSMs,
		HTTPPhaseProcessingMs: msg.HTTPPhaseProcessingMs,
		HTTPPhaseTransferMs:   msg.HTTPPhaseTransferMs,
		DNSFirstAnswerValue:   msg.DNSFirstAnswerValue,
		DNSFirstAnswerType:    msg.DNSFirstAnswerType,
		DNSAnswerCount:        float64(msg.DNSAnswerCount),
		DNSAuthorityCount:     float64(msg.DNSAuthorityCount),
		DNSAdditionalCount:    float64(msg.DNSAdditionalCount),
		TCPHost:               msg.TCPHost,
		TCPPort:               msg.TCPPort,
	}
	if msg.TLSCertExpiryUnix > 0 {
		state.TLSCertExpiry = float64(msg.TLSCertExpiryUnix)
	}
	for _, ar := range msg.AssertionResults {
		state.AssertionResults = append(state.AssertionResults, metrics.AssertionResult{
			Name:   ar.Name,
			Expr:   ar.Expr,
			Result: ar.Result,
		})
	}
	c.Store.Upsert(name, state)
}

func (c *Consumer) onTestResult(ctx context.Context, msg results.TestResult) {
	name := types.NamespacedName{Namespace: msg.Namespace, Name: msg.Name}
	ts := float64(msg.Timestamp.Unix())
	if msg.Kind == results.KindPlaywrightTest {
		c.Store.RecordPlaywrightResult(ctx, name, msg.Success, msg.DurationMs, ts, msg.Tests)
		return
	}
	c.Store.RecordTestResult(ctx, name, string(msg.Kind), msg.Success, msg.DurationMs, ts)
}

func toAPIDepends(in []results.DependencyRef) []syntheticsv1alpha1.DependencyRef {
	if len(in) == 0 {
		return nil
	}
	out := make([]syntheticsv1alpha1.DependencyRef, len(in))
	for i, d := range in {
		out[i] = syntheticsv1alpha1.DependencyRef{
			Kind: syntheticsv1alpha1.DependencyKind(d.Kind),
			Name: d.Name,
		}
	}
	return out
}
