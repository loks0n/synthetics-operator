package controllers

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
	internalmetrics "github.com/loks0n/synthetics-operator/internal/metrics"
	internalprobes "github.com/loks0n/synthetics-operator/internal/probes"
)

func unitScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(syntheticsv1alpha1.AddToScheme(s))
	return s
}

func newUnitReconciler(k8sClient client.Client, sched *fakeScheduler) *HTTPProbeReconciler {
	store, err := internalmetrics.NewStore()
	if err != nil {
		panic(err)
	}
	return &HTTPProbeReconciler{
		Client:       k8sClient,
		Scheme:       unitScheme(),
		Scheduler:    sched,
		HTTPExecutor: internalprobes.HTTPExecutor{},
		Metrics:      store,
		Clock:        time.Now,
	}
}

func TestHTTPProbeReconcileUnit_RegistersProbe(t *testing.T) {
	probe := &syntheticsv1alpha1.HTTPProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "my-probe", Namespace: "default"},
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Interval: metav1.Duration{Duration: 30 * time.Second},
			Timeout:  metav1.Duration{Duration: 5 * time.Second},
			Request:  syntheticsv1alpha1.HTTPRequestSpec{URL: "http://example.com", Method: "GET"},
		},
	}

	k8sClient := fakeclient.NewClientBuilder().
		WithScheme(unitScheme()).
		WithStatusSubresource(probe).
		WithObjects(probe).
		Build()

	sched := newFakeScheduler()
	r := newUnitReconciler(k8sClient, sched)

	key := types.NamespacedName{Namespace: "default", Name: "my-probe"}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if !sched.isActive(key) {
		t.Fatal("expected probe to be registered in scheduler")
	}
}

func TestHTTPProbeReconcileUnit_UnregistersOnSuspend(t *testing.T) {
	probe := &syntheticsv1alpha1.HTTPProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "suspended", Namespace: "default"},
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Interval: metav1.Duration{Duration: 30 * time.Second},
			Timeout:  metav1.Duration{Duration: 5 * time.Second},
			Suspend:  true,
			Request:  syntheticsv1alpha1.HTTPRequestSpec{URL: "http://example.com", Method: "GET"},
		},
	}

	k8sClient := fakeclient.NewClientBuilder().
		WithScheme(unitScheme()).
		WithStatusSubresource(probe).
		WithObjects(probe).
		Build()

	sched := newFakeScheduler()
	r := newUnitReconciler(k8sClient, sched)

	key := types.NamespacedName{Namespace: "default", Name: "suspended"}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if sched.isActive(key) {
		t.Fatal("suspended probe should not be registered in scheduler")
	}
	if len(sched.removed) == 0 {
		t.Fatal("expected Unregister to be called for suspended probe")
	}
}

func TestHTTPProbeReconcileUnit_UnregistersOnDelete(t *testing.T) {
	k8sClient := fakeclient.NewClientBuilder().
		WithScheme(unitScheme()).
		Build()

	sched := newFakeScheduler()
	r := newUnitReconciler(k8sClient, sched)

	// Pre-populate scheduler as if this probe was previously running.
	key := types.NamespacedName{Namespace: "default", Name: "gone"}
	sched.Register(internalprobes.Job{Key: key, Interval: 30 * time.Second, Timeout: time.Second, Run: func(context.Context) {}})

	// Reconcile a probe that no longer exists (404).
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("reconcile of missing probe should not error, got %v", err)
	}

	if sched.isActive(key) {
		t.Fatal("deleted probe should be unregistered from scheduler")
	}
}
