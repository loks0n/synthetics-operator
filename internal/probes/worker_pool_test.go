package probes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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

// --- httpProbeApplier tests: pure tests of the state-transition logic ---
// These require no k8s fake client and no HTTP server.

func TestHTTPProbeApplierSuccessResetsCounter(t *testing.T) {
	applier := &httpProbeApplier{result: Result{
		Success:   true,
		Completed: time.Now(),
		Message:   "received status 200",
	}}

	current := &syntheticsv1alpha1.HTTPProbe{}
	current.Status.ConsecutiveFailures = 3

	state := applier.apply(current)

	if current.Status.ConsecutiveFailures != 0 {
		t.Fatalf("expected counter reset to 0, got %d", current.Status.ConsecutiveFailures)
	}
	if state.ConsecutiveFailures != 0 {
		t.Fatalf("expected metrics ConsecutiveFailures=0, got %f", state.ConsecutiveFailures)
	}
	if state.Success != 1 {
		t.Fatalf("expected Success=1, got %f", state.Success)
	}
	if current.Status.LastSuccessTime == nil {
		t.Fatal("expected LastSuccessTime to be set on success")
	}
	if current.Status.LastFailureTime != nil {
		t.Fatal("expected LastFailureTime to be nil on success")
	}
}

func TestHTTPProbeApplierFailureIncrementsCounter(t *testing.T) {
	applier := &httpProbeApplier{result: Result{
		Success:   false,
		Completed: time.Now(),
		Message:   "status 503 != 200",
	}}

	current := &syntheticsv1alpha1.HTTPProbe{}
	current.Status.ConsecutiveFailures = 2

	state := applier.apply(current)

	if current.Status.ConsecutiveFailures != 3 {
		t.Fatalf("expected 3, got %d", current.Status.ConsecutiveFailures)
	}
	if state.ConsecutiveFailures != 3 {
		t.Fatalf("expected metrics ConsecutiveFailures=3, got %f", state.ConsecutiveFailures)
	}
	if state.Success != 0 {
		t.Fatal("expected Success=0 on failure")
	}
	if current.Status.LastFailureTime == nil {
		t.Fatal("expected LastFailureTime to be set on failure")
	}
	var cond *metav1.Condition
	for i := range current.Status.Conditions {
		if current.Status.Conditions[i].Type == syntheticsv1alpha1.ConditionReady {
			cond = &current.Status.Conditions[i]
		}
	}
	if cond == nil || cond.Reason != syntheticsv1alpha1.ReasonProbeFailed {
		t.Fatalf("expected ProbeFailed condition, got %v", cond)
	}
}

func TestHTTPProbeApplierConfigErrorDoesNotIncrementCounter(t *testing.T) {
	applier := &httpProbeApplier{result: Result{
		ConfigError: true,
		Completed:   time.Now(),
		Message:     "invalid url",
	}}

	current := &syntheticsv1alpha1.HTTPProbe{}
	current.Status.ConsecutiveFailures = 5

	state := applier.apply(current)

	if current.Status.ConsecutiveFailures != 5 {
		t.Fatalf("expected counter unchanged at 5, got %d", current.Status.ConsecutiveFailures)
	}
	if state.ConsecutiveFailures != 5 {
		t.Fatalf("expected metrics ConsecutiveFailures=5, got %f", state.ConsecutiveFailures)
	}
	if state.ConfigError != 1 {
		t.Fatal("expected ConfigError=1 in metrics")
	}
	var cond *metav1.Condition
	for i := range current.Status.Conditions {
		if current.Status.Conditions[i].Type == syntheticsv1alpha1.ConditionReady {
			cond = &current.Status.Conditions[i]
		}
	}
	if cond == nil || cond.Reason != syntheticsv1alpha1.ReasonConfigError {
		t.Fatalf("expected ConfigError condition, got %v", cond)
	}
}

func TestHTTPProbeApplierAssertionResultsConverted(t *testing.T) {
	applier := &httpProbeApplier{result: Result{
		Success:   false,
		Completed: time.Now(),
		AssertionResults: []AssertionResult{
			{Type: "status", Name: "status", Passed: false},
			{Type: "latency", Name: "latency", Passed: true},
		},
	}}

	state := applier.apply(&syntheticsv1alpha1.HTTPProbe{})

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

func TestHTTPProbeApplierMetricsConsistentWithStatus(t *testing.T) {
	// Verify that the ConsecutiveFailures value in the returned ProbeState
	// always matches what was written to current.Status.
	applier := &httpProbeApplier{result: Result{Success: false, Completed: time.Now()}}

	current := &syntheticsv1alpha1.HTTPProbe{}
	current.Status.ConsecutiveFailures = 7

	state := applier.apply(current)

	if state.ConsecutiveFailures != float64(current.Status.ConsecutiveFailures) {
		t.Fatalf("metrics ConsecutiveFailures (%f) out of sync with status (%d)",
			state.ConsecutiveFailures, current.Status.ConsecutiveFailures)
	}
}

// --- WorkerPool infrastructure tests ---

func TestNewWorkerPoolMinConcurrency(t *testing.T) {
	store, err := internalmetrics.NewStore()
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	pool := NewWorkerPool(logr.Discard(), 0, store, nil)
	if cap(pool.queue) != 16 {
		t.Fatalf("expected queue cap 16 for min concurrency, got %d", cap(pool.queue))
	}
}

func TestRunProbeProbeDeletedMidRun(t *testing.T) {
	probe := &syntheticsv1alpha1.HTTPProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "gone", Namespace: "default"},
		Spec:       syntheticsv1alpha1.HTTPProbeSpec{Timeout: metav1.Duration{Duration: time.Second}},
	}
	scheme := runtime.NewScheme()
	utilruntime.Must(syntheticsv1alpha1.AddToScheme(scheme))
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	store, err := internalmetrics.NewStore()
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	pool := NewWorkerPool(logr.Discard(), 1, store, k8sClient)
	job := newHTTPProbeJob(probe, fixedExecutor{result: Result{Success: true, Completed: time.Now()}})
	pool.runProbe(context.Background(), job)
}

