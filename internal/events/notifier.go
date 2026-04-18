// Package events emits Kubernetes events on probe and test outcome
// transitions. `kubectl describe httpprobe foo` then shows a timeline of
// "Probe became active" / "Probe failed" markers without the user having to
// scrape /metrics.
package events

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
	internalmetrics "github.com/loks0n/synthetics-operator/internal/metrics"
)

// Event reason strings. Kubernetes convention: CamelCase, short.
const (
	ReasonProbeActive = "ProbeActive"
	ReasonProbeFailed = "ProbeFailed"
	ReasonTestActive  = "TestActive"
	ReasonTestFailed  = "TestFailed"
)

const getTimeout = 5 * time.Second

// Notifier holds the dependencies needed to fetch CRs and emit events
// against them. Methods on Notifier are safe for concurrent use; the
// underlying client and recorder both are.
type Notifier struct {
	client   client.Client
	recorder record.EventRecorder
}

func New(c client.Client, recorder record.EventRecorder) *Notifier {
	return &Notifier{client: c, recorder: recorder}
}

// OnProbeTransition emits an event against the HTTPProbe or DNSProbe whose
// Result flipped. Silent on missing CRs (already deleted) or unknown kinds.
func (n *Notifier) OnProbeTransition(name types.NamespacedName, kind string, prev, next internalmetrics.Result) {
	var obj client.Object
	switch kind {
	case "HTTPProbe":
		obj = &syntheticsv1alpha1.HTTPProbe{}
	case "DNSProbe":
		obj = &syntheticsv1alpha1.DNSProbe{}
	default:
		return
	}
	n.emit(obj, name, ReasonProbeActive, ReasonProbeFailed, prev, next)
}

// OnTestTransition emits an event against the K6Test or PlaywrightTest whose
// Result flipped. Silent on missing CRs (already deleted) or unknown kinds.
func (n *Notifier) OnTestTransition(name types.NamespacedName, kind string, prev, next internalmetrics.Result) {
	var obj client.Object
	switch kind {
	case "K6Test":
		obj = &syntheticsv1alpha1.K6Test{}
	case "PlaywrightTest":
		obj = &syntheticsv1alpha1.PlaywrightTest{}
	default:
		return
	}
	n.emit(obj, name, ReasonTestActive, ReasonTestFailed, prev, next)
}

func (n *Notifier) emit(obj client.Object, name types.NamespacedName, activeReason, failedReason string, prev, next internalmetrics.Result) {
	ctx, cancel := context.WithTimeout(context.Background(), getTimeout)
	defer cancel()
	if err := n.client.Get(ctx, name, obj); err != nil {
		// NotFound means the CR was deleted between update and notification —
		// event emission is best-effort, so drop silently. Other errors are
		// rare (auth, timeout) and also not worth failing on.
		_ = apierrors.IsNotFound(err)
		return
	}

	switch next {
	case internalmetrics.ResultOK:
		msg := "run succeeded"
		if prev != "" {
			msg = fmt.Sprintf("recovered from %s", prev)
		}
		n.recorder.Event(obj, corev1.EventTypeNormal, activeReason, msg)
	default:
		msg := fmt.Sprintf("run failed: %s", next)
		n.recorder.Event(obj, corev1.EventTypeWarning, failedReason, msg)
	}
}

