package controllers

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
	"github.com/loks0n/synthetics-operator/internal/natsbus"
	"github.com/loks0n/synthetics-operator/internal/results"
)

// SpecResyncer periodically re-publishes a SpecUpdate for every live CR.
//
// Spec snapshots travel over core NATS, which is fire-and-forget: a spec
// published while the bus is still connecting — or before the metrics
// consumer has established its subscription, e.g. when all deployments
// restart together — is lost permanently. Probe jobs self-heal because the
// scheduler re-publishes them every tick and each job carries its own spec;
// specs were published exactly once per reconcile, so a lost spec left
// probes running but invisible to the metrics store until the controller
// was restarted by hand.
//
// Re-broadcasting is safe: consumers upsert specs by identity, so a
// duplicate is a no-op. Deletion remains reconcile-driven via tombstones.
type SpecResyncer struct {
	Client    client.Reader
	Publisher natsbus.Publisher
	Interval  time.Duration
	Log       logr.Logger
	// Requests, when set, lets a subscriber ask for a resync now instead of
	// waiting out Interval. A component that restarts alone — one metrics
	// replica, one heartbeat receiver — otherwise runs blind until the next
	// tick, and for the receiver that gap is visible to users as 404s on
	// tokens it hasn't learned yet.
	Requests ResyncRequests
	// TokenReader resolves each Heartbeat's token, which lives in a Secret
	// rather than on the CR. Heartbeats are skipped when it is nil.
	TokenReader HeartbeatTokenReader
}

// ResyncRequests is the subscribe side of synthetics.specs.resync.
type ResyncRequests interface {
	SubscribeSpecResyncRequests(ctx context.Context, handler func(context.Context), opts ...natsbus.SubscribeOption) error
}

// HeartbeatTokenReader resolves the current token for a Heartbeat. Satisfied
// by HeartbeatReconciler, which owns token provisioning.
type HeartbeatTokenReader interface {
	Token(ctx context.Context, beat *syntheticsv1alpha1.Heartbeat) (string, bool)
}

// NeedLeaderElection ensures only the active controller replica re-publishes.
func (r *SpecResyncer) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable: one resync immediately, then one per
// interval until the context ends.
func (r *SpecResyncer) Start(ctx context.Context) error {
	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()

	requested := make(chan struct{}, 1)
	requestErr := make(chan error, 1)
	if r.Requests != nil {
		ready := make(chan struct{})
		go func() {
			requestErr <- r.Requests.SubscribeSpecResyncRequests(ctx, func(context.Context) {
				// Non-blocking: several subscribers restarting together
				// should collapse into one resync, not queue one each.
				select {
				case requested <- struct{}{}:
				default:
				}
			}, natsbus.WithReady(ready))
		}()

		// Subscribe to resync requests before the initial broadcast. On a full
		// cold start that first broadcast can happen before consumers have their
		// own spec subscriptions; if their follow-up request also lands before
		// the controller is listening, core NATS drops it and they wait for the
		// periodic tick.
		select {
		case <-ready:
		case err := <-requestErr:
			if err != nil && ctx.Err() == nil {
				r.Log.Error(err, "subscribing to spec resync requests")
			}
		case <-ctx.Done():
			return nil
		}
	}

	r.resync(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.resync(ctx)
		case <-requested:
			r.Log.V(1).Info("resyncing specs on request")
			r.resync(ctx)
		case err := <-requestErr:
			if err != nil && ctx.Err() == nil {
				r.Log.Error(err, "subscribing to spec resync requests")
			}
		}
	}
}

func (r *SpecResyncer) resyncHTTPProbe(ctx context.Context, p *syntheticsv1alpha1.HTTPProbe) {
	if !p.DeletionTimestamp.IsZero() {
		return
	}
	headers, err := resolveRequestHeaders(ctx, r.Client, p.Namespace, p.Spec.Request)
	if err != nil {
		// Skip rather than republish with stripped headers; the reconciler owns
		// surfacing the error, and the next resync tick retries.
		r.Log.Error(err, "resolving headers for spec resync", "httpprobe", p.Name, "namespace", p.Namespace)
		return
	}
	r.publish(ctx, httpProbeSpecUpdate(p, headers))
}

