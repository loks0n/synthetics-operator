package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// TCPTarget identifies the socket a TCPProbe connects to.
type TCPTarget struct {
	// Host is a DNS hostname, IPv4 address, or IPv6 address. Do not include a
	// URL scheme or port; Port is configured separately.
	Host string `json:"host"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
}

type TCPProbeSpec struct {
	Interval   metav1.Duration `json:"interval,omitempty"`
	Timeout    metav1.Duration `json:"timeout,omitempty"`
	Suspend    bool            `json:"suspend,omitempty"`
	Target     TCPTarget       `json:"target"`
	Assertions []Assertion     `json:"assertions,omitempty"`
	// Depends lists other probes or tests in the same namespace whose failure
	// should suppress alerts on this probe. See DependencyRef.
	Depends []DependencyRef `json:"depends,omitempty"`
	// MetricLabels are user-supplied key/value pairs appended to every
	// Prometheus metric the operator emits for this probe.
	MetricLabels map[string]string `json:"metricLabels,omitempty"`
}

type TCPProbeStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=tcpprobes,scope=Namespaced,shortName=tp
// +kubebuilder:printcolumn:name="Host",type=string,JSONPath=`.spec.target.host`
// +kubebuilder:printcolumn:name="Port",type=integer,JSONPath=`.spec.target.port`
// +kubebuilder:printcolumn:name="Interval",type=string,JSONPath=`.spec.interval`
// +kubebuilder:printcolumn:name="Suspend",type=boolean,JSONPath=`.spec.suspend`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type TCPProbe struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TCPProbeSpec   `json:"spec,omitempty"`
	Status TCPProbeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type TCPProbeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TCPProbe `json:"items"`
}

func init() {
	SchemeBuilder.Register(&TCPProbe{}, &TCPProbeList{})
}
