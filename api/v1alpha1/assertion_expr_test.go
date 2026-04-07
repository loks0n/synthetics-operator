package v1alpha1

import (
	"testing"
)

func TestSplitAssertionExprValidCases(t *testing.T) {
	cases := []struct {
		expr      string
		wantVar   string
		wantOp    string
		wantValue float64
	}{
		{"status_code = 200", "status_code", "=", 200},
		{"status_code != 404", "status_code", "!=", 404},
		{"duration_ms < 500", "duration_ms", "<", 500},
		{"duration_ms <= 1000", "duration_ms", "<=", 1000},
		{"answer_count > 0", "answer_count", ">", 0},
		{"answer_count >= 1", "answer_count", ">=", 1},
		{"ssl_expiry_days >= 14", "ssl_expiry_days", ">=", 14},
		// whitespace variants
		{"status_code=200", "status_code", "=", 200},
		{"  duration_ms  <  500  ", "duration_ms", "<", 500},
		// float values
		{"duration_ms < 1.5", "duration_ms", "<", 1.5},
	}

	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			varName, op, value, err := SplitAssertionExpr(tc.expr)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if varName != tc.wantVar {
				t.Errorf("varName: got %q, want %q", varName, tc.wantVar)
			}
			if op != tc.wantOp {
				t.Errorf("op: got %q, want %q", op, tc.wantOp)
			}
			if value != tc.wantValue {
				t.Errorf("value: got %f, want %f", value, tc.wantValue)
			}
		})
	}
}

func TestSplitAssertionExprErrors(t *testing.T) {
	cases := []struct {
		name string
		expr string
	}{
		{"no operator", "status_code 200"},
		{"empty string", ""},
		{"only variable", "status_code"},
		{"non-numeric value", "status_code = abc"},
		{"missing value", "status_code ="},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := SplitAssertionExpr(tc.expr)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestSplitAssertionExprMulticharPrecedence(t *testing.T) {
	// "<=" must parse as <= not as "<" with "=" left over
	_, op, _, err := SplitAssertionExpr("duration_ms <= 500")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if op != "<=" {
		t.Errorf("expected op=<=, got %q", op)
	}

	_, op, _, err = SplitAssertionExpr("duration_ms >= 500")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if op != ">=" {
		t.Errorf("expected op=>=, got %q", op)
	}

	_, op, _, err = SplitAssertionExpr("status_code != 404")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if op != "!=" {
		t.Errorf("expected op=!=, got %q", op)
	}
}

func TestValidateAssertionExprHTTPVars(t *testing.T) {
	cases := []struct {
		expr    string
		wantErr bool
	}{
		{"status_code = 200", false},
		{"duration_ms < 500", false},
		{"ssl_expiry_days >= 14", false},
		{"answer_count > 0", true}, // DNS var not valid for HTTP
		{"unknown_var = 1", true},
		{"status_code = notanumber", true},
	}

	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			err := ValidateAssertionExpr(tc.expr, ValidHTTPAssertionVars)
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

func TestValidateAssertionExprDNSVars(t *testing.T) {
	cases := []struct {
		expr    string
		wantErr bool
	}{
		{"answer_count > 0", false},
		{"duration_ms < 500", false},
		{"status_code = 200", true}, // HTTP var not valid for DNS
		{"ssl_expiry_days >= 14", true},
		{"unknown_var = 1", true},
	}

	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			err := ValidateAssertionExpr(tc.expr, ValidDNSAssertionVars)
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}
