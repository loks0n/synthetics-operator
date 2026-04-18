package probes

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
	internalmetrics "github.com/loks0n/synthetics-operator/internal/metrics"
)

// runDNSJob is a test helper that builds a Job, runs it synchronously, and
// returns the ProbeState recorded in the store.
func runDNSJob(t *testing.T, probe *syntheticsv1alpha1.DNSProbe, exec DNSExecutor) internalmetrics.ProbeState {
	t.Helper()
	store, err := internalmetrics.NewStore()
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	job := NewDNSJob(probe, exec, store)
	job.Run(context.Background())
	state, _ := store.Snapshot(job.Key)
	return state
}

func dnsProbe(name, qtype, resolver string) *syntheticsv1alpha1.DNSProbe {
	return &syntheticsv1alpha1.DNSProbe{
		Spec: syntheticsv1alpha1.DNSProbeSpec{
			Interval: metav1.Duration{Duration: 30 * time.Second},
			Timeout:  metav1.Duration{Duration: 10 * time.Second},
			Query: syntheticsv1alpha1.DNSQuery{
				Name:     name,
				Type:     qtype,
				Resolver: resolver,
			},
		},
	}
}

func TestDNSExecutorRealQuery(t *testing.T) {
	exec := DNSExecutor{}
	probe := dnsProbe("one.one.one.one", "A", "1.1.1.1:53")

	result := exec.Execute(context.Background(), probe)

	if !result.Success() {
		t.Fatalf("expected success, got message: %s", result.Message)
	}
	if result.ConfigError {
		t.Fatal("unexpected config error")
	}
	if result.AnswerCount == 0 {
		t.Fatal("expected at least one answer")
	}
	if result.FirstAnswerType != "A" {
		t.Errorf("expected FirstAnswerType=A, got %s", result.FirstAnswerType)
	}
	if result.FirstAnswerValue == "" {
		t.Error("expected non-empty FirstAnswerValue")
	}
	if result.Duration <= 0 {
		t.Error("expected positive duration")
	}
}

func TestDNSExecutorEmptyName(t *testing.T) {
	exec := DNSExecutor{}
	probe := dnsProbe("", "A", "1.1.1.1:53")

	result := exec.Execute(context.Background(), probe)

	if !result.ConfigError {
		t.Fatal("expected config error for empty name")
	}
}

func TestDNSExecutorBadResolverFormat(t *testing.T) {
	exec := DNSExecutor{}
	probe := dnsProbe("example.com", "A", "not-a-resolver")

	result := exec.Execute(context.Background(), probe)

	if !result.ConfigError {
		t.Fatal("expected config error for bad resolver format")
	}
}

func TestDNSExecutorUnreachableResolver(t *testing.T) {
	exec := DNSExecutor{}
	probe := dnsProbe("example.com", "A", "192.0.2.1:53") // TEST-NET, not routable

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	result := exec.Execute(ctx, probe)

	if result.ConfigError {
		t.Fatal("unreachable resolver should not be a config error")
	}
	if result.Success() {
		t.Fatal("expected failure for unreachable resolver")
	}
}

func TestDNSExecutorAAAAQuery(t *testing.T) {
	exec := DNSExecutor{}
	probe := dnsProbe("one.one.one.one", "AAAA", "1.1.1.1:53")

	result := exec.Execute(context.Background(), probe)

	if !result.Success() {
		t.Fatalf("expected AAAA query to succeed, got: %s", result.Message)
	}
	if result.FirstAnswerType != "AAAA" && result.AnswerCount > 0 {
		t.Errorf("expected FirstAnswerType=AAAA, got %s", result.FirstAnswerType)
	}
}

func TestDNSExecutorTXTQuery(t *testing.T) {
	exec := DNSExecutor{}
	// google.com has TXT records
	probe := dnsProbe("google.com", "TXT", "8.8.8.8:53")

	result := exec.Execute(context.Background(), probe)

	if !result.Success() {
		t.Fatalf("expected TXT query to succeed, got: %s", result.Message)
	}
}

func TestDNSExecutorSystemResolver(t *testing.T) {
	exec := DNSExecutor{}
	// no resolver specified — uses 8.8.8.8:53 fallback
	probe := dnsProbe("one.one.one.one", "A", "")

	result := exec.Execute(context.Background(), probe)

	if !result.Success() {
		t.Fatalf("expected success with system resolver fallback, got: %s", result.Message)
	}
}

