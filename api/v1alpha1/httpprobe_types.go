package v1alpha1

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// ConditionSuspended mirrors spec.suspend. Runtime pass/fail state lives
	// in metrics, not here — the CR is config-only.
	ConditionSuspended = "Suspended"

	ReasonSuspended = "Suspended"
	ReasonResumed   = "Resumed"
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

type HTTPProbeSpec struct {
	Interval   metav1.Duration `json:"interval,omitempty"`
	Timeout    metav1.Duration `json:"timeout,omitempty"`
	Suspend    bool            `json:"suspend,omitempty"`
	Request    HTTPRequestSpec `json:"request"`
	TLS        *TLSConfig      `json:"tls,omitempty"`
	Assertions []Assertion     `json:"assertions,omitempty"`
}

type HTTPProbeStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	LastRunTime        *metav1.Time       `json:"lastRunTime,omitempty"`
	LastSuccessTime    *metav1.Time       `json:"lastSuccessTime,omitempty"`
	LastFailureTime    *metav1.Time       `json:"lastFailureTime,omitempty"`
	Summary            *ProbeSummary      `json:"summary,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
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
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.request.url`
// +kubebuilder:printcolumn:name="Interval",type=string,JSONPath=`.spec.interval`
// +kubebuilder:printcolumn:name="Suspend",type=boolean,JSONPath=`.spec.suspend`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
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

func defaultIntervalTimeout(interval, timeout *metav1.Duration) {
	if interval.Duration == 0 {
		interval.Duration = 30 * time.Second
	}
	if timeout.Duration == 0 {
		// Cap default timeout at the interval so a sub-second interval doesn't
		// fail validation with "timeout > interval" blaming a default the user
		// didn't set.
		timeout.Duration = min(10*time.Second, interval.Duration)
	}
}
