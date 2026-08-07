package metrics

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	apimetric "go.opentelemetry.io/otel/metric"
	"k8s.io/apimachinery/pkg/types"
)

// HeartbeatKind is the kind string heartbeats are keyed and labelled with.
const HeartbeatKind = "Heartbeat"

// HeartbeatState is what the store remembers about one Heartbeat: its health
// contract, and what the last ping said.
//
// Unlike every other kind, the stored state does not include an outcome. A
// heartbeat's health is a function of the current time — a ping that was
// perfectly healthy a minute ago is a missed deadline now — so the outcome is
// derived on every scrape by evaluate rather than written on ingest.
type HeartbeatState struct {
	// PeriodSeconds and GraceSeconds come from the spec stream.
	PeriodSeconds float64
	GraceSeconds  float64
	// LastPingUnix is 0 when no ping has ever arrived.
	LastPingUnix float64
	// ReportedFailure records that the last ping arrived via /fail or with a
	// non-zero exit code. A heartbeat that keeps checking in to say the job
	// failed is unhealthy, even though it is perfectly fresh.
	ReportedFailure bool
	ExitCode        float64
}

// deadline is the instant by which the next ping must arrive. Grace defaults
// to Period, mirroring the admission webhook, so a spec that predates the
// grace field still gets a sane window instead of a zero one.
func (h HeartbeatState) deadline() float64 {
	grace := h.GraceSeconds
	if grace <= 0 {
		grace = h.PeriodSeconds
	}
	return h.LastPingUnix + h.PeriodSeconds + grace
}

// evaluate classifies the heartbeat as of now.
//
// pending is deliberately distinct from missed. A Heartbeat that has never
// been pinged is usually one whose job hasn't been pointed at it yet, and
// paging someone for that on every fresh deploy trains them to ignore the
// alert. Better Stack draws the same line.
func (h HeartbeatState) evaluate(now time.Time) Result {
	switch {
	case h.LastPingUnix <= 0:
		return ResultPending
	case float64(now.Unix()) > h.deadline():
		return ResultMissed
	case h.ReportedFailure:
		return ResultReported
	default:
		return ResultOK
	}
}

func (s *Store) clock() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// SetHeartbeatSpec records a Heartbeat's health contract from the spec
// stream. Suspended heartbeats are dropped outright rather than stored with a
// flag: a suspended heartbeat should produce no series at all, so an alert on
// `synthetics_heartbeat == 0` can't fire for one, and a dashboard doesn't
// show a row that isn't being evaluated.
//
// seedPingUnix and seedFailed replay the CR's status. They only apply when
// the store has no live state, so a resync can't overwrite a ping this
// process has already seen with an older one from status.
func (s *Store) SetHeartbeatSpec(name types.NamespacedName, periodSeconds, graceSeconds float64, suspend bool, seedPingUnix float64, seedFailed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := crKey{kind: HeartbeatKind, name: name}

	if suspend {
		delete(s.heartbeats, key)
		return
	}

	state, existing := s.heartbeats[key]
	state.PeriodSeconds = periodSeconds
	state.GraceSeconds = graceSeconds
	if !existing && seedPingUnix > 0 {
		state.LastPingUnix = seedPingUnix
		state.ReportedFailure = seedFailed
	}
	s.heartbeats[key] = state
}

// RecordHeartbeatPing ingests a ping. A ping for a Heartbeat whose spec
// hasn't landed yet still creates state: the spec is moments behind on the
// same stream, and dropping the ping would lose the only evidence that the
// job ran.
func (s *Store) RecordHeartbeatPing(ctx context.Context, name types.NamespacedName, failed bool, exitCode int, receivedUnix float64) {
	outcome := "ok"
	if failed {
		outcome = "failed"
	}
	s.instr.heartbeatReceivedTotal.Add(ctx, 1, apimetric.WithAttributes(
		attribute.String("name", name.Name),
		attribute.String("namespace", name.Namespace),
		attribute.String("outcome", outcome),
	))

	s.mu.Lock()
	key := crKey{kind: HeartbeatKind, name: name}
	state := s.heartbeats[key]
	previous := state.evaluate(s.clock())
	// Out-of-order delivery is possible on a reconnect; an older ping must
	// not walk the last-received timestamp backwards.
	if receivedUnix >= state.LastPingUnix {
		state.LastPingUnix = receivedUnix
		state.ReportedFailure = failed
		state.ExitCode = float64(exitCode)
	}
	s.heartbeats[key] = state
	next := state.evaluate(s.clock())
	callback := s.OnProbeTransition
	s.mu.Unlock()

	// Only ingest-time transitions fire an Event — ok to reported_failure and
	// back. Crossing into `missed` happens on a clock, not on a message, so
	// nothing here observes it; that transition is Prometheus's to alert on.
	if callback != nil && previous != next {
		callback(name, HeartbeatKind, previous, next)
	}
}

// SnapshotHeartbeat returns the stored state for one Heartbeat.
func (s *Store) SnapshotHeartbeat(name types.NamespacedName) (HeartbeatState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.heartbeats[crKey{kind: HeartbeatKind, name: name}]
	return state, ok
}

// observeHeartbeat emits one heartbeat's series. Caller must hold s.mu.RLock.
func (s *Store) observeHeartbeat(observer apimetric.Observer, name types.NamespacedName, state HeartbeatState, instr instruments, now time.Time) {
	attrs := []attribute.KeyValue{
		attribute.String("name", name.Name),
		attribute.String("namespace", name.Namespace),
		attribute.String("kind", HeartbeatKind),
	}
	attrs = append(attrs, s.userLabelsLocked(HeartbeatKind, name)...)

	result := state.evaluate(now)
	observer.ObserveFloat64(instr.heartbeatGauge, result.successValue(), apimetric.WithAttributes(attrs...))
	observer.ObserveFloat64(instr.heartbeatResultInfo, 1, apimetric.WithAttributes(
		append(attrs, attribute.String("result", string(result)))...))
	observer.ObserveFloat64(instr.heartbeatLastReceived, state.LastPingUnix, apimetric.WithAttributes(attrs...))
	observer.ObserveFloat64(instr.heartbeatPeriodSeconds, state.PeriodSeconds, apimetric.WithAttributes(attrs...))
	observer.ObserveFloat64(instr.heartbeatGraceSeconds, state.GraceSeconds, apimetric.WithAttributes(attrs...))
	observer.ObserveFloat64(instr.heartbeatExitCodeGauge, state.ExitCode, apimetric.WithAttributes(attrs...))

	if result != ResultOK {
		if dep, ok := s.findUnhealthyDep(HeartbeatKind, name); ok {
			observer.ObserveFloat64(instr.heartbeatSuppressedGauge, 1, apimetric.WithAttributes(
				append(attrs,
					attribute.String("unhealthy_dependency", dep.Name),
					attribute.String("unhealthy_dependency_kind", string(dep.Kind)),
				)...))
		}
	}
}
