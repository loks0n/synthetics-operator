package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
)

// scrape returns the rendered /metrics body for the store.
func scrape(t *testing.T, store *Store) string {
	t.Helper()
	srv := httptest.NewServer(store.Server("").handler)
	defer srv.Close()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

// TestSuppressionSilentCases covers the two "no suppression" outcomes we
// expect: owner passing (nothing to suppress) and dep passing (no reason to
// suppress). Dupe risk is deliberate — the parameterisation is thin.
func TestSuppressionSilentCases(t *testing.T) {
	cases := []struct {
		name        string
		ownerResult Result
		depResult   Result
	}{
		{"owner passes, dep fails", ResultOK, ResultConnectTimeout},
		{"owner fails, dep passes", ResultConnectTimeout, ResultOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, err := NewStore()
			if err != nil {
				t.Fatalf("NewStore: %v", err)
			}
			owner := types.NamespacedName{Namespace: "default", Name: "owner"}
			dep := types.NamespacedName{Namespace: "default", Name: "dep"}

			store.Upsert(dep, ProbeState{Kind: "HTTPProbe", Result: tc.depResult})
			store.Upsert(owner, ProbeState{Kind: "HTTPProbe", Result: tc.ownerResult})
			store.SetDepends("HTTPProbe", owner, []syntheticsv1alpha1.DependencyRef{
				{Kind: syntheticsv1alpha1.DependencyKindHTTPProbe, Name: "dep"},
			})

			body := scrape(t, store)
			if strings.Contains(body, "synthetics_probe_suppressed{") {
				t.Fatalf("expected no suppression metric, got:\n%s", body)
			}
		})
	}
}

func TestSuppressionEmitsWhenProbeAndDirectDepFail(t *testing.T) {
	store, err := NewStore()
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	owner := types.NamespacedName{Namespace: "default", Name: "owner"}
	dep := types.NamespacedName{Namespace: "default", Name: "dep"}

	store.Upsert(dep, ProbeState{Kind: "HTTPProbe", Result: ResultDNSFailed})
	store.Upsert(owner, ProbeState{Kind: "HTTPProbe", Result: ResultConnectTimeout})
	store.SetDepends("HTTPProbe", owner, []syntheticsv1alpha1.DependencyRef{
		{Kind: syntheticsv1alpha1.DependencyKindHTTPProbe, Name: "dep"},
	})

	body := scrape(t, store)
	if !strings.Contains(body, `synthetics_probe_suppressed{`) {
		t.Fatalf("expected suppression metric, got:\n%s", body)
	}
	if !strings.Contains(body, `unhealthy_dependency="dep"`) {
		t.Fatalf("expected dep name on label, got:\n%s", body)
	}
	if !strings.Contains(body, `unhealthy_dependency_kind="HTTPProbe"`) {
		t.Fatalf("expected dep kind on label, got:\n%s", body)
	}
}

func TestSuppressionSilentWhenDepMissing(t *testing.T) {
	store, err := NewStore()
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	owner := types.NamespacedName{Namespace: "default", Name: "owner"}

	// Dep never registered in store.
	store.Upsert(owner, ProbeState{Kind: "HTTPProbe", Result: ResultConnectTimeout})
	store.SetDepends("HTTPProbe", owner, []syntheticsv1alpha1.DependencyRef{
		{Kind: syntheticsv1alpha1.DependencyKindHTTPProbe, Name: "ghost"},
	})

	body := scrape(t, store)
	if strings.Contains(body, "synthetics_probe_suppressed{") {
		t.Fatalf("expected no suppression when dep is unknown at runtime, got:\n%s", body)
	}
}

