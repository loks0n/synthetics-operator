package controllers

import (
	"maps"
	"testing"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
)

func TestRunnerNodeSelector(t *testing.T) {
	cases := []struct {
		name     string
		defaults map[string]string
		runner   *syntheticsv1alpha1.RunnerSpec
		want     map[string]string
	}{
		{
			name: "nothing set leaves the pod spec field unset",
		},
		{
			name:     "operator default applies when the test says nothing",
			defaults: map[string]string{"workload": "monitoring"},
			runner:   &syntheticsv1alpha1.RunnerSpec{},
			want:     map[string]string{"workload": "monitoring"},
		},
		{
			name:   "test selector applies with no operator default",
			runner: &syntheticsv1alpha1.RunnerSpec{NodeSelector: map[string]string{"workload": "browsers"}},
			want:   map[string]string{"workload": "browsers"},
		},
		{
			name:     "a test adding a key keeps the operator default",
			defaults: map[string]string{"workload": "monitoring"},
			runner:   &syntheticsv1alpha1.RunnerSpec{NodeSelector: map[string]string{"disk": "ssd"}},
			want:     map[string]string{"workload": "monitoring", "disk": "ssd"},
		},
		{
			name:     "a test overriding a key wins for that key only",
			defaults: map[string]string{"workload": "monitoring", "region": "fra1"},
			runner:   &syntheticsv1alpha1.RunnerSpec{NodeSelector: map[string]string{"workload": "browsers"}},
			want:     map[string]string{"workload": "browsers", "region": "fra1"},
		},
		{
			name:     "a nil runner block still gets the operator default",
			defaults: map[string]string{"workload": "monitoring"},
			want:     map[string]string{"workload": "monitoring"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runnerNodeSelector(tc.defaults, tc.runner)
			if !maps.Equal(got, tc.want) {
				t.Fatalf("runnerNodeSelector() = %v, want %v", got, tc.want)
			}
		})
	}
}

// The operator default must not be mutated by merging a test's selector over
// it — every subsequent test would inherit the previous one's keys.
func TestRunnerNodeSelectorDoesNotMutateDefaults(t *testing.T) {
	defaults := map[string]string{"workload": "monitoring"}
	runner := &syntheticsv1alpha1.RunnerSpec{NodeSelector: map[string]string{"disk": "ssd"}}

	runnerNodeSelector(defaults, runner)

	if !maps.Equal(defaults, map[string]string{"workload": "monitoring"}) {
		t.Fatalf("defaults were mutated: %v", defaults)
	}
}
