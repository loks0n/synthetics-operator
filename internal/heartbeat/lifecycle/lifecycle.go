// Package lifecycle owns the pure rules for Heartbeat state: spec seeding,
// ping ingestion, suspension, and freshness evaluation.
package lifecycle

import "time"

// Result is the health classification for one Heartbeat at one instant.
type Result string

const (
	ResultOK              Result = "ok"
	ResultPending         Result = "pending"
	ResultMissed          Result = "missed"
	ResultReportedFailure Result = "reported_failure"
)

// State is the in-memory lifecycle state for one Heartbeat.
type State struct {
	PeriodSeconds   float64
	GraceSeconds    float64
	LastPingUnix    float64
	ReportedFailure bool
	ExitCode        float64
	Suspended       bool
}

// Seed is durable last-ping state replayed from the Kubernetes API.
type Seed struct {
	LastPingUnix float64
	Failed       bool
}

// ApplySpec records the health contract. A seed only applies when no live
// state exists, so an old status value cannot overwrite a ping already seen.
func ApplySpec(state State, existing bool, periodSeconds, graceSeconds float64, suspend bool, seed Seed) State {
	state.PeriodSeconds = periodSeconds
	state.GraceSeconds = graceSeconds
	state.Suspended = suspend
	if !existing && seed.LastPingUnix > 0 {
		state.LastPingUnix = seed.LastPingUnix
		state.ReportedFailure = seed.Failed
	}
	return state
}

// RecordPing applies one received ping. Older pings never move time backwards.
func RecordPing(state State, failed bool, exitCode int, receivedUnix float64) State {
	if receivedUnix >= state.LastPingUnix {
		state.LastPingUnix = receivedUnix
		state.ReportedFailure = failed
		state.ExitCode = float64(exitCode)
	}
	return state
}

// Deadline is the instant by which the next ping must arrive.
func (s State) Deadline() float64 {
	grace := s.GraceSeconds
	if grace <= 0 {
		grace = s.PeriodSeconds
	}
	return s.LastPingUnix + s.PeriodSeconds + grace
}

// Evaluate classifies the Heartbeat as of now.
func (s State) Evaluate(now time.Time) Result {
	switch {
	case s.LastPingUnix <= 0:
		return ResultPending
	case float64(now.Unix()) > s.Deadline():
		return ResultMissed
	case s.ReportedFailure:
		return ResultReportedFailure
	default:
		return ResultOK
	}
}