// resyncHeartbeats republishes every Heartbeat spec. Unlike the other kinds
// this needs a Secret read per CR to recover the token, so it is skipped
// entirely when no TokenReader is wired.
func (r *SpecResyncer) resyncHeartbeats(ctx context.Context) {
	if r.TokenReader == nil {
		return
	}
	var heartbeats syntheticsv1alpha1.HeartbeatList
	if err := r.Client.List(ctx, &heartbeats); err != nil {
		r.Log.Error(err, "listing Heartbeats for spec resync")
		return
	}
	for i := range heartbeats.Items {
		beat := &heartbeats.Items[i]
		if !beat.DeletionTimestamp.IsZero() {
			continue
		}
		token, ok := r.TokenReader.Token(ctx, beat)
		if !ok {
			// The reconciler owns reporting why; republishing without a token
			// would evict a working token from the receiver's index.
			continue
		}
		r.publish(ctx, heartbeatSpecUpdate(beat, token))
	}
}

func (r *SpecResyncer) resync(ctx context.Context) {
	var httpProbes syntheticsv1alpha1.HTTPProbeList
	if err := r.Client.List(ctx, &httpProbes); err != nil {
		r.Log.Error(err, "listing HTTPProbes for spec resync")
	} else {
		for i := range httpProbes.Items {
			r.resyncHTTPProbe(ctx, &httpProbes.Items[i])
		}
	}

	var dnsProbes syntheticsv1alpha1.DNSProbeList
	if err := r.Client.List(ctx, &dnsProbes); err != nil {
		r.Log.Error(err, "listing DNSProbes for spec resync")
	} else {
		for i := range dnsProbes.Items {
			if p := &dnsProbes.Items[i]; p.DeletionTimestamp.IsZero() {
				r.publish(ctx, dnsProbeSpecUpdate(p))
			}
		}
	}

	var k6Tests syntheticsv1alpha1.K6TestList
	var tcpProbes syntheticsv1alpha1.TCPProbeList
	if err := r.Client.List(ctx, &tcpProbes); err != nil {
		r.Log.Error(err, "listing TCPProbes for spec resync")
	} else {
		for i := range tcpProbes.Items {
			if p := &tcpProbes.Items[i]; p.DeletionTimestamp.IsZero() {
				r.publish(ctx, tcpProbeSpecUpdate(p))
			}
		}
	}

	if err := r.Client.List(ctx, &k6Tests); err != nil {
		r.Log.Error(err, "listing K6Tests for spec resync")
	} else {
		for i := range k6Tests.Items {
			if t := &k6Tests.Items[i]; t.DeletionTimestamp.IsZero() {
				r.publish(ctx, k6TestSpecUpdate(t))
			}
		}
	}

	r.resyncHeartbeats(ctx)

	var playwrightTests syntheticsv1alpha1.PlaywrightTestList
	if err := r.Client.List(ctx, &playwrightTests); err != nil {
		r.Log.Error(err, "listing PlaywrightTests for spec resync")
	} else {
		for i := range playwrightTests.Items {
			if t := &playwrightTests.Items[i]; t.DeletionTimestamp.IsZero() {
				r.publish(ctx, playwrightTestSpecUpdate(t))
			}
		}
	}
}

func (r *SpecResyncer) publish(ctx context.Context, msg results.SpecUpdate) {
	if err := r.Publisher.PublishSpec(ctx, msg); err != nil {
		r.Log.Error(err, "re-publishing spec", "kind", msg.Kind, "namespace", msg.Namespace, "name", msg.Name)
	}
}
