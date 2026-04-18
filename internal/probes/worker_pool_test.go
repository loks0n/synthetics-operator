package probes

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
)

// --- HTTPExecutor tests (unchanged: test the executor in isolation) ---

func TestHTTPExecutorSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("expected GET, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	result := HTTPExecutor{}.Execute(context.Background(), &syntheticsv1alpha1.HTTPProbe{
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Request: syntheticsv1alpha1.HTTPRequestSpec{URL: server.URL, Method: http.MethodGet},
			Timeout: metav1.Duration{Duration: time.Second},
		},
	})

	if !result.Success() {
		t.Fatalf("expected success, got %+v", result)
	}
	if result.ConfigError {
		t.Fatalf("unexpected config error: %+v", result)
	}
}

func TestHTTPExecutorConfigError(t *testing.T) {
	result := HTTPExecutor{}.Execute(context.Background(), &syntheticsv1alpha1.HTTPProbe{
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Request: syntheticsv1alpha1.HTTPRequestSpec{URL: "://bad-url", Method: http.MethodGet},
		},
	})

	if !result.ConfigError {
		t.Fatalf("expected config error, got %+v", result)
	}
}

func TestHTTPExecutorStatusMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	result := HTTPExecutor{}.Execute(context.Background(), &syntheticsv1alpha1.HTTPProbe{
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Request: syntheticsv1alpha1.HTTPRequestSpec{URL: server.URL, Method: http.MethodGet},
			Timeout: metav1.Duration{Duration: time.Second},
		},
	})

	// Any HTTP response is a success now (status code > 0)
	if !result.Success() {
		t.Fatal("expected success: any HTTP response is a success")
	}
	if result.ConfigError {
		t.Fatal("404 response is not a config error")
	}
	if result.StatusCode != http.StatusNotFound {
		t.Fatalf("expected status code 404, got %d", result.StatusCode)
	}
}

func TestHTTPExecutorTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	result := HTTPExecutor{}.Execute(ctx, &syntheticsv1alpha1.HTTPProbe{
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Request: syntheticsv1alpha1.HTTPRequestSpec{URL: server.URL, Method: http.MethodGet},
		},
	})

	if result.Success() {
		t.Fatal("expected failure on timeout")
	}
	if result.ConfigError {
		t.Fatal("timeout is not a config error")
	}
}

func TestHTTPExecutorNetworkError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	url := server.URL
	server.Close()

	result := HTTPExecutor{}.Execute(context.Background(), &syntheticsv1alpha1.HTTPProbe{
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Request: syntheticsv1alpha1.HTTPRequestSpec{URL: url, Method: http.MethodGet},
			Timeout: metav1.Duration{Duration: time.Second},
		},
	})

	if result.Success() {
		t.Fatal("expected failure on connection refused")
	}
	if result.ConfigError {
		t.Fatal("network error is not a config error")
	}
}

func TestHTTPExecutorSendsHeaders(t *testing.T) {
	var received string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.Header.Get("X-Test-Header")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	HTTPExecutor{}.Execute(context.Background(), &syntheticsv1alpha1.HTTPProbe{
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Request: syntheticsv1alpha1.HTTPRequestSpec{
				URL:     server.URL,
				Method:  http.MethodGet,
				Headers: map[string]string{"X-Test-Header": "hello"},
			},
			Timeout: metav1.Duration{Duration: time.Second},
		},
	})

	if received != "hello" {
		t.Fatalf("expected header value 'hello', got %q", received)
	}
}

func TestHTTPExecutorPOSTSendsBody(t *testing.T) {
	var receivedBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		b, _ := io.ReadAll(r.Body)
		receivedBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	result := HTTPExecutor{}.Execute(context.Background(), &syntheticsv1alpha1.HTTPProbe{
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Request: syntheticsv1alpha1.HTTPRequestSpec{
				URL:    server.URL,
				Method: http.MethodPost,
				Body:   `{"hello":"world"}`,
			},
			Timeout: metav1.Duration{Duration: time.Second},
		},
	})

	if !result.Success() {
		t.Fatalf("expected success, got %+v", result)
	}
	if receivedBody != `{"hello":"world"}` {
		t.Fatalf("expected body %q, got %q", `{"hello":"world"}`, receivedBody)
	}
}

