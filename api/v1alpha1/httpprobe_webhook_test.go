package v1alpha1

import (
	"context"
	"testing"
	"time"
)

func TestHttpProbeDefault(t *testing.T) {
	probe := &HttpProbe{}
	if err := probe.Default(context.Background(), nil); err != nil {
		t.Fatalf("default failed: %v", err)
	}

	if probe.Spec.Interval.Duration != 30*time.Second {
		t.Fatalf("unexpected interval: %v", probe.Spec.Interval.Duration)
	}
	if probe.Spec.Timeout.Duration != 10*time.Second {
		t.Fatalf("unexpected timeout: %v", probe.Spec.Timeout.Duration)
	}
	if probe.Spec.Request.Method != "GET" {
		t.Fatalf("unexpected method: %s", probe.Spec.Request.Method)
	}
	if probe.Spec.Assertions.Status != 200 {
		t.Fatalf("unexpected status assertion: %d", probe.Spec.Assertions.Status)
	}
	if probe.Status.Summary != nil {
		t.Fatal("defaulting should not mutate status")
	}
}

func TestHttpProbeValidate(t *testing.T) {
	probe := &HttpProbe{}
	_ = probe.Default(context.Background(), nil)
	probe.Spec.Request.URL = "https://example.com/health"

	if _, err := probe.ValidateCreate(context.Background(), nil); err != nil {
		t.Fatalf("expected valid probe, got %v", err)
	}

	probe.Spec.Request.Method = "POST"
	if _, err := probe.ValidateCreate(context.Background(), nil); err == nil {
		t.Fatal("expected POST validation to fail")
	}
}
