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

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
	internalmetrics "github.com/loks0n/synthetics-operator/internal/metrics"
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

	if !result.Success {
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
	if !result.Success {
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

	if result.Success {
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

	if result.Success {
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

	if !result.Success {
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

	if !result.Success {
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

	if result.Success {
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

	if !result.Success {
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

// --- resultToProbeState tests: pure conversion of Result → ProbeState ---

func TestResultToProbeStateSuccess(t *testing.T) {
	state := resultToProbeState(Result{
		Success:    true,
		StatusCode: 200,
		Completed:  time.Now(),
		Duration:   50 * time.Millisecond,
	})

	if state.Success != 1 {
		t.Fatalf("expected Success=1, got %f", state.Success)
	}
	if state.ConfigError != 0 {
		t.Fatalf("expected ConfigError=0, got %f", state.ConfigError)
	}
	if state.DurationMilliseconds != 50 {
		t.Fatalf("expected DurationMilliseconds=50, got %f", state.DurationMilliseconds)
	}
}

func TestResultToProbeStateFailure(t *testing.T) {
	// No status code = not a success
	state := resultToProbeState(Result{Success: false, StatusCode: 0, Completed: time.Now()})

	if state.Success != 0 {
		t.Fatal("expected Success=0 when status code is 0")
	}
	if state.ConfigError != 0 {
		t.Fatal("expected ConfigError=0 for probe failure (not config error)")
	}
}

func TestResultToProbeStateConfigError(t *testing.T) {
	state := resultToProbeState(Result{ConfigError: true, Completed: time.Now()})

	if state.ConfigError != 1 {
		t.Fatal("expected ConfigError=1")
	}
	if state.Success != 0 {
		t.Fatal("expected Success=0 on config error")
	}
}

func TestResultToProbeStateTLSCertExpiry(t *testing.T) {
	expiry := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	state := resultToProbeState(Result{
		Success:        true,
		StatusCode:     200,
		Completed:      time.Now(),
		CertExpiryTime: &expiry,
	})

	if state.TLSCertExpiry != float64(expiry.Unix()) {
		t.Fatalf("expected TLSCertExpiry %f, got %f", float64(expiry.Unix()), state.TLSCertExpiry)
	}
}

func TestResultToProbeStateNoTLSCertExpiry(t *testing.T) {
	state := resultToProbeState(Result{Success: true, StatusCode: 200, Completed: time.Now()})

	if state.TLSCertExpiry != 0 {
		t.Fatalf("expected TLSCertExpiry=0 for non-TLS result, got %f", state.TLSCertExpiry)
	}
}

// --- WorkerPool tests ---

func TestNewWorkerPoolMinConcurrency(t *testing.T) {
	store, err := internalmetrics.NewStore()
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	pool := NewWorkerPool(logr.Discard(), 0, store)
	if cap(pool.queue) != 16 {
		t.Fatalf("expected queue cap 16 for min concurrency, got %d", cap(pool.queue))
	}
}

type fixedExecutor struct{ result Result }

func (f fixedExecutor) Execute(_ context.Context, _ *syntheticsv1alpha1.HTTPProbe) Result {
	return f.result
}

// --- newHTTPProbeJob assertion evaluation tests ---

func makeHTTPProbe(assertions []syntheticsv1alpha1.Assertion) *syntheticsv1alpha1.HTTPProbe {
	return &syntheticsv1alpha1.HTTPProbe{
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Timeout:    metav1.Duration{Duration: time.Second},
			Assertions: assertions,
		},
	}
}

func TestHTTPProbeJobAssertionStatusCodePass(t *testing.T) {
	probe := makeHTTPProbe([]syntheticsv1alpha1.Assertion{
		{Name: "status_ok", Expr: "status_code = 200"},
	})
	exec := fixedExecutor{result: Result{Success: true, StatusCode: 200}}
	job := newHTTPProbeJob(probe, exec)

	state := job.run(context.Background())

	if state.Success != 1 {
		t.Fatalf("expected success=1, got %f (reason=%q)", state.Success, state.FailureReason)
	}
	if state.FailureReason != "" {
		t.Fatalf("expected no failure reason, got %q", state.FailureReason)
	}
}

func TestHTTPProbeJobAssertionStatusCodeFail(t *testing.T) {
	probe := makeHTTPProbe([]syntheticsv1alpha1.Assertion{
		{Name: "status_ok", Expr: "status_code = 200"},
	})
	exec := fixedExecutor{result: Result{Success: true, StatusCode: 503}}
	job := newHTTPProbeJob(probe, exec)

	state := job.run(context.Background())

	if state.Success != 0 {
		t.Fatal("expected success=0 when assertion fails")
	}
	if state.FailureReason != "status_ok" {
		t.Fatalf("expected FailureReason=status_ok, got %q", state.FailureReason)
	}
}

func TestHTTPProbeJobAssertionDurationPass(t *testing.T) {
	probe := makeHTTPProbe([]syntheticsv1alpha1.Assertion{
		{Name: "fast", Expr: "duration_ms < 500"},
	})
	exec := fixedExecutor{result: Result{Success: true, StatusCode: 200, Duration: 100 * time.Millisecond}}
	job := newHTTPProbeJob(probe, exec)

	state := job.run(context.Background())

	if state.Success != 1 {
		t.Fatalf("expected success=1, got %f (reason=%q)", state.Success, state.FailureReason)
	}
}

func TestHTTPProbeJobAssertionDurationFail(t *testing.T) {
	probe := makeHTTPProbe([]syntheticsv1alpha1.Assertion{
		{Name: "fast", Expr: "duration_ms < 500"},
	})
	exec := fixedExecutor{result: Result{Success: true, StatusCode: 200, Duration: 600 * time.Millisecond}}
	job := newHTTPProbeJob(probe, exec)

	state := job.run(context.Background())

	if state.Success != 0 {
		t.Fatal("expected success=0 when duration assertion fails")
	}
	if state.FailureReason != "fast" {
		t.Fatalf("expected FailureReason=fast, got %q", state.FailureReason)
	}
}

func TestHTTPProbeJobAssertionSSLExpiryWithCert(t *testing.T) {
	// Cert expiry far in the future: assertion passes.
	expiry := time.Now().Add(30 * 24 * time.Hour)
	probe := makeHTTPProbe([]syntheticsv1alpha1.Assertion{
		{Name: "ssl_ok", Expr: "ssl_expiry_days >= 14"},
	})
	exec := fixedExecutor{result: Result{
		Success:        true,
		StatusCode:     200,
		CertExpiryTime: &expiry,
	}}
	job := newHTTPProbeJob(probe, exec)

	state := job.run(context.Background())

	if state.Success != 1 {
		t.Fatalf("expected success=1, got %f (reason=%q)", state.Success, state.FailureReason)
	}
}

func TestHTTPProbeJobAssertionSSLExpiryExpiringSoon(t *testing.T) {
	expiry := time.Now().Add(5 * 24 * time.Hour)
	probe := makeHTTPProbe([]syntheticsv1alpha1.Assertion{
		{Name: "ssl_ok", Expr: "ssl_expiry_days >= 14"},
	})
	exec := fixedExecutor{result: Result{
		Success:        true,
		StatusCode:     200,
		CertExpiryTime: &expiry,
	}}
	job := newHTTPProbeJob(probe, exec)

	state := job.run(context.Background())

	if state.Success != 0 {
		t.Fatal("expected success=0 when SSL cert expires soon")
	}
	if state.FailureReason != "ssl_ok" {
		t.Fatalf("expected FailureReason=ssl_ok, got %q", state.FailureReason)
	}
}

func TestHTTPProbeJobAssertionSSLExpiryNoCert(t *testing.T) {
	// ssl_expiry_days is -1 when no cert; assertion >= 14 should fail.
	probe := makeHTTPProbe([]syntheticsv1alpha1.Assertion{
		{Name: "ssl_ok", Expr: "ssl_expiry_days >= 14"},
	})
	exec := fixedExecutor{result: Result{Success: true, StatusCode: 200}}
	job := newHTTPProbeJob(probe, exec)

	state := job.run(context.Background())

	if state.Success != 0 {
		t.Fatal("expected success=0 when no cert and ssl_expiry_days assertion used")
	}
}

func TestHTTPProbeJobConfigErrorReasonSet(t *testing.T) {
	probe := makeHTTPProbe([]syntheticsv1alpha1.Assertion{
		{Name: "status_ok", Expr: "status_code = 200"},
	})
	exec := fixedExecutor{result: Result{ConfigError: true}}
	job := newHTTPProbeJob(probe, exec)

	state := job.run(context.Background())

	if state.Success != 0 {
		t.Fatal("expected success=0 on config error")
	}
	if state.FailureReason != ReasonConfigError {
		t.Fatalf("expected FailureReason=%q, got %q", ReasonConfigError, state.FailureReason)
	}
}

func TestHTTPProbeJobNoAssertionsConnectionError(t *testing.T) {
	// No assertions, status code 0 (connection failure).
	probe := makeHTTPProbe(nil)
	exec := fixedExecutor{result: Result{Success: false, StatusCode: 0}}
	job := newHTTPProbeJob(probe, exec)

	state := job.run(context.Background())

	if state.Success != 0 {
		t.Fatal("expected success=0 on connection failure")
	}
	if state.FailureReason != ReasonConnectionError {
		t.Fatalf("expected FailureReason=%q, got %q", ReasonConnectionError, state.FailureReason)
	}
}

func TestHTTPProbeJobNoAssertionsConfigError(t *testing.T) {
	probe := makeHTTPProbe(nil)
	exec := fixedExecutor{result: Result{ConfigError: true}}
	job := newHTTPProbeJob(probe, exec)

	state := job.run(context.Background())

	if state.FailureReason != ReasonConfigError {
		t.Fatalf("expected FailureReason=%q, got %q", ReasonConfigError, state.FailureReason)
	}
}
