package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConfigMapKeyRef identifies a key within a ConfigMap.
type ConfigMapKeyRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// ScriptRef points to a test script stored in a ConfigMap. Shared by all
// CronJob-backed test kinds.
type ScriptRef struct {
	ConfigMap ConfigMapKeyRef `json:"configMap"`
}

// RunnerSpec configures pod-level concerns for CronJob-backed test runners.
// Use it to inject environment variables, mount secrets, pin resource requests,
// or control pod placement. It does not configure the test script itself — for
// that, use Script.
type RunnerSpec struct {
	// Env is additional environment variables to set on the runner container,
	// merged with the operator-set defaults.
	Env []corev1.EnvVar `json:"env,omitempty"`
	// EnvFrom bulk-loads environment variables from Secrets or ConfigMaps.
	EnvFrom []corev1.EnvFromSource `json:"envFrom,omitempty"`
	// Resources sets CPU/memory requests and limits on the runner container.
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// Affinity controls pod placement, e.g. spreading runs across nodes.
	Affinity *corev1.Affinity `json:"affinity,omitempty"`
}

// K6TestSpec defines a k6 load test that runs on a schedule as a CronJob.
type K6TestSpec struct {
	// Interval between runs (Go duration). Minimum 1m; must evenly divide 60
	// minutes (sub-hour) or evenly divide 24 hours. Default 1h.
	Interval metav1.Duration `json:"interval,omitempty"`
	// Suspend pauses the CronJob without deleting it. Already-running pods are
	// not terminated.
	Suspend bool `json:"suspend,omitempty"`
	// K6Version pins the grafana/k6 image tag used for the runner container.
	K6Version string `json:"k6Version"`
	// Script points to the k6 JavaScript script stored in a ConfigMap.
	Script ScriptRef `json:"script"`
	// TTLAfterFinished is how long to keep Job pods after they complete, for
	// log inspection. Default 1h.
	TTLAfterFinished metav1.Duration `json:"ttlAfterFinished,omitempty"`
	// Runner configures pod-level concerns for the runner container.
	Runner *RunnerSpec `json:"runner,omitempty"`
}

// K6TestStatus reflects the reconciler's view of the K6Test. Runtime pass/fail
// lives in metrics (synthetics_test and synthetics_test_playwright_*), not
// here — the CR is config-only.
type K6TestStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=k6tests,scope=Namespaced,shortName=k6
// +kubebuilder:printcolumn:name="Interval",type=string,JSONPath=`.spec.interval`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.k6Version`
// +kubebuilder:printcolumn:name="Suspend",type=boolean,JSONPath=`.spec.suspend`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type K6Test struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   K6TestSpec   `json:"spec,omitempty"`
	Status K6TestStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type K6TestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []K6Test `json:"items"`
}

func init() {
	SchemeBuilder.Register(&K6Test{}, &K6TestList{})
}
