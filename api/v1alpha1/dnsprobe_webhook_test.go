package v1alpha1

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDNSWebhookHandlerObjectSplit(t *testing.T) {
	handler := &DNSProbe{}
	probe := &DNSProbe{
		Spec: DNSProbeSpec{
			Query: DNSQuery{Name: "example.com"},
		},
	}

	if err := handler.Default(context.Background(), probe); err != nil {
		t.Fatalf("Default failed: %v", err)
	}
	// Mutations must land on probe, not handler.
	if probe.Spec.Interval.Duration != 30*time.Second {
		t.Errorf("expected probe.Interval=30s, got %v", probe.Spec.Interval.Duration)
	}
	if handler.Spec.Interval.Duration != 0 {
		t.Errorf("handler should be unchanged, got interval=%v", handler.Spec.Interval.Duration)
	}

	if _, err := handler.ValidateCreate(context.Background(), probe); err != nil {
		t.Fatalf("ValidateCreate failed on valid probe: %v", err)
	}
	if _, err := handler.ValidateUpdate(context.Background(), nil, probe); err != nil {
		t.Fatalf("ValidateUpdate failed on valid probe: %v", err)
	}
}

func TestDNSProbeDefault(t *testing.T) {
	probe := &DNSProbe{
		Spec: DNSProbeSpec{
			Query: DNSQuery{Name: "example.com"},
		},
	}
	if err := probe.Default(context.Background(), probe); err != nil {
		t.Fatalf("default failed: %v", err)
	}

	if probe.Spec.Interval.Duration != 30*time.Second {
		t.Fatalf("unexpected interval: %v", probe.Spec.Interval.Duration)
	}
	if probe.Spec.Timeout.Duration != 10*time.Second {
		t.Fatalf("unexpected timeout: %v", probe.Spec.Timeout.Duration)
	}
	if probe.Spec.Query.Type != "A" {
		t.Fatalf("unexpected query type: %s", probe.Spec.Query.Type)
	}
}

func TestDNSProbeDefaultDoesNotOverwrite(t *testing.T) {
	probe := &DNSProbe{
		Spec: DNSProbeSpec{
			Interval: metav1.Duration{Duration: 60 * time.Second},
			Timeout:  metav1.Duration{Duration: 5 * time.Second},
			Query:    DNSQuery{Name: "example.com", Type: "AAAA"},
		},
	}
	if err := probe.Default(context.Background(), probe); err != nil {
		t.Fatalf("default failed: %v", err)
	}
	if probe.Spec.Interval.Duration != 60*time.Second {
		t.Fatalf("interval should not be overwritten, got %v", probe.Spec.Interval.Duration)
	}
	if probe.Spec.Timeout.Duration != 5*time.Second {
		t.Fatalf("timeout should not be overwritten, got %v", probe.Spec.Timeout.Duration)
	}
	if probe.Spec.Query.Type != "AAAA" {
		t.Fatalf("query type should not be overwritten, got %s", probe.Spec.Query.Type)
	}
}

