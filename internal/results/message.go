package results

import "time"

// Kind identifies the CRD type that produced the result.
type Kind string

const (
	KindHTTPProbe Kind = "HTTPProbe"
	KindDNSProbe  Kind = "DNSProbe"
)

// ProbeResult is the JSON payload published by the results-writer sidecar
// and consumed by the operator's NATS subscriber.
type ProbeResult struct {
	Kind       Kind      `json:"kind"`
	Name       string    `json:"name"`
	Namespace  string    `json:"namespace"`
	Success    bool      `json:"success"`
	Timestamp  time.Time `json:"timestamp"`
	DurationMs int64     `json:"durationMs"`
}
