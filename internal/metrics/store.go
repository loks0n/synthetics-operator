package metrics

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"

	promclient "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	apimetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/metric"
	"k8s.io/apimachinery/pkg/types"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
	"github.com/loks0n/synthetics-operator/internal/results"
)

// Result is the closed enum emitted on the `result` label of the
// synthetics_probe and synthetics_test gauges. It names what happened on the
// last run: ok on success, a failure category otherwise.
type Result string

const (
	ResultOK              Result = "ok"
	ResultConfigError     Result = "config_error"
	ResultDNSFailed       Result = "dns_failed"
	ResultConnectRefused  Result = "connect_refused"
	ResultConnectTimeout  Result = "connect_timeout"
	ResultTLSFailed       Result = "tls_failed"
	ResultRecvTimeout     Result = "recv_timeout"
	ResultAssertionFailed Result = "assertion_failed"
	ResultTestFailed      Result = "test_failed"
)

// successValue is the 0|1 value emitted on synthetics_probe and synthetics_test.
func (r Result) successValue() float64 {
	if r == ResultOK {
		return 1
	}
	return 0
}

// AssertionResult holds the outcome of a single named assertion.
type AssertionResult struct {
	Name   string
	Expr   string
	Result float64 // 1 = pass, 0 = fail
}

// ProbeState holds the last-run state of an HTTPProbe or DNSProbe.
type ProbeState struct {
	Kind                 string // "HTTPProbe" | "DNSProbe"
	Result               Result
	FailedAssertion      string // populated only when Result == ResultAssertionFailed
	DurationMilliseconds float64
	LastRunTimestamp     float64
	// HTTP-specific fields
	HTTPStatusCode        float64
	HTTPVersion           float64
	URL                   string
	Method                string
	TLSCertExpiry         float64 // Unix timestamp of leaf cert NotAfter; 0 if no TLS
	HTTPPhaseDNSMs        float64
	HTTPPhaseConnectMs    float64
	HTTPPhaseTLSMs        float64
	HTTPPhaseProcessingMs float64
	HTTPPhaseTransferMs   float64
	AssertionResults      []AssertionResult
	// DNS-specific fields
	DNSFirstAnswerValue string
	DNSFirstAnswerType  string
	DNSAnswerCount      float64
	DNSAuthorityCount   float64
	DNSAdditionalCount  float64
}

// TestState holds the last-run state of a K6Test or PlaywrightTest.
type TestState struct {
	Kind                 string // "K6Test" | "PlaywrightTest"
	Result               Result
	DurationMilliseconds float64
	LastRunTimestamp     float64
	// PlaywrightTest-specific per-test breakdown
	PlaywrightTests []results.TestCase
}

type instruments struct {
	// Probe family — HTTPProbe, DNSProbe
	probeGauge               apimetric.Float64ObservableGauge
	probeResultInfo          apimetric.Float64ObservableGauge
	probeSuppressedGauge     apimetric.Float64ObservableGauge
	probeDurationGauge       apimetric.Float64ObservableGauge
	probeLastRunGauge        apimetric.Float64ObservableGauge
	tlsCertExpiryGauge       apimetric.Float64ObservableGauge
	httpStatusCodeGauge      apimetric.Float64ObservableGauge
	httpVersionGauge         apimetric.Float64ObservableGauge
	httpPhaseDurationGauge   apimetric.Float64ObservableGauge
	httpInfoGauge            apimetric.Float64ObservableGauge
	assertionResultGauge     apimetric.Float64ObservableGauge
	dnsResponseMsGauge       apimetric.Float64ObservableGauge
	dnsFirstAnswerValueGauge apimetric.Float64ObservableGauge
	dnsFirstAnswerTypeGauge  apimetric.Float64ObservableGauge
	dnsAnswerCountGauge      apimetric.Float64ObservableGauge
	dnsAuthorityCountGauge   apimetric.Float64ObservableGauge
	dnsAdditionalCountGauge  apimetric.Float64ObservableGauge
	// Test family — K6Test, PlaywrightTest
	testGauge                apimetric.Float64ObservableGauge
	testResultInfo           apimetric.Float64ObservableGauge
	testSuppressedGauge      apimetric.Float64ObservableGauge
	testDurationGauge        apimetric.Float64ObservableGauge
	testLastRunGauge         apimetric.Float64ObservableGauge
	playwrightCasePassed     apimetric.Float64ObservableGauge
	playwrightCaseDurationMs apimetric.Float64ObservableGauge
	playwrightCasesTotal     apimetric.Float64ObservableGauge
	playwrightCasesPassed    apimetric.Float64ObservableGauge
	playwrightCasesFailed    apimetric.Float64ObservableGauge
	// Counters
	resultsReceivedTotal  apimetric.Int64Counter
	resultsParseFailTotal apimetric.Int64Counter
}

