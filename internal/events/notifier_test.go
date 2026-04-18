package events

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
	internalmetrics "github.com/loks0n/synthetics-operator/internal/metrics"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := syntheticsv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add to scheme: %v", err)
	}
	return s
}

func drainReasons(recorder *record.FakeRecorder) []string {
	var reasons []string
	for {
		select {
		case e := <-recorder.Events:
			reasons = append(reasons, e)
		default:
			return reasons
		}
	}
}

func TestOnProbeTransitionEmitsActiveOnRecovery(t *testing.T) {
	scheme := newScheme(t)
	probe := &syntheticsv1alpha1.HTTPProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(probe).Build()
	recorder := record.NewFakeRecorder(10)

	n := New(c, recorder)
	n.OnProbeTransition(types.NamespacedName{Name: "p", Namespace: "default"}, "HTTPProbe", internalmetrics.ResultConnectTimeout, internalmetrics.ResultOK)

	events := drainReasons(recorder)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %v", len(events), events)
	}
	if !contains(events[0], ReasonProbeActive) || !contains(events[0], corev1.EventTypeNormal) {
		t.Fatalf("expected Normal ProbeActive event, got %q", events[0])
	}
}

func TestOnProbeTransitionEmitsFailedOnDrop(t *testing.T) {
	scheme := newScheme(t)
	probe := &syntheticsv1alpha1.HTTPProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(probe).Build()
	recorder := record.NewFakeRecorder(10)

	n := New(c, recorder)
	n.OnProbeTransition(types.NamespacedName{Name: "p", Namespace: "default"}, "HTTPProbe", internalmetrics.ResultOK, internalmetrics.ResultAssertionFailed)

	events := drainReasons(recorder)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if !contains(events[0], ReasonProbeFailed) || !contains(events[0], corev1.EventTypeWarning) {
		t.Fatalf("expected Warning ProbeFailed event, got %q", events[0])
	}
	if !contains(events[0], "assertion_failed") {
		t.Fatalf("expected message to include result class, got %q", events[0])
	}
}

func TestOnProbeTransitionSilentWhenCRMissing(t *testing.T) {
	scheme := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	recorder := record.NewFakeRecorder(10)

	n := New(c, recorder)
	n.OnProbeTransition(types.NamespacedName{Name: "gone", Namespace: "default"}, "HTTPProbe", internalmetrics.ResultOK, internalmetrics.ResultConnectTimeout)

	if events := drainReasons(recorder); len(events) != 0 {
		t.Fatalf("expected no events for deleted CR, got %v", events)
	}
}

func TestOnTestTransitionEmitsAgainstRightKind(t *testing.T) {
	scheme := newScheme(t)
	test := &syntheticsv1alpha1.PlaywrightTest{
		ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "default"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(test).Build()
	recorder := record.NewFakeRecorder(10)

	n := New(c, recorder)
	n.OnTestTransition(types.NamespacedName{Name: "t", Namespace: "default"}, "PlaywrightTest", internalmetrics.ResultOK, internalmetrics.ResultTestFailed)

	events := drainReasons(recorder)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if !contains(events[0], ReasonTestFailed) {
		t.Fatalf("expected TestFailed event, got %q", events[0])
	}
}

func TestOnProbeTransitionUnknownKindSilent(t *testing.T) {
	scheme := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	recorder := record.NewFakeRecorder(10)

	n := New(c, recorder)
	n.OnProbeTransition(types.NamespacedName{Name: "x", Namespace: "default"}, "MysteryKind", "", internalmetrics.ResultOK)

	if events := drainReasons(recorder); len(events) != 0 {
		t.Fatalf("expected no events for unknown kind, got %v", events)
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
