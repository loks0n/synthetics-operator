package controllers

import (
	"context"
	"sync"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
	"github.com/loks0n/synthetics-operator/internal/heartbeat/lifecycle"
	"github.com/loks0n/synthetics-operator/internal/natsbus"
	"github.com/loks0n/synthetics-operator/internal/results"
)

// PingSubscriber is the slice of the NATS client this writer needs.
type PingSubscriber interface {
	SubscribeHeartbeatPings(ctx context.Context, handler func(context.Context, results.HeartbeatPing), opts ...natsbus.SubscribeOption) error
}

// HeartbeatPingWriter persists the most recent ping to each Heartbeat's
// status.
//
// Metrics are served from the metrics deployment's in-memory store, which is
// lost on restart. For an HTTPProbe that costs one interval — thirty seconds
// of blank graph. For a heartbeat with a one-day period it would mean a full
// day of reporting "pending", or worse, of alerting. Writing the last ping to
// the API server gives the spec resync something durable to reseed from.
//
// Every ping is not worth an etcd write: a hundred one-minute heartbeats
// would be a hundred writes a minute in perpetuity. Writes are therefore
// throttled per Heartbeat, with any change of outcome forced through
// immediately so a failure is never the write that got skipped.
type HeartbeatPingWriter struct {
	Client client.Client
	Bus    PingSubscriber
	Log    logr.Logger
	// MinWriteInterval floors the gap between status writes for one
	// Heartbeat. Zero picks a fraction of the heartbeat's own period.
	MinWriteInterval time.Duration

	mu        sync.Mutex
	lastWrite map[types.NamespacedName]time.Time
}

// NeedLeaderElection is true: a status write is idempotent, but having every
// replica issue one multiplies API server load by the replica count for no
// benefit.
func (*HeartbeatPingWriter) NeedLeaderElection() bool { return true }

func (w *HeartbeatPingWriter) Start(ctx context.Context) error {
	w.mu.Lock()
	if w.lastWrite == nil {
		w.lastWrite = map[types.NamespacedName]time.Time{}
	}
	w.mu.Unlock()
	return w.Bus.SubscribeHeartbeatPings(ctx, w.onPing)
}

func (w *HeartbeatPingWriter) onPing(ctx context.Context, ping results.HeartbeatPing) {
	name := types.NamespacedName{Namespace: ping.Namespace, Name: ping.Name}

	var beat syntheticsv1alpha1.Heartbeat
	if err := w.Client.Get(ctx, name, &beat); err != nil {
		if !apierrors.IsNotFound(err) {
			w.Log.Error(err, "reading Heartbeat for ping", "namespace", name.Namespace, "heartbeat", name.Name)
		}
		return
	}

	result := syntheticsv1alpha1.HeartbeatResultOK
	if ping.Failed {
		result = syntheticsv1alpha1.HeartbeatResultFailed
	}

	if !w.shouldWrite(name, ping.ReceivedAt, result != beat.Status.LastResult, w.throttleFor(&beat)) {
		return
	}

	original := beat.DeepCopy()
	received := metav1.NewTime(ping.ReceivedAt)
	beat.Status.LastPingTime = &received
	beat.Status.LastResult = result

	if err := w.Client.Status().Patch(ctx, &beat, client.MergeFrom(original)); err != nil {
		// Roll the throttle back so the next ping retries instead of waiting
		// out an interval it never actually recorded.
		w.forget(name)
		if !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			w.Log.Error(err, "patching Heartbeat status", "namespace", name.Namespace, "heartbeat", name.Name)
		}
	}
}

// throttleFor picks the minimum gap between writes for one Heartbeat. A
// quarter of the period keeps status fresh enough that a reseed after a
// restart is never off by more than a quarter of the alerting window.
func (w *HeartbeatPingWriter) throttleFor(beat *syntheticsv1alpha1.Heartbeat) time.Duration {
	return lifecycle.WriteInterval(beat.Spec.Period.Duration, w.MinWriteInterval)
}

// shouldWrite applies the throttle and records the decision. A changed
// outcome always writes: throttling a transition into or out of failure is
// exactly the write worth keeping.
func (w *HeartbeatPingWriter) shouldWrite(name types.NamespacedName, at time.Time, outcomeChanged bool, throttle time.Duration) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.lastWrite == nil {
		w.lastWrite = map[types.NamespacedName]time.Time{}
	}
	previous, seen := w.lastWrite[name]
	if !lifecycle.ShouldPersistPing(previous, seen, at, outcomeChanged, throttle) {
		return false
	}
	w.lastWrite[name] = at
	return true
}

func (w *HeartbeatPingWriter) forget(name types.NamespacedName) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.lastWrite, name)
}
