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

func TestHTTPExecutorSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("expected GET, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	result := HTTPExecutor{}.Execute(context.Background(), &syntheticsv1alpha1.HttpProbe{
		Spec: syntheticsv1alpha1.HttpProbeSpec{
			Request: syntheticsv1alpha1.HTTPRequestSpec{
				URL:    server.URL,
				Method: http.MethodGet,
			},
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
	result := HTTPExecutor{}.Execute(context.Background(), &syntheticsv1alpha1.HttpProbe{
		Spec: syntheticsv1alpha1.HttpProbeSpec{
			Request: syntheticsv1alpha1.HTTPRequestSpec{
				URL:    "://bad-url",
				Method: http.MethodGet,
			},
			Assertions: syntheticsv1alpha1.HTTPAssertions{Status: http.StatusOK},
		},
	})

	if !result.ConfigError {
		t.Fatalf("expected config error, got %+v", result)
	}
}

func TestHTTPExecutorStatusMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	result := HTTPExecutor{}.Execute(context.Background(), &syntheticsv1alpha1.HttpProbe{
		Spec: syntheticsv1alpha1.HttpProbeSpec{
			Request: syntheticsv1alpha1.HTTPRequestSpec{
				URL:    server.URL,
				Method: http.MethodGet,
			},
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	result := HTTPExecutor{}.Execute(ctx, &syntheticsv1alpha1.HttpProbe{
		Spec: syntheticsv1alpha1.HttpProbeSpec{
			Request: syntheticsv1alpha1.HTTPRequestSpec{
				URL:    server.URL,
				Method: http.MethodGet,
			},
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
	server.Close() // close before request

	result := HTTPExecutor{}.Execute(context.Background(), &syntheticsv1alpha1.HttpProbe{
		Spec: syntheticsv1alpha1.HttpProbeSpec{
			Request: syntheticsv1alpha1.HTTPRequestSpec{
				URL:    url,
				Method: http.MethodGet,
			},
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

	HTTPExecutor{}.Execute(context.Background(), &syntheticsv1alpha1.HttpProbe{
		Spec: syntheticsv1alpha1.HttpProbeSpec{
			Request: syntheticsv1alpha1.HTTPRequestSpec{
				URL:    server.URL,
				Method: http.MethodGet,
				Headers: map[string]string{
					"X-Test-Header": "hello",
				},
			},
			Assertions: syntheticsv1alpha1.HTTPAssertions{Status: http.StatusOK},
			Timeout:    metav1.Duration{Duration: time.Second},
		},
	})

	if received != "hello" {
		t.Fatalf("expected header value 'hello', got %q", received)
	}
}

// newFakePool creates a WorkerPool with a fake k8s client preloaded with the given probe.
func newFakePool(t *testing.T, executor Executor, probe *syntheticsv1alpha1.HttpProbe) (*WorkerPool, *syntheticsv1alpha1.HttpProbe) {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(syntheticsv1alpha1.AddToScheme(scheme))

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&syntheticsv1alpha1.HttpProbe{}).
		WithObjects(probe.DeepCopy()).
		Build()

	store, err := internalmetrics.NewStore()
	if err != nil {
		t.Fatalf("create store: %v", err)
	}

	pool := NewWorkerPool(logr.Discard(), 1, executor, store, k8sClient)

	updated := probe.DeepCopy()
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Namespace: probe.Namespace, Name: probe.Name}, updated); err != nil {
		t.Fatalf("get probe: %v", err)
	}
	return pool, updated
}

type fixedExecutor struct{ result Result }

func (f fixedExecutor) Execute(_ context.Context, _ *syntheticsv1alpha1.HttpProbe) Result {
	return f.result
}

func TestRunProbeConsecutiveFailures(t *testing.T) {
	probe := &syntheticsv1alpha1.HttpProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: syntheticsv1alpha1.HttpProbeSpec{
			Timeout: metav1.Duration{Duration: time.Second},
		},
	}
	pool, p := newFakePool(t, fixedExecutor{result: Result{Success: false, Completed: time.Now()}}, probe)

	pool.runProbe(context.Background(), p)
	pool.runProbe(context.Background(), p)

	var updated syntheticsv1alpha1.HttpProbe
	if err := pool.client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test"}, &updated); err != nil {
		t.Fatalf("get probe: %v", err)
	}
	if updated.Status.ConsecutiveFailures != 2 {
		t.Fatalf("expected 2 consecutive failures, got %d", updated.Status.ConsecutiveFailures)
	}
}

func TestRunProbeConsecutiveFailuresResetOnSuccess(t *testing.T) {
	probe := &syntheticsv1alpha1.HttpProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: syntheticsv1alpha1.HttpProbeSpec{
			Timeout: metav1.Duration{Duration: time.Second},
		},
	}
	pool, p := newFakePool(t, fixedExecutor{result: Result{Success: false, Completed: time.Now()}}, probe)

	pool.runProbe(context.Background(), p)
	pool.runProbe(context.Background(), p)

	pool.executor = fixedExecutor{result: Result{Success: true, Completed: time.Now()}}
	pool.runProbe(context.Background(), p)

	var updated syntheticsv1alpha1.HttpProbe
	if err := pool.client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test"}, &updated); err != nil {
		t.Fatalf("get probe: %v", err)
	}
	if updated.Status.ConsecutiveFailures != 0 {
		t.Fatalf("expected 0 consecutive failures after success, got %d", updated.Status.ConsecutiveFailures)
	}
}

func TestRunProbeConfigErrorCondition(t *testing.T) {
	probe := &syntheticsv1alpha1.HttpProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: syntheticsv1alpha1.HttpProbeSpec{
			Timeout: metav1.Duration{Duration: time.Second},
		},
	}
	pool, p := newFakePool(t, fixedExecutor{result: Result{ConfigError: true, Message: "invalid url", Completed: time.Now()}}, probe)
	pool.runProbe(context.Background(), p)

	var updated syntheticsv1alpha1.HttpProbe
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
