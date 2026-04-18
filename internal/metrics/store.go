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

type instruments struct {
	successGauge             apimetric.Float64ObservableGauge
	durationGauge            apimetric.Float64ObservableGauge
	lastRunGauge             apimetric.Float64ObservableGauge
	configErrorGauge         apimetric.Float64ObservableGauge
	tlsCertExpiryGauge       apimetric.Float64ObservableGauge
	httpStatusCodeGauge      apimetric.Float64ObservableGauge
	httpVersionGauge         apimetric.Float64ObservableGauge
	httpPhaseDurationGauge   apimetric.Float64ObservableGauge
	httpInfoGauge            apimetric.Float64ObservableGauge
	assertionResultGauge     apimetric.Float64ObservableGauge
	dnsSuccessGauge          apimetric.Float64ObservableGauge
	dnsResponseMsGauge       apimetric.Float64ObservableGauge
	dnsFirstAnswerValueGauge apimetric.Float64ObservableGauge
	dnsFirstAnswerTypeGauge  apimetric.Float64ObservableGauge
	dnsAnswerCountGauge      apimetric.Float64ObservableGauge
	dnsAuthorityCountGauge   apimetric.Float64ObservableGauge
	dnsAdditionalCountGauge  apimetric.Float64ObservableGauge
	resultsReceivedTotal     apimetric.Int64Counter
	resultsParseFailTotal    apimetric.Int64Counter
}

type Store struct {
	mu       sync.RWMutex
	probes   map[types.NamespacedName]ProbeState
	registry *promclient.Registry
	exporter *otelprom.Exporter
	provider *metric.MeterProvider
	instr    instruments
}

func NewStore() (*Store, error) {
	registry := promclient.NewRegistry()
	exporter, err := otelprom.New(otelprom.WithRegisterer(registry))
	if err != nil {
		return nil, err
	}
	provider := metric.NewMeterProvider(metric.WithReader(exporter))
	meter := provider.Meter("synthetics-operator")

	instr, err := newInstruments(meter)
	if err != nil {
		return nil, err
	}

	store := &Store{
		probes:   map[types.NamespacedName]ProbeState{},
		registry: registry,
		exporter: exporter,
		provider: provider,
		instr:    instr,
	}

	if err := store.registerCallback(meter); err != nil {
		return nil, err
	}
	return store, nil
}

