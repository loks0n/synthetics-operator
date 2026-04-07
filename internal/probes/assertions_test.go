package probes

import (
	"testing"

	"github.com/loks0n/synthetics-operator/api/v1alpha1"
)

func TestEvalAssertionAllOperators(t *testing.T) {
	ctx := assertionContext{"x": 100}

	cases := []struct {
		expr string
		want bool
	}{
		{"x = 100", true},
		{"x = 99", false},
		{"x != 99", true},
		{"x != 100", false},
		{"x < 101", true},
		{"x < 100", false},
		{"x <= 100", true},
		{"x <= 99", false},
		{"x > 99", true},
		{"x > 100", false},
		{"x >= 100", true},
		{"x >= 101", false},
	}

	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			got, err := evalAssertion(tc.expr, ctx)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("evalAssertion(%q) = %v, want %v", tc.expr, got, tc.want)
			}
		})
	}
}

func TestEvalAssertionUnknownVariable(t *testing.T) {
	ctx := assertionContext{"x": 1}
	_, err := evalAssertion("unknown = 1", ctx)
	if err == nil {
		t.Fatal("expected error for unknown variable, got nil")
	}
}

func TestEvalAssertionParseError(t *testing.T) {
	ctx := assertionContext{"x": 1}
	_, err := evalAssertion("no operator here", ctx)
	if err == nil {
		t.Fatal("expected error for unparseable expression, got nil")
	}
}

func TestEvalAssertionsAllPass(t *testing.T) {
	assertions := []v1alpha1.Assertion{
		{Name: "check_a", Expr: "x > 0"},
		{Name: "check_b", Expr: "x < 200"},
	}
	ctx := assertionContext{"x": 100}

	ok, reason, _ := evalAssertions(assertions, ctx)

	if !ok {
		t.Fatalf("expected all assertions to pass, got reason=%q", reason)
	}
	if reason != "" {
		t.Fatalf("expected empty reason, got %q", reason)
	}
}

func TestEvalAssertionsFirstFails(t *testing.T) {
	assertions := []v1alpha1.Assertion{
		{Name: "first", Expr: "x > 200"},
		{Name: "second", Expr: "x < 200"},
	}
	ctx := assertionContext{"x": 100}

	ok, reason, _ := evalAssertions(assertions, ctx)

	if ok {
		t.Fatal("expected assertions to fail")
	}
	if reason != "first" {
		t.Fatalf("expected reason=first, got %q", reason)
	}
}

func TestEvalAssertionsSecondFails(t *testing.T) {
	assertions := []v1alpha1.Assertion{
		{Name: "first", Expr: "x > 0"},
		{Name: "second", Expr: "x > 200"},
	}
	ctx := assertionContext{"x": 100}

	ok, reason, _ := evalAssertions(assertions, ctx)

	if ok {
		t.Fatal("expected assertions to fail")
	}
	if reason != "second" {
		t.Fatalf("expected reason=second, got %q", reason)
	}
}

func TestEvalAssertionsShortCircuit(t *testing.T) {
	// Third assertion has an unknown variable; it should never be evaluated
	// because the second assertion fails first.
	assertions := []v1alpha1.Assertion{
		{Name: "first", Expr: "x > 0"},
		{Name: "second", Expr: "x > 200"},
		{Name: "third", Expr: "unknown_var = 1"},
	}
	ctx := assertionContext{"x": 100}

	ok, reason, _ := evalAssertions(assertions, ctx)

	if ok {
		t.Fatal("expected failure")
	}
	if reason != "second" {
		t.Fatalf("expected short-circuit at second, got %q", reason)
	}
}

func TestEvalAssertionsEmpty(t *testing.T) {
	ok, reason, _ := evalAssertions(nil, assertionContext{})
	if !ok {
		t.Fatal("empty assertions should always pass")
	}
	if reason != "" {
		t.Fatalf("expected empty reason, got %q", reason)
	}
}
