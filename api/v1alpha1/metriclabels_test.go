package v1alpha1

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation/field"
)

func TestValidateMetricLabelsEmpty(t *testing.T) {
	if errs := ValidateMetricLabels(nil, field.NewPath("spec", "metricLabels")); len(errs) != 0 {
		t.Fatalf("expected no errors for nil labels, got %v", errs)
	}
	if errs := ValidateMetricLabels(map[string]string{}, field.NewPath("spec", "metricLabels")); len(errs) != 0 {
		t.Fatalf("expected no errors for empty labels, got %v", errs)
	}
}

func TestValidateMetricLabelsValid(t *testing.T) {
	labels := map[string]string{
		"team":         "payments",
		"env":          "production",
		"tier":         "critical",
		"with_under":   "ok",
		"CamelCaseOK":  "ok",
		"_leading_ok":  "ok",
		"letters_nums": "ok",
	}
	if errs := ValidateMetricLabels(labels, field.NewPath("spec", "metricLabels")); len(errs) != 0 {
		t.Fatalf("expected no errors for valid labels, got %v", errs)
	}
}

func TestValidateMetricLabelsInvalidName(t *testing.T) {
	cases := []string{
		"has-dash",
		"has.dot",
		"1leading-digit",
		"with space",
		"__reserved",
	}
	for _, key := range cases {
		t.Run(key, func(t *testing.T) {
			errs := ValidateMetricLabels(map[string]string{key: "x"}, field.NewPath("spec", "metricLabels"))
			if len(errs) == 0 {
				t.Fatalf("expected error for key %q, got none", key)
			}
		})
	}
}

func TestValidateMetricLabelsSystemCollision(t *testing.T) {
	for _, key := range []string{"name", "namespace", "kind", "result", "url", "method", "suite", "test", "unhealthy_dependency", "le"} {
		t.Run(key, func(t *testing.T) {
			errs := ValidateMetricLabels(map[string]string{key: "x"}, field.NewPath("spec", "metricLabels"))
			if len(errs) == 0 {
				t.Fatalf("expected collision error for reserved key %q, got none", key)
			}
			if !strings.Contains(errs[0].Error(), "system label") {
				t.Fatalf("expected 'system label' in error, got %v", errs)
			}
		})
	}
}

func TestValidateMetricLabelsAllowsAnyValue(t *testing.T) {
	// We do not enforce value content. Users are trusted to avoid high cardinality.
	labels := map[string]string{
		"team":      "payments",
		"long":      strings.Repeat("x", 1024),
		"with_uuid": "b2d9f18d-9a3f-4e78-9a1e-e4f9b2d9f18d",
		"ip":        "10.0.0.1",
		"emoji":     "🚀",
	}
	if errs := ValidateMetricLabels(labels, field.NewPath("spec", "metricLabels")); len(errs) != 0 {
		t.Fatalf("expected no errors (values are unrestricted), got %v", errs)
	}
}
