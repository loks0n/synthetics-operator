package metrics

import (
	"strings"
	"testing"
	"time"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
)

// baseTime is an arbitrary fixed instant; heartbeat health is entirely
// relative, so the absolute value only has to stay constant across a test.
var baseTime = time.Unix(1700000000, 0).UTC()

// heartbeatStore returns a store whose clock the test controls, plus a setter
// for advancing it.
func heartbeatStore(t *testing.T) (*Store, func(time.Duration)) {
	t.Helper()
	store, err := NewStore()
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	now := baseTime
	store.Now = func() time.Time { return now }
	return store, func(d time.Duration) { now = now.Add(d) }
}

func mustContain(t *testing.T, text string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("metrics output missing %q\n%s", want, text)
		}
	}
}

var backup = types.NamespacedName{Namespace: "prod", Name: "db-backup"}

// assertNoHeartbeatGauges checks the point-in-time series are gone.
// synthetics_heartbeat_received_total is deliberately excluded: it is a
// counter, and a counter that vanishes reads downstream as a reset.
func assertNoHeartbeatGauges(t *testing.T, text string) {
	t.Helper()
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(line, "synthetics_heartbeat_received_total") {
			continue
		}
		if strings.HasPrefix(line, "synthetics_heartbeat") && strings.Contains(line, `name="db-backup"`) {
			t.Errorf("expected no heartbeat gauge series, got: %s", line)
		}
	}
}

// A Heartbeat that has never been pinged is pending, not missed. Alerting on
// a heartbeat nobody has wired up yet is noise, so the two must stay
// distinguishable on the result label.
func TestHeartbeatWithoutAPingIsPending(t *testing.T) {
	store, _ := heartbeatStore(t)
	store.SetHeartbeatSpec(backup, 60, 180, false, 0, false)

	mustContain(t, scrape(t, store),
		`synthetics_heartbeat{kind="Heartbeat",name="db-backup",namespace="prod"} 0`,
		`synthetics_heartbeat_result_info{kind="Heartbeat",name="db-backup",namespace="prod",result="pending"} 1`,
	)
}

func TestHeartbeatIsHealthyInsideTheWindow(t *testing.T) {
	store, advance := heartbeatStore(t)
	store.SetHeartbeatSpec(backup, 60, 180, false, 0, false)
	store.RecordHeartbeatPing(t.Context(), backup, false, 0, float64(baseTime.Unix()))

	// period 60 + grace 180 = 240s of headroom.
	advance(239 * time.Second)

	mustContain(t, scrape(t, store),
		`synthetics_heartbeat{kind="Heartbeat",name="db-backup",namespace="prod"} 1`,
		`result="ok"`,
		`synthetics_heartbeat_expected_period_seconds{kind="Heartbeat",name="db-backup",namespace="prod"} 60`,
		`synthetics_heartbeat_grace_seconds{kind="Heartbeat",name="db-backup",namespace="prod"} 180`,
	)
}

// Health is a function of the clock, not of any message. Nothing arrives to
// mark a heartbeat late, so the scrape itself has to notice.
func TestHeartbeatGoesMissedWhenTheDeadlinePasses(t *testing.T) {
	store, advance := heartbeatStore(t)
	store.SetHeartbeatSpec(backup, 60, 180, false, 0, false)
	store.RecordHeartbeatPing(t.Context(), backup, false, 0, float64(baseTime.Unix()))

	advance(241 * time.Second)

	mustContain(t, scrape(t, store),
		`synthetics_heartbeat{kind="Heartbeat",name="db-backup",namespace="prod"} 0`,
		`result="missed"`,
	)
}

// Zero grace must not mean a zero window — that would report every heartbeat
// missed the instant its period elapsed.
func TestZeroGraceFallsBackToThePeriod(t *testing.T) {
	store, advance := heartbeatStore(t)
	store.SetHeartbeatSpec(backup, 60, 0, false, 0, false)
	store.RecordHeartbeatPing(t.Context(), backup, false, 0, float64(baseTime.Unix()))

	advance(119 * time.Second)
	mustContain(t, scrape(t, store), `synthetics_heartbeat{kind="Heartbeat",name="db-backup",namespace="prod"} 1`)

	advance(2 * time.Second)
	mustContain(t, scrape(t, store), `synthetics_heartbeat{kind="Heartbeat",name="db-backup",namespace="prod"} 0`)
}

