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
	Body    string            `json:"body,omitempty"`
}

// TLSConfig controls TLS verification for the probe request.
type TLSConfig struct {
	// InsecureSkipVerify disables server certificate verification. Use for
	// self-signed certificates in dev/test environments.
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`
	// CACert is a PEM-encoded CA certificate to trust in addition to the
	// system roots. Use for internal PKI or self-signed CA certificates.
	CACert string `json:"caCert,omitempty"`
}

type HTTPAssertions struct {
	Status  int               `json:"status,omitempty"`
	Latency *LatencyAssertion `json:"latency,omitempty"`
	Body    *BodyAssertion    `json:"body,omitempty"`
}

type LatencyAssertion struct {
	MaxMs int `json:"maxMs"`
}

type BodyAssertion struct {
	Contains string          `json:"contains,omitempty"`
	JSON     []JSONAssertion `json:"json,omitempty"`
}

type JSONAssertion struct {
	Path  string `json:"path"`
	Value string `json:"value"`
}

type HTTPProbeSpec struct {
	Interval   metav1.Duration `json:"interval,omitempty"`
	Timeout    metav1.Duration `json:"timeout,omitempty"`
	Suspend    bool            `json:"suspend,omitempty"`
	Request    HTTPRequestSpec `json:"request"`
	Assertions HTTPAssertions  `json:"assertions,omitempty"`
	TLS        *TLSConfig      `json:"tls,omitempty"`
}

type HTTPProbeStatus struct {
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
type HTTPProbe struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HTTPProbeSpec   `json:"spec,omitempty"`
	Status HTTPProbeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type HTTPProbeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HTTPProbe `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HTTPProbe{}, &HTTPProbeList{})
}
