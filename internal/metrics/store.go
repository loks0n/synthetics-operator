package metrics

import (
	"context"
	"net/http"
	"sync"
	"time"

	promclient "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	apimetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/metric"
	"k8s.io/apimachinery/pkg/types"
)

// AssertionResult holds the outcome of a single named assertion.
type AssertionResult struct {
	Name   string
	Expr   string
	Result float64 // 1 = pass, 0 = fail
}

type ProbeState struct {
	Kind                 string // "HTTPProbe" or "DNSProbe"; empty treated as "HTTPProbe"
	Success              float64
	FailureReason        string // reason for failure; empty when successful
	DurationMilliseconds float64
	LastRunTimestamp     float64
	ConfigError          float64
	TLSCertExpiry        float64 // Unix timestamp of leaf cert NotAfter; 0 if no TLS
	HTTPStatusCode       float64 // HTTP response status code; 0 if no response (error/timeout)
	HTTPVersion          float64 // HTTP version: 1.0, 1.1, 2.0, 3.0; 0 if no response
	URL                  string  // HTTP request URL; empty for DNS probes
	Method               string  // HTTP request method; empty for DNS probes
	// HTTP phase timings
	HTTPPhaseDNSMs        float64
	HTTPPhaseConnectMs    float64
	HTTPPhaseTLSMs        float64
	HTTPPhaseProcessingMs float64
	HTTPPhaseTransferMs   float64
	// Per-assertion results, populated when the probe has assertions configured.
	AssertionResults []AssertionResult
	// DNS-specific fields
	DNSFirstAnswerValue string
	DNSFirstAnswerType  string
	DNSAnswerCount      float64
	DNSAuthorityCount   float64
	DNSAdditionalCount  float64
}

type Store struct {
	mu       sync.RWMutex
	probes   map[types.NamespacedName]ProbeState
	registry *promclient.Registry
	exporter *otelprom.Exporter
	provider *metric.MeterProvider
}