func TestDNSProbeValidateRules(t *testing.T) {
	validBase := func() *DNSProbe {
		p := &DNSProbe{
			Spec: DNSProbeSpec{
				Query: DNSQuery{Name: "example.com"},
			},
		}
		_ = p.Default(context.Background(), p)
		return p
	}

	cases := []struct {
		name    string
		mutate  func(*DNSProbe)
		wantErr bool
	}{
		{
			name:    "zero interval rejected",
			mutate:  func(p *DNSProbe) { p.Spec.Interval.Duration = 0 },
			wantErr: true,
		},
		{
			name:    "zero timeout rejected",
			mutate:  func(p *DNSProbe) { p.Spec.Timeout.Duration = 0 },
			wantErr: true,
		},
		{
			name: "timeout greater than interval rejected",
			mutate: func(p *DNSProbe) {
				p.Spec.Interval.Duration = 5 * time.Second
				p.Spec.Timeout.Duration = 10 * time.Second
			},
			wantErr: true,
		},
		{
			name:    "empty name rejected",
			mutate:  func(p *DNSProbe) { p.Spec.Query.Name = "" },
			wantErr: true,
		},
		{
			name:    "whitespace-only name rejected",
			mutate:  func(p *DNSProbe) { p.Spec.Query.Name = "   " },
			wantErr: true,
		},
		{
			name:    "type A accepted",
			mutate:  func(p *DNSProbe) { p.Spec.Query.Type = "A" },
			wantErr: false,
		},
		{
			name:    "type AAAA accepted",
			mutate:  func(p *DNSProbe) { p.Spec.Query.Type = "AAAA" },
			wantErr: false,
		},
		{
			name:    "type CNAME accepted",
			mutate:  func(p *DNSProbe) { p.Spec.Query.Type = "CNAME" },
			wantErr: false,
		},
		{
			name:    "type MX accepted",
			mutate:  func(p *DNSProbe) { p.Spec.Query.Type = "MX" },
			wantErr: false,
		},
		{
			name:    "type TXT accepted",
			mutate:  func(p *DNSProbe) { p.Spec.Query.Type = "TXT" },
			wantErr: false,
		},
		{
			name:    "type NS accepted",
			mutate:  func(p *DNSProbe) { p.Spec.Query.Type = "NS" },
			wantErr: false,
		},
		{
			name:    "type PTR accepted",
			mutate:  func(p *DNSProbe) { p.Spec.Query.Type = "PTR" },
			wantErr: false,
		},
		{
			name:    "invalid type rejected",
			mutate:  func(p *DNSProbe) { p.Spec.Query.Type = "SOA" },
			wantErr: true,
		},
		{
			name:    "valid resolver accepted",
			mutate:  func(p *DNSProbe) { p.Spec.Query.Resolver = "8.8.8.8:53" },
			wantErr: false,
		},
		{
			name:    "resolver without port rejected",
			mutate:  func(p *DNSProbe) { p.Spec.Query.Resolver = "8.8.8.8" },
			wantErr: true,
		},
		{
			name:    "resolver with empty host rejected",
			mutate:  func(p *DNSProbe) { p.Spec.Query.Resolver = ":53" },
			wantErr: true,
		},
		{
			name:    "empty resolver accepted",
			mutate:  func(p *DNSProbe) { p.Spec.Query.Resolver = "" },
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validBase()
			tc.mutate(p)
			_, err := p.ValidateCreate(context.Background(), p)
			if tc.wantErr && err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

func TestDNSProbeAssertionValidation(t *testing.T) {
	validBase := func() *DNSProbe {
		p := &DNSProbe{
			Spec: DNSProbeSpec{
				Query: DNSQuery{Name: "example.com"},
			},
		}
		_ = p.Default(context.Background(), p)
		return p
	}

	cases := []struct {
		name    string
		mutate  func(*DNSProbe)
		wantErr bool
	}{
		{
			name: "valid answer_count assertion accepted",
			mutate: func(p *DNSProbe) {
				p.Spec.Assertions = []Assertion{{Name: "has_answers", Expr: "answer_count > 0"}}
			},
			wantErr: false,
		},
		{
			name: "valid duration_ms assertion accepted",
			mutate: func(p *DNSProbe) {
				p.Spec.Assertions = []Assertion{{Name: "fast", Expr: "duration_ms < 500"}}
			},
			wantErr: false,
		},
		{
			name: "HTTP variable rejected for DNS probe",
			mutate: func(p *DNSProbe) {
				p.Spec.Assertions = []Assertion{{Name: "bad", Expr: "status_code = 200"}}
			},
			wantErr: true,
		},
		{
			name: "ssl_expiry_days rejected for DNS probe",
			mutate: func(p *DNSProbe) {
				p.Spec.Assertions = []Assertion{{Name: "bad", Expr: "ssl_expiry_days >= 14"}}
			},
			wantErr: true,
		},
		{
			name: "unknown variable rejected",
			mutate: func(p *DNSProbe) {
				p.Spec.Assertions = []Assertion{{Name: "bad", Expr: "unknown_var = 1"}}
			},
			wantErr: true,
		},
		{
			name: "invalid expression rejected",
			mutate: func(p *DNSProbe) {
				p.Spec.Assertions = []Assertion{{Name: "bad", Expr: "not an expression"}}
			},
			wantErr: true,
		},
		{
			name: "empty assertion name rejected",
			mutate: func(p *DNSProbe) {
				p.Spec.Assertions = []Assertion{{Name: "", Expr: "answer_count > 0"}}
			},
			wantErr: true,
		},
		{
			name: "multiple valid assertions accepted",
			mutate: func(p *DNSProbe) {
				p.Spec.Assertions = []Assertion{
					{Name: "has_answers", Expr: "answer_count > 0"},
					{Name: "fast", Expr: "duration_ms < 500"},
				}
			},
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validBase()
			tc.mutate(p)
			_, err := p.ValidateCreate(context.Background(), p)
			if tc.wantErr && err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

func TestDNSProbeValidateUpdate(t *testing.T) {
	valid := &DNSProbe{
		Spec: DNSProbeSpec{
			Query: DNSQuery{Name: "example.com"},
		},
	}
	_ = valid.Default(context.Background(), valid)

	if _, err := valid.ValidateUpdate(context.Background(), nil, valid); err != nil {
		t.Fatalf("expected valid update, got %v", err)
	}

	invalid := &DNSProbe{
		Spec: DNSProbeSpec{
			Query: DNSQuery{Name: ""},
		},
	}
	_ = invalid.Default(context.Background(), invalid)
	if _, err := invalid.ValidateUpdate(context.Background(), nil, invalid); err == nil {
		t.Fatal("expected ValidateUpdate to reject empty name")
	}
}

func TestDNSProbeValidateDelete(t *testing.T) {
	probe := &DNSProbe{}
	_, err := probe.ValidateDelete(context.Background(), probe)
	if err != nil {
		t.Fatalf("ValidateDelete should always succeed, got %v", err)
	}
}
