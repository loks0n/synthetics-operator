// Package results defines the JSON wire types carried over NATS between the
// four deployments: controller, webhook, prober, and metrics.
//
// Subjects:
//   - synthetics.specs            — controller → prober + metrics
//   - synthetics.probes.jobs      — controller scheduler → prober (queue group)
//   - synthetics.probes.results   — prober → metrics
//   - synthetics.tests.results    — test-sidecar (CronJob pod) → metrics
package results

import "time"

// Subject constants. Colocate them here so every producer/consumer sees the
// same strings — a typo in a subject name is silent.
const (
	SubjectSpecs        = "synthetics.specs"
	SubjectProbeJobs    = "synthetics.probes.jobs"
	SubjectProbeResults = "synthetics.probes.results"
	SubjectTestResults  = "synthetics.tests.results"
	ProberQueue         = "synthetics-probers"
)

// Kind identifies the CRD type that produced a result. Values match the
// Kubernetes kind string exactly so cross-referencing with kubectl output
// is straightforward.
type Kind string

const (
	KindHTTPProbe      Kind = "HTTPProbe"
	KindDNSProbe       Kind = "DNSProbe"
	KindK6Test         Kind = "K6Test"
	KindPlaywrightTest Kind = "PlaywrightTest"
)

// DependencyRef mirrors api/v1alpha1.DependencyRef on the wire. Duplicated
// here to keep `internal/results` free of k8s API dependencies — the
// consumers of these messages don't need the full v1alpha1 package.
type DependencyRef struct {
	Kind Kind   `json:"kind"`
	Name string `json:"name"`
}

// Assertion is the wire form of api/v1alpha1.Assertion. Workers evaluate it;
// the metrics consumer doesn't reinterpret it.
type Assertion struct {
	Name string `json:"name"`
	Expr string `json:"expr"`
}

