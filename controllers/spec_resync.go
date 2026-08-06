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
}

// NeedLeaderElection ensures only the active controller replica re-publishes.
func (r *SpecResyncer) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable: one resync immediately, then one per
// interval until the context ends.
func (r *SpecResyncer) Start(ctx context.Context) error {
	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()

	r.resync(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.resync(ctx)
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