// TransitionFn is invoked when a probe or test's Result changes from one run
// to the next (including from "never run" to any outcome). Wired by main.go
// to an events.Notifier; nil in tests that don't exercise the event path.
type TransitionFn func(name types.NamespacedName, kind string, prev, next Result)

// crKey identifies a CR by kind + namespaced name. Used as the map key for
// per-CR ancillary state (depends list, user metric labels). Needed because
// names collide across kinds (HTTPProbe "foo" and DNSProbe "foo" can coexist
// in the same namespace).
type crKey struct {
	kind string
	name types.NamespacedName
}

// Legacy alias; kept so old dependsKey references don't need renaming.
type dependsKey = crKey

// Store is the in-process metrics state. Probes and tests live in separate
// maps so their metric families stay cleanly separated: probes use
// synthetics_probe_*, tests use synthetics_test_*. Depends + MetricLabels are
// sibling maps written by reconcilers on spec change, read by the observe
// callback.
type Store struct {
	mu                sync.RWMutex
	probes            map[types.NamespacedName]ProbeState
	tests             map[types.NamespacedName]TestState
	depends           map[crKey][]syntheticsv1alpha1.DependencyRef
	metricLabels      map[crKey]map[string]string
	registry          *promclient.Registry
	exporter          *otelprom.Exporter
	provider          *metric.MeterProvider
	instr             instruments
	OnProbeTransition TransitionFn
	OnTestTransition  TransitionFn
}

func NewStore() (*Store, error) {
	registry := promclient.NewRegistry()
	// WithoutScopeInfo drops the otel_scope_name / otel_scope_version /
	// otel_scope_schema_url labels that clutter every metric series.
	// WithoutTargetInfo drops the target_info gauge for the same reason.
	exporter, err := otelprom.New(
		otelprom.WithRegisterer(registry),
		otelprom.WithoutScopeInfo(),
		otelprom.WithoutTargetInfo(),
	)
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
		probes:       map[types.NamespacedName]ProbeState{},
		tests:        map[types.NamespacedName]TestState{},
		depends:      map[crKey][]syntheticsv1alpha1.DependencyRef{},
		metricLabels: map[crKey]map[string]string{},
		registry:     registry,
		exporter:     exporter,
		provider:     provider,
		instr:        instr,
	}

	if err := store.registerCallback(meter); err != nil {
		return nil, err
	}
	return store, nil
}

