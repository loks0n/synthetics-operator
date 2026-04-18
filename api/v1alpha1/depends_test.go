package v1alpha1

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newDependsTestReader(t *testing.T, objs ...client.Object) client.Reader {
	t.Helper()
	scheme, err := SchemeBuilder.Build()
	if err != nil {
		t.Fatalf("build scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func TestValidateDependsEmpty(t *testing.T) {
	errs := ValidateDepends(context.Background(), nil, DependencyKindHTTPProbe, "default", "owner", nil, field.NewPath("spec", "depends"))
	if len(errs) != 0 {
		t.Fatalf("expected no errors for empty deps, got %v", errs)
	}
}

func TestValidateDependsUnknownKind(t *testing.T) {
	reader := newDependsTestReader(t)
	errs := ValidateDepends(context.Background(), reader, DependencyKindHTTPProbe, "default", "owner",
		[]DependencyRef{{Kind: "BogusKind", Name: "x"}}, field.NewPath("spec", "depends"))
	if len(errs) == 0 || !strings.Contains(errs[0].Error(), "BogusKind") {
		t.Fatalf("expected NotSupported error for unknown kind, got %v", errs)
	}
}

func TestValidateDependsInvalidName(t *testing.T) {
	reader := newDependsTestReader(t)
	errs := ValidateDepends(context.Background(), reader, DependencyKindHTTPProbe, "default", "owner",
		[]DependencyRef{{Kind: DependencyKindHTTPProbe, Name: "Has_Underscore"}}, field.NewPath("spec", "depends"))
	if len(errs) == 0 {
		t.Fatal("expected name validation error, got none")
	}
}

func TestValidateDependsSelfReference(t *testing.T) {
	reader := newDependsTestReader(t)
	errs := ValidateDepends(context.Background(), reader, DependencyKindHTTPProbe, "default", "self",
		[]DependencyRef{{Kind: DependencyKindHTTPProbe, Name: "self"}}, field.NewPath("spec", "depends"))
	if len(errs) == 0 || !strings.Contains(errs[0].Error(), "cannot depend on itself") {
		t.Fatalf("expected self-reference error, got %v", errs)
	}
}

func TestValidateDependsDuplicate(t *testing.T) {
	target := &HTTPProbe{ObjectMeta: metav1.ObjectMeta{Name: "dep", Namespace: "default"}}
	reader := newDependsTestReader(t, target)
	errs := ValidateDepends(context.Background(), reader, DependencyKindHTTPProbe, "default", "owner",
		[]DependencyRef{
			{Kind: DependencyKindHTTPProbe, Name: "dep"},
			{Kind: DependencyKindHTTPProbe, Name: "dep"},
		}, field.NewPath("spec", "depends"))
	if len(errs) == 0 {
		t.Fatal("expected duplicate error, got none")
	}
	if !strings.Contains(errs[0].Error(), "Duplicate") && !strings.Contains(errs[0].Error(), "duplicate") {
		t.Fatalf("expected duplicate error, got %v", errs)
	}
}

func TestValidateDependsTargetMissing(t *testing.T) {
	reader := newDependsTestReader(t)
	errs := ValidateDepends(context.Background(), reader, DependencyKindHTTPProbe, "default", "owner",
		[]DependencyRef{{Kind: DependencyKindHTTPProbe, Name: "missing"}}, field.NewPath("spec", "depends"))
	if len(errs) == 0 || !strings.Contains(errs[0].Error(), "not found") {
		t.Fatalf("expected not-found error, got %v", errs)
	}
}

func TestValidateDependsCrossKindExists(t *testing.T) {
	dns := &DNSProbe{ObjectMeta: metav1.ObjectMeta{Name: "api-dns", Namespace: "default"}}
	reader := newDependsTestReader(t, dns)
	// Owner is a PlaywrightTest depending on a DNSProbe — cross-kind should work.
	errs := ValidateDepends(context.Background(), reader, DependencyKindPlaywrightTest, "default", "checkout",
		[]DependencyRef{{Kind: DependencyKindDNSProbe, Name: "api-dns"}}, field.NewPath("spec", "depends"))
	if len(errs) != 0 {
		t.Fatalf("expected no errors for valid cross-kind dep, got %v", errs)
	}
}

func TestValidateDependsCycleDetected(t *testing.T) {
	// A → B, B → A. When we admit A's update to add depends on B, B already has
	// depends on A, so the cycle walk hits A and rejects.
	b := &HTTPProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "default"},
		Spec: HTTPProbeSpec{
			Depends: []DependencyRef{{Kind: DependencyKindHTTPProbe, Name: "a"}},
		},
	}
	a := &HTTPProbe{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"}}
	reader := newDependsTestReader(t, a, b)

	errs := ValidateDepends(context.Background(), reader, DependencyKindHTTPProbe, "default", "a",
		[]DependencyRef{{Kind: DependencyKindHTTPProbe, Name: "b"}}, field.NewPath("spec", "depends"))

	if len(errs) == 0 || !strings.Contains(errs[0].Error(), "cycle") {
		t.Fatalf("expected cycle error, got %v", errs)
	}
}

func TestValidateDependsTransitiveNoCycle(t *testing.T) {
	// A → B → C (no cycle). Should pass.
	c := &HTTPProbe{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"}}
	b := &HTTPProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "default"},
		Spec: HTTPProbeSpec{
			Depends: []DependencyRef{{Kind: DependencyKindHTTPProbe, Name: "c"}},
		},
	}
	reader := newDependsTestReader(t, b, c)

	errs := ValidateDepends(context.Background(), reader, DependencyKindHTTPProbe, "default", "a",
		[]DependencyRef{{Kind: DependencyKindHTTPProbe, Name: "b"}}, field.NewPath("spec", "depends"))

	if len(errs) != 0 {
		t.Fatalf("expected no errors for transitive chain without cycle, got %v", errs)
	}
}