// HTTPProbeSpecPayload carries everything a prober needs to execute
// an HTTPProbe. It's a subset of api/v1alpha1.HTTPProbeSpec deliberately —
// workers don't need status or metadata.
type HTTPProbeSpecPayload struct {
	TimeoutMs  int64             `json:"timeoutMs"`
	URL        string            `json:"url"`
	Method     string            `json:"method"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       string            `json:"body,omitempty"`
	TLS        *TLSConfig        `json:"tls,omitempty"`
	Assertions []Assertion       `json:"assertions,omitempty"`
}

// TLSConfig mirrors api/v1alpha1.TLSConfig.
type TLSConfig struct {
	InsecureSkipVerify bool   `json:"insecureSkipVerify,omitempty"`
	CACert             string `json:"caCert,omitempty"`
}

// DNSProbeSpecPayload carries everything a prober needs to execute a
// DNSProbe.
type DNSProbeSpecPayload struct {
	TimeoutMs  int64       `json:"timeoutMs"`
	Name       string      `json:"name"`
	Type       string      `json:"type"`
	Resolver   string      `json:"resolver,omitempty"`
	Assertions []Assertion `json:"assertions,omitempty"`
}

// SpecUpdate is published on synthetics.specs whenever a CR's spec changes
// or the CR is deleted. Probe-workers and metrics-consumers each maintain a
// cache of these — that cache is the only way they learn about deps /
// metricLabels / executable specs, since they don't watch the k8s API.
//
// Deleted=true is a tombstone: remove all state for {Kind, Namespace, Name}.
type SpecUpdate struct {
	Kind         Kind              `json:"kind"`
	Name         string            `json:"name"`
	Namespace    string            `json:"namespace"`
	Generation   int64             `json:"generation"`
	Deleted      bool              `json:"deleted,omitempty"`
	Suspend      bool              `json:"suspend,omitempty"`
	IntervalMs   int64             `json:"intervalMs,omitempty"`
	Depends      []DependencyRef   `json:"depends,omitempty"`
	MetricLabels map[string]string `json:"metricLabels,omitempty"`
	// Exactly one of the following is set, matching Kind. Tests don't carry
	// an executable payload — the CronJob owns execution; the message is
	// just the spec metadata the metrics consumer needs.
	HTTPProbe *HTTPProbeSpecPayload `json:"httpProbe,omitempty"`
	DNSProbe  *DNSProbeSpecPayload  `json:"dnsProbe,omitempty"`
}

// ProbeJob is published by the controller's scheduler on each scheduled
// tick. Workers pull from a NATS queue group (`ProberQueue`) so each
// job is handled by exactly one worker. The job is deliberately thin — the
// worker resolves the rest from its spec cache.
type ProbeJob struct {
	Kind        Kind      `json:"kind"`
	Name        string    `json:"name"`
	Namespace   string    `json:"namespace"`
	Generation  int64     `json:"generation"`
	ScheduledAt time.Time `json:"scheduledAt"`
}

// AssertionResult is the per-assertion 0/1 outcome carried back to the
// metrics consumer for emission on synthetics_probe_assertion_result.
type AssertionResult struct {
	Name   string  `json:"name"`
	Expr   string  `json:"expr"`
	Result float64 `json:"result"`
}

// ProbeResult is published by probers after each execution. Carries
// everything the metrics consumer needs to emit the probe's metric family —
// result class, duration, HTTP/DNS telemetry, per-assertion results.
//
// The metrics consumer joins this against its SpecUpdate cache to resolve
// metricLabels and depends; those are not duplicated here.
type ProbeResult struct {
	Kind            Kind      `json:"kind"`
	Name            string    `json:"name"`
	Namespace       string    `json:"namespace"`
	Generation      int64     `json:"generation"`
	Timestamp       time.Time `json:"timestamp"`
	DurationMs      int64     `json:"durationMs"`
	Result          string    `json:"result"`
	FailedAssertion string    `json:"failedAssertion,omitempty"`

	// HTTP telemetry
	HTTPStatusCode        int               `json:"httpStatusCode,omitempty"`
	HTTPVersion           float64           `json:"httpVersion,omitempty"`
	URL                   string            `json:"url,omitempty"`
	Method                string            `json:"method,omitempty"`
	TLSCertExpiryUnix     int64             `json:"tlsCertExpiryUnix,omitempty"`
	HTTPPhaseDNSMs        float64           `json:"httpPhaseDnsMs,omitempty"`
	HTTPPhaseConnectMs    float64           `json:"httpPhaseConnectMs,omitempty"`
	HTTPPhaseTLSMs        float64           `json:"httpPhaseTlsMs,omitempty"`
	HTTPPhaseProcessingMs float64           `json:"httpPhaseProcessingMs,omitempty"`
	HTTPPhaseTransferMs   float64           `json:"httpPhaseTransferMs,omitempty"`
	AssertionResults      []AssertionResult `json:"assertionResults,omitempty"`

	// DNS telemetry
	DNSFirstAnswerValue string `json:"dnsFirstAnswerValue,omitempty"`
	DNSFirstAnswerType  string `json:"dnsFirstAnswerType,omitempty"`
	DNSAnswerCount      int    `json:"dnsAnswerCount,omitempty"`
	DNSAuthorityCount   int    `json:"dnsAuthorityCount,omitempty"`
	DNSAdditionalCount  int    `json:"dnsAdditionalCount,omitempty"`
}

// TestCase is a single case within a PlaywrightTest run, produced from the
// Playwright JSON reporter output.
type TestCase struct {
	Suite      string `json:"suite"`
	Test       string `json:"test"`
	Passed     bool   `json:"passed"`
	DurationMs int64  `json:"durationMs"`
}

// TestResult is the JSON payload published by the test-sidecar and consumed
// by the metrics-consumer. Tests is populated only by kinds that emit
// per-case breakdowns (PlaywrightTest).
type TestResult struct {
	Kind       Kind       `json:"kind"`
	Name       string     `json:"name"`
	Namespace  string     `json:"namespace"`
	Success    bool       `json:"success"`
	Timestamp  time.Time  `json:"timestamp"`
	DurationMs int64      `json:"durationMs"`
	Tests      []TestCase `json:"tests,omitempty"`
}
