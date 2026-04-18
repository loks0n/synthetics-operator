package controllers

import (
	"context"
	"sync"
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
	"github.com/loks0n/synthetics-operator/internal/results"
)

// fakePublisher captures the SpecUpdates the reconciler emits.
type fakePublisher struct {
	mu    sync.Mutex
	specs []results.SpecUpdate
	jobs  []results.ProbeJob
	rs    []results.ProbeResult
}

func (f *fakePublisher) PublishSpec(_ context.Context, msg results.SpecUpdate) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.specs = append(f.specs, msg)
	return nil
}
func (f *fakePublisher) PublishProbeJob(_ context.Context, msg results.ProbeJob) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs = append(f.jobs, msg)
	return nil
}
func (f *fakePublisher) PublishProbeResult(_ context.Context, msg results.ProbeResult) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rs = append(f.rs, msg)
	return nil
}
func (f *fakePublisher) latestSpec() *results.SpecUpdate {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.specs) == 0 {
		return nil
	}
	s := f.specs[len(f.specs)-1]
	return &s
}

func unitScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(syntheticsv1alpha1.AddToScheme(s))
	return s
}

func newUnitReconciler(k8sClient client.Client, sched *fakeScheduler, pub *fakePublisher) *HTTPProbeReconciler {
	return &HTTPProbeReconciler{
		Client:    k8sClient,
		Scheme:    unitScheme(),
		Scheduler: sched,
		Publisher: pub,
		Clock:     time.Now,
	}
}

func TestHTTPProbeReconcile_RegistersAndPublishes(t *testing.T) {
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
	pub := &fakePublisher{}
	r := newUnitReconciler(k8sClient, sched, pub)

	key := types.NamespacedName{Namespace: "default", Name: "my-probe"}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if !sched.isActive(key) {
		t.Fatal("expected probe to be registered in scheduler")
	}
	spec := pub.latestSpec()
	if spec == nil || spec.Kind != results.KindHTTPProbe || spec.Deleted {
		t.Fatalf("expected an HTTPProbe spec publish, got %+v", spec)
	}
	if spec.HTTPProbe == nil || spec.HTTPProbe.URL != "http://example.com" {
		t.Fatalf("expected HTTPProbe payload URL preserved, got %+v", spec.HTTPProbe)
	}
}

func TestHTTPProbeReconcile_UnregistersOnSuspend(t *testing.T) {
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
	pub := &fakePublisher{}
	r := newUnitReconciler(k8sClient, sched, pub)

	key := types.NamespacedName{Namespace: "default", Name: "suspended"}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if sched.isActive(key) {
		t.Fatal("suspended probe should not be registered")
	}
	if len(sched.removed) == 0 {
		t.Fatal("expected Unregister for suspended probe")
	}
	// spec is still published so downstream caches learn about the suspend.
	if pub.latestSpec() == nil || !pub.latestSpec().Suspend {
		t.Fatal("expected suspended spec publish")
	}
}

func TestHTTPProbeReconcile_TombstonesOnDelete(t *testing.T) {
	k8sClient := fakeclient.NewClientBuilder().WithScheme(unitScheme()).Build()

	sched := newFakeScheduler()
	pub := &fakePublisher{}
	r := newUnitReconciler(k8sClient, sched, pub)

	key := types.NamespacedName{Namespace: "default", Name: "gone"}
	sched.Register(key, results.KindHTTPProbe, 30*time.Second)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile of missing probe should not error: %v", err)
	}

	if sched.isActive(key) {
		t.Fatal("deleted probe should be unregistered")
	}
	spec := pub.latestSpec()
	if spec == nil || !spec.Deleted {
		t.Fatalf("expected tombstone spec publish, got %+v", spec)
	}
}
