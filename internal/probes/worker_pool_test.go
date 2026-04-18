package probes

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
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

// --- resultToProbeState tests: pure conversion of Result → ProbeState ---

func TestResultToProbeStateFields(t *testing.T) {
	state := resultToProbeState(Result{
		StatusCode: 200,
		Completed:  time.Now(),
		Duration:   50 * time.Millisecond,
	})

	if state.DurationMilliseconds != 50 {
		t.Fatalf("expected DurationMilliseconds=50, got %f", state.DurationMilliseconds)
	}
	if state.HTTPStatusCode != 200 {
		t.Fatalf("expected HTTPStatusCode=200, got %f", state.HTTPStatusCode)
	}
}

func TestResultToProbeStateTLSCertExpiry(t *testing.T) {
	expiry := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	state := resultToProbeState(Result{
		StatusCode:     200,
		Completed:      time.Now(),
		CertExpiryTime: &expiry,
	})

	if state.TLSCertExpiry != float64(expiry.Unix()) {
		t.Fatalf("expected TLSCertExpiry %f, got %f", float64(expiry.Unix()), state.TLSCertExpiry)
	}
}

func TestResultToProbeStateNoTLSCertExpiry(t *testing.T) {
	state := resultToProbeState(Result{StatusCode: 200, Completed: time.Now()})

	if state.TLSCertExpiry != 0 {
		t.Fatalf("expected TLSCertExpiry=0 for non-TLS result, got %f", state.TLSCertExpiry)
	}
}

// --- WorkerPool tests ---

