package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// SMTPTLSMode selects how an SMTPProbe negotiates TLS.
// +kubebuilder:validation:Enum=none;starttls;implicit
type SMTPTLSMode string

const (
	// SMTPTLSModeNone speaks plain SMTP and never negotiates TLS.
	SMTPTLSModeNone SMTPTLSMode = "none"
	// SMTPTLSModeStartTLS upgrades a plain session with the STARTTLS command
	// and fails when the server does not advertise it.
	SMTPTLSModeStartTLS SMTPTLSMode = "starttls"
	// SMTPTLSModeImplicit performs the TLS handshake before the banner, as on
	// port 465 (RFC 8314).
	SMTPTLSModeImplicit SMTPTLSMode = "implicit"
)

// DefaultSMTPTLSMode is the mode a port conventionally uses: implicit TLS on
// 465 (RFC 8314), STARTTLS on submission port 587 (RFC 6409), plain SMTP
// elsewhere, where STARTTLS is opportunistic between relays.
func DefaultSMTPTLSMode(port int32) SMTPTLSMode {
	switch port {
	case 465:
		return SMTPTLSModeImplicit
	case 587:
		return SMTPTLSModeStartTLS
	}
	return SMTPTLSModeNone
}

// SMTPTLSConfig selects the TLS mode and how the server certificate is verified.
type SMTPTLSConfig struct {
	// Mode is none, starttls or implicit. Defaults from the port: implicit on
	// 465, starttls on 587, none otherwise.
	// +optional
	Mode      SMTPTLSMode `json:"mode,omitempty"`
	TLSConfig `json:",inline"`
}

type SMTPProbeSpec struct {
	Interval metav1.Duration `json:"interval,omitempty"`
	Timeout  metav1.Duration `json:"timeout,omitempty"`
	Suspend  bool            `json:"suspend,omitempty"`
	// Target is the mail server's host and port, in the same shape as a TCPProbe.
	Target     TCPTarget     `json:"target"`
	TLS        SMTPTLSConfig `json:"tls,omitempty"`
	Assertions []Assertion   `json:"assertions,omitempty"`
	// Depends lists other probes or tests in the same namespace whose failure
	// should suppress alerts on this probe. See DependencyRef.
	Depends []DependencyRef `json:"depends,omitempty"`
	// MetricLabels are user-supplied key/value pairs appended to every
	// Prometheus metric the operator emits for this probe.
	MetricLabels map[string]string `json:"metricLabels,omitempty"`
}

type SMTPProbeStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=smtpprobes,scope=Namespaced,shortName=sp
// +kubebuilder:printcolumn:name="Host",type=string,JSONPath=`.spec.target.host`
// +kubebuilder:printcolumn:name="Port",type=integer,JSONPath=`.spec.target.port`
// +kubebuilder:printcolumn:name="TLS",type=string,JSONPath=`.spec.tls.mode`
// +kubebuilder:printcolumn:name="Interval",type=string,JSONPath=`.spec.interval`
// +kubebuilder:printcolumn:name="Suspend",type=boolean,JSONPath=`.spec.suspend`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SMTPProbe struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SMTPProbeSpec   `json:"spec,omitempty"`
	Status SMTPProbeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type SMTPProbeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SMTPProbe `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SMTPProbe{}, &SMTPProbeList{})
}
