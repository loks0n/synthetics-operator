package probes

import (
	"fmt"
	"time"

	"github.com/loks0n/synthetics-operator/api/v1alpha1"
	"github.com/loks0n/synthetics-operator/internal/results"
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

// evalAssertions runs all assertions against ctx. Returns ok=true when every
// assertion passed, the name of the first failing assertion (empty when ok),
// and a per-assertion result slice (always len(assertions)).
func evalAssertions(assertions []v1alpha1.Assertion, ctx assertionContext) (ok bool, failedName string, out []results.AssertionResult) {
	out = make([]results.AssertionResult, 0, len(assertions))
	ok = true
	for _, a := range assertions {
		passed, _ := evalAssertion(a.Expr, ctx)
		r := results.AssertionResult{Name: a.Name, Expr: a.Expr}
		if passed {
			r.Result = 1
		} else {
			r.Result = 0
			if ok {
				failedName = a.Name
				ok = false
			}
		}
		out = append(out, r)
	}
	return ok, failedName, out
}

// outcomeFromOK translates evalAssertions' bool into the wire-level
// result-class string.
func outcomeFromOK(ok bool) string {
	if ok {
		return "ok"
	}
	return "assertion_failed"
}

// EvalHTTPAssertions evaluates HTTP probe assertions against the raw Result.
// Returns the outcome result-class string, the failing assertion name (if
// any), and the per-assertion results. Used by the probe-worker.
func EvalHTTPAssertions(r Result, assertions []v1alpha1.Assertion) (string, string, []results.AssertionResult) {
	sslExpiryDays := float64(-1)
	if r.CertExpiryTime != nil {
		sslExpiryDays = time.Until(*r.CertExpiryTime).Hours() / 24
	}
	ctx := assertionContext{
		"status_code":     float64(r.StatusCode),
		"duration_ms":     float64(r.Duration.Milliseconds()),
		"ssl_expiry_days": sslExpiryDays,
	}
	ok, failed, out := evalAssertions(assertions, ctx)
	return outcomeFromOK(ok), failed, out
}

// EvalDNSAssertions evaluates DNS probe assertions against the raw result.
func EvalDNSAssertions(r DNSResult, assertions []v1alpha1.Assertion) (string, string, []results.AssertionResult) {
	ctx := assertionContext{
		"answer_count": float64(r.AnswerCount),
		"duration_ms":  float64(r.Duration.Milliseconds()),
	}
	ok, failed, out := evalAssertions(assertions, ctx)
	return outcomeFromOK(ok), failed, out
}
