package controllers

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
	internalmetrics "github.com/loks0n/synthetics-operator/internal/metrics"
	internalprobes "github.com/loks0n/synthetics-operator/internal/probes"
)

func TestHttpProbeReconcileRegistersProbe(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set")
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(syntheticsv1alpha1.AddToScheme(scheme))

	testEnv := &envtest.Environment{
		CRDDirectoryPaths: []string{filepath.Join("..", "config", "crd", "bases")},
	}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	defer func() {
		_ = testEnv.Stop()
	}()

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	if err := k8sClient.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace: %v", err)
	}

	store, err := internalmetrics.NewStore()
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	scheduler := internalprobes.NewScheduler(logr.Discard(), internalprobes.NewWorkerPool(logr.Discard(), 1, internalprobes.HTTPExecutor{}, store, k8sClient))
	ctx := t.Context()
	go func() { _ = scheduler.Start(ctx) }()

	reconciler := &HttpProbeReconciler{
		Client:    k8sClient,
		Scheme:    scheme,
		Scheduler: scheduler,
		Metrics:   store,
		Clock:     time.Now,
	}

	probe := &syntheticsv1alpha1.HttpProbe{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api-health",
			Namespace: "default",
		},
		Spec: syntheticsv1alpha1.HttpProbeSpec{
			Interval: metav1.Duration{Duration: 30 * time.Second},
			Timeout:  metav1.Duration{Duration: 10 * time.Second},
			Request: syntheticsv1alpha1.HTTPRequestSpec{
				URL:    "https://example.com",
				Method: "GET",
			},
			Assertions: syntheticsv1alpha1.HTTPAssertions{Status: 200},
		},
	}
	if err := k8sClient.Create(context.Background(), probe); err != nil {
		t.Fatalf("create probe: %v", err)
	}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(probe)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var updated syntheticsv1alpha1.HttpProbe
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(probe), &updated); err != nil {
		t.Fatalf("get updated probe: %v", err)
	}
	if updated.Status.Summary == nil || updated.Status.Summary.Message == "" {
		t.Fatalf("expected summary to be populated, got %#v", updated.Status.Summary)
	}
}
