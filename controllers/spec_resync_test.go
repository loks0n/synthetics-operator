package controllers

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
	"github.com/loks0n/synthetics-operator/internal/results"
)

func TestSpecResyncRepublishesAllLiveSpecs(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := syntheticsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	probe := &syntheticsv1alpha1.HTTPProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Request: syntheticsv1alpha1.HTTPRequestSpec{URL: "https://example.com"},
		},
	}
	test := &syntheticsv1alpha1.PlaywrightTest{
		ObjectMeta: metav1.ObjectMeta{Name: "flow", Namespace: "default"},
	}
	tcp := &syntheticsv1alpha1.TCPProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "database", Namespace: "default"},
		Spec:       syntheticsv1alpha1.TCPProbeSpec{Target: syntheticsv1alpha1.TCPTarget{Host: "database", Port: 5432}},
	}

	cl := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(probe, test, tcp).Build()
	pub := &fakePublisher{}

	r := &SpecResyncer{Client: cl, Publisher: pub, Log: log.Log}
	r.resync(context.Background())

	pub.mu.Lock()
	defer pub.mu.Unlock()
	if len(pub.specs) != 3 {
		t.Fatalf("expected 3 spec updates, got %d: %+v", len(pub.specs), pub.specs)
	}
	seen := map[results.Kind]string{}
	for _, s := range pub.specs {
		if s.Deleted {
			t.Fatalf("resync must not publish tombstones, got %+v", s)
		}
		seen[s.Kind] = s.Name
	}
	if seen[results.KindHTTPProbe] != "web" || seen[results.KindPlaywrightTest] != "flow" || seen[results.KindTCPProbe] != "database" {
		t.Fatalf("unexpected specs republished: %v", seen)
	}
}
