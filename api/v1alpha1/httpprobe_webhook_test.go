package v1alpha1

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// validTestCACert is a real self-signed CA certificate generated at test init time.
var validTestCACert = func() string {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic("generate test key: " + err.Error())
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-ca"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		panic("create test cert: " + err.Error())
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}()

// TestWebhookHandlerObjectSplit verifies that the webhook methods operate on
// the obj parameter, not the receiver. This mirrors how controller-runtime
// calls them in production: the registered handler is always an empty
// &HTTPProbe{}, and the actual incoming object arrives as the obj argument.
func TestWebhookHandlerObjectSplit(t *testing.T) {
	handler := &HTTPProbe{} // empty — like the instance passed to WithDefaulter/WithValidator
	probe := &HTTPProbe{}   // the "incoming" object

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

	probe.Spec.Request.URL = "http://example.com"
	if _, err := handler.ValidateCreate(context.Background(), probe); err != nil {
		t.Fatalf("ValidateCreate failed on valid probe: %v", err)
	}
	if _, err := handler.ValidateUpdate(context.Background(), nil, probe); err != nil {
		t.Fatalf("ValidateUpdate failed on valid probe: %v", err)
	}
}

func TestHTTPProbeDefault(t *testing.T) {
	probe := &HTTPProbe{}
	if err := probe.Default(context.Background(), probe); err != nil {
		t.Fatalf("default failed: %v", err)
	}

	if probe.Spec.Interval.Duration != 30*time.Second {
		t.Fatalf("unexpected interval: %v", probe.Spec.Interval.Duration)
	}
	if probe.Spec.Timeout.Duration != 10*time.Second {
		t.Fatalf("unexpected timeout: %v", probe.Spec.Timeout.Duration)
	}
	if probe.Spec.Request.Method != http.MethodGet {
		t.Fatalf("unexpected method: %s", probe.Spec.Request.Method)
	}
	if probe.Spec.Assertions.Status != 200 {
		t.Fatalf("unexpected status assertion: %d", probe.Spec.Assertions.Status)
	}
	if probe.Status.Summary != nil {
		t.Fatal("defaulting should not mutate status")
	}
}

