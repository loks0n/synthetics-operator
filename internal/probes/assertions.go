package probes

import (
	"fmt"

	"github.com/loks0n/synthetics-operator/api/v1alpha1"
	internalmetrics "github.com/loks0n/synthetics-operator/internal/metrics"
)

// assertionContext maps variable names to their runtime values.
type assertionContext map[string]float64

// evalAssertion parses and evaluates a single assertion expression.
func evalAssertion(expr string, ctx assertionContext) (bool, error) {
	varName, op, value, err := v1alpha1.SplitAssertionExpr(expr)
	if err != nil {
		return false, fmt.Errorf("parse %q: %w", expr, err)
	}

	actual, ok := ctx[varName]
	if !ok {
		return false, fmt.Errorf("unknown variable %q", varName)
	}

	switch op {
	case "=":
		return actual == value, nil
	case "!=":
		return actual != value, nil
	case "<":
		return actual < value, nil
	case "<=":
		return actual <= value, nil
	case ">":
		return actual > value, nil
	case ">=":
		return actual >= value, nil
	default:
		return false, fmt.Errorf("unknown operator %q", op)
	}
}

// evalAssertions runs all assertions against ctx. It returns ok=false and the
// name of the first failing assertion when any assertion fails, plus a
// per-assertion result slice for every assertion.
func evalAssertions(assertions []v1alpha1.Assertion, ctx assertionContext) (ok bool, failedName string, results []internalmetrics.AssertionResult) {
	results = make([]internalmetrics.AssertionResult, 0, len(assertions))
	ok = true
	for _, a := range assertions {
		passed, _ := evalAssertion(a.Expr, ctx)
		r := internalmetrics.AssertionResult{Name: a.Name, Expr: a.Expr}
		if passed {
			r.Result = 1
		} else {
			r.Result = 0
			if ok {
				failedName = a.Name
				ok = false
			}
		}
		results = append(results, r)
	}
	return ok, failedName, results
}
