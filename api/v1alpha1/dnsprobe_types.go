package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type DNSQuery struct {
	Name     string `json:"name"`
	Type     string `json:"type,omitempty"`     // A, AAAA, CNAME, MX, TXT, NS, PTR — default A
	Resolver string `json:"resolver,omitempty"` // e.g. "8.8.8.8:53", default system resolver
}

type DNSProbeSpec struct {
	Interval   metav1.Duration `json:"interval,omitempty"`
	Timeout    metav1.Duration `json:"timeout,omitempty"`
	Suspend    bool            `json:"suspend,omitempty"`
	Query      DNSQuery        `json:"query"`
	Assertions []Assertion     `json:"assertions,omitempty"`
}

type DNSProbeStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=dnsprobes,scope=Namespaced,shortName=dp
// +kubebuilder:printcolumn:name="Name",type=string,JSONPath=`.spec.query.name`,priority=0
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.query.type`
// +kubebuilder:printcolumn:name="Interval",type=string,JSONPath=`.spec.interval`
// +kubebuilder:printcolumn:name="Suspend",type=boolean,JSONPath=`.spec.suspend`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type DNSProbe struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DNSProbeSpec   `json:"spec,omitempty"`
	Status DNSProbeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type DNSProbeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DNSProbe `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DNSProbe{}, &DNSProbeList{})
}