func TestHTTPProbeDefaultDoesNotOverwrite(t *testing.T) {
	probe := &HTTPProbe{
		Spec: HTTPProbeSpec{
			Interval:   metav1.Duration{Duration: 60 * time.Second},
			Timeout:    metav1.Duration{Duration: 5 * time.Second},
			Request:    HTTPRequestSpec{Method: "GET"},
			Assertions: HTTPAssertions{Status: 201},
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
	if probe.Spec.Assertions.Status != 201 {
		t.Fatalf("status should not be overwritten, got %d", probe.Spec.Assertions.Status)
	}
}

func TestHTTPProbeValidate(t *testing.T) {
	probe := &HTTPProbe{}
	_ = probe.Default(context.Background(), probe)
	probe.Spec.Request.URL = "http://127.0.0.1/health"

	if _, err := probe.ValidateCreate(context.Background(), probe); err != nil {
		t.Fatalf("expected valid probe, got %v", err)
	}

	probe.Spec.Request.Method = "DELETE"
	if _, err := probe.ValidateCreate(context.Background(), probe); err == nil {
		t.Fatal("expected DELETE validation to fail")
	}
}

func TestHTTPProbeValidateRules(t *testing.T) {
	validBase := func() *HTTPProbe {
		p := &HTTPProbe{}
		_ = p.Default(context.Background(), p)
		p.Spec.Request.URL = "http://127.0.0.1/health"
		return p
	}

	cases := []struct {
		name    string
		mutate  func(*HTTPProbe)
		wantErr bool
	}{
		{
			name:    "zero interval rejected",
			mutate:  func(p *HTTPProbe) { p.Spec.Interval.Duration = 0 },
			wantErr: true,
		},
		{
			name:    "zero timeout rejected",
			mutate:  func(p *HTTPProbe) { p.Spec.Timeout.Duration = 0 },
			wantErr: true,
		},
		{
			name: "timeout greater than interval rejected",
			mutate: func(p *HTTPProbe) {
				p.Spec.Interval.Duration = 5 * time.Second
				p.Spec.Timeout.Duration = 10 * time.Second
			},
			wantErr: true,
		},
		{
			name:    "empty URL rejected",
			mutate:  func(p *HTTPProbe) { p.Spec.Request.URL = "" },
			wantErr: true,
		},
		{
			name:    "relative URL rejected",
			mutate:  func(p *HTTPProbe) { p.Spec.Request.URL = "/health" },
			wantErr: true,
		},
		{
			name:    "non-http scheme rejected",
			mutate:  func(p *HTTPProbe) { p.Spec.Request.URL = "ftp://127.0.0.1" },
			wantErr: true,
		},
		{
			name:    "status code below 100 rejected",
			mutate:  func(p *HTTPProbe) { p.Spec.Assertions.Status = 99 },
			wantErr: true,
		},
		{
			name:    "status code above 599 rejected",
			mutate:  func(p *HTTPProbe) { p.Spec.Assertions.Status = 600 },
			wantErr: true,
		},
		{
			name:    "http scheme accepted",
			mutate:  func(p *HTTPProbe) { p.Spec.Request.URL = "http://127.0.0.1/health" },
			wantErr: false,
		},
		{
			name:    "status 404 accepted",
			mutate:  func(p *HTTPProbe) { p.Spec.Assertions.Status = 404 },
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

func TestHTTPProbeValidatePhase2Rules(t *testing.T) {
	validBase := func() *HTTPProbe {
		p := &HTTPProbe{}
		_ = p.Default(context.Background(), p)
		p.Spec.Request.URL = "http://127.0.0.1/health"
		return p
	}

	cases := []struct {
		name    string
		mutate  func(*HTTPProbe)
		wantErr bool
	}{
		{
			name:    "HEAD method accepted",
			mutate:  func(p *HTTPProbe) { p.Spec.Request.Method = "HEAD" },
			wantErr: false,
		},
		{
			name:    "POST method accepted",
			mutate:  func(p *HTTPProbe) { p.Spec.Request.Method = "POST" },
			wantErr: false,
		},
		{
			name:    "DELETE method rejected",
			mutate:  func(p *HTTPProbe) { p.Spec.Request.Method = "DELETE" },
			wantErr: true,
		},
		{
			name: "POST with request body accepted",
			mutate: func(p *HTTPProbe) {
				p.Spec.Request.Method = "POST"
				p.Spec.Request.Body = `{"key":"value"}`
			},
			wantErr: false,
		},
		{
			name: "HEAD with request body rejected",
			mutate: func(p *HTTPProbe) {
				p.Spec.Request.Method = "HEAD"
				p.Spec.Request.Body = "some body"
			},
			wantErr: true,
		},
		{
			name: "valid latency assertion",
			mutate: func(p *HTTPProbe) {
				p.Spec.Assertions.Latency = &LatencyAssertion{MaxMs: 500}
			},
			wantErr: false,
		},
		{
			name: "zero latency maxMs rejected",
			mutate: func(p *HTTPProbe) {
				p.Spec.Assertions.Latency = &LatencyAssertion{MaxMs: 0}
			},
			wantErr: true,
		},
		{
			name: "negative latency maxMs rejected",
			mutate: func(p *HTTPProbe) {
				p.Spec.Assertions.Latency = &LatencyAssertion{MaxMs: -1}
			},
			wantErr: true,
		},
		{
			name: "valid body contains assertion",
			mutate: func(p *HTTPProbe) {
				p.Spec.Assertions.Body = &BodyAssertion{Contains: "ok"}
			},
			wantErr: false,
		},
		{
			name: "valid body json assertion",
			mutate: func(p *HTTPProbe) {
				p.Spec.Assertions.Body = &BodyAssertion{
					JSON: []JSONAssertion{{Path: "$.status", Value: "ok"}},
				}
			},
			wantErr: false,
		},
		{
			name: "body json path without dollar rejected",
			mutate: func(p *HTTPProbe) {
				p.Spec.Assertions.Body = &BodyAssertion{
					JSON: []JSONAssertion{{Path: "status", Value: "ok"}},
				}
			},
			wantErr: true,
		},
		{
			name: "body json path with only dollar accepted",
			mutate: func(p *HTTPProbe) {
				p.Spec.Assertions.Body = &BodyAssertion{
					JSON: []JSONAssertion{{Path: "$", Value: "ok"}},
				}
			},
			wantErr: false,
		},
		{
			name: "body assertions with HEAD rejected",
			mutate: func(p *HTTPProbe) {
				p.Spec.Request.Method = "HEAD"
				p.Spec.Assertions.Body = &BodyAssertion{Contains: "ok"}
			},
			wantErr: true,
		},
		{
			name: "tls insecureSkipVerify accepted",
			mutate: func(p *HTTPProbe) {
				p.Spec.TLS = &TLSConfig{InsecureSkipVerify: true}
			},
			wantErr: false,
		},
		{
			name: "tls valid CA cert accepted",
			mutate: func(p *HTTPProbe) {
				p.Spec.TLS = &TLSConfig{CACert: validTestCACert}
			},
			wantErr: false,
		},
		{
			name: "tls invalid CA cert rejected",
			mutate: func(p *HTTPProbe) {
				p.Spec.TLS = &TLSConfig{CACert: "not-valid-pem"}
			},
			wantErr: true,
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

func TestHTTPProbeValidateUpdate(t *testing.T) {
	valid := &HTTPProbe{}
	_ = valid.Default(context.Background(), valid)
	valid.Spec.Request.URL = "http://127.0.0.1/health"

	if _, err := valid.ValidateUpdate(context.Background(), nil, valid); err != nil {
		t.Fatalf("expected valid update, got %v", err)
	}

	invalid := &HTTPProbe{}
	_ = invalid.Default(context.Background(), invalid)
	invalid.Spec.Request.URL = "not-a-url"
	if _, err := invalid.ValidateUpdate(context.Background(), nil, invalid); err == nil {
		t.Fatal("expected ValidateUpdate to reject invalid URL")
	}
}
