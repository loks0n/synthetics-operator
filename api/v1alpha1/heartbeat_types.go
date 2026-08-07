package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// HeartbeatSpec configures an inbound liveness check: a job somewhere outside
// the cluster pings a generated URL on every successful run, and the operator
// reports the heartbeat down once a ping fails to arrive in time.
//
// This inverts every other kind in this API. The operator does not reach out
// to a target on a schedule; it waits. There is therefore no timeout, no
// assertion language, and no prober involvement — Period and Grace are the
// whole health contract.
type HeartbeatSpec struct {
	// Period is how often a ping is expected. Required.
	Period metav1.Duration `json:"period"`
	// Grace is extra slack allowed on top of Period before the heartbeat is
	// reported down. A job that runs every minute but occasionally takes two
	// wants period 1m, grace 3m. Defaults to Period.
	Grace metav1.Duration `json:"grace,omitempty"`
	// Suspend stops evaluating freshness without deleting the Heartbeat or
	// rotating its token. Pings that arrive while suspended are accepted and
	// acknowledged, but emit no metrics. Use for planned maintenance.
	Suspend bool `json:"suspend,omitempty"`
	// TokenSecretRef adopts an existing token instead of generating one. The
	// referenced Secret must already exist in the same namespace and is left
	// untouched by the operator — you own its lifecycle and rotation. Leave
	// empty to have the operator mint a token into a Secret it owns.
	TokenSecretRef *TokenSecretRef `json:"tokenSecretRef,omitempty"`
	// Depends lists other probes or tests in the same namespace whose failure
	// should suppress alerts on this heartbeat. See DependencyRef.
	Depends []DependencyRef `json:"depends,omitempty"`
	// MetricLabels are user-supplied key/value pairs appended to every
	// Prometheus metric the operator emits for this heartbeat.
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
	// URL is the full endpoint the monitored job should ping. Readable by
	// anyone with get access to this Heartbeat; the same value is also written
	// to the token Secret for jobs that should not need CRD permissions.
	URL string `json:"url,omitempty"`
	// TokenSecretName names the Secret holding the token, whether the operator
	// generated it or spec.tokenSecretRef supplied it.
	TokenSecretName string `json:"tokenSecretName,omitempty"`
	// LastPingTime is the arrival time of the most recent ping. Persisted here
	// so a metrics-consumer restart reseeds from the API server rather than
	// reporting every heartbeat as pending until its next ping — which for a
	// daily backup job would mean a day of false alerts.
	LastPingTime *metav1.Time `json:"lastPingTime,omitempty"`
	// LastResult is the outcome the most recent ping reported: ok for a plain
	// ping or exit code 0, failed for /fail or a non-zero exit code.
	LastResult string `json:"lastResult,omitempty"`
}

// Heartbeat outcome values written to status.lastResult.
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
// Named "Last Result", not "Result": this is what the most recent ping said,
// not whether the heartbeat is currently healthy. A heartbeat whose last ping
// succeeded an hour ago still reads "ok" here while its metric has long since
// gone to `missed`. Freshness is evaluated at scrape time and deliberately
// has no CR field — see synthetics_heartbeat.
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

// Deadline is the instant by which the next ping must arrive, measured from
// the last one. Zero Grace defaults to Period, matching the webhook default.
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