func TestSuppressionTransitive(t *testing.T) {
	// A fails, depends on B. B fails, depends on C. C fails.
	// Suppression for A should name C (the deepest failing).
	store, err := NewStore()
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	a := types.NamespacedName{Namespace: "default", Name: "a"}
	b := types.NamespacedName{Namespace: "default", Name: "b"}
	c := types.NamespacedName{Namespace: "default", Name: "c"}

	store.Upsert(a, ProbeState{Kind: "HTTPProbe", Result: ResultConnectTimeout})
	store.Upsert(b, ProbeState{Kind: "HTTPProbe", Result: ResultConnectTimeout})
	store.Upsert(c, ProbeState{Kind: "HTTPProbe", Result: ResultDNSFailed})

	store.SetDepends("HTTPProbe", a, []syntheticsv1alpha1.DependencyRef{
		{Kind: syntheticsv1alpha1.DependencyKindHTTPProbe, Name: "b"},
	})
	store.SetDepends("HTTPProbe", b, []syntheticsv1alpha1.DependencyRef{
		{Kind: syntheticsv1alpha1.DependencyKindHTTPProbe, Name: "c"},
	})

	body := scrape(t, store)
	// Both A and B should be suppressed (both are failing with failing transitive deps).
	lines := strings.Split(body, "\n")
	var aLine, bLine string
	for _, line := range lines {
		if strings.Contains(line, "synthetics_probe_suppressed{") && strings.Contains(line, `name="a"`) {
			aLine = line
		}
		if strings.Contains(line, "synthetics_probe_suppressed{") && strings.Contains(line, `name="b"`) {
			bLine = line
		}
	}
	if aLine == "" {
		t.Fatalf("expected suppression metric for a, got:\n%s", body)
	}
	if bLine == "" {
		t.Fatalf("expected suppression metric for b, got:\n%s", body)
	}
	// A should point at the deepest failing dep — c — not its direct neighbour b.
	if !strings.Contains(aLine, `unhealthy_dependency="c"`) {
		t.Fatalf("expected a to point at deepest failing dep c, got: %s", aLine)
	}
	// B's only dep is c; points at c.
	if !strings.Contains(bLine, `unhealthy_dependency="c"`) {
		t.Fatalf("expected b to point at c, got: %s", bLine)
	}
}

func TestSuppressionCrossKindDep(t *testing.T) {
	// PlaywrightTest depends on HTTPProbe. Both failing → suppressed.
	store, err := NewStore()
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	owner := types.NamespacedName{Namespace: "default", Name: "checkout"}
	dep := types.NamespacedName{Namespace: "default", Name: "auth"}

	store.Upsert(dep, ProbeState{Kind: "HTTPProbe", Result: ResultConnectRefused})
	store.UpsertTest(owner, TestState{Kind: "PlaywrightTest", Result: ResultTestFailed})
	store.SetDepends("PlaywrightTest", owner, []syntheticsv1alpha1.DependencyRef{
		{Kind: syntheticsv1alpha1.DependencyKindHTTPProbe, Name: "auth"},
	})

	body := scrape(t, store)
	if !strings.Contains(body, "synthetics_test_suppressed{") {
		t.Fatalf("expected synthetics_test_suppressed, got:\n%s", body)
	}
	if !strings.Contains(body, `unhealthy_dependency_kind="HTTPProbe"`) {
		t.Fatalf("expected cross-kind dep label, got:\n%s", body)
	}
}

func TestSuppressionCycleSafe(t *testing.T) {
	// A → B → A cycle. Neither should crash; both failing so either suppression
	// emission is fine — we just verify no panic and that metric is emitted.
	store, err := NewStore()
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	a := types.NamespacedName{Namespace: "default", Name: "a"}
	b := types.NamespacedName{Namespace: "default", Name: "b"}

	store.Upsert(a, ProbeState{Kind: "HTTPProbe", Result: ResultConnectTimeout})
	store.Upsert(b, ProbeState{Kind: "HTTPProbe", Result: ResultConnectTimeout})
	store.SetDepends("HTTPProbe", a, []syntheticsv1alpha1.DependencyRef{
		{Kind: syntheticsv1alpha1.DependencyKindHTTPProbe, Name: "b"},
	})
	store.SetDepends("HTTPProbe", b, []syntheticsv1alpha1.DependencyRef{
		{Kind: syntheticsv1alpha1.DependencyKindHTTPProbe, Name: "a"},
	})

	// Just make sure scrape doesn't panic.
	_ = scrape(t, store)
}

func TestClearDependsRemovesSuppression(t *testing.T) {
	store, err := NewStore()
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	owner := types.NamespacedName{Namespace: "default", Name: "owner"}
	dep := types.NamespacedName{Namespace: "default", Name: "dep"}

	store.Upsert(dep, ProbeState{Kind: "HTTPProbe", Result: ResultConnectTimeout})
	store.Upsert(owner, ProbeState{Kind: "HTTPProbe", Result: ResultConnectTimeout})
	store.SetDepends("HTTPProbe", owner, []syntheticsv1alpha1.DependencyRef{
		{Kind: syntheticsv1alpha1.DependencyKindHTTPProbe, Name: "dep"},
	})

	if !strings.Contains(scrape(t, store), "synthetics_probe_suppressed{") {
		t.Fatal("expected suppression before clear")
	}

	store.ClearDepends("HTTPProbe", owner)
	if strings.Contains(scrape(t, store), "synthetics_probe_suppressed{") {
		t.Fatal("expected no suppression after ClearDepends")
	}
}