func newInstruments(meter apimetric.Meter) (instruments, error) {
	var instr instruments
	var err error

	// Probe family
	if instr.probeGauge, err = meter.Float64ObservableGauge("synthetics_probe",
		apimetric.WithDescription("Probe pass (1) / fail (0) for the last run. For the classification of the outcome, join against synthetics_probe_result_info.")); err != nil {
		return instr, err
	}
	if instr.probeResultInfo, err = meter.Float64ObservableGauge("synthetics_probe_result_info",
		apimetric.WithDescription("Info gauge (always 1) carrying the current result + failed_assertion labels for each probe. Designed to be joined with synthetics_probe; only the current result/assertion combination is emitted.")); err != nil {
		return instr, err
	}
	if instr.probeSuppressedGauge, err = meter.Float64ObservableGauge("synthetics_probe_suppressed",
		apimetric.WithDescription("1 when a failing probe is suppressed because a transitive dependency is failing. Labels name the deepest unhealthy dep.")); err != nil {
		return instr, err
	}
	if instr.probeDurationGauge, err = meter.Float64ObservableGauge("synthetics_probe_duration_ms"); err != nil {
		return instr, err
	}
	if instr.probeLastRunGauge, err = meter.Float64ObservableGauge("synthetics_probe_last_run_timestamp"); err != nil {
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
		apimetric.WithDescription("Per-assertion pass (1) / fail (0) for the last probe run")); err != nil {
		return instr, err
	}
	if instr.httpInfoGauge, err = meter.Float64ObservableGauge("synthetics_probe_http_info",
		apimetric.WithDescription("Static HTTPProbe configuration; value is always 1")); err != nil {
		return instr, err
	}
	if instr.dnsResponseMsGauge, err = meter.Float64ObservableGauge("synthetics_probe_dns_response_ms"); err != nil {
		return instr, err
	}
	if instr.dnsFirstAnswerValueGauge, err = meter.Float64ObservableGauge("synthetics_probe_dns_response_first_answer_value"); err != nil {
		return instr, err
	}
	if instr.dnsFirstAnswerTypeGauge, err = meter.Float64ObservableGauge("synthetics_probe_dns_response_first_answer_type"); err != nil {
		return instr, err
	}
	if instr.dnsAnswerCountGauge, err = meter.Float64ObservableGauge("synthetics_probe_dns_response_answer_count"); err != nil {
		return instr, err
	}
	if instr.dnsAuthorityCountGauge, err = meter.Float64ObservableGauge("synthetics_probe_dns_response_authority_count"); err != nil {
		return instr, err
	}
	if instr.dnsAdditionalCountGauge, err = meter.Float64ObservableGauge("synthetics_probe_dns_response_additional_count"); err != nil {
		return instr, err
	}

	// Test family
	if instr.testGauge, err = meter.Float64ObservableGauge("synthetics_test",
		apimetric.WithDescription("Test pass (1) / fail (0) for the last run. For the classification of the outcome, join against synthetics_test_result_info.")); err != nil {
		return instr, err
	}
	if instr.testResultInfo, err = meter.Float64ObservableGauge("synthetics_test_result_info",
		apimetric.WithDescription("Info gauge (always 1) carrying the current result label for each test. Designed to be joined with synthetics_test; only the current result is emitted.")); err != nil {
		return instr, err
	}
	if instr.testSuppressedGauge, err = meter.Float64ObservableGauge("synthetics_test_suppressed",
		apimetric.WithDescription("1 when a failing test is suppressed because a transitive dependency is failing. Labels name the deepest unhealthy dep.")); err != nil {
		return instr, err
	}
	if instr.testDurationGauge, err = meter.Float64ObservableGauge("synthetics_test_duration_ms"); err != nil {
		return instr, err
	}
	if instr.testLastRunGauge, err = meter.Float64ObservableGauge("synthetics_test_last_run_timestamp"); err != nil {
		return instr, err
	}
	if instr.playwrightCasePassed, err = meter.Float64ObservableGauge("synthetics_test_playwright_case_passed",
		apimetric.WithDescription("Per-case pass (1) / fail (0) for the last PlaywrightTest run")); err != nil {
		return instr, err
	}
	if instr.playwrightCaseDurationMs, err = meter.Float64ObservableGauge("synthetics_test_playwright_case_duration_ms",
		apimetric.WithDescription("Per-case duration in milliseconds for the last PlaywrightTest run")); err != nil {
		return instr, err
	}
	if instr.playwrightCasesTotal, err = meter.Float64ObservableGauge("synthetics_test_playwright_cases_total",
		apimetric.WithDescription("Total number of cases in the last PlaywrightTest run")); err != nil {
		return instr, err
	}
	if instr.playwrightCasesPassed, err = meter.Float64ObservableGauge("synthetics_test_playwright_cases_passed",
		apimetric.WithDescription("Number of passing cases in the last PlaywrightTest run")); err != nil {
		return instr, err
	}
	if instr.playwrightCasesFailed, err = meter.Float64ObservableGauge("synthetics_test_playwright_cases_failed",
		apimetric.WithDescription("Number of failing cases in the last PlaywrightTest run")); err != nil {
		return instr, err
	}

	// Counters
	if instr.resultsReceivedTotal, err = meter.Int64Counter("synthetics_test_results_received_total",
		apimetric.WithDescription("Total test results received via NATS")); err != nil {
		return instr, err
	}
	if instr.resultsParseFailTotal, err = meter.Int64Counter("synthetics_test_results_parse_failed_total",
		apimetric.WithDescription("Total test result messages that failed to parse")); err != nil {
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
		for name, state := range s.tests {
			s.observeTest(observer, name, state, instr)
		}
		return nil
	},
		// Probe family instruments
		instr.probeGauge, instr.probeResultInfo, instr.probeSuppressedGauge,
		instr.probeDurationGauge, instr.probeLastRunGauge,
		instr.tlsCertExpiryGauge, instr.httpStatusCodeGauge, instr.httpVersionGauge,
		instr.httpPhaseDurationGauge, instr.httpInfoGauge, instr.assertionResultGauge,
		instr.dnsResponseMsGauge, instr.dnsFirstAnswerValueGauge, instr.dnsFirstAnswerTypeGauge,
		instr.dnsAnswerCountGauge, instr.dnsAuthorityCountGauge, instr.dnsAdditionalCountGauge,
		// Test family instruments
		instr.testGauge, instr.testResultInfo, instr.testSuppressedGauge,
		instr.testDurationGauge, instr.testLastRunGauge,
		instr.playwrightCasePassed, instr.playwrightCaseDurationMs,
		instr.playwrightCasesTotal, instr.playwrightCasesPassed, instr.playwrightCasesFailed,
	)
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
	attrs = append(attrs, s.userLabelsLocked(kind, name)...)

	observer.ObserveFloat64(instr.probeGauge, state.Result.successValue(), apimetric.WithAttributes(attrs...))
	observer.ObserveFloat64(instr.probeResultInfo, 1, apimetric.WithAttributes(
		append(attrs,
			attribute.String("result", string(state.Result)),
			attribute.String("failed_assertion", state.FailedAssertion),
		)...))
	observer.ObserveFloat64(instr.probeDurationGauge, state.DurationMilliseconds, apimetric.WithAttributes(attrs...))
	observer.ObserveFloat64(instr.probeLastRunGauge, state.LastRunTimestamp, apimetric.WithAttributes(attrs...))

	if state.Result != ResultOK {
		if dep, ok := s.findUnhealthyDep(kind, name); ok {
			observer.ObserveFloat64(instr.probeSuppressedGauge, 1, apimetric.WithAttributes(
				append(attrs,
					attribute.String("unhealthy_dependency", dep.Name),
					attribute.String("unhealthy_dependency_kind", string(dep.Kind)),
				)...))
		}
	}

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

