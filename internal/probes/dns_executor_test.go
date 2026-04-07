package probes

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
)

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

	if !result.Success {
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
	if result.Success {
		t.Fatal("expected failure for unreachable resolver")
	}
}

func TestDNSExecutorAAAAQuery(t *testing.T) {
	exec := DNSExecutor{}
	probe := dnsProbe("one.one.one.one", "AAAA", "1.1.1.1:53")

	result := exec.Execute(context.Background(), probe)

	if !result.Success {
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

	if !result.Success {
		t.Fatalf("expected TXT query to succeed, got: %s", result.Message)
	}
}

func TestDNSExecutorSystemResolver(t *testing.T) {
	exec := DNSExecutor{}
	// no resolver specified — uses 8.8.8.8:53 fallback
	probe := dnsProbe("one.one.one.one", "A", "")

	result := exec.Execute(context.Background(), probe)

	if !result.Success {
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
	if result.Success {
		t.Fatal("expected failure for cancelled context")
	}
}

func TestNewDNSProbeJob(t *testing.T) {
	probe := dnsProbe("one.one.one.one", "A", "1.1.1.1:53")
	probe.Namespace = "default"
	probe.Name = "test-probe"

	exec := DNSExecutor{}
	job := newDNSProbeJob(probe, exec)

	if job.key.Namespace != "default" {
		t.Errorf("unexpected namespace: %s", job.key.Namespace)
	}
	if job.key.Name != "test-probe" {
		t.Errorf("unexpected name: %s", job.key.Name)
	}
	if job.timeout != 10*time.Second {
		t.Errorf("unexpected timeout: %v", job.timeout)
	}

	state := job.run(context.Background())
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

	exec := DNSExecutor{}
	job := newDNSProbeJob(probe, exec)

	state := job.run(context.Background())

	if state.Success != 1 {
		t.Fatalf("expected success=1, got %f (reason=%q)", state.Success, state.FailureReason)
	}
	if state.FailureReason != "" {
		t.Fatalf("expected no failure reason, got %q", state.FailureReason)
	}
}

func TestDNSProbeJobAssertionDurationPass(t *testing.T) {
	probe := dnsProbeWithAssertions([]syntheticsv1alpha1.Assertion{
		{Name: "fast", Expr: "duration_ms < 5000"},
	})

	exec := DNSExecutor{}
	job := newDNSProbeJob(probe, exec)

	state := job.run(context.Background())

	if state.Success != 1 {
		t.Fatalf("expected success=1, got %f (reason=%q)", state.Success, state.FailureReason)
	}
}

func TestDNSProbeJobAssertionAnswerCountFail(t *testing.T) {
	// Query a non-existent name — expect 0 answers, so assertion fails.
	probe := dnsProbeWithAssertions([]syntheticsv1alpha1.Assertion{
		{Name: "has_answers", Expr: "answer_count > 0"},
	})
	// Use a guaranteed NXDOMAIN name.
	probe.Spec.Query.Name = "this-domain-does-not-exist.invalid"

	exec := DNSExecutor{}
	job := newDNSProbeJob(probe, exec)

	state := job.run(context.Background())

	// The executor itself succeeds (NXDOMAIN is a valid DNS response); the
	// assertion over answer_count is what should fail.
	if state.Success != 0 {
		t.Fatal("expected success=0 when answer_count assertion fails")
	}
	if state.FailureReason != "has_answers" {
		t.Fatalf("expected FailureReason=has_answers, got %q", state.FailureReason)
	}
}

func TestDNSProbeJobConfigErrorReasonSet(t *testing.T) {
	probe := dnsProbeWithAssertions([]syntheticsv1alpha1.Assertion{
		{Name: "has_answers", Expr: "answer_count > 0"},
	})
	probe.Spec.Query.Name = "" // triggers config error in executor

	exec := DNSExecutor{}
	job := newDNSProbeJob(probe, exec)

	state := job.run(context.Background())

	if state.Success != 0 {
		t.Fatal("expected success=0 on config error")
	}
	if state.FailureReason != ReasonConfigError {
		t.Fatalf("expected FailureReason=%q, got %q", ReasonConfigError, state.FailureReason)
	}
}

func TestDNSProbeJobNoAssertionsConnectionError(t *testing.T) {
	probe := dnsProbe("example.com", "A", "192.0.2.1:53") // unreachable
	probe.Spec.Assertions = nil

	exec := DNSExecutor{}
	job := newDNSProbeJob(probe, exec)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	state := job.run(ctx)

	if state.Success != 0 {
		t.Fatal("expected success=0 for connection failure")
	}
	if state.FailureReason != ReasonConnectionError {
		t.Fatalf("expected FailureReason=%q, got %q", ReasonConnectionError, state.FailureReason)
	}
}