func TestDNSExecutorContextCancellation(t *testing.T) {
	exec := DNSExecutor{}
	probe := dnsProbe("example.com", "A", "192.0.2.1:53") // unreachable

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	result := exec.Execute(ctx, probe)

	if result.ConfigError {
		t.Fatal("context cancellation should not be a config error")
	}
	if result.Success() {
		t.Fatal("expected failure for cancelled context")
	}
}

func TestNewDNSJob(t *testing.T) {
	probe := dnsProbe("one.one.one.one", "A", "1.1.1.1:53")
	probe.Namespace = "default"
	probe.Name = "test-probe"

	store, err := internalmetrics.NewStore()
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	job := NewDNSJob(probe, DNSExecutor{}, store)

	if job.Key.Namespace != "default" {
		t.Errorf("unexpected namespace: %s", job.Key.Namespace)
	}
	if job.Key.Name != "test-probe" {
		t.Errorf("unexpected name: %s", job.Key.Name)
	}
	if job.Timeout != 10*time.Second {
		t.Errorf("unexpected timeout: %v", job.Timeout)
	}

	job.Run(context.Background())
	state, ok := store.Snapshot(job.Key)
	if !ok {
		t.Fatal("expected state in store after Run")
	}
	if state.Kind != "DNSProbe" {
		t.Errorf("expected Kind=DNSProbe, got %s", state.Kind)
	}
}

// --- newDNSProbeJob assertion evaluation tests ---

func dnsProbeWithAssertions(assertions []syntheticsv1alpha1.Assertion) *syntheticsv1alpha1.DNSProbe {
	p := dnsProbe("one.one.one.one", "A", "1.1.1.1:53")
	p.Spec.Assertions = assertions
	return p
}

func TestDNSProbeJobAssertionAnswerCountPass(t *testing.T) {
	probe := dnsProbeWithAssertions([]syntheticsv1alpha1.Assertion{
		{Name: "has_answers", Expr: "answer_count > 0"},
	})
	state := runDNSJob(t, probe, DNSExecutor{})

	if state.Result != internalmetrics.ResultOK {
		t.Fatalf("expected Result=ok, got %q (failed_assertion=%q)", state.Result, state.FailedAssertion)
	}
	if state.FailedAssertion != "" {
		t.Fatalf("expected no failed_assertion, got %q", state.FailedAssertion)
	}
}

func TestDNSProbeJobAssertionDurationPass(t *testing.T) {
	probe := dnsProbeWithAssertions([]syntheticsv1alpha1.Assertion{
		{Name: "fast", Expr: "duration_ms < 5000"},
	})
	state := runDNSJob(t, probe, DNSExecutor{})

	if state.Result != internalmetrics.ResultOK {
		t.Fatalf("expected Result=ok, got %q (failed_assertion=%q)", state.Result, state.FailedAssertion)
	}
}

func TestDNSProbeJobAssertionAnswerCountFail(t *testing.T) {
	// Query a non-existent name — NXDOMAIN is a valid DNS response with 0
	// answers, so the assertion fails (not the transport).
	probe := dnsProbeWithAssertions([]syntheticsv1alpha1.Assertion{
		{Name: "has_answers", Expr: "answer_count > 0"},
	})
	probe.Spec.Query.Name = "this-domain-does-not-exist.invalid"

	state := runDNSJob(t, probe, DNSExecutor{})

	if state.Result != internalmetrics.ResultAssertionFailed {
		t.Fatalf("expected Result=assertion_failed, got %q", state.Result)
	}
	if state.FailedAssertion != "has_answers" {
		t.Fatalf("expected FailedAssertion=has_answers, got %q", state.FailedAssertion)
	}
}

func TestDNSProbeJobConfigError(t *testing.T) {
	probe := dnsProbeWithAssertions([]syntheticsv1alpha1.Assertion{
		{Name: "has_answers", Expr: "answer_count > 0"},
	})
	probe.Spec.Query.Name = "" // triggers config error in executor

	state := runDNSJob(t, probe, DNSExecutor{})

	if state.Result != internalmetrics.ResultConfigError {
		t.Fatalf("expected Result=config_error, got %q", state.Result)
	}
}

func TestDNSProbeJobNoAssertionsResolverError(t *testing.T) {
	probe := dnsProbe("example.com", "A", "192.0.2.1:53") // unreachable
	probe.Spec.Assertions = nil

	store, err := internalmetrics.NewStore()
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	job := NewDNSJob(probe, DNSExecutor{}, store)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	job.Run(ctx)
	state, _ := store.Snapshot(job.Key)

	if state.Result != internalmetrics.ResultDNSFailed {
		t.Fatalf("expected Result=dns_failed, got %q", state.Result)
	}
}