// A job that checks in punctually to report its own failure is unhealthy even
// though the heartbeat itself is perfectly fresh.
func TestReportedFailureIsUnhealthyDespiteFreshness(t *testing.T) {
	store, _ := heartbeatStore(t)
	store.SetHeartbeatSpec(backup, 60, 180, false, 0, false)
	store.RecordHeartbeatPing(t.Context(), backup, true, 2, float64(baseTime.Unix()))

	mustContain(t, scrape(t, store),
		`synthetics_heartbeat{kind="Heartbeat",name="db-backup",namespace="prod"} 0`,
		`result="reported_failure"`,
		`synthetics_heartbeat_last_exit_code{kind="Heartbeat",name="db-backup",namespace="prod"} 2`,
	)
}

func TestSuccessfulPingClearsAPriorFailure(t *testing.T) {
	store, advance := heartbeatStore(t)
	store.SetHeartbeatSpec(backup, 60, 180, false, 0, false)
	store.RecordHeartbeatPing(t.Context(), backup, true, 1, float64(baseTime.Unix()))
	advance(10 * time.Second)
	store.RecordHeartbeatPing(t.Context(), backup, false, 0, float64(baseTime.Add(10*time.Second).Unix()))

	mustContain(t, scrape(t, store),
		`synthetics_heartbeat{kind="Heartbeat",name="db-backup",namespace="prod"} 1`,
		`result="ok"`,
	)
}

// A suspended heartbeat must produce no series at all, so that an alert on
// `synthetics_heartbeat == 0` can't fire for one during maintenance.
func TestSuspendedHeartbeatEmitsNothing(t *testing.T) {
	store, _ := heartbeatStore(t)
	store.SetHeartbeatSpec(backup, 60, 180, false, 0, false)
	store.RecordHeartbeatPing(t.Context(), backup, false, 0, float64(baseTime.Unix()))
	store.SetHeartbeatSpec(backup, 60, 180, true, 0, false)

	assertNoHeartbeatGauges(t, scrape(t, store))
}

// The seed replays CR status after a restart. Without it a daily backup job
// would read as pending — and alert — for a full day.
func TestSpecSeedsLastPingOnAColdStore(t *testing.T) {
	store, _ := heartbeatStore(t)
	store.SetHeartbeatSpec(backup, 86400, 3600, false, float64(baseTime.Add(-time.Hour).Unix()), false)

	mustContain(t, scrape(t, store),
		`synthetics_heartbeat{kind="Heartbeat",name="db-backup",namespace="prod"} 1`,
		`result="ok"`,
	)
}

// A resync replays whatever status said, which is older than a ping this
// process has already ingested. The live value has to win.
func TestSpecSeedDoesNotOverwriteALivePing(t *testing.T) {
	store, _ := heartbeatStore(t)
	store.SetHeartbeatSpec(backup, 60, 180, false, 0, false)
	store.RecordHeartbeatPing(t.Context(), backup, false, 0, float64(baseTime.Unix()))

	stale := float64(baseTime.Add(-time.Hour).Unix())
	store.SetHeartbeatSpec(backup, 60, 180, false, stale, true)

	state, ok := store.SnapshotHeartbeat(backup)
	if !ok {
		t.Fatal("expected heartbeat state to exist")
	}
	if state.LastPingUnix != float64(baseTime.Unix()) {
		t.Fatalf("LastPingUnix = %v, want the live ping %v", state.LastPingUnix, baseTime.Unix())
	}
	if state.ReportedFailure {
		t.Fatal("stale seed overwrote the live ping's outcome")
	}
}

// Reconnects can redeliver out of order; an older ping must not walk the
// last-received timestamp backwards.
func TestOutOfOrderPingIsIgnored(t *testing.T) {
	store, _ := heartbeatStore(t)
	store.SetHeartbeatSpec(backup, 60, 180, false, 0, false)
	store.RecordHeartbeatPing(t.Context(), backup, false, 0, float64(baseTime.Unix()))
	store.RecordHeartbeatPing(t.Context(), backup, true, 9, float64(baseTime.Add(-time.Minute).Unix()))

	state, _ := store.SnapshotHeartbeat(backup)
	if state.LastPingUnix != float64(baseTime.Unix()) {
		t.Fatalf("LastPingUnix = %v, want %v", state.LastPingUnix, baseTime.Unix())
	}
	if state.ReportedFailure {
		t.Fatal("an out-of-order ping must not change the recorded outcome")
	}
}

