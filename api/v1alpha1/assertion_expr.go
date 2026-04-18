package v1alpha1

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Assertion is a named pass/fail check evaluated against a probe result.
// The Expr field uses a simple expression language:
//
//	variable op value
//
// Operators: =, !=, <, <=, >, >=
// Value must be a numeric literal.
//
// HTTP variables:  status_code, duration_ms, ssl_expiry_days
// DNS variables:   answer_count, duration_ms
type Assertion struct {
	// Name identifies the assertion and is used as the "reason" label on the
	// probe_success metric when this assertion fails.
	Name string `json:"name"`
	// Expr is the expression to evaluate, e.g. "status_code = 200".
	Expr string `json:"expr"`
}

func httpAssertionVars() []string { return []string{"status_code", "duration_ms", "ssl_expiry_days"} }
func dnsAssertionVars() []string  { return []string{"answer_count", "duration_ms"} }

// ValidateAssertionExpr checks that expr is syntactically valid and references
// one of the allowed variables.
func ValidateAssertionExpr(expr string, validVars []string) error {
	varName, _, _, err := SplitAssertionExpr(expr)
	if err != nil {
		return err
	}
	if !slices.Contains(validVars, varName) {
		return fmt.Errorf("unknown variable %q: valid variables are %s",
			varName, strings.Join(validVars, ", "))
	}
	return nil
}

// SplitAssertionExpr splits "variable op value" into its three parts.
// Multi-character operators are tried before single-character ones.
func SplitAssertionExpr(expr string) (varName, op string, value float64, err error) {
	for _, candidate := range []string{"<=", ">=", "!=", "<", ">", "="} {
		before, after, found := strings.Cut(expr, candidate)
		if !found {
			continue
		}
		varName = strings.TrimSpace(before)
		op = candidate
		value, err = strconv.ParseFloat(strings.TrimSpace(after), 64)
		if err != nil {
			err = fmt.Errorf("invalid value: %w", err)
		}
		return varName, op, value, err
	}
	return "", "", 0, errors.New("no operator found; use one of =, !=, <, <=, >, >=")
}
