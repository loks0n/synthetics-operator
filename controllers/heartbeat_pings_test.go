package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
	"github.com/loks0n/synthetics-operator/internal/results"
)

var pingStart = time.Unix(1700000000, 0).UTC()

func newPingWriter(t *testing.T, beats ...*syntheticsv1alpha1.Heartbeat) (*HeartbeatPingWriter, client.Client) {
	t.Helper()
	builder := fakeclient.NewClientBuilder().WithScheme(unitScheme())
	for _, beat := range beats {
		builder = builder.WithStatusSubresource(beat).WithObjects(beat)
	}
	k8sClient := builder.Build()
	return &HeartbeatPingWriter{Client: k8sClient, Log: logr.Discard()}, k8sClient
}

func ping(name string, at time.Time, failed bool) results.HeartbeatPing {
	return results.HeartbeatPing{Namespace: "prod", Name: name, ReceivedAt: at, Failed: failed}
}

func heartbeatStatus(t *testing.T, k8sClient client.Client, name string) syntheticsv1alpha1.HeartbeatStatus {
	t.Helper()
	var beat syntheticsv1alpha1.Heartbeat
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Namespace: "prod", Name: name}, &beat); err != nil {
		t.Fatalf("loading Heartbeat: %v", err)
	}
	return beat.Status
}

func TestPingWriterRecordsFirstPing(t *testing.T) {
	writer, k8sClient := newPingWriter(t, newHeartbeat("db-backup", nil))
	writer.onPing(context.Background(), ping("db-backup", pingStart, false))

	status := heartbeatStatus(t, k8sClient, "db-backup")
	if status.LastPingTime == nil || !status.LastPingTime.Time.Equal(pingStart) {
		t.Fatalf("lastPingTime = %v, want %v", status.LastPingTime, pingStart)
	}
	if status.LastResult != syntheticsv1alpha1.HeartbeatResultOK {
		t.Errorf("lastResult = %q, want ok", status.LastResult)
	}
}

// A minute-period heartbeat pinging every minute must not mean an etcd write
// every minute, forever, per heartbeat.
func TestPingWriterThrottlesRepeatedPings(t *testing.T) {
	writer, k8sClient := newPingWriter(t, newHeartbeat("db-backup", nil))

	writer.onPing(context.Background(), ping("db-backup", pingStart, false))
	// Period is 1m, so the throttle is 15s. This one falls inside it.
	writer.onPing(context.Background(), ping("db-backup", pingStart.Add(10*time.Second), false))

	if got := heartbeatStatus(t, k8sClient, "db-backup").LastPingTime.Time; !got.Equal(pingStart) {
		t.Fatalf("lastPingTime = %v, want the throttled write to have been skipped at %v", got, pingStart)
	}
}

func TestPingWriterWritesOnceTheThrottleElapses(t *testing.T) {
	writer, k8sClient := newPingWriter(t, newHeartbeat("db-backup", nil))

	writer.onPing(context.Background(), ping("db-backup", pingStart, false))
	later := pingStart.Add(20 * time.Second)
	writer.onPing(context.Background(), ping("db-backup", later, false))

	if got := heartbeatStatus(t, k8sClient, "db-backup").LastPingTime.Time; !got.Equal(later) {
		t.Fatalf("lastPingTime = %v, want %v", got, later)
	}
}

// Throttling a transition into failure would drop precisely the write worth
// keeping, so a changed outcome must always be persisted immediately.
func TestPingWriterAlwaysWritesAnOutcomeChange(t *testing.T) {
	writer, k8sClient := newPingWriter(t, newHeartbeat("db-backup", nil))

	writer.onPing(context.Background(), ping("db-backup", pingStart, false))
	failedAt := pingStart.Add(time.Second) // well inside the throttle
	writer.onPing(context.Background(), ping("db-backup", failedAt, true))

	status := heartbeatStatus(t, k8sClient, "db-backup")
	if status.LastResult != syntheticsv1alpha1.HeartbeatResultFailed {
		t.Fatalf("lastResult = %q, want failed", status.LastResult)
	}
	if !status.LastPingTime.Time.Equal(failedAt) {
		t.Errorf("lastPingTime = %v, want %v", status.LastPingTime.Time, failedAt)
	}
}

func TestPingWriterRecoveryIsWrittenImmediately(t *testing.T) {
	beat := newHeartbeat("db-backup", func(h *syntheticsv1alpha1.Heartbeat) {
		h.Status.LastResult = syntheticsv1alpha1.HeartbeatResultFailed
	})
	writer, k8sClient := newPingWriter(t, beat)

	writer.onPing(context.Background(), ping("db-backup", pingStart, false))

	if got := heartbeatStatus(t, k8sClient, "db-backup").LastResult; got != syntheticsv1alpha1.HeartbeatResultOK {
		t.Fatalf("lastResult = %q, want ok", got)
	}
}

func TestPingWriterThrottleIsPerHeartbeat(t *testing.T) {
	writer, k8sClient := newPingWriter(t, newHeartbeat("one", nil), newHeartbeat("two", nil))

	writer.onPing(context.Background(), ping("one", pingStart, false))
	writer.onPing(context.Background(), ping("two", pingStart, false))

	for _, name := range []string{"one", "two"} {
		if heartbeatStatus(t, k8sClient, name).LastPingTime == nil {
			t.Errorf("%s was not written; the throttle is leaking across heartbeats", name)
		}
	}
}

// A ping for a Heartbeat that has since been deleted is normal during a
// rollout and must not error or panic.
func TestPingWriterIgnoresUnknownHeartbeat(t *testing.T) {
	writer, _ := newPingWriter(t)
	writer.onPing(context.Background(), ping("gone", pingStart, false))
}

// The throttle is bookkeeping, not truth: if the write failed, the next ping
// has to be allowed through rather than waiting out an interval that recorded
// nothing.
func TestPingWriterRetriesAfterAFailedWrite(t *testing.T) {
	writer, _ := newPingWriter(t)
	name := types.NamespacedName{Namespace: "prod", Name: "db-backup"}

	if !writer.shouldWrite(name, pingStart, false, time.Minute) {
		t.Fatal("first write should be allowed")
	}
	writer.forget(name)
	if !writer.shouldWrite(name, pingStart.Add(time.Second), false, time.Minute) {
		t.Fatal("after forget, the next ping must be allowed through")
	}
}

func TestPingWriterThrottleScalesWithPeriod(t *testing.T) {
	writer, _ := newPingWriter(t)
	daily := newHeartbeat("db-backup", func(h *syntheticsv1alpha1.Heartbeat) {
		h.Spec.Period = metav1.Duration{Duration: 24 * time.Hour}
	})
	if got, want := writer.throttleFor(daily), 6*time.Hour; got != want {
		t.Fatalf("throttleFor(24h period) = %v, want %v", got, want)
	}
}

func TestPingWriterNeedsLeaderElection(t *testing.T) {
	writer, _ := newPingWriter(t)
	if !writer.NeedLeaderElection() {
		t.Fatal("status writes should happen on the leader only")
	}
}