func TestHeartbeatReceivedCounterSplitsByOutcome(t *testing.T) {
	store, _ := heartbeatStore(t)
	store.SetHeartbeatSpec(backup, 60, 180, false, 0, false)
	store.RecordHeartbeatPing(t.Context(), backup, false, 0, float64(baseTime.Unix()))
	store.RecordHeartbeatPing(t.Context(), backup, true, 1, float64(baseTime.Add(time.Second).Unix()))

	mustContain(t, scrape(t, store),
		`synthetics_heartbeat_received_total{name="db-backup",namespace="prod",outcome="ok"} 1`,
		`synthetics_heartbeat_received_total{name="db-backup",namespace="prod",outcome="failed"} 1`,
	)
}

func TestHeartbeatCarriesUserMetricLabels(t *testing.T) {
	store, _ := heartbeatStore(t)
	store.SetMetricLabels(HeartbeatKind, backup, map[string]string{"team": "infra"})
	store.SetHeartbeatSpec(backup, 60, 180, false, 0, false)
	store.RecordHeartbeatPing(t.Context(), backup, false, 0, float64(baseTime.Unix()))

	mustContain(t, scrape(t, store), `team="infra"`)
}

func TestHeartbeatDeleteRemovesSeries(t *testing.T) {
	store, _ := heartbeatStore(t)
	store.SetHeartbeatSpec(backup, 60, 180, false, 0, false)
	store.RecordHeartbeatPing(t.Context(), backup, false, 0, float64(baseTime.Unix()))
	store.Delete(HeartbeatKind, backup)

	assertNoHeartbeatGauges(t, scrape(t, store))
}

// Suppression exists so a heartbeat that stopped because its upstream died
// doesn't page anyone separately.
func TestUnhealthyHeartbeatIsSuppressedByAFailingDependency(t *testing.T) {
	store, advance := heartbeatStore(t)
	upstream := types.NamespacedName{Namespace: "prod", Name: "api"}
	store.Upsert(upstream, ProbeState{Kind: "HTTPProbe", Result: ResultConnectRefused})
	store.SetDepends(HeartbeatKind, backup, []syntheticsv1alpha1.DependencyRef{
		{Kind: syntheticsv1alpha1.DependencyKindHTTPProbe, Name: "api"},
	})
	store.SetHeartbeatSpec(backup, 60, 60, false, 0, false)
	store.RecordHeartbeatPing(t.Context(), backup, false, 0, float64(baseTime.Unix()))
	advance(5 * time.Minute)

	mustContain(t, scrape(t, store), `synthetics_heartbeat_suppressed{`, `unhealthy_dependency="api"`)
}

// The reverse direction: other kinds must be able to depend on a Heartbeat.
func TestFailingHeartbeatSuppressesADependentProbe(t *testing.T) {
	store, advance := heartbeatStore(t)
	dependent := types.NamespacedName{Namespace: "prod", Name: "api"}
	store.SetHeartbeatSpec(backup, 60, 60, false, 0, false)
	store.RecordHeartbeatPing(t.Context(), backup, false, 0, float64(baseTime.Unix()))
	store.SetDepends("HTTPProbe", dependent, []syntheticsv1alpha1.DependencyRef{
		{Kind: syntheticsv1alpha1.DependencyKindHeartbeat, Name: "db-backup"},
	})
	store.Upsert(dependent, ProbeState{Kind: "HTTPProbe", Result: ResultConnectRefused})
	advance(5 * time.Minute)

	mustContain(t, scrape(t, store),
		`synthetics_probe_suppressed{`,
		`unhealthy_dependency="db-backup"`,
		`unhealthy_dependency_kind="Heartbeat"`,
	)
}

func TestHeartbeatTransitionCallbackFiresOnOutcomeChange(t *testing.T) {
	store, _ := heartbeatStore(t)
	type transition struct{ previous, next Result }
	var seen []transition
	store.OnProbeTransition = func(_ types.NamespacedName, kind string, previous, next Result) {
		if kind == HeartbeatKind {
			seen = append(seen, transition{previous, next})
		}
	}

	store.SetHeartbeatSpec(backup, 60, 180, false, 0, false)
	store.RecordHeartbeatPing(t.Context(), backup, false, 0, float64(baseTime.Unix()))
	store.RecordHeartbeatPing(t.Context(), backup, true, 1, float64(baseTime.Add(time.Second).Unix()))

	want := []transition{
		{ResultPending, ResultOK},
		{ResultOK, ResultReported},
	}
	if len(seen) != len(want) {
		t.Fatalf("transitions = %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("transition %d = %v, want %v", i, seen[i], want[i])
		}
	}
}