func (s *Store) observeTest(observer apimetric.Observer, name types.NamespacedName, state TestState, instr instruments) {
	attrs := []attribute.KeyValue{
		attribute.String("name", name.Name),
		attribute.String("namespace", name.Namespace),
		attribute.String("kind", state.Kind),
	}
	attrs = append(attrs, s.userLabelsLocked(state.Kind, name)...)

	observer.ObserveFloat64(instr.testGauge, state.Result.successValue(), apimetric.WithAttributes(attrs...))
	observer.ObserveFloat64(instr.testResultInfo, 1, apimetric.WithAttributes(
		append(attrs, attribute.String("result", string(state.Result)))...))
	observer.ObserveFloat64(instr.testDurationGauge, state.DurationMilliseconds, apimetric.WithAttributes(attrs...))
	observer.ObserveFloat64(instr.testLastRunGauge, state.LastRunTimestamp, apimetric.WithAttributes(attrs...))

	if state.Result != ResultOK {
		if dep, ok := s.findUnhealthyDep(state.Kind, name); ok {
			observer.ObserveFloat64(instr.testSuppressedGauge, 1, apimetric.WithAttributes(
				append(attrs,
					attribute.String("unhealthy_dependency", dep.Name),
					attribute.String("unhealthy_dependency_kind", string(dep.Kind)),
				)...))
		}
	}

	if state.Kind == string(results.KindPlaywrightTest) {
		s.observePlaywright(observer, attrs, state, instr)
	}
}

func (s *Store) observePlaywright(observer apimetric.Observer, attrs []attribute.KeyValue, state TestState, instr instruments) {
	var passed, failed float64
	for _, tc := range state.PlaywrightTests {
		passVal := float64(0)
		if tc.Passed {
			passVal = 1
			passed++
		} else {
			failed++
		}
		caseAttrs := append(attrs,
			attribute.String("suite", tc.Suite),
			attribute.String("test", tc.Test),
		)
		observer.ObserveFloat64(instr.playwrightCasePassed, passVal, apimetric.WithAttributes(caseAttrs...))
		observer.ObserveFloat64(instr.playwrightCaseDurationMs, float64(tc.DurationMs), apimetric.WithAttributes(caseAttrs...))
	}
	observer.ObserveFloat64(instr.playwrightCasesTotal, float64(len(state.PlaywrightTests)), apimetric.WithAttributes(attrs...))
	observer.ObserveFloat64(instr.playwrightCasesPassed, passed, apimetric.WithAttributes(attrs...))
	observer.ObserveFloat64(instr.playwrightCasesFailed, failed, apimetric.WithAttributes(attrs...))
}