func TestHTTPExecutorTLSInsecureSkipVerify(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	result := HTTPExecutor{}.Execute(context.Background(), &syntheticsv1alpha1.HTTPProbe{
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Request: syntheticsv1alpha1.HTTPRequestSpec{URL: server.URL, Method: http.MethodGet},
			Timeout: metav1.Duration{Duration: time.Second},
			TLS:     &syntheticsv1alpha1.TLSConfig{InsecureSkipVerify: true},
		},
	})

	if !result.Success() {
		t.Fatalf("expected success with insecureSkipVerify, got %+v", result)
	}
}

func TestHTTPExecutorTLSFailsWithoutConfig(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	result := HTTPExecutor{}.Execute(context.Background(), &syntheticsv1alpha1.HTTPProbe{
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Request: syntheticsv1alpha1.HTTPRequestSpec{URL: server.URL, Method: http.MethodGet},
			Timeout: metav1.Duration{Duration: time.Second},
		},
	})

	if result.Success() {
		t.Fatal("expected TLS failure against self-signed cert with no TLS config")
	}
	if result.ConfigError {
		t.Fatal("TLS verification failure is not a config error")
	}
}

func TestHTTPExecutorTLSCustomCA(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	leaf, err := x509.ParseCertificate(server.TLS.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatalf("parse server cert: %v", err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw})

	result := HTTPExecutor{}.Execute(context.Background(), &syntheticsv1alpha1.HTTPProbe{
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Request: syntheticsv1alpha1.HTTPRequestSpec{URL: server.URL, Method: http.MethodGet},
			Timeout: metav1.Duration{Duration: time.Second},
			TLS:     &syntheticsv1alpha1.TLSConfig{CACert: string(caPEM)},
		},
	})

	if !result.Success() {
		t.Fatalf("expected success with custom CA cert, got %+v", result)
	}
}

func TestHTTPExecutorTLSCertExpiryPopulated(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	result := HTTPExecutor{}.Execute(context.Background(), &syntheticsv1alpha1.HTTPProbe{
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Request: syntheticsv1alpha1.HTTPRequestSpec{URL: server.URL, Method: http.MethodGet},
			Timeout: metav1.Duration{Duration: time.Second},
			TLS:     &syntheticsv1alpha1.TLSConfig{InsecureSkipVerify: true},
		},
	})

	if result.CertExpiryTime == nil {
		t.Fatal("expected CertExpiryTime to be set for HTTPS probe")
	}
	if result.CertExpiryTime.IsZero() {
		t.Fatal("expected non-zero CertExpiryTime")
	}
}

func TestHTTPExecutorHTTPNoCertExpiry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	result := HTTPExecutor{}.Execute(context.Background(), &syntheticsv1alpha1.HTTPProbe{
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Request: syntheticsv1alpha1.HTTPRequestSpec{URL: server.URL, Method: http.MethodGet},
			Timeout: metav1.Duration{Duration: time.Second},
		},
	})

	if result.CertExpiryTime != nil {
		t.Fatal("expected CertExpiryTime to be nil for plain HTTP probe")
	}
}

func TestHTTPExecutorTLSInvalidCACert(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	result := HTTPExecutor{}.Execute(context.Background(), &syntheticsv1alpha1.HTTPProbe{
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Request: syntheticsv1alpha1.HTTPRequestSpec{URL: server.URL, Method: http.MethodGet},
			Timeout: metav1.Duration{Duration: time.Second},
			TLS:     &syntheticsv1alpha1.TLSConfig{CACert: "not-valid-pem"},
		},
	})

	if !result.ConfigError {
		t.Fatalf("expected config error for invalid CA cert PEM, got %+v", result)
	}
}

func TestHTTPExecutorBuildRequestError(t *testing.T) {
	result := HTTPExecutor{}.Execute(context.Background(), &syntheticsv1alpha1.HTTPProbe{
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Request: syntheticsv1alpha1.HTTPRequestSpec{URL: "http://127.0.0.1/", Method: "INVALID METHOD WITH SPACES"},
		},
	})

	if !result.ConfigError {
		t.Fatalf("expected config error for invalid method, got %+v", result)
	}
}

// --- assertion evaluation tests against EvalHTTPAssertions ---

func makeHTTPProbe(assertions []syntheticsv1alpha1.Assertion) *syntheticsv1alpha1.HTTPProbe {
	return &syntheticsv1alpha1.HTTPProbe{
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Timeout:    metav1.Duration{Duration: time.Second},
			Assertions: assertions,
		},
	}
}

