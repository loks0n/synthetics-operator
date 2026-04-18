package controllers

import (
	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
	"github.com/loks0n/synthetics-operator/internal/results"
)

// toDependsRefs converts a CR's API-type depends list to the wire form.
func toDependsRefs(in []syntheticsv1alpha1.DependencyRef) []results.DependencyRef {
	if len(in) == 0 {
		return nil
	}
	out := make([]results.DependencyRef, len(in))
	for i, d := range in {
		out[i] = results.DependencyRef{Kind: results.Kind(d.Kind), Name: d.Name}
	}
	return out
}

func toAssertions(in []syntheticsv1alpha1.Assertion) []results.Assertion {
	if len(in) == 0 {
		return nil
	}
	out := make([]results.Assertion, len(in))
	for i, a := range in {
		out[i] = results.Assertion{Name: a.Name, Expr: a.Expr}
	}
	return out
}

// httpProbeSpecUpdate builds a SpecUpdate message from an HTTPProbe.
func httpProbeSpecUpdate(p *syntheticsv1alpha1.HTTPProbe) results.SpecUpdate {
	payload := results.HTTPProbeSpecPayload{
		TimeoutMs:  p.Spec.Timeout.Milliseconds(),
		URL:        p.Spec.Request.URL,
		Method:     p.Spec.Request.Method,
		Headers:    p.Spec.Request.Headers,
		Body:       p.Spec.Request.Body,
		Assertions: toAssertions(p.Spec.Assertions),
	}
	if p.Spec.TLS != nil {
		payload.TLS = &results.TLSConfig{
			InsecureSkipVerify: p.Spec.TLS.InsecureSkipVerify,
			CACert:             p.Spec.TLS.CACert,
		}
	}
	return results.SpecUpdate{
		Kind:         results.KindHTTPProbe,
		Name:         p.Name,
		Namespace:    p.Namespace,
		Generation:   p.Generation,
		Suspend:      p.Spec.Suspend,
		IntervalMs:   p.Spec.Interval.Milliseconds(),
		Depends:      toDependsRefs(p.Spec.Depends),
		MetricLabels: p.Spec.MetricLabels,
		HTTPProbe:    &payload,
	}
}

// dnsProbeSpecUpdate builds a SpecUpdate message from a DNSProbe.
func dnsProbeSpecUpdate(p *syntheticsv1alpha1.DNSProbe) results.SpecUpdate {
	payload := results.DNSProbeSpecPayload{
		TimeoutMs:  p.Spec.Timeout.Milliseconds(),
		Name:       p.Spec.Query.Name,
		Type:       p.Spec.Query.Type,
		Resolver:   p.Spec.Query.Resolver,
		Assertions: toAssertions(p.Spec.Assertions),
	}
	return results.SpecUpdate{
		Kind:         results.KindDNSProbe,
		Name:         p.Name,
		Namespace:    p.Namespace,
		Generation:   p.Generation,
		Suspend:      p.Spec.Suspend,
		IntervalMs:   p.Spec.Interval.Milliseconds(),
		Depends:      toDependsRefs(p.Spec.Depends),
		MetricLabels: p.Spec.MetricLabels,
		DNSProbe:     &payload,
	}
}

// k6TestSpecUpdate builds a SpecUpdate for a K6Test. No executable payload —
// test execution is owned by the CronJob; the message exists so the metrics
// consumer learns about depends + metricLabels.
func k6TestSpecUpdate(t *syntheticsv1alpha1.K6Test) results.SpecUpdate {
	return results.SpecUpdate{
		Kind:         results.KindK6Test,
		Name:         t.Name,
		Namespace:    t.Namespace,
		Generation:   t.Generation,
		Suspend:      t.Spec.Suspend,
		IntervalMs:   t.Spec.Interval.Milliseconds(),
		Depends:      toDependsRefs(t.Spec.Depends),
		MetricLabels: t.Spec.MetricLabels,
	}
}

func playwrightTestSpecUpdate(t *syntheticsv1alpha1.PlaywrightTest) results.SpecUpdate {
	return results.SpecUpdate{
		Kind:         results.KindPlaywrightTest,
		Name:         t.Name,
		Namespace:    t.Namespace,
		Generation:   t.Generation,
		Suspend:      t.Spec.Suspend,
		IntervalMs:   t.Spec.Interval.Milliseconds(),
		Depends:      toDependsRefs(t.Spec.Depends),
		MetricLabels: t.Spec.MetricLabels,
	}
}

// tombstone builds a SpecUpdate marking the CR as deleted. Consumers remove
// all cached state for the identity.
func tombstone(kind results.Kind, namespace, name string) results.SpecUpdate {
	return results.SpecUpdate{
		Kind:      kind,
		Name:      name,
		Namespace: namespace,
		Deleted:   true,
	}
}
