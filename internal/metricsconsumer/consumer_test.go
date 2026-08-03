package metricsconsumer

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

	"github.com/loks0n/synthetics-operator/internal/metrics"
	"github.com/loks0n/synthetics-operator/internal/results"
)

func newTestConsumer(t *testing.T) *Consumer {
	t.Helper()
	store, err := metrics.NewStore()
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return &Consumer{Log: logr.Discard(), Store: store}
}

func probeResult() results.ProbeResult {
	return results.ProbeResult{
		Kind:      results.KindHTTPProbe,
		Name:      "probe",
		Namespace: "default",
		Result:    "ok",
		Timestamp: time.Unix(1700000000, 0),
	}
}

func probeSpec() results.SpecUpdate {
	return results.SpecUpdate{
		Kind:         results.KindHTTPProbe,
		Name:         "probe",
		Namespace:    "default",
		MetricLabels: map[string]string{"team": "payments"},
	}
}

// A result that arrives before its spec would be recorded without the user's
// metricLabels, which Prometheus reads as a separate series from the labelled
// one its peers export — so it is held back instead.
func TestProbeResultDroppedUntilSpecArrives(t *testing.T) {
	c := newTestConsumer(t)
	name := types.NamespacedName{Namespace: "default", Name: "probe"}

	c.onProbeResult(context.Background(), probeResult())
	if _, ok := c.Store.Snapshot(name); ok {
		t.Fatal("result recorded before its spec arrived")
	}

	c.onSpec(context.Background(), probeSpec())
	c.onProbeResult(context.Background(), probeResult())
	if _, ok := c.Store.Snapshot(name); !ok {
		t.Fatal("result should be recorded once the spec is known")
	}
}

// A tombstone must also stop later results from re-creating the series.
func TestProbeResultDroppedAfterTombstone(t *testing.T) {
	c := newTestConsumer(t)
	name := types.NamespacedName{Namespace: "default", Name: "probe"}

	c.onSpec(context.Background(), probeSpec())
	c.onProbeResult(context.Background(), probeResult())
	if _, ok := c.Store.Snapshot(name); !ok {
		t.Fatal("precondition: result should be recorded")
	}

	tombstone := probeSpec()
	tombstone.Deleted = true
	c.onSpec(context.Background(), tombstone)

	c.onProbeResult(context.Background(), probeResult())
	if _, ok := c.Store.Snapshot(name); ok {
		t.Fatal("result after tombstone should not resurrect the series")
	}
}

func TestTestResultDroppedUntilSpecArrives(t *testing.T) {
	c := newTestConsumer(t)
	name := types.NamespacedName{Namespace: "default", Name: "load"}

	res := results.TestResult{
		Kind:      results.KindK6Test,
		Name:      "load",
		Namespace: "default",
		Success:   true,
		Timestamp: time.Unix(1700000000, 0),
	}

	c.onTestResult(context.Background(), res)
	if _, ok := c.Store.SnapshotTest(name); ok {
		t.Fatal("test result recorded before its spec arrived")
	}

	c.onSpec(context.Background(), results.SpecUpdate{
		Kind:      results.KindK6Test,
		Name:      "load",
		Namespace: "default",
	})
	c.onTestResult(context.Background(), res)
	if _, ok := c.Store.SnapshotTest(name); !ok {
		t.Fatal("test result should be recorded once the spec is known")
	}
}
