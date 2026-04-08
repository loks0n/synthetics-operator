package controllers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
	internalmetrics "github.com/loks0n/synthetics-operator/internal/metrics"
	internalprobes "github.com/loks0n/synthetics-operator/internal/probes"
)

func setupEnvtest(t *testing.T) (client.Client, func()) {
	t.Helper()
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

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		_ = testEnv.Stop()
		t.Fatalf("create client: %v", err)
	}
	if err := k8sClient.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}); err != nil && !apierrors.IsAlreadyExists(err) {
		_ = testEnv.Stop()
		t.Fatalf("create namespace: %v", err)
	}

	return k8sClient, func() { _ = testEnv.Stop() }
}

func newReconciler(t *testing.T, k8sClient client.Client) *HTTPProbeReconciler {
	t.Helper()
	scheme := k8sClient.Scheme()
	store, err := internalmetrics.NewStore()
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	scheduler := internalprobes.NewScheduler(logr.Discard(), internalprobes.NewWorkerPool(logr.Discard(), 1))
	ctx := t.Context()
	go func() { _ = scheduler.Start(ctx) }()

	return &HTTPProbeReconciler{
		Client:       k8sClient,
		Scheme:       scheme,
		Scheduler:    scheduler,
		HTTPExecutor: internalprobes.HTTPExecutor{},
		Metrics:      store,
		Clock:        time.Now,
	}
}

func TestHTTPProbeReconcileRegistersProbe(t *testing.T) {
	k8sClient, stop := setupEnvtest(t)
	defer stop()
	reconciler := newReconciler(t, k8sClient)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	probe := &syntheticsv1alpha1.HTTPProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "api-health", Namespace: "default"},
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Interval: metav1.Duration{Duration: 30 * time.Second},
			Timeout:  metav1.Duration{Duration: 10 * time.Second},
			Request:  syntheticsv1alpha1.HTTPRequestSpec{URL: server.URL, Method: "GET"},
		},
	}
	if err := k8sClient.Create(context.Background(), probe); err != nil {
		t.Fatalf("create probe: %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(probe)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var updated syntheticsv1alpha1.HTTPProbe
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(probe), &updated); err != nil {
		t.Fatalf("get updated probe: %v", err)
	}
	if updated.Status.ObservedGeneration != probe.Generation {
		t.Fatalf("expected ObservedGeneration=%d, got %d", probe.Generation, updated.Status.ObservedGeneration)
	}
	suspended := apimeta.FindStatusCondition(updated.Status.Conditions, syntheticsv1alpha1.ConditionSuspended)
	if suspended == nil {
		t.Fatal("expected Suspended condition")
	}
	if suspended.Status != metav1.ConditionFalse {
		t.Fatalf("expected Suspended=False, got %s", suspended.Status)
	}
}

func TestHTTPProbeReconcileInitializingCondition(t *testing.T) {
	k8sClient, stop := setupEnvtest(t)
	defer stop()
	reconciler := newReconciler(t, k8sClient)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	probe := &syntheticsv1alpha1.HTTPProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "init-probe", Namespace: "default"},
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Interval: metav1.Duration{Duration: 30 * time.Second},
			Timeout:  metav1.Duration{Duration: 10 * time.Second},
			Request:  syntheticsv1alpha1.HTTPRequestSpec{URL: server.URL, Method: "GET"},
		},
	}
	if err := k8sClient.Create(context.Background(), probe); err != nil {
		t.Fatalf("create probe: %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(probe)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var updated syntheticsv1alpha1.HTTPProbe
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(probe), &updated); err != nil {
		t.Fatalf("get probe: %v", err)
	}

	ready := apimeta.FindStatusCondition(updated.Status.Conditions, syntheticsv1alpha1.ConditionReady)
	if ready == nil {
		t.Fatal("expected Ready condition")
	}
	if ready.Status != metav1.ConditionUnknown {
		t.Fatalf("expected Ready=Unknown, got %s", ready.Status)
	}
	if ready.Reason != syntheticsv1alpha1.ReasonInitializing {
		t.Fatalf("expected reason Initializing, got %s", ready.Reason)
	}
}

func TestHTTPProbeReconcileSuspend(t *testing.T) {
	k8sClient, stop := setupEnvtest(t)
	defer stop()
	reconciler := newReconciler(t, k8sClient)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	probe := &syntheticsv1alpha1.HTTPProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "suspended-probe", Namespace: "default"},
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Interval: metav1.Duration{Duration: 30 * time.Second},
			Timeout:  metav1.Duration{Duration: 10 * time.Second},
			Suspend:  true,
			Request:  syntheticsv1alpha1.HTTPRequestSpec{URL: server.URL, Method: "GET"},
		},
	}
	if err := k8sClient.Create(context.Background(), probe); err != nil {
		t.Fatalf("create probe: %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(probe)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var updated syntheticsv1alpha1.HTTPProbe
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(probe), &updated); err != nil {
		t.Fatalf("get probe: %v", err)
	}

	suspended := apimeta.FindStatusCondition(updated.Status.Conditions, syntheticsv1alpha1.ConditionSuspended)
	if suspended == nil {
		t.Fatal("expected Suspended condition")
	}
	if suspended.Status != metav1.ConditionTrue {
		t.Fatalf("expected Suspended=True, got %s", suspended.Status)
	}
	if suspended.Reason != syntheticsv1alpha1.ReasonSuspended {
		t.Fatalf("expected reason Suspended, got %s", suspended.Reason)
	}

	// probe should not be registered in the scheduler
	key := types.NamespacedName{Namespace: "default", Name: "suspended-probe"}
	if _, ok := reconciler.Metrics.Snapshot(key); ok {
		t.Fatal("suspended probe should not have metrics")
	}
}

func TestHTTPProbeReconcileNotFound(t *testing.T) {
	k8sClient, stop := setupEnvtest(t)
	defer stop()
	reconciler := newReconciler(t, k8sClient)

	// Reconcile a probe that doesn't exist — should not error
	key := types.NamespacedName{Namespace: "default", Name: "gone"}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile of missing probe should not error, got %v", err)
	}
}