func TestNewWorkerPoolMinConcurrency(t *testing.T) {
	pool := NewWorkerPool(logr.Discard(), 0)
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

// runHTTPJob is a test helper that builds a Job, runs it synchronously, and
// returns the ProbeState recorded in the store.
func runHTTPJob(t *testing.T, probe *syntheticsv1alpha1.HTTPProbe, exec Executor) internalmetrics.ProbeState {
	t.Helper()
	store, err := internalmetrics.NewStore()
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	job := NewHTTPJob(probe, exec, store)
	job.Run(context.Background())
	state, _ := store.Snapshot(job.Key)
	return state
}

func TestHTTPProbeJobAssertionStatusCodePass(t *testing.T) {
	probe := makeHTTPProbe([]syntheticsv1alpha1.Assertion{
		{Name: "status_ok", Expr: "status_code = 200"},
	})
	state := runHTTPJob(t, probe, fixedExecutor{result: Result{StatusCode: 200}})

	if state.Result != internalmetrics.ResultOK {
		t.Fatalf("expected Result=ok, got %q (failed_assertion=%q)", state.Result, state.FailedAssertion)
	}
	if state.FailedAssertion != "" {
		t.Fatalf("expected no failed_assertion, got %q", state.FailedAssertion)
	}
}

func TestHTTPProbeJobAssertionStatusCodeFail(t *testing.T) {
	probe := makeHTTPProbe([]syntheticsv1alpha1.Assertion{
		{Name: "status_ok", Expr: "status_code = 200"},
	})
	state := runHTTPJob(t, probe, fixedExecutor{result: Result{StatusCode: 503}})

	if state.Result != internalmetrics.ResultAssertionFailed {
		t.Fatalf("expected Result=assertion_failed, got %q", state.Result)
	}
	if state.FailedAssertion != "status_ok" {
		t.Fatalf("expected FailedAssertion=status_ok, got %q", state.FailedAssertion)
	}
}

func TestHTTPProbeJobAssertionDurationPass(t *testing.T) {
	probe := makeHTTPProbe([]syntheticsv1alpha1.Assertion{
		{Name: "fast", Expr: "duration_ms < 500"},
	})
	state := runHTTPJob(t, probe, fixedExecutor{result: Result{StatusCode: 200, Duration: 100 * time.Millisecond}})

	if state.Result != internalmetrics.ResultOK {
		t.Fatalf("expected Result=ok, got %q (failed_assertion=%q)", state.Result, state.FailedAssertion)
	}
}

func TestHTTPProbeJobAssertionDurationFail(t *testing.T) {
	probe := makeHTTPProbe([]syntheticsv1alpha1.Assertion{
		{Name: "fast", Expr: "duration_ms < 500"},
	})
	state := runHTTPJob(t, probe, fixedExecutor{result: Result{StatusCode: 200, Duration: 600 * time.Millisecond}})

	if state.Result != internalmetrics.ResultAssertionFailed {
		t.Fatalf("expected Result=assertion_failed, got %q", state.Result)
	}
	if state.FailedAssertion != "fast" {
		t.Fatalf("expected FailedAssertion=fast, got %q", state.FailedAssertion)
	}
}

func TestHTTPProbeJobAssertionSSLExpiryWithCert(t *testing.T) {
	expiry := time.Now().Add(30 * 24 * time.Hour)
	probe := makeHTTPProbe([]syntheticsv1alpha1.Assertion{
		{Name: "ssl_ok", Expr: "ssl_expiry_days >= 14"},
	})
	state := runHTTPJob(t, probe, fixedExecutor{result: Result{
		StatusCode:     200,
		CertExpiryTime: &expiry,
	}})

	if state.Result != internalmetrics.ResultOK {
		t.Fatalf("expected Result=ok, got %q", state.Result)
	}
}

func TestHTTPProbeJobAssertionSSLExpiryExpiringSoon(t *testing.T) {
	expiry := time.Now().Add(5 * 24 * time.Hour)
	probe := makeHTTPProbe([]syntheticsv1alpha1.Assertion{
		{Name: "ssl_ok", Expr: "ssl_expiry_days >= 14"},
	})
	state := runHTTPJob(t, probe, fixedExecutor{result: Result{
		StatusCode:     200,
		CertExpiryTime: &expiry,
	}})

	if state.Result != internalmetrics.ResultAssertionFailed {
		t.Fatalf("expected Result=assertion_failed, got %q", state.Result)
	}
	if state.FailedAssertion != "ssl_ok" {
		t.Fatalf("expected FailedAssertion=ssl_ok, got %q", state.FailedAssertion)
	}
}

func TestHTTPProbeJobAssertionSSLExpiryNoCert(t *testing.T) {
	probe := makeHTTPProbe([]syntheticsv1alpha1.Assertion{
		{Name: "ssl_ok", Expr: "ssl_expiry_days >= 14"},
	})
	state := runHTTPJob(t, probe, fixedExecutor{result: Result{StatusCode: 200}})

	if state.Result != internalmetrics.ResultAssertionFailed {
		t.Fatalf("expected Result=assertion_failed when no cert, got %q", state.Result)
	}
}

func TestHTTPProbeJobConfigError(t *testing.T) {
	probe := makeHTTPProbe([]syntheticsv1alpha1.Assertion{
		{Name: "status_ok", Expr: "status_code = 200"},
	})
	state := runHTTPJob(t, probe, fixedExecutor{result: Result{ConfigError: true}})

	if state.Result != internalmetrics.ResultConfigError {
		t.Fatalf("expected Result=config_error, got %q", state.Result)
	}
}

func TestHTTPProbeJobTransportError(t *testing.T) {
	// No assertions, transport returned an error.
	probe := makeHTTPProbe(nil)
	state := runHTTPJob(t, probe, fixedExecutor{result: Result{TransportErr: errors.New("connection refused")}})

	if state.Result == internalmetrics.ResultOK {
		t.Fatalf("expected non-ok Result on transport error, got %q", state.Result)
	}
	// Raw error string won't classify as dial/DNS/TLS — falls through to connect_refused.
	if state.Result != internalmetrics.ResultConnectRefused {
		t.Fatalf("expected Result=connect_refused for fallback, got %q", state.Result)
	}
}

func TestHTTPProbeJobNoAssertionsNoTransportError(t *testing.T) {
	// No assertions, transport OK — probe is ok (no checks to fail).
	probe := makeHTTPProbe(nil)
	state := runHTTPJob(t, probe, fixedExecutor{result: Result{StatusCode: 200}})

	if state.Result != internalmetrics.ResultOK {
		t.Fatalf("expected Result=ok, got %q", state.Result)
	}
}
