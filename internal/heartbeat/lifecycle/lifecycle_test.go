package lifecycle

import (
	"testing"
	"time"
)

var base = time.Unix(1700000000, 0)

func TestEvaluatePendingUntilFirstPing(t *testing.T) {
	state := State{PeriodSeconds: 60, GraceSeconds: 180}
	if got := state.Evaluate(base); got != ResultPending {
		t.Fatalf("Evaluate = %s, want %s", got, ResultPending)
	}
}

func TestEvaluateFreshMissedAndReportedFailure(t *testing.T) {
	state := State{PeriodSeconds: 60, GraceSeconds: 180}
	state = RecordPing(state, false, 0, float64(base.Unix()))
	if got := state.Evaluate(base.Add(4 * time.Minute)); got != ResultOK {
		t.Fatalf("fresh Evaluate = %s, want %s", got, ResultOK)
	}
	if got := state.Evaluate(base.Add(4*time.Minute + time.Second)); got != ResultMissed {
		t.Fatalf("late Evaluate = %s, want %s", got, ResultMissed)
	}

	state = RecordPing(state, true, 2, float64(base.Add(time.Minute).Unix()))
	if got := state.Evaluate(base.Add(2 * time.Minute)); got != ResultReportedFailure {
		t.Fatalf("failed Evaluate = %s, want %s", got, ResultReportedFailure)
	}
}

func TestGraceDefaultsToPeriod(t *testing.T) {
	state := State{PeriodSeconds: 60}
	state = RecordPing(state, false, 0, float64(base.Unix()))
	if got := state.Evaluate(base.Add(2 * time.Minute)); got != ResultOK {
		t.Fatalf("at deadline Evaluate = %s, want %s", got, ResultOK)
	}
	if got := state.Evaluate(base.Add(2*time.Minute + time.Second)); got != ResultMissed {
		t.Fatalf("past deadline Evaluate = %s, want %s", got, ResultMissed)
	}
}

func TestApplySpecSeedsOnlyEmptyState(t *testing.T) {
	state := ApplySpec(State{}, false, 86400, 3600, false, Seed{LastPingUnix: float64(base.Unix()), Failed: true})
	if state.LastPingUnix != float64(base.Unix()) || !state.ReportedFailure {
		t.Fatalf("seeded state = %+v", state)
	}

	live := RecordPing(state, false, 0, float64(base.Add(time.Hour).Unix()))
	resynced := ApplySpec(live, true, 86400, 3600, false, Seed{LastPingUnix: float64(base.Add(-time.Hour).Unix()), Failed: true})
	if resynced.LastPingUnix != live.LastPingUnix || resynced.ReportedFailure != live.ReportedFailure {
		t.Fatalf("resync overwrote live ping: got %+v, want %+v", resynced, live)
	}
}

func TestRecordPingIgnoresOlderPings(t *testing.T) {
	state := RecordPing(State{}, false, 0, float64(base.Unix()))
	state = RecordPing(state, true, 9, float64(base.Add(-time.Minute).Unix()))
	if state.LastPingUnix != float64(base.Unix()) || state.ReportedFailure || state.ExitCode != 0 {
		t.Fatalf("older ping changed state: %+v", state)
	}
}

func TestWriteInterval(t *testing.T) {
	if got := WriteInterval(time.Minute, 0); got != 15*time.Second {
		t.Fatalf("WriteInterval = %s, want 15s", got)
	}
	if got := WriteInterval(time.Minute, 5*time.Second); got != 5*time.Second {
		t.Fatalf("WriteInterval override = %s, want 5s", got)
	}
	if got := WriteInterval(0, 0); got != 15*time.Second {
		t.Fatalf("WriteInterval fallback = %s, want 15s", got)
	}
}

func TestShouldPersistPing(t *testing.T) {
	if !ShouldPersistPing(time.Time{}, false, base, false, time.Minute) {
		t.Fatal("first ping should persist")
	}
	if !ShouldPersistPing(base, true, base.Add(time.Second), true, time.Minute) {
		t.Fatal("outcome change should persist")
	}
	if ShouldPersistPing(base, true, base.Add(time.Second), false, time.Minute) {
		t.Fatal("unchanged ping inside interval should not persist")
	}
	if !ShouldPersistPing(base, true, base.Add(time.Minute), false, time.Minute) {
		t.Fatal("unchanged ping after interval should persist")
	}
}
