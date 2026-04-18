package results

import "time"

// Kind identifies the CRD type that produced the result.
type Kind string

const (
	KindK6Test         Kind = "K6Test"
	KindPlaywrightTest Kind = "PlaywrightTest"
)

// TestCase is a single test within a PlaywrightTest run, produced from the
// Playwright JSON reporter output.
type TestCase struct {
	Suite      string `json:"suite"`
	Test       string `json:"test"`
	Passed     bool   `json:"passed"`
	DurationMs int64  `json:"durationMs"`
}

// TestResult is the JSON payload published by the test-sidecar and consumed
// by the operator's NATS subscriber. Tests is populated only by kinds that
// emit per-test breakdowns (PlaywrightTest).
type TestResult struct {
	Kind       Kind       `json:"kind"`
	Name       string     `json:"name"`
	Namespace  string     `json:"namespace"`
	Success    bool       `json:"success"`
	Timestamp  time.Time  `json:"timestamp"`
	DurationMs int64      `json:"durationMs"`
	Tests      []TestCase `json:"tests,omitempty"`
}
