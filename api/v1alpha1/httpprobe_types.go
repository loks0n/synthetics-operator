package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	ConditionReady     = "Ready"
	ConditionSuspended = "Suspended"

	ReasonInitializing   = "Initializing"
	ReasonProbeSucceeded = "ProbeSucceeded"
	ReasonProbeFailed    = "ProbeFailed"
	ReasonConfigError    = "ConfigError"
	ReasonSuspended      = "Suspended"
	ReasonResumed        = "Resumed"
)

type HTTPRequestSpec struct {
	URL     string            `json:"url"`
	Method  string            `json:"method,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

type HTTPAssertions struct {
	Status int `json:"status,omitempty"`
}

type HttpProbeSpec struct {
	Interval   metav1.Duration `json:"interval,omitempty"`
	Timeout    metav1.Duration `json:"timeout,omitempty"`
	Suspend    bool            `json:"suspend,omitempty"`
	Request    HTTPRequestSpec `json:"request"`
	Assertions HTTPAssertions  `json:"assertions,omitempty"`
}

type HttpProbeStatus struct {
	ObservedGeneration  int64              `json:"observedGeneration,omitempty"`
	LastRunTime         *metav1.Time       `json:"lastRunTime,omitempty"`
	LastSuccessTime     *metav1.Time       `json:"lastSuccessTime,omitempty"`
	LastFailureTime     *metav1.Time       `json:"lastFailureTime,omitempty"`
	ConsecutiveFailures int64              `json:"consecutiveFailures,omitempty"`
	Summary             *ProbeSummary      `json:"summary,omitempty"`
	Conditions          []metav1.Condition `json:"conditions,omitempty"`
}

type ProbeSummary struct {
	Success     bool   `json:"success,omitempty"`
	ConfigError bool   `json:"configError,omitempty"`
	StatusCode  int    `json:"statusCode,omitempty"`
	Message     string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=httpprobes,scope=Namespaced,shortName=hp
type HttpProbe struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HttpProbeSpec   `json:"spec,omitempty"`
	Status HttpProbeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type HttpProbeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HttpProbe `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HttpProbe{}, &HttpProbeList{})
}
