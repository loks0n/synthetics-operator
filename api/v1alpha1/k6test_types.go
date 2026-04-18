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

// K6ScriptRef points to the k6 script stored in a ConfigMap.
type K6ScriptRef struct {
	ConfigMap ConfigMapKeyRef `json:"configMap"`
}

// RunnerSpec configures pod-level concerns for CronJob test runners.
type RunnerSpec struct {
	Env       []corev1.EnvVar             `json:"env,omitempty"`
	EnvFrom   []corev1.EnvFromSource      `json:"envFrom,omitempty"`
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	Affinity  *corev1.Affinity            `json:"affinity,omitempty"`
}

type K6TestSpec struct {
	Interval         metav1.Duration `json:"interval,omitempty"`
	Suspend          bool            `json:"suspend,omitempty"`
	K6Version        string          `json:"k6Version"`
	Script           K6ScriptRef     `json:"script"`
	TTLAfterFinished metav1.Duration `json:"ttlAfterFinished,omitempty"`
	Runner           *RunnerSpec     `json:"runner,omitempty"`
}

type K6TestStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=k6tests,scope=Namespaced,shortName=k6
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
