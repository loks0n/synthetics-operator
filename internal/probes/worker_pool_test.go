package probes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
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
