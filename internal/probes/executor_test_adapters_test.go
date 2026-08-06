package probes

import (
	"context"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
	"github.com/loks0n/synthetics-operator/internal/results"
)

// These adapters keep transport-focused tests concise. Production execution
// enters through Executor.Execute with wire data and never reconstructs CRs.
func (e HTTPExecutor) executeProbe(ctx context.Context, probe *syntheticsv1alpha1.HTTPProbe) httpResult {
	request := httpRequest{
		URL:     probe.Spec.Request.URL,
		Method:  probe.Spec.Request.Method,
		Headers: probe.Spec.Request.Headers,
		Body:    probe.Spec.Request.Body,
	}
	if probe.Spec.TLS != nil {
		request.TLS = &results.TLSConfig{
			InsecureSkipVerify: probe.Spec.TLS.InsecureSkipVerify,
			CACert:             probe.Spec.TLS.CACert,
		}
	}
	return e.executeRequest(ctx, request)
}

func (e DNSExecutor) executeProbe(ctx context.Context, probe *syntheticsv1alpha1.DNSProbe) dnsResult {
	return e.executeQuery(ctx, dnsQuery{
		Name: probe.Spec.Query.Name, Type: probe.Spec.Query.Type, Resolver: probe.Spec.Query.Resolver,
	})
}

func (e TCPExecutor) executeProbe(ctx context.Context, probe *syntheticsv1alpha1.TCPProbe) tcpResult {
	return e.connect(ctx, tcpTarget{Host: probe.Spec.Target.Host, Port: probe.Spec.Target.Port})
}