func TestEvalHTTPAssertions_StatusPass(t *testing.T) {
	probe := makeHTTPProbe([]syntheticsv1alpha1.Assertion{{Name: "status_ok", Expr: "status_code = 200"}})
	outcome, failed, _ := EvalHTTPAssertions(Result{StatusCode: 200}, probe.Spec.Assertions)
	if outcome != "ok" || failed != "" {
		t.Fatalf("expected ok, got outcome=%q failed=%q", outcome, failed)
	}
}

func TestEvalHTTPAssertions_StatusFail(t *testing.T) {
	probe := makeHTTPProbe([]syntheticsv1alpha1.Assertion{{Name: "status_ok", Expr: "status_code = 200"}})
	outcome, failed, _ := EvalHTTPAssertions(Result{StatusCode: 503}, probe.Spec.Assertions)
	if outcome != "assertion_failed" || failed != "status_ok" {
		t.Fatalf("expected assertion_failed status_ok, got outcome=%q failed=%q", outcome, failed)
	}
}

func TestEvalHTTPAssertions_DurationPass(t *testing.T) {
	probe := makeHTTPProbe([]syntheticsv1alpha1.Assertion{{Name: "fast", Expr: "duration_ms < 500"}})
	outcome, _, _ := EvalHTTPAssertions(Result{StatusCode: 200, Duration: 100 * time.Millisecond}, probe.Spec.Assertions)
	if outcome != "ok" {
		t.Fatalf("expected ok, got %q", outcome)
	}
}

func TestEvalHTTPAssertions_DurationFail(t *testing.T) {
	probe := makeHTTPProbe([]syntheticsv1alpha1.Assertion{{Name: "fast", Expr: "duration_ms < 500"}})
	outcome, failed, _ := EvalHTTPAssertions(Result{StatusCode: 200, Duration: 600 * time.Millisecond}, probe.Spec.Assertions)
	if outcome != "assertion_failed" || failed != "fast" {
		t.Fatalf("expected assertion_failed fast, got outcome=%q failed=%q", outcome, failed)
	}
}

func TestEvalHTTPAssertions_SSLExpiryWithCert(t *testing.T) {
	expiry := time.Now().Add(30 * 24 * time.Hour)
	probe := makeHTTPProbe([]syntheticsv1alpha1.Assertion{{Name: "ssl_ok", Expr: "ssl_expiry_days >= 14"}})
	outcome, _, _ := EvalHTTPAssertions(Result{StatusCode: 200, CertExpiryTime: &expiry}, probe.Spec.Assertions)
	if outcome != "ok" {
		t.Fatalf("expected ok, got %q", outcome)
	}
}

func TestEvalHTTPAssertions_SSLExpiryExpiringSoon(t *testing.T) {
	expiry := time.Now().Add(5 * 24 * time.Hour)
	probe := makeHTTPProbe([]syntheticsv1alpha1.Assertion{{Name: "ssl_ok", Expr: "ssl_expiry_days >= 14"}})
	outcome, failed, _ := EvalHTTPAssertions(Result{StatusCode: 200, CertExpiryTime: &expiry}, probe.Spec.Assertions)
	if outcome != "assertion_failed" || failed != "ssl_ok" {
		t.Fatalf("expected assertion_failed ssl_ok, got outcome=%q failed=%q", outcome, failed)
	}
}

func TestEvalHTTPAssertions_SSLExpiryNoCert(t *testing.T) {
	probe := makeHTTPProbe([]syntheticsv1alpha1.Assertion{{Name: "ssl_ok", Expr: "ssl_expiry_days >= 14"}})
	outcome, _, _ := EvalHTTPAssertions(Result{StatusCode: 200}, probe.Spec.Assertions)
	if outcome != "assertion_failed" {
		t.Fatalf("expected assertion_failed when no cert, got %q", outcome)
	}
}

func TestClassifyHTTPTransport_FallbacksToConnectRefused(t *testing.T) {
	// Raw error string won't classify as dns/dial/tls — falls through to connect_refused.
	outcome := ClassifyHTTPTransport(unknownError{})
	if outcome != "connect_refused" {
		t.Fatalf("expected connect_refused fallback, got %q", outcome)
	}
}

type unknownError struct{}

func (unknownError) Error() string { return "something else" }
