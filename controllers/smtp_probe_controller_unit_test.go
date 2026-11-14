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

func TestSMTPProbeReconcilePublishesResolvedTLSMode(t *testing.T) {
	submission := &syntheticsv1alpha1.SMTPProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "submission", Namespace: "mail"},
		Spec: syntheticsv1alpha1.SMTPProbeSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Timeout:  metav1.Duration{Duration: 5 * time.Second},
			// Mode left empty, as on an object admitted before defaulting existed.
			Target: syntheticsv1alpha1.TCPTarget{Host: "smtp.example.com", Port: 587},
			TLS:    syntheticsv1alpha1.SMTPTLSConfig{TLSConfig: syntheticsv1alpha1.TLSConfig{InsecureSkipVerify: true}},
		},
	}
	cl := fakeclient.NewClientBuilder().WithScheme(unitScheme()).WithStatusSubresource(submission).WithObjects(submission).Build()
	scheduler := newFakeScheduler()
	publisher := &fakePublisher{}
	reconciler := &SMTPProbeReconciler{Client: cl, Scheme: unitScheme(), Scheduler: scheduler, Publisher: publisher, Clock: time.Now}
	key := types.NamespacedName{Namespace: "mail", Name: "submission"}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if !scheduler.isActive(results.KindSMTPProbe, key) {
		t.Fatal("SMTPProbe was not scheduled")
	}
	published := publisher.latestSpec()
	if published == nil || published.SMTPProbe == nil {
		t.Fatalf("no SMTPProbe payload published: %+v", published)
	}
	payload := published.SMTPProbe
	if payload.TLSMode != "starttls" || payload.TLS == nil || !payload.TLS.InsecureSkipVerify || payload.TimeoutMs != 5000 {
		t.Fatalf("payload does not carry the resolved TLS policy: %+v", payload)
	}

	if err := cl.Delete(context.Background(), submission); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if scheduler.isActive(results.KindSMTPProbe, key) {
		t.Fatal("deleted SMTPProbe stayed scheduled")
	}
	if tombstone := publisher.latestSpec(); tombstone == nil || !tombstone.Deleted || tombstone.Kind != results.KindSMTPProbe {
		t.Fatalf("unexpected tombstone: %+v", tombstone)
	}
}
