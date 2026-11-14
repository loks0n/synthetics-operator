package v1alpha1

import (
	"context"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const smtpTestCACert = `-----BEGIN CERTIFICATE-----
MIIBhTCCASugAwIBAgIQIRi6zePL6mKjOipn+dNuaTAKBggqhkjOPQQDAjASMRAw
DgYDVQQKEwdBY21lIENvMB4XDTE3MTAyMDE5NDMwNloXDTE4MTAyMDE5NDMwNlow
EjEQMA4GA1UEChMHQWNtZSBDbzBZMBMGByqGSM49AgEGCCqGSM49AwEHA0IABD0d
7VNhbWvZLWPuj/RtHFjvtJBEwOkhbN/BnnE8rnZR8+sbwnc/KhCk3FhnpHZnQz7B
5aETbbIgmuvewdjvSBSjYzBhMA4GA1UdDwEB/wQEAwICpDATBgNVHSUEDDAKBggr
BgEFBQcDATAPBgNVHRMBAf8EBTADAQH/MCkGA1UdEQQiMCCCDmxvY2FsaG9zdDo1
NDUzgg4xMjcuMC4wLjE6NTQ1MzAKBggqhkjOPQQDAgNIADBFAiEA2zpJEPQyz6/l
Wf86aX6PepsntZv2GYlA5UpabfT2EZICICpJ5h/iI+i341gBmLiAFQOyTDT+/wQc
6MF9+Yw1Yy0t
-----END CERTIFICATE-----`

func smtpProbeOnPort(port int32) *SMTPProbe {
	p := &SMTPProbe{Spec: SMTPProbeSpec{Target: TCPTarget{Host: "mail.example.com", Port: port}}}
	_ = p.Default(context.Background(), p)
	return p
}

func TestSMTPProbeDefaultsTLSModeFromPort(t *testing.T) {
	for port, want := range map[int32]SMTPTLSMode{
		25:   SMTPTLSModeNone,
		465:  SMTPTLSModeImplicit,
		587:  SMTPTLSModeStartTLS,
		2525: SMTPTLSModeNone,
	} {
		if got := smtpProbeOnPort(port).Spec.TLS.Mode; got != want {
			t.Errorf("port %d: mode=%q, want %q", port, got, want)
		}
	}

	explicit := &SMTPProbe{Spec: SMTPProbeSpec{
		Target: TCPTarget{Host: "mx.example.com", Port: 25},
		TLS:    SMTPTLSConfig{Mode: SMTPTLSModeStartTLS},
	}}
	_ = explicit.Default(context.Background(), explicit)
	if explicit.Spec.TLS.Mode != SMTPTLSModeStartTLS {
		t.Fatalf("explicit mode overwritten: %q", explicit.Spec.TLS.Mode)
	}
}

func TestSMTPProbeValidation(t *testing.T) {
	scheme, err := SchemeBuilder.Build()
	if err != nil {
		t.Fatal(err)
	}
	validator := &SMTPProbeValidator{reader: fake.NewClientBuilder().WithScheme(scheme).Build()}

	cases := []struct {
		name    string
		port    int32
		mutate  func(*SMTPProbe)
		wantErr bool
	}{
		{name: "submission defaults", port: 587, mutate: func(*SMTPProbe) {}},
		{name: "starttls with caCert", port: 587, mutate: func(p *SMTPProbe) { p.Spec.TLS.CACert = smtpTestCACert }},
		{name: "implicit skip verify", port: 465, mutate: func(p *SMTPProbe) { p.Spec.TLS.InsecureSkipVerify = true }},
		{name: "smtp assertions", port: 465, mutate: func(p *SMTPProbe) {
			p.Spec.Assertions = []Assertion{
				{Name: "accepted", Expr: "smtp_code = 250"},
				{Name: "cert", Expr: "ssl_expiry_days > 14"},
				{Name: "fast", Expr: "duration_ms < 2000"},
			}
		}},
		{name: "unknown mode", port: 25, mutate: func(p *SMTPProbe) { p.Spec.TLS.Mode = "ssl" }, wantErr: true},
		{name: "caCert without TLS", port: 25, mutate: func(p *SMTPProbe) { p.Spec.TLS.CACert = smtpTestCACert }, wantErr: true},
		{name: "skip verify without TLS", port: 25, mutate: func(p *SMTPProbe) { p.Spec.TLS.InsecureSkipVerify = true }, wantErr: true},
		{name: "invalid caCert", port: 587, mutate: func(p *SMTPProbe) { p.Spec.TLS.CACert = "not a pem" }, wantErr: true},
		{name: "HTTP assertion", port: 587, mutate: func(p *SMTPProbe) { p.Spec.Assertions = []Assertion{{Name: "s", Expr: "status_code = 200"}} }, wantErr: true},
		{name: "host with port", port: 587, mutate: func(p *SMTPProbe) { p.Spec.Target.Host = "mail.example.com:587" }, wantErr: true},
		{name: "zero port", port: 0, mutate: func(*SMTPProbe) {}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := smtpProbeOnPort(tc.port)
			tc.mutate(p)
			_, err := validator.ValidateCreate(context.Background(), p)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateCreate error=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}