func NewStore() (*Store, error) {
	registry := promclient.NewRegistry()
	exporter, err := otelprom.New(otelprom.WithRegisterer(registry))
	if err != nil {
		return nil, err
	}
	provider := metric.NewMeterProvider(metric.WithReader(exporter))
	meter := provider.Meter("synthetics-operator")

	store := &Store{
		probes:   map[types.NamespacedName]ProbeState{},
		registry: registry,
		exporter: exporter,
		provider: provider,
	}

	successGauge, err := meter.Float64ObservableGauge("synthetics_probe_up")
	if err != nil {
		return nil, err
	}
	durationGauge, err := meter.Float64ObservableGauge("synthetics_probe_duration_ms")
	if err != nil {
		return nil, err
	}
	lastRunGauge, err := meter.Float64ObservableGauge("synthetics_last_run_timestamp")
	if err != nil {
		return nil, err
	}
	configErrorGauge, err := meter.Float64ObservableGauge("synthetics_probe_config_error")
	if err != nil {
		return nil, err
	}
	tlsCertExpiryGauge, err := meter.Float64ObservableGauge("synthetics_probe_tls_cert_expiry_timestamp_seconds")
	if err != nil {
		return nil, err
	}
	httpStatusCodeGauge, err := meter.Float64ObservableGauge("synthetics_probe_http_status_code")
	if err != nil {
		return nil, err
	}
	httpVersionGauge, err := meter.Float64ObservableGauge("synthetics_probe_http_version")
	if err != nil {
		return nil, err
	}
	httpPhaseDurationGauge, err := meter.Float64ObservableGauge("synthetics_probe_http_phase_duration_ms")
	if err != nil {
		return nil, err
	}

	dnsSuccessGauge, err := meter.Float64ObservableGauge("synthetics_dns_success")
	if err != nil {
		return nil, err
	}
	dnsResponseMsGauge, err := meter.Float64ObservableGauge("synthetics_dns_response_ms")
	if err != nil {
		return nil, err
	}
	dnsFirstAnswerValueGauge, err := meter.Float64ObservableGauge("synthetics_dns_response_first_answer_value")
	if err != nil {
		return nil, err
	}
	dnsFirstAnswerTypeGauge, err := meter.Float64ObservableGauge("synthetics_dns_response_first_answer_type")
	if err != nil {
		return nil, err
	}
	dnsAnswerCountGauge, err := meter.Float64ObservableGauge("synthetics_dns_response_answer_count")
	if err != nil {
		return nil, err
	}
	dnsAuthorityCountGauge, err := meter.Float64ObservableGauge("synthetics_dns_response_authority_count")
	if err != nil {
		return nil, err
	}
	dnsAdditionalCountGauge, err := meter.Float64ObservableGauge("synthetics_dns_response_additional_count")
	if err != nil {
		return nil, err
	}
	assertionResultGauge, err := meter.Float64ObservableGauge("synthetics_probe_assertion_result",
		apimetric.WithDescription("Per-assertion pass (1) / fail (0) result for the last probe run"))
	if err != nil {
		return nil, err
	}
	httpInfoGauge, err := meter.Float64ObservableGauge("synthetics_probe_http_info",
		apimetric.WithDescription("Static information about an HTTP probe (value is always 1)"))
	if err != nil {
		return nil, err
	}

	_, err = meter.RegisterCallback(func(_ context.Context, observer apimetric.Observer) error {
		store.mu.RLock()
		defer store.mu.RUnlock()
		for name, state := range store.probes {
			kind := state.Kind
			if kind == "" {
				kind = "HTTPProbe"
			}
			attrs := []attribute.KeyValue{
				attribute.String("name", name.Name),
				attribute.String("namespace", name.Namespace),
				attribute.String("kind", kind),
			}
			if kind == "HTTPProbe" && state.URL != "" {
				attrs = append(attrs, attribute.String("url", state.URL))
			}

			// Shared gauges for all probe types.
			successAttrs := append(attrs, attribute.String("reason", state.FailureReason))
			observer.ObserveFloat64(successGauge, state.Success, apimetric.WithAttributes(successAttrs...))
			observer.ObserveFloat64(durationGauge, state.DurationMilliseconds, apimetric.WithAttributes(attrs...))
			observer.ObserveFloat64(lastRunGauge, state.LastRunTimestamp, apimetric.WithAttributes(attrs...))
			observer.ObserveFloat64(configErrorGauge, state.ConfigError, apimetric.WithAttributes(attrs...))

			for _, ar := range state.AssertionResults {
				aAttrs := append(attrs,
					attribute.String("assertion", ar.Name),
					attribute.String("expr", ar.Expr),
				)
				observer.ObserveFloat64(assertionResultGauge, ar.Result, apimetric.WithAttributes(aAttrs...))
			}

			if kind == "HTTPProbe" {
				infoAttrs := append(attrs,
					attribute.String("method", state.Method),
				)
				observer.ObserveFloat64(httpInfoGauge, 1, apimetric.WithAttributes(infoAttrs...))

				observer.ObserveFloat64(httpStatusCodeGauge, state.HTTPStatusCode, apimetric.WithAttributes(attrs...))
				observer.ObserveFloat64(httpVersionGauge, state.HTTPVersion, apimetric.WithAttributes(attrs...))
				if state.TLSCertExpiry > 0 {
					observer.ObserveFloat64(tlsCertExpiryGauge, state.TLSCertExpiry, apimetric.WithAttributes(attrs...))
				}
				phases := []struct {
					name string
					val  float64
				}{
					{"dns", state.HTTPPhaseDNSMs},
					{"connect", state.HTTPPhaseConnectMs},
					{"tls", state.HTTPPhaseTLSMs},
					{"processing", state.HTTPPhaseProcessingMs},
					{"transfer", state.HTTPPhaseTransferMs},
				}
				for _, ph := range phases {
					phAttrs := append(attrs, attribute.String("phase", ph.name))
					observer.ObserveFloat64(httpPhaseDurationGauge, ph.val, apimetric.WithAttributes(phAttrs...))
				}
			}

			if kind == "DNSProbe" {
				observer.ObserveFloat64(dnsSuccessGauge, state.Success, apimetric.WithAttributes(attrs...))
				observer.ObserveFloat64(dnsResponseMsGauge, state.DurationMilliseconds, apimetric.WithAttributes(attrs...))
				observer.ObserveFloat64(dnsAnswerCountGauge, state.DNSAnswerCount, apimetric.WithAttributes(attrs...))
				observer.ObserveFloat64(dnsAuthorityCountGauge, state.DNSAuthorityCount, apimetric.WithAttributes(attrs...))
				observer.ObserveFloat64(dnsAdditionalCountGauge, state.DNSAdditionalCount, apimetric.WithAttributes(attrs...))
				if state.DNSFirstAnswerValue != "" {
					valueAttrs := append(attrs, attribute.String("value", state.DNSFirstAnswerValue))
					observer.ObserveFloat64(dnsFirstAnswerValueGauge, 1, apimetric.WithAttributes(valueAttrs...))
				}
				if state.DNSFirstAnswerType != "" {
					typeAttrs := append(attrs, attribute.String("type", state.DNSFirstAnswerType))
					observer.ObserveFloat64(dnsFirstAnswerTypeGauge, 1, apimetric.WithAttributes(typeAttrs...))
				}
			}
		}
		return nil
	}, successGauge, durationGauge, lastRunGauge, configErrorGauge, tlsCertExpiryGauge, httpStatusCodeGauge, httpVersionGauge,
		httpPhaseDurationGauge, httpInfoGauge, assertionResultGauge,
		dnsSuccessGauge, dnsResponseMsGauge, dnsFirstAnswerValueGauge, dnsFirstAnswerTypeGauge,
		dnsAnswerCountGauge, dnsAuthorityCountGauge, dnsAdditionalCountGauge)
	if err != nil {
		return nil, err
	}

	return store, nil
}

func (s *Store) Upsert(name types.NamespacedName, state ProbeState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.probes[name] = state
}

func (s *Store) Delete(name types.NamespacedName) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.probes, name)
}

func (s *Store) Snapshot(name types.NamespacedName) (ProbeState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.probes[name]
	return state, ok
}

func (s *Store) Server(addr string) *Server {
	return &Server{
		addr: addr,
		handler: promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{
			EnableOpenMetrics: true,
		}),
	}
}

type Server struct {
	addr    string
	handler http.Handler
}

func (s *Server) Start(ctx context.Context) error {
	server := &http.Server{
		Addr:              s.addr,
		Handler:           s.handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
