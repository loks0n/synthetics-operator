package controllers

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
	"github.com/loks0n/synthetics-operator/internal/results"
)

func TestTCPProbeReconcileRegistersAndPublishes(t *testing.T) {
	probe := &syntheticsv1alpha1.TCPProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "mysql", Namespace: "default"},
		Spec: syntheticsv1alpha1.TCPProbeSpec{
			Interval: metav1.Duration{Duration: 30 * time.Second},
			Timeout:  metav1.Duration{Duration: 5 * time.Second},
			Target:   syntheticsv1alpha1.TCPTarget{Host: "mysql.default.svc", Port: 3306},
		},
	}
	cl := fakeclient.NewClientBuilder().WithScheme(unitScheme()).WithStatusSubresource(probe).WithObjects(probe).Build()
	sched := newFakeScheduler()
	pub := &fakePublisher{}
	r := &TCPProbeReconciler{Client: cl, Scheme: unitScheme(), Scheduler: sched, Publisher: pub, Clock: time.Now}
	key := types.NamespacedName{Namespace: probe.Namespace, Name: probe.Name}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if !sched.isActive(results.KindTCPProbe, key) {
		t.Fatal("expected TCPProbe to be scheduled")
	}
	spec := pub.latestSpec()
	if spec == nil || spec.Kind != results.KindTCPProbe || spec.TCPProbe == nil {
		t.Fatalf("unexpected published spec: %+v", spec)
	}
	if spec.TCPProbe.Host != probe.Spec.Target.Host || spec.TCPProbe.Port != probe.Spec.Target.Port {
		t.Fatalf("target not preserved: %+v", spec.TCPProbe)
	}
}

func TestTCPProbeReconcileTombstoneIsKindScoped(t *testing.T) {
	cl := fakeclient.NewClientBuilder().WithScheme(unitScheme()).Build()
	sched := newFakeScheduler()
	pub := &fakePublisher{}
	r := &TCPProbeReconciler{Client: cl, Scheme: unitScheme(), Scheduler: sched, Publisher: pub, Clock: time.Now}
	key := types.NamespacedName{Namespace: "default", Name: "shared"}
	sched.Register(results.SpecUpdate{Kind: results.KindTCPProbe, Namespace: key.Namespace, Name: key.Name, IntervalMs: 30000})
	sched.Register(results.SpecUpdate{Kind: results.KindHTTPProbe, Namespace: key.Namespace, Name: key.Name, IntervalMs: 30000})

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if sched.isActive(results.KindTCPProbe, key) {
		t.Fatal("deleted TCPProbe remained scheduled")
	}
	if !sched.isActive(results.KindHTTPProbe, key) {
		t.Fatal("TCPProbe deletion removed same-named HTTPProbe")
	}
	if spec := pub.latestSpec(); spec == nil || !spec.Deleted || spec.Kind != results.KindTCPProbe {
		t.Fatalf("unexpected tombstone: %+v", spec)
	}
}
