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

func TestDNSExecutorAssertionPass(t *testing.T) {
	exec := DNSExecutor{}
	// PTR for 1.1.1.1 reliably returns "one.one.one.one" — a stable single-value record.
	probe := dnsProbe("1.1.1.1.in-addr.arpa", "PTR", "1.1.1.1:53")
	probe.Spec.Assertions.FirstAnswerValue = "one.one.one.one"

	result := exec.Execute(context.Background(), probe)

	if !result.Success {
		t.Fatalf("expected assertion to pass, got message: %s", result.Message)
	}
}

func TestDNSExecutorAssertionFail(t *testing.T) {
	exec := DNSExecutor{}
	probe := dnsProbe("one.one.one.one", "A", "1.1.1.1:53")
	probe.Spec.Assertions.FirstAnswerValue = "999.999.999.999"

	result := exec.Execute(context.Background(), probe)

	if result.Success {
		t.Fatal("expected assertion failure")
	}
	if result.ConfigError {
		t.Fatal("unexpected config error for assertion failure")
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
