package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// HeartbeatSpec configures an inbound check-in endpoint for jobs that report
// liveness by pinging the operator.
type HeartbeatSpec struct {
	// Period is how often a ping is expected. Required.
	Period metav1.Duration `json:"period"`
	// Grace is extra slack after Period before the heartbeat is down. Defaults to Period.
	Grace metav1.Duration `json:"grace,omitempty"`
	// Suspend stops emitting heartbeat metrics without rotating the token.
	Suspend bool `json:"suspend,omitempty"`
	// TokenSecretRef adopts an existing token Secret instead of generating one.
	TokenSecretRef *TokenSecretRef `json:"tokenSecretRef,omitempty"`
	// Depends lists same-namespace checks whose failure suppresses this heartbeat.
	Depends []DependencyRef `json:"depends,omitempty"`
	// MetricLabels are appended to every metric emitted for this heartbeat.
	MetricLabels map[string]string `json:"metricLabels,omitempty"`
}

// TokenSecretRef points at the Secret key holding a heartbeat token.
type TokenSecretRef struct {
	Name string `json:"name"`
	// +kubebuilder:default=token
	Key string `json:"key,omitempty"`
}

type HeartbeatStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
	// URL is the full endpoint the monitored job should ping.
	URL string `json:"url,omitempty"`
	// TokenSecretName names the Secret holding the token.
	TokenSecretName string `json:"tokenSecretName,omitempty"`
	// LastPingTime is the arrival time of the most recent ping.
	LastPingTime *metav1.Time `json:"lastPingTime,omitempty"`
	// LastResult is what the most recent ping reported: ok or failed.
	LastResult string `json:"lastResult,omitempty"`
}

const (
	HeartbeatResultOK     = "ok"
	HeartbeatResultFailed = "failed"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=heartbeats,scope=Namespaced,shortName=hb
// +kubebuilder:printcolumn:name="Period",type=string,JSONPath=`.spec.period`
// +kubebuilder:printcolumn:name="Grace",type=string,JSONPath=`.spec.grace`
// +kubebuilder:printcolumn:name="Last Ping",type=date,JSONPath=`.status.lastPingTime`
// +kubebuilder:printcolumn:name="Last Result",type=string,JSONPath=`.status.lastResult`
// +kubebuilder:printcolumn:name="Suspend",type=boolean,JSONPath=`.spec.suspend`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Heartbeat struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HeartbeatSpec   `json:"spec,omitempty"`
	Status HeartbeatStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type HeartbeatList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Heartbeat `json:"items"`
}

// Deadline returns when the next ping is late.
func (h *Heartbeat) Deadline(lastPing metav1.Time) metav1.Time {
	grace := h.Spec.Grace.Duration
	if grace <= 0 {
		grace = h.Spec.Period.Duration
	}
	return metav1.NewTime(lastPing.Add(h.Spec.Period.Duration + grace))
}

func init() {
	SchemeBuilder.Register(&Heartbeat{}, &HeartbeatList{})
}
