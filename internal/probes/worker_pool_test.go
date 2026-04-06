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
	"k8s.io/apimachinery/pkg/types"

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
			Request:    syntheticsv1alpha1.HTTPRequestSpec{URL: server.URL, Method: http.MethodGet},
			Assertions: syntheticsv1alpha1.HTTPAssertions{Status: http.StatusOK},
			Timeout:    metav1.Duration{Duration: time.Second},
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
			Request:    syntheticsv1alpha1.HTTPRequestSpec{URL: "://bad-url", Method: http.MethodGet},
			Assertions: syntheticsv1alpha1.HTTPAssertions{Status: http.StatusOK},
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
			Request:    syntheticsv1alpha1.HTTPRequestSpec{URL: server.URL, Method: http.MethodGet},
			Assertions: syntheticsv1alpha1.HTTPAssertions{Status: http.StatusOK},
			Timeout:    metav1.Duration{Duration: time.Second},
		},
	})

	if result.Success {
		t.Fatal("expected failure on status mismatch")
	}
	if result.ConfigError {
		t.Fatal("status mismatch is not a config error")
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
			Request:    syntheticsv1alpha1.HTTPRequestSpec{URL: server.URL, Method: http.MethodGet},
			Assertions: syntheticsv1alpha1.HTTPAssertions{Status: http.StatusOK},
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
			Request:    syntheticsv1alpha1.HTTPRequestSpec{URL: url, Method: http.MethodGet},
			Assertions: syntheticsv1alpha1.HTTPAssertions{Status: http.StatusOK},
			Timeout:    metav1.Duration{Duration: time.Second},
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
			Assertions: syntheticsv1alpha1.HTTPAssertions{Status: http.StatusOK},
			Timeout:    metav1.Duration{Duration: time.Second},
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
			Assertions: syntheticsv1alpha1.HTTPAssertions{Status: http.StatusOK},
			Timeout:    metav1.Duration{Duration: time.Second},
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
			Request:    syntheticsv1alpha1.HTTPRequestSpec{URL: server.URL, Method: http.MethodGet},
			Assertions: syntheticsv1alpha1.HTTPAssertions{Status: http.StatusOK},
			Timeout:    metav1.Duration{Duration: time.Second},
			TLS:        &syntheticsv1alpha1.TLSConfig{InsecureSkipVerify: true},
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
			Request:    syntheticsv1alpha1.HTTPRequestSpec{URL: server.URL, Method: http.MethodGet},
			Assertions: syntheticsv1alpha1.HTTPAssertions{Status: http.StatusOK},
			Timeout:    metav1.Duration{Duration: time.Second},
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
			Request:    syntheticsv1alpha1.HTTPRequestSpec{URL: server.URL, Method: http.MethodGet},
			Assertions: syntheticsv1alpha1.HTTPAssertions{Status: http.StatusOK},
			Timeout:    metav1.Duration{Duration: time.Second},
			TLS:        &syntheticsv1alpha1.TLSConfig{CACert: string(caPEM)},
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
			Request:    syntheticsv1alpha1.HTTPRequestSpec{URL: server.URL, Method: http.MethodGet},
			Assertions: syntheticsv1alpha1.HTTPAssertions{Status: http.StatusOK},
			Timeout:    metav1.Duration{Duration: time.Second},
			TLS:        &syntheticsv1alpha1.TLSConfig{InsecureSkipVerify: true},
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
			Request:    syntheticsv1alpha1.HTTPRequestSpec{URL: server.URL, Method: http.MethodGet},
			Assertions: syntheticsv1alpha1.HTTPAssertions{Status: http.StatusOK},
			Timeout:    metav1.Duration{Duration: time.Second},
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
			Request:    syntheticsv1alpha1.HTTPRequestSpec{URL: server.URL, Method: http.MethodGet},
			Assertions: syntheticsv1alpha1.HTTPAssertions{Status: http.StatusOK},
			Timeout:    metav1.Duration{Duration: time.Second},
			TLS:        &syntheticsv1alpha1.TLSConfig{CACert: "not-valid-pem"},
		},
	})

	if !result.ConfigError {
		t.Fatalf("expected config error for invalid CA cert PEM, got %+v", result)
	}
}

func TestHTTPExecutorBuildRequestError(t *testing.T) {
	result := HTTPExecutor{}.Execute(context.Background(), &syntheticsv1alpha1.HTTPProbe{
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Request:    syntheticsv1alpha1.HTTPRequestSpec{URL: "http://127.0.0.1/", Method: "INVALID METHOD WITH SPACES"},
			Assertions: syntheticsv1alpha1.HTTPAssertions{Status: http.StatusOK},
		},
	})

	if !result.ConfigError {
		t.Fatalf("expected config error for invalid method, got %+v", result)
	}
}

