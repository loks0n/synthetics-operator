package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PlaywrightTestSpec defines a Playwright browser test that runs on a
// schedule as a CronJob. Pass/fail is determined by the test script's
// expect() outcomes; the operator emits per-case metrics from Playwright's
// JSON reporter output.
type PlaywrightTestSpec struct {
	// Interval between runs (Go duration). Minimum 1m; must evenly divide 60
	// minutes (sub-hour) or evenly divide 24 hours. Default 1h.
	Interval metav1.Duration `json:"interval,omitempty"`
	// Suspend pauses the CronJob without deleting it.
	Suspend bool `json:"suspend,omitempty"`
	// Script points to the Playwright *.spec.js file stored in a ConfigMap.
	// Playwright version and browser (Chromium) are pinned by the operator's
	// playwright-runner image.
	Script ScriptRef `json:"script"`
	// TTLAfterFinished is how long to keep Job pods after they complete.
	// Default 1h.
	TTLAfterFinished metav1.Duration `json:"ttlAfterFinished,omitempty"`
	// Runner configures pod-level concerns for the runner container.
	Runner *RunnerSpec `json:"runner,omitempty"`
}

// PlaywrightTestStatus reflects the reconciler's view of the PlaywrightTest.
// Runtime pass/fail lives in metrics (synthetics_test and
// synthetics_test_playwright_*), not here — the CR is config-only.
type PlaywrightTestStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=playwrighttests,scope=Namespaced,shortName=pw
// +kubebuilder:printcolumn:name="Interval",type=string,JSONPath=`.spec.interval`
// +kubebuilder:printcolumn:name="Suspend",type=boolean,JSONPath=`.spec.suspend`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type PlaywrightTest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PlaywrightTestSpec   `json:"spec,omitempty"`
	Status PlaywrightTestStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type PlaywrightTestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PlaywrightTest `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PlaywrightTest{}, &PlaywrightTestList{})
}
