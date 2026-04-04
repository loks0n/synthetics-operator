package v1alpha1

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func TestHttpProbeDefaultDoesNotOverwrite(t *testing.T) {
	probe := &HttpProbe{
		Spec: HttpProbeSpec{
			Interval:   metav1.Duration{Duration: 60 * time.Second},
			Timeout:    metav1.Duration{Duration: 5 * time.Second},
			Request:    HTTPRequestSpec{Method: "GET"},
			Assertions: HTTPAssertions{Status: 201},
		},
	}
	if err := probe.Default(context.Background(), nil); err != nil {
		t.Fatalf("default failed: %v", err)
	}
	if probe.Spec.Interval.Duration != 60*time.Second {
		t.Fatalf("interval should not be overwritten, got %v", probe.Spec.Interval.Duration)
	}
	if probe.Spec.Timeout.Duration != 5*time.Second {
		t.Fatalf("timeout should not be overwritten, got %v", probe.Spec.Timeout.Duration)
	}
	if probe.Spec.Assertions.Status != 201 {
		t.Fatalf("status should not be overwritten, got %d", probe.Spec.Assertions.Status)
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

func TestHttpProbeValidateRules(t *testing.T) {
	validBase := func() *HttpProbe {
		p := &HttpProbe{}
		_ = p.Default(context.Background(), nil)
		p.Spec.Request.URL = "https://example.com/health"
		return p
	}

	cases := []struct {
		name    string
		mutate  func(*HttpProbe)
		wantErr bool
	}{
		{
			name:    "zero interval rejected",
			mutate:  func(p *HttpProbe) { p.Spec.Interval.Duration = 0 },
			wantErr: true,
		},
		{
			name:    "zero timeout rejected",
			mutate:  func(p *HttpProbe) { p.Spec.Timeout.Duration = 0 },
			wantErr: true,
		},
		{
			name: "timeout greater than interval rejected",
			mutate: func(p *HttpProbe) {
				p.Spec.Interval.Duration = 5 * time.Second
				p.Spec.Timeout.Duration = 10 * time.Second
			},
			wantErr: true,
		},
		{
			name:    "empty URL rejected",
			mutate:  func(p *HttpProbe) { p.Spec.Request.URL = "" },
			wantErr: true,
		},
		{
			name:    "relative URL rejected",
			mutate:  func(p *HttpProbe) { p.Spec.Request.URL = "/health" },
			wantErr: true,
		},
		{
			name:    "non-http scheme rejected",
			mutate:  func(p *HttpProbe) { p.Spec.Request.URL = "ftp://example.com" },
			wantErr: true,
		},
		{
			name:    "status code below 100 rejected",
			mutate:  func(p *HttpProbe) { p.Spec.Assertions.Status = 99 },
			wantErr: true,
		},
		{
			name:    "status code above 599 rejected",
			mutate:  func(p *HttpProbe) { p.Spec.Assertions.Status = 600 },
			wantErr: true,
		},
		{
			name:    "http scheme accepted",
			mutate:  func(p *HttpProbe) { p.Spec.Request.URL = "http://example.com/health" },
			wantErr: false,
		},
		{
			name:    "status 404 accepted",
			mutate:  func(p *HttpProbe) { p.Spec.Assertions.Status = 404 },
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validBase()
			tc.mutate(p)
			_, err := p.ValidateCreate(context.Background(), nil)
			if tc.wantErr && err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

func TestHttpProbeValidateUpdate(t *testing.T) {
	valid := &HttpProbe{}
	_ = valid.Default(context.Background(), nil)
	valid.Spec.Request.URL = "https://example.com/health"

	if _, err := valid.ValidateUpdate(context.Background(), nil, nil); err != nil {
		t.Fatalf("expected valid update, got %v", err)
	}

	invalid := &HttpProbe{}
	_ = invalid.Default(context.Background(), nil)
	invalid.Spec.Request.URL = "not-a-url"
	if _, err := invalid.ValidateUpdate(context.Background(), nil, nil); err == nil {
		t.Fatal("expected ValidateUpdate to reject invalid URL")
	}
}