func TestHTTPExecutorLatencyAssertionPass(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	result := HTTPExecutor{}.Execute(context.Background(), &syntheticsv1alpha1.HTTPProbe{
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Request:    syntheticsv1alpha1.HTTPRequestSpec{URL: server.URL, Method: http.MethodGet},
			Assertions: syntheticsv1alpha1.HTTPAssertions{Status: http.StatusOK, Latency: &syntheticsv1alpha1.LatencyAssertion{MaxMs: 5000}},
			Timeout:    metav1.Duration{Duration: time.Second},
		},
	})

	if !result.Success {
		t.Fatalf("expected success, got %+v", result)
	}
	for _, ar := range result.AssertionResults {
		if !ar.Passed {
			t.Fatalf("expected all assertions to pass, failed: %+v", ar)
		}
	}
}

func TestHTTPExecutorLatencyAssertionFail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	result := HTTPExecutor{}.Execute(context.Background(), &syntheticsv1alpha1.HTTPProbe{
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Request:    syntheticsv1alpha1.HTTPRequestSpec{URL: server.URL, Method: http.MethodGet},
			Assertions: syntheticsv1alpha1.HTTPAssertions{Status: http.StatusOK, Latency: &syntheticsv1alpha1.LatencyAssertion{MaxMs: 1}},
			Timeout:    metav1.Duration{Duration: time.Second},
		},
	})

	if result.Success {
		t.Fatal("expected failure on latency exceeded")
	}
	var found *AssertionResult
	for i := range result.AssertionResults {
		if result.AssertionResults[i].Type == "latency" {
			found = &result.AssertionResults[i]
		}
	}
	if found == nil || found.Passed {
		t.Fatal("expected latency assertion to fail")
	}
}

func TestHTTPExecutorBodyContainsPass(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	result := HTTPExecutor{}.Execute(context.Background(), &syntheticsv1alpha1.HTTPProbe{
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Request:    syntheticsv1alpha1.HTTPRequestSpec{URL: server.URL, Method: http.MethodGet},
			Assertions: syntheticsv1alpha1.HTTPAssertions{Status: http.StatusOK, Body: &syntheticsv1alpha1.BodyAssertion{Contains: `"status":"ok"`}},
			Timeout:    metav1.Duration{Duration: time.Second},
		},
	})

	if !result.Success {
		t.Fatalf("expected success, got %+v", result)
	}
}

func TestHTTPExecutorBodyContainsFail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"error"}`))
	}))
	defer server.Close()

	result := HTTPExecutor{}.Execute(context.Background(), &syntheticsv1alpha1.HTTPProbe{
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Request:    syntheticsv1alpha1.HTTPRequestSpec{URL: server.URL, Method: http.MethodGet},
			Assertions: syntheticsv1alpha1.HTTPAssertions{Status: http.StatusOK, Body: &syntheticsv1alpha1.BodyAssertion{Contains: `"status":"ok"`}},
			Timeout:    metav1.Duration{Duration: time.Second},
		},
	})

	if result.Success {
		t.Fatal("expected failure on body not containing expected string")
	}
}

func TestHTTPExecutorBodyJSONPass(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok","count":42}`))
	}))
	defer server.Close()

	result := HTTPExecutor{}.Execute(context.Background(), &syntheticsv1alpha1.HTTPProbe{
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Request: syntheticsv1alpha1.HTTPRequestSpec{URL: server.URL, Method: http.MethodGet},
			Assertions: syntheticsv1alpha1.HTTPAssertions{
				Status: http.StatusOK,
				Body:   &syntheticsv1alpha1.BodyAssertion{JSON: []syntheticsv1alpha1.JSONAssertion{{Path: "$.status", Value: "ok"}, {Path: "$.count", Value: "42"}}},
			},
			Timeout: metav1.Duration{Duration: time.Second},
		},
	})

	if !result.Success {
		t.Fatalf("expected success, got %+v", result)
	}
}