func newInstruments(meter apimetric.Meter) (instruments, error) {
	var instr instruments
	var err error

	if instr.successGauge, err = meter.Float64ObservableGauge("synthetics_probe_up"); err != nil {
		return instr, err
	}
	if instr.durationGauge, err = meter.Float64ObservableGauge("synthetics_probe_duration_ms"); err != nil {
		return instr, err
	}
	if instr.lastRunGauge, err = meter.Float64ObservableGauge("synthetics_last_run_timestamp"); err != nil {
		return instr, err
	}
	if instr.configErrorGauge, err = meter.Float64ObservableGauge("synthetics_probe_config_error"); err != nil {
		return instr, err
	}
	if instr.tlsCertExpiryGauge, err = meter.Float64ObservableGauge("synthetics_probe_tls_cert_expiry_timestamp_seconds"); err != nil {
		return instr, err
	}
	if instr.httpStatusCodeGauge, err = meter.Float64ObservableGauge("synthetics_probe_http_status_code"); err != nil {
		return instr, err
	}
	if instr.httpVersionGauge, err = meter.Float64ObservableGauge("synthetics_probe_http_version"); err != nil {
		return instr, err
	}
	if instr.httpPhaseDurationGauge, err = meter.Float64ObservableGauge("synthetics_probe_http_phase_duration_ms"); err != nil {
		return instr, err
	}
	if instr.assertionResultGauge, err = meter.Float64ObservableGauge("synthetics_probe_assertion_result",
		apimetric.WithDescription("Per-assertion pass (1) / fail (0) result for the last probe run")); err != nil {
		return instr, err
	}
	if instr.httpInfoGauge, err = meter.Float64ObservableGauge("synthetics_probe_http_info",
		apimetric.WithDescription("Static information about an HTTP probe (value is always 1)")); err != nil {
		return instr, err
	}
	if instr.dnsSuccessGauge, err = meter.Float64ObservableGauge("synthetics_dns_success"); err != nil {
		return instr, err
	}
	if instr.dnsResponseMsGauge, err = meter.Float64ObservableGauge("synthetics_dns_response_ms"); err != nil {
		return instr, err
	}
	if instr.dnsFirstAnswerValueGauge, err = meter.Float64ObservableGauge("synthetics_dns_response_first_answer_value"); err != nil {
		return instr, err
	}
	if instr.dnsFirstAnswerTypeGauge, err = meter.Float64ObservableGauge("synthetics_dns_response_first_answer_type"); err != nil {
		return instr, err
	}
	if instr.dnsAnswerCountGauge, err = meter.Float64ObservableGauge("synthetics_dns_response_answer_count"); err != nil {
		return instr, err
	}
	if instr.dnsAuthorityCountGauge, err = meter.Float64ObservableGauge("synthetics_dns_response_authority_count"); err != nil {
		return instr, err
	}
	if instr.dnsAdditionalCountGauge, err = meter.Float64ObservableGauge("synthetics_dns_response_additional_count"); err != nil {
		return instr, err
	}
	if instr.resultsReceivedTotal, err = meter.Int64Counter("synthetics_cronjob_results_received_total",
		apimetric.WithDescription("Total CronJob probe results received via NATS")); err != nil {
		return instr, err
	}
	if instr.resultsParseFailTotal, err = meter.Int64Counter("synthetics_cronjob_results_parse_failed_total",
		apimetric.WithDescription("Total CronJob probe result messages that failed to parse")); err != nil {
		return instr, err
	}
	return instr, nil
}

func (s *Store) registerCallback(meter apimetric.Meter) error {
	instr := s.instr
	_, err := meter.RegisterCallback(func(_ context.Context, observer apimetric.Observer) error {
		s.mu.RLock()
		defer s.mu.RUnlock()
		for name, state := range s.probes {
			s.observeProbe(observer, name, state, instr)
		}
		return nil
	}, instr.successGauge, instr.durationGauge, instr.lastRunGauge, instr.configErrorGauge,
		instr.tlsCertExpiryGauge, instr.httpStatusCodeGauge, instr.httpVersionGauge,
		instr.httpPhaseDurationGauge, instr.httpInfoGauge, instr.assertionResultGauge,
		instr.dnsSuccessGauge, instr.dnsResponseMsGauge, instr.dnsFirstAnswerValueGauge,
		instr.dnsFirstAnswerTypeGauge, instr.dnsAnswerCountGauge, instr.dnsAuthorityCountGauge,
		instr.dnsAdditionalCountGauge)
	return err
}

func (s *Store) observeProbe(observer apimetric.Observer, name types.NamespacedName, state ProbeState, instr instruments) {
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

	observer.ObserveFloat64(instr.successGauge, state.Success, apimetric.WithAttributes(
		append(attrs, attribute.String("reason", state.FailureReason))...))
	observer.ObserveFloat64(instr.durationGauge, state.DurationMilliseconds, apimetric.WithAttributes(attrs...))
	observer.ObserveFloat64(instr.lastRunGauge, state.LastRunTimestamp, apimetric.WithAttributes(attrs...))
	observer.ObserveFloat64(instr.configErrorGauge, state.ConfigError, apimetric.WithAttributes(attrs...))

	for _, ar := range state.AssertionResults {
		observer.ObserveFloat64(instr.assertionResultGauge, ar.Result, apimetric.WithAttributes(
			append(attrs, attribute.String("assertion", ar.Name), attribute.String("expr", ar.Expr))...))
	}

	switch kind {
	case "HTTPProbe":
		s.observeHTTP(observer, attrs, state, instr)
	case "DNSProbe":
		s.observeDNS(observer, attrs, state, instr)
	}
}