// RecordTestResult ingests a K6Test result from the NATS consumer. For
// PlaywrightTest see RecordPlaywrightResult.
func (s *Store) RecordTestResult(ctx context.Context, name types.NamespacedName, kind string, success bool, durationMs int64, ts float64) {
	s.instr.resultsReceivedTotal.Add(ctx, 1, apimetric.WithAttributes(
		attribute.String("kind", kind),
		attribute.String("name", name.Name),
		attribute.String("namespace", name.Namespace),
	))
	s.UpsertTest(name, TestState{
		Kind:                 kind,
		Result:               resultFromSuccess(success),
		DurationMilliseconds: float64(durationMs),
		LastRunTimestamp:     ts,
	})
}

// RecordPlaywrightResult is the PlaywrightTest-flavoured RecordTestResult: it
// stores the per-test breakdown so synthetics_playwright_test_* gauges can be
// observed alongside synthetics_test_up.
func (s *Store) RecordPlaywrightResult(ctx context.Context, name types.NamespacedName, success bool, durationMs int64, ts float64, tests []results.TestCase) {
	s.instr.resultsReceivedTotal.Add(ctx, 1, apimetric.WithAttributes(
		attribute.String("kind", string(results.KindPlaywrightTest)),
		attribute.String("name", name.Name),
		attribute.String("namespace", name.Namespace),
	))
	s.UpsertTest(name, TestState{
		Kind:                 string(results.KindPlaywrightTest),
		Result:               resultFromSuccess(success),
		DurationMilliseconds: float64(durationMs),
		LastRunTimestamp:     ts,
		PlaywrightTests:      tests,
	})
}

func resultFromSuccess(success bool) Result {
	if success {
		return ResultOK
	}
	return ResultTestFailed
}

// RecordParseFailure increments the parse-failure counter for NATS messages.
func (s *Store) RecordParseFailure(ctx context.Context) {
	s.instr.resultsParseFailTotal.Add(ctx, 1)
}

// Upsert writes a probe's last-run state. Called by the in-process probe
// workers after each execution. Fires OnProbeTransition when the Result
// changes from the previous stored state.
func (s *Store) Upsert(name types.NamespacedName, state ProbeState) {
	s.mu.Lock()
	prev, had := s.probes[name]
	s.probes[name] = state
	cb := s.OnProbeTransition
	s.mu.Unlock()
	var prevResult Result
	if had {
		prevResult = prev.Result
	}
	if cb != nil && prevResult != state.Result {
		cb(name, state.Kind, prevResult, state.Result)
	}
}

// UpsertTest writes a test's last-run state. Called by the NATS consumer when
// a TestResult arrives. Fires OnTestTransition when the Result changes from
// the previous stored state.
func (s *Store) UpsertTest(name types.NamespacedName, state TestState) {
	s.mu.Lock()
	prev, had := s.tests[name]
	s.tests[name] = state
	cb := s.OnTestTransition
	s.mu.Unlock()
	var prevResult Result
	if had {
		prevResult = prev.Result
	}
	if cb != nil && prevResult != state.Result {
		cb(name, state.Kind, prevResult, state.Result)
	}
}

// Delete removes a probe or test from the store by name. Safe to call when
// the name doesn't exist in either map. Does not touch the depends map —
// call ClearDepends separately; reconcilers typically call both.
func (s *Store) Delete(name types.NamespacedName) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.probes, name)
	delete(s.tests, name)
}

// SetDepends records the dependency list for a probe or test. The reconciler
// calls this on every reconcile; the observe callback reads it to evaluate
// suppression. An empty list clears the entry.
func (s *Store) SetDepends(kind string, name types.NamespacedName, deps []syntheticsv1alpha1.DependencyRef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := crKey{kind: kind, name: name}
	if len(deps) == 0 {
		delete(s.depends, key)
		return
	}
	out := make([]syntheticsv1alpha1.DependencyRef, len(deps))
	copy(out, deps)
	s.depends[key] = out
}

// ClearDepends removes the dependency entry for a probe or test. Called by
// reconcilers on deletion.
func (s *Store) ClearDepends(kind string, name types.NamespacedName) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.depends, crKey{kind: kind, name: name})
}