func TestHTTPExecutorBodyJSONFail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"degraded"}`))
	}))
	defer server.Close()

	result := HTTPExecutor{}.Execute(context.Background(), &syntheticsv1alpha1.HTTPProbe{
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Request: syntheticsv1alpha1.HTTPRequestSpec{URL: server.URL, Method: http.MethodGet},
			Assertions: syntheticsv1alpha1.HTTPAssertions{
				Status: http.StatusOK,
				Body:   &syntheticsv1alpha1.BodyAssertion{JSON: []syntheticsv1alpha1.JSONAssertion{{Path: "$.status", Value: "ok"}}},
			},
			Timeout: metav1.Duration{Duration: time.Second},
		},
	})

	if result.Success {
		t.Fatal("expected failure on JSON value mismatch")
	}
	var found *AssertionResult
	for i := range result.AssertionResults {
		if result.AssertionResults[i].Type == "body_json" {
			found = &result.AssertionResults[i]
		}
	}
	if found == nil || found.Passed {
		t.Fatal("expected body_json assertion to fail")
	}
}

func TestHTTPExecutorBodyJSONMissingKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	result := HTTPExecutor{}.Execute(context.Background(), &syntheticsv1alpha1.HTTPProbe{
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Request: syntheticsv1alpha1.HTTPRequestSpec{URL: server.URL, Method: http.MethodGet},
			Assertions: syntheticsv1alpha1.HTTPAssertions{
				Status: http.StatusOK,
				Body:   &syntheticsv1alpha1.BodyAssertion{JSON: []syntheticsv1alpha1.JSONAssertion{{Path: "$.missing", Value: "value"}}},
			},
			Timeout: metav1.Duration{Duration: time.Second},
		},
	})

	if result.Success {
		t.Fatal("expected failure on missing JSON key")
	}
}

func TestHTTPExecutorBodyJSONNestedPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"health":"green"}}`))
	}))
	defer server.Close()

	result := HTTPExecutor{}.Execute(context.Background(), &syntheticsv1alpha1.HTTPProbe{
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Request: syntheticsv1alpha1.HTTPRequestSpec{URL: server.URL, Method: http.MethodGet},
			Assertions: syntheticsv1alpha1.HTTPAssertions{
				Status: http.StatusOK,
				Body:   &syntheticsv1alpha1.BodyAssertion{JSON: []syntheticsv1alpha1.JSONAssertion{{Path: "$.data.health", Value: "green"}}},
			},
			Timeout: metav1.Duration{Duration: time.Second},
		},
	})

	if !result.Success {
		t.Fatalf("expected success on nested path, got %+v", result)
	}
}

// --- evalJSONPath unit tests ---

