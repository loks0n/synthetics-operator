package metrics

import (
	"context"
	"time"

	"github.com/loks0n/synthetics-operator/internal/heartbeat/lifecycle"
	"go.opentelemetry.io/otel/attribute"
	apimetric "go.opentelemetry.io/otel/metric"
	"k8s.io/apimachinery/pkg/types"
)

// HeartbeatKind is the kind string heartbeats are keyed and labelled with.
const HeartbeatKind = "Heartbeat"

// HeartbeatState is what the store remembers about one Heartbeat.
type HeartbeatState = lifecycle.State

func heartbeatResult(result lifecycle.Result) Result {
	return Result(result)
}

func (s *Store) clock() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// SetHeartbeatSpec records a Heartbeat's health contract from the spec
// stream. A suspended heartbeat produces no series at all — so an alert on
// `synthetics_heartbeat == 0` can't fire for one, and a dashboard doesn't
// show a row that isn't being evaluated — but its state is retained so the
// last ping survives the suspension and pings arriving during it have
// somewhere to land. See HeartbeatState.Suspended.
//
// seedPingUnix and seedFailed replay the CR's status. They only apply when
// the store has no live state, so a resync can't overwrite a ping this
// process has already seen with an older one from status.
func (s *Store) SetHeartbeatSpec(name types.NamespacedName, periodSeconds, graceSeconds float64, suspend bool, seedPingUnix float64, seedFailed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := crKey{kind: HeartbeatKind, name: name}

	state, existing := s.heartbeats[key]
	s.heartbeats[key] = lifecycle.ApplySpec(state, existing, periodSeconds, graceSeconds, suspend, lifecycle.Seed{
		LastPingUnix: seedPingUnix,
		Failed:       seedFailed,
	})
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

	now := s.clock()

	s.mu.Lock()
	key := crKey{kind: HeartbeatKind, name: name}
	state := s.heartbeats[key]
	previous := heartbeatResult(state.Evaluate(now))
	state = lifecycle.RecordPing(state, failed, exitCode, receivedUnix)
	s.heartbeats[key] = state
	next := heartbeatResult(state.Evaluate(now))
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
	if state.Suspended {
		return
	}

	attrs := []attribute.KeyValue{
		attribute.String("name", name.Name),
		attribute.String("namespace", name.Namespace),
		attribute.String("kind", HeartbeatKind),
	}
	attrs = append(attrs, s.userLabelsLocked(HeartbeatKind, name)...)

	result := heartbeatResult(state.Evaluate(now))
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