func (s *Store) observeHTTP(observer apimetric.Observer, attrs []attribute.KeyValue, state ProbeState, instr instruments) {
	observer.ObserveFloat64(instr.httpInfoGauge, 1, apimetric.WithAttributes(
		append(attrs, attribute.String("method", state.Method))...))
	observer.ObserveFloat64(instr.httpStatusCodeGauge, state.HTTPStatusCode, apimetric.WithAttributes(attrs...))
	observer.ObserveFloat64(instr.httpVersionGauge, state.HTTPVersion, apimetric.WithAttributes(attrs...))
	if state.TLSCertExpiry > 0 {
		observer.ObserveFloat64(instr.tlsCertExpiryGauge, state.TLSCertExpiry, apimetric.WithAttributes(attrs...))
	}
	for _, ph := range []struct {
		name string
		val  float64
	}{
		{"dns", state.HTTPPhaseDNSMs},
		{"connect", state.HTTPPhaseConnectMs},
		{"tls", state.HTTPPhaseTLSMs},
		{"processing", state.HTTPPhaseProcessingMs},
		{"transfer", state.HTTPPhaseTransferMs},
	} {
		observer.ObserveFloat64(instr.httpPhaseDurationGauge, ph.val, apimetric.WithAttributes(
			append(attrs, attribute.String("phase", ph.name))...))
	}
}

func (s *Store) observeDNS(observer apimetric.Observer, attrs []attribute.KeyValue, state ProbeState, instr instruments) {
	observer.ObserveFloat64(instr.dnsSuccessGauge, state.Success, apimetric.WithAttributes(attrs...))
	observer.ObserveFloat64(instr.dnsResponseMsGauge, state.DurationMilliseconds, apimetric.WithAttributes(attrs...))
	observer.ObserveFloat64(instr.dnsAnswerCountGauge, state.DNSAnswerCount, apimetric.WithAttributes(attrs...))
	observer.ObserveFloat64(instr.dnsAuthorityCountGauge, state.DNSAuthorityCount, apimetric.WithAttributes(attrs...))
	observer.ObserveFloat64(instr.dnsAdditionalCountGauge, state.DNSAdditionalCount, apimetric.WithAttributes(attrs...))
	if state.DNSFirstAnswerValue != "" {
		observer.ObserveFloat64(instr.dnsFirstAnswerValueGauge, 1, apimetric.WithAttributes(
			append(attrs, attribute.String("value", state.DNSFirstAnswerValue))...))
	}
	if state.DNSFirstAnswerType != "" {
		observer.ObserveFloat64(instr.dnsFirstAnswerTypeGauge, 1, apimetric.WithAttributes(
			append(attrs, attribute.String("type", state.DNSFirstAnswerType))...))
	}
}

// RecordCronJobResult ingests a parsed result from the NATS consumer and
// updates the probe's state in the store so it appears in metrics.
func (s *Store) RecordCronJobResult(ctx context.Context, name types.NamespacedName, kind string, success bool, durationMs int64, ts float64) {
	s.instr.resultsReceivedTotal.Add(ctx, 1, apimetric.WithAttributes(
		attribute.String("kind", kind),
		attribute.String("name", name.Name),
		attribute.String("namespace", name.Namespace),
	))
	successVal := float64(0)
	if success {
		successVal = 1
	}
	s.Upsert(name, ProbeState{
		Kind:                 kind,
		Success:              successVal,
		DurationMilliseconds: float64(durationMs),
		LastRunTimestamp:     ts,
	})
}

// RecordParseFailure increments the parse-failure counter for NATS messages.
func (s *Store) RecordParseFailure(ctx context.Context) {
	s.instr.resultsParseFailTotal.Add(ctx, 1)
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