func TestEvalJSONPath(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		path    string
		want    string
		wantErr bool
	}{
		{name: "string field", body: `{"status":"ok"}`, path: "$.status", want: "ok"},
		{name: "integer field", body: `{"count":42}`, path: "$.count", want: "42"},
		{name: "float field", body: `{"ratio":1.5}`, path: "$.ratio", want: "1.5"},
		{name: "bool true", body: `{"ok":true}`, path: "$.ok", want: "true"},
		{name: "bool false", body: `{"ok":false}`, path: "$.ok", want: "false"},
		{name: "null field", body: `{"x":null}`, path: "$.x", want: "null"},
		{name: "nested path", body: `{"a":{"b":"deep"}}`, path: "$.a.b", want: "deep"},
		{name: "root dollar", body: `{"k":"v"}`, path: "$", want: `{"k":"v"}`},
		{name: "complex value marshaled", body: `{"arr":[1,2,3]}`, path: "$.arr", want: `[1,2,3]`},
		{name: "invalid path no dollar", body: `{}`, path: "status", wantErr: true},
		{name: "invalid JSON body", body: `not-json`, path: "$.x", wantErr: true},
		{name: "missing key", body: `{"a":1}`, path: "$.b", wantErr: true},
		{name: "navigate into non-object", body: `{"a":"string"}`, path: "$.a.b", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := evalJSONPath([]byte(tc.body), tc.path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// --- resultToProbeState tests: pure conversion of Result → ProbeState ---

func TestResultToProbeStateSuccess(t *testing.T) {
	state := resultToProbeState(Result{
		Success:   true,
		Completed: time.Now(),
		Duration:  50 * time.Millisecond,
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
	// ConsecutiveFailures is not set by resultToProbeState; runProbe fills it in
	if state.ConsecutiveFailures != 0 {
		t.Fatalf("expected ConsecutiveFailures=0 from conversion, got %f", state.ConsecutiveFailures)
	}
}

func TestResultToProbeStateFailure(t *testing.T) {
	state := resultToProbeState(Result{Success: false, Completed: time.Now()})

	if state.Success != 0 {
		t.Fatal("expected Success=0")
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

func TestResultToProbeStateAssertionsConverted(t *testing.T) {
	state := resultToProbeState(Result{
		Completed: time.Now(),
		AssertionResults: []AssertionResult{
			{Type: "status", Name: "status", Passed: false},
			{Type: "latency", Name: "latency", Passed: true},
		},
	})

	if len(state.AssertionResults) != 2 {
		t.Fatalf("expected 2 assertion results, got %d", len(state.AssertionResults))
	}
	if state.AssertionResults[0].Passed != 0 {
		t.Fatal("status assertion should be Passed=0")
	}
	if state.AssertionResults[1].Passed != 1 {
		t.Fatal("latency assertion should be Passed=1")
	}
}

func TestResultToProbeStateTLSCertExpiry(t *testing.T) {
	expiry := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	state := resultToProbeState(Result{
		Success:        true,
		Completed:      time.Now(),
		CertExpiryTime: &expiry,
	})

	if state.TLSCertExpiry != float64(expiry.Unix()) {
		t.Fatalf("expected TLSCertExpiry %f, got %f", float64(expiry.Unix()), state.TLSCertExpiry)
	}
}

func TestResultToProbeStateNoTLSCertExpiry(t *testing.T) {
	state := resultToProbeState(Result{Success: true, Completed: time.Now()})

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

// newFakePool creates a WorkerPool backed by an in-memory metrics store.
func newFakePool(t *testing.T) *WorkerPool {
	t.Helper()
	store, err := internalmetrics.NewStore()
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	return NewWorkerPool(logr.Discard(), 1, store)
}

type fixedExecutor struct{ result Result }

func (f fixedExecutor) Execute(_ context.Context, _ *syntheticsv1alpha1.HTTPProbe) Result {
	return f.result
}

func TestRunProbeConsecutiveFailures(t *testing.T) {
	pool := newFakePool(t)
	probe := &syntheticsv1alpha1.HTTPProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec:       syntheticsv1alpha1.HTTPProbeSpec{Timeout: metav1.Duration{Duration: time.Second}},
	}

	job := newHTTPProbeJob(probe, fixedExecutor{result: Result{Success: false, Completed: time.Now()}})
	pool.runProbe(context.Background(), job)
	pool.runProbe(context.Background(), job)

	state, ok := pool.metrics.Snapshot(types.NamespacedName{Namespace: "default", Name: "test"})
	if !ok {
		t.Fatal("expected state in metrics store")
	}
	if state.ConsecutiveFailures != 2 {
		t.Fatalf("expected 2 consecutive failures, got %f", state.ConsecutiveFailures)
	}
}

func TestRunProbeConsecutiveFailuresResetOnSuccess(t *testing.T) {
	pool := newFakePool(t)
	probe := &syntheticsv1alpha1.HTTPProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec:       syntheticsv1alpha1.HTTPProbeSpec{Timeout: metav1.Duration{Duration: time.Second}},
	}

	failJob := newHTTPProbeJob(probe, fixedExecutor{result: Result{Success: false, Completed: time.Now()}})
	pool.runProbe(context.Background(), failJob)
	pool.runProbe(context.Background(), failJob)

	successJob := newHTTPProbeJob(probe, fixedExecutor{result: Result{Success: true, Completed: time.Now()}})
	pool.runProbe(context.Background(), successJob)

	state, ok := pool.metrics.Snapshot(types.NamespacedName{Namespace: "default", Name: "test"})
	if !ok {
		t.Fatal("expected state in metrics store")
	}
	if state.ConsecutiveFailures != 0 {
		t.Fatalf("expected 0 consecutive failures after success, got %f", state.ConsecutiveFailures)
	}
}

func TestRunProbeConfigErrorDoesNotIncrementFailures(t *testing.T) {
	pool := newFakePool(t)
	probe := &syntheticsv1alpha1.HTTPProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec:       syntheticsv1alpha1.HTTPProbeSpec{Timeout: metav1.Duration{Duration: time.Second}},
	}
	key := types.NamespacedName{Namespace: "default", Name: "test"}

	// Seed two prior failures
	failJob := newHTTPProbeJob(probe, fixedExecutor{result: Result{Success: false, Completed: time.Now()}})
	pool.runProbe(context.Background(), failJob)
	pool.runProbe(context.Background(), failJob)

	// Config error should leave ConsecutiveFailures unchanged
	configErrJob := newHTTPProbeJob(probe, fixedExecutor{result: Result{ConfigError: true, Completed: time.Now()}})
	pool.runProbe(context.Background(), configErrJob)

	state, ok := pool.metrics.Snapshot(key)
	if !ok {
		t.Fatal("expected state in metrics store")
	}
	if state.ConsecutiveFailures != 2 {
		t.Fatalf("expected ConsecutiveFailures unchanged at 2, got %f", state.ConsecutiveFailures)
	}
	if state.ConfigError != 1 {
		t.Fatalf("expected ConfigError=1, got %f", state.ConfigError)
	}
}
