package v1alpha1

import (
	"testing"

	"k8s.io/apimachinery/pkg/util/validation/field"
)

func TestValidateRunnerNodeSelector(t *testing.T) {
	cases := []struct {
		name     string
		selector map[string]string
		wantErrs int
	}{
		{
			name: "empty selector",
		},
		{
			name:     "bare key",
			selector: map[string]string{"workload": "monitoring"},
		},
		{
			name:     "prefixed key",
			selector: map[string]string{"node.kubernetes.io/instance-type": "s-4vcpu-8gb"},
		},
		{
			name:     "empty value matches nodes carrying the bare label",
			selector: map[string]string{"workload": ""},
		},
		{
			name:     "key with a space",
			selector: map[string]string{"work load": "monitoring"},
			wantErrs: 1,
		},
		{
			name:     "value with a space",
			selector: map[string]string{"workload": "monitoring pool"},
			wantErrs: 1,
		},
		{
			name:     "both halves invalid are reported separately",
			selector: map[string]string{"work load": "monitoring pool"},
			wantErrs: 2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := ValidateRunnerNodeSelector(tc.selector, field.NewPath("spec", "runner", "nodeSelector"))
			if len(errs) != tc.wantErrs {
				t.Fatalf("ValidateRunnerNodeSelector() = %d errors (%v), want %d", len(errs), errs, tc.wantErrs)
			}
		})
	}
}
