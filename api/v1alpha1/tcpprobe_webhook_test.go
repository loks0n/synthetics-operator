package v1alpha1

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testTCPValidator() *TCPProbeValidator {
	scheme, err := SchemeBuilder.Build()
	if err != nil {
		panic(err)
	}
	return &TCPProbeValidator{reader: fake.NewClientBuilder().WithScheme(scheme).Build()}
}

func validTCPProbe() *TCPProbe {
	p := &TCPProbe{Spec: TCPProbeSpec{Target: TCPTarget{Host: "mysql.default.svc.cluster.local", Port: 3306}}}
	_ = p.Default(context.Background(), p)
	return p
}

func TestTCPProbeDefault(t *testing.T) {
	p := &TCPProbe{Spec: TCPProbeSpec{Target: TCPTarget{Host: "127.0.0.1", Port: 5432}}}
	if err := p.Default(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if p.Spec.Interval.Duration != 30*time.Second || p.Spec.Timeout.Duration != 10*time.Second {
		t.Fatalf("unexpected defaults: interval=%s timeout=%s", p.Spec.Interval.Duration, p.Spec.Timeout.Duration)
	}
}

func TestTCPProbeValidation(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*TCPProbe)
		wantErr bool
	}{
		{name: "dns host", mutate: func(*TCPProbe) {}, wantErr: false},
		{name: "fqdn", mutate: func(p *TCPProbe) { p.Spec.Target.Host = "mysql.example.com." }, wantErr: false},
		{name: "ipv4", mutate: func(p *TCPProbe) { p.Spec.Target.Host = "127.0.0.1" }, wantErr: false},
		{name: "ipv6", mutate: func(p *TCPProbe) { p.Spec.Target.Host = "2001:db8::1" }, wantErr: false},
		{name: "empty host", mutate: func(p *TCPProbe) { p.Spec.Target.Host = " " }, wantErr: true},
		{name: "scheme rejected", mutate: func(p *TCPProbe) { p.Spec.Target.Host = "tcp://example.com" }, wantErr: true},
		{name: "embedded port rejected", mutate: func(p *TCPProbe) { p.Spec.Target.Host = "example.com:443" }, wantErr: true},
		{name: "zero port", mutate: func(p *TCPProbe) { p.Spec.Target.Port = 0 }, wantErr: true},
		{name: "port too high", mutate: func(p *TCPProbe) { p.Spec.Target.Port = 65536 }, wantErr: true},
		{name: "timeout over interval", mutate: func(p *TCPProbe) {
			p.Spec.Interval = metav1.Duration{Duration: time.Second}
			p.Spec.Timeout = metav1.Duration{Duration: 2 * time.Second}
		}, wantErr: true},
		{name: "duration assertion", mutate: func(p *TCPProbe) { p.Spec.Assertions = []Assertion{{Name: "fast", Expr: "duration_ms < 1000"}} }, wantErr: false},
		{name: "HTTP assertion rejected", mutate: func(p *TCPProbe) { p.Spec.Assertions = []Assertion{{Name: "status", Expr: "status_code = 200"}} }, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validTCPProbe()
			tc.mutate(p)
			_, err := testTCPValidator().ValidateCreate(context.Background(), p)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateCreate error=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}