// --- WorkerPool integration tests (use fake k8s client) ---

// newFakePool creates a WorkerPool backed by a fake k8s client preloaded with probe.
func newFakePool(t *testing.T, probe *syntheticsv1alpha1.HTTPProbe) (*WorkerPool, *syntheticsv1alpha1.HTTPProbe) {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(syntheticsv1alpha1.AddToScheme(scheme))

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&syntheticsv1alpha1.HTTPProbe{}).
		WithObjects(probe.DeepCopy()).
		Build()

	store, err := internalmetrics.NewStore()
	if err != nil {
		t.Fatalf("create store: %v", err)
	}

	pool := NewWorkerPool(logr.Discard(), 1, store, k8sClient)

	updated := probe.DeepCopy()
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Namespace: probe.Namespace, Name: probe.Name}, updated); err != nil {
		t.Fatalf("get probe: %v", err)
	}
	return pool, updated
}

type fixedExecutor struct{ result Result }

func (f fixedExecutor) Execute(_ context.Context, _ *syntheticsv1alpha1.HTTPProbe) Result {
	return f.result
}

func TestRunProbeConsecutiveFailures(t *testing.T) {
	probe := &syntheticsv1alpha1.HTTPProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec:       syntheticsv1alpha1.HTTPProbeSpec{Timeout: metav1.Duration{Duration: time.Second}},
	}
	pool, p := newFakePool(t, probe)

	job := newHTTPProbeJob(p, fixedExecutor{result: Result{Success: false, Completed: time.Now()}})
	pool.runProbe(context.Background(), job)
	pool.runProbe(context.Background(), job)

	var updated syntheticsv1alpha1.HTTPProbe
	if err := pool.client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test"}, &updated); err != nil {
		t.Fatalf("get probe: %v", err)
	}
	if updated.Status.ConsecutiveFailures != 2 {
		t.Fatalf("expected 2 consecutive failures, got %d", updated.Status.ConsecutiveFailures)
	}
}

func TestRunProbeConsecutiveFailuresResetOnSuccess(t *testing.T) {
	probe := &syntheticsv1alpha1.HTTPProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec:       syntheticsv1alpha1.HTTPProbeSpec{Timeout: metav1.Duration{Duration: time.Second}},
	}
	pool, p := newFakePool(t, probe)

	failJob := newHTTPProbeJob(p, fixedExecutor{result: Result{Success: false, Completed: time.Now()}})
	pool.runProbe(context.Background(), failJob)
	pool.runProbe(context.Background(), failJob)

	successJob := newHTTPProbeJob(p, fixedExecutor{result: Result{Success: true, Completed: time.Now()}})
	pool.runProbe(context.Background(), successJob)

	var updated syntheticsv1alpha1.HTTPProbe
	if err := pool.client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test"}, &updated); err != nil {
		t.Fatalf("get probe: %v", err)
	}
	if updated.Status.ConsecutiveFailures != 0 {
		t.Fatalf("expected 0 consecutive failures after success, got %d", updated.Status.ConsecutiveFailures)
	}
}

func TestRunProbeConfigErrorCondition(t *testing.T) {
	probe := &syntheticsv1alpha1.HTTPProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec:       syntheticsv1alpha1.HTTPProbeSpec{Timeout: metav1.Duration{Duration: time.Second}},
	}
	pool, p := newFakePool(t, probe)

	job := newHTTPProbeJob(p, fixedExecutor{result: Result{ConfigError: true, Message: "invalid url", Completed: time.Now()}})
	pool.runProbe(context.Background(), job)

	var updated syntheticsv1alpha1.HTTPProbe
	if err := pool.client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test"}, &updated); err != nil {
		t.Fatalf("get probe: %v", err)
	}

	var readyCondition *metav1.Condition
	for i := range updated.Status.Conditions {
		if updated.Status.Conditions[i].Type == syntheticsv1alpha1.ConditionReady {
			readyCondition = &updated.Status.Conditions[i]
			break
		}
	}
	if readyCondition == nil {
		t.Fatal("expected Ready condition to be set")
	}
	if readyCondition.Reason != syntheticsv1alpha1.ReasonConfigError {
		t.Fatalf("expected reason ConfigError, got %s", readyCondition.Reason)
	}
}
