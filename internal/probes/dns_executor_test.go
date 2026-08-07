package probes

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
	"github.com/loks0n/synthetics-operator/internal/results"
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

	result := exec.executeProbe(context.Background(), probe)

	if !result.success() {
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
	result := DNSExecutor{}.executeProbe(context.Background(), dnsProbe("", "A", "1.1.1.1:53"))
	if !result.ConfigError {
		t.Fatal("expected config error for empty name")
	}
}

func TestDNSExecutorBadResolverFormat(t *testing.T) {
	result := DNSExecutor{}.executeProbe(context.Background(), dnsProbe("example.com", "A", "not-a-resolver"))
	if !result.ConfigError {
		t.Fatal("expected config error for bad resolver format")
	}
}

func TestDNSExecutorUnreachableResolver(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	result := DNSExecutor{}.executeProbe(ctx, dnsProbe("example.com", "A", "192.0.2.1:53"))

	if result.ConfigError {
		t.Fatal("unreachable resolver should not be a config error")
	}
	if result.success() {
		t.Fatal("expected failure for unreachable resolver")
	}
}

func TestDNSExecutorAAAAQuery(t *testing.T) {
	result := DNSExecutor{}.executeProbe(context.Background(), dnsProbe("one.one.one.one", "AAAA", "1.1.1.1:53"))
	if !result.success() {
		t.Fatalf("expected AAAA query to succeed, got: %s", result.Message)
	}
	if result.FirstAnswerType != "AAAA" && result.AnswerCount > 0 {
		t.Errorf("expected FirstAnswerType=AAAA, got %s", result.FirstAnswerType)
	}
}

func TestDNSExecutorTXTQuery(t *testing.T) {
	result := DNSExecutor{}.executeProbe(context.Background(), dnsProbe("google.com", "TXT", "8.8.8.8:53"))
	if !result.success() {
		t.Fatalf("expected TXT query to succeed, got: %s", result.Message)
	}
}

func TestDNSExecutorSystemResolver(t *testing.T) {
	result := DNSExecutor{}.executeProbe(context.Background(), dnsProbe("one.one.one.one", "A", ""))
	if !result.success() {
		t.Fatalf("expected success with system resolver fallback, got: %s", result.Message)
	}
}

func TestDNSExecutorContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := DNSExecutor{}.executeProbe(ctx, dnsProbe("example.com", "A", "192.0.2.1:53"))

	if result.ConfigError {
		t.Fatal("context cancellation should not be a config error")
	}
	if result.success() {
		t.Fatal("expected failure for cancelled context")
	}
}

// --- assertion evaluation tests against evalDNSAssertions ---

func TestEvalDNSAssertionsAnswerCountPass(t *testing.T) {
	outcome, failed, _ := evalDNSAssertions(
		dnsResult{AnswerCount: 1, Duration: 5 * time.Millisecond},
		[]results.Assertion{{Name: "has_answers", Expr: "answer_count > 0"}},
	)
	if outcome != "ok" || failed != "" {
		t.Fatalf("expected ok, got outcome=%q failed=%q", outcome, failed)
	}
}

func TestEvalDNSAssertionsAnswerCountFail(t *testing.T) {
	outcome, failed, _ := evalDNSAssertions(
		dnsResult{AnswerCount: 0, Duration: 5 * time.Millisecond},
		[]results.Assertion{{Name: "has_answers", Expr: "answer_count > 0"}},
	)
	if outcome != "assertion_failed" || failed != "has_answers" {
		t.Fatalf("expected assertion_failed has_answers, got outcome=%q failed=%q", outcome, failed)
	}
}

func TestEvalDNSAssertionsDurationPass(t *testing.T) {
	outcome, _, _ := evalDNSAssertions(
		dnsResult{AnswerCount: 1, Duration: 5 * time.Millisecond},
		[]results.Assertion{{Name: "fast", Expr: "duration_ms < 5000"}},
	)
	if outcome != "ok" {
		t.Fatalf("expected ok, got %q", outcome)
	}
}