// SetMetricLabels records user-supplied label key/value pairs (from
// spec.metricLabels) for a probe or test. The observe callback appends them
// to every metric the operator emits for that CR. An empty or nil map
// clears the entry.
func (s *Store) SetMetricLabels(kind string, name types.NamespacedName, labels map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := crKey{kind: kind, name: name}
	if len(labels) == 0 {
		delete(s.metricLabels, key)
		return
	}
	// Copy to protect against later mutation of the caller's map.
	out := make(map[string]string, len(labels))
	maps.Copy(out, labels)
	s.metricLabels[key] = out
}

// ClearMetricLabels removes the metricLabels entry for a probe or test.
func (s *Store) ClearMetricLabels(kind string, name types.NamespacedName) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.metricLabels, crKey{kind: kind, name: name})
}

// userLabelsLocked returns the attribute list for a CR's metricLabels.
// Stable, alphabetical key order so label sets are deterministic across
// scrapes. Caller must hold s.mu.RLock.
func (s *Store) userLabelsLocked(kind string, name types.NamespacedName) []attribute.KeyValue {
	labels := s.metricLabels[crKey{kind: kind, name: name}]
	if len(labels) == 0 {
		return nil
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]attribute.KeyValue, 0, len(keys))
	for _, k := range keys {
		out = append(out, attribute.String(k, labels[k]))
	}
	return out
}

// findUnhealthyDep walks the transitive dependency graph from origin. Returns
// the deepest failing dep found (first hit of a failing node in depth-first
// order), or ok=false if no transitive dep is failing. Handles cycles via the
// visited set and caps depth at 16 to fail-open on pathological graphs.
//
// Caller must hold s.mu.RLock.
const maxDependsDepth = 16

func (s *Store) findUnhealthyDep(originKind string, origin types.NamespacedName) (failing syntheticsv1alpha1.DependencyRef, ok bool) {
	visited := map[dependsKey]bool{{kind: originKind, name: origin}: true}
	deps := s.depends[dependsKey{kind: originKind, name: origin}]
	return s.walkDepsLocked(origin.Namespace, deps, visited, 0)
}

func (s *Store) walkDepsLocked(namespace string, deps []syntheticsv1alpha1.DependencyRef, visited map[dependsKey]bool, depth int) (syntheticsv1alpha1.DependencyRef, bool) {
	if depth >= maxDependsDepth {
		return syntheticsv1alpha1.DependencyRef{}, false
	}
	// Visit deeper first so the deepest failing ancestor wins; record the
	// direct dep as a fallback once we confirm nothing deeper is failing.
	for _, dep := range deps {
		depName := types.NamespacedName{Namespace: namespace, Name: dep.Name}
		key := dependsKey{kind: string(dep.Kind), name: depName}
		if visited[key] {
			continue
		}
		visited[key] = true
		if deeper, ok := s.walkDepsLocked(namespace, s.depends[key], visited, depth+1); ok {
			return deeper, true
		}
		if s.isFailingLocked(string(dep.Kind), depName) {
			return dep, true
		}
	}
	return syntheticsv1alpha1.DependencyRef{}, false
}

func (s *Store) isFailingLocked(kind string, name types.NamespacedName) bool {
	switch kind {
	case string(syntheticsv1alpha1.DependencyKindHTTPProbe), string(syntheticsv1alpha1.DependencyKindDNSProbe):
		state, ok := s.probes[name]
		return ok && state.Result != ResultOK
	case string(syntheticsv1alpha1.DependencyKindK6Test), string(syntheticsv1alpha1.DependencyKindPlaywrightTest):
		state, ok := s.tests[name]
		return ok && state.Result != ResultOK
	}
	return false
}

// Snapshot returns the last-run state for a probe. Returns (zero, false) if
// the name isn't a probe — including if it's a test, which Snapshot does not
// surface.
func (s *Store) Snapshot(name types.NamespacedName) (ProbeState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.probes[name]
	return state, ok
}

// SnapshotTest returns the last-run state for a test.
func (s *Store) SnapshotTest(name types.NamespacedName) (TestState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.tests[name]
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

// NeedLeaderElection returns false so controller-runtime's manager runs the
// metrics server on every replica, not just the leader. Prometheus scrapes
// the Service which fronts all replicas; non-leader followers need to serve
// /metrics too or 2-of-3 scrapes would fail in a multi-replica deployment.
//
// Followers' stores will be empty (workers + NATS consumer are leader-only
// by design — we don't want duplicate probe execution or double-ingest),
// but a valid empty response is still better than a connection refused.
func (s *Server) NeedLeaderElection() bool { return false }

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
