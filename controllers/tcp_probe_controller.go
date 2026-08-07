package controllers

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
	"github.com/loks0n/synthetics-operator/internal/natsbus"
	"github.com/loks0n/synthetics-operator/internal/results"
)

// TCPProbeReconciler publishes TCPProbe specs and registers their intervals.
// Execution is owned by the stateless prober deployment.
type TCPProbeReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Scheduler ProbeScheduler
	Publisher natsbus.Publisher
	Clock     func() time.Time
}

func (r *TCPProbeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var probe syntheticsv1alpha1.TCPProbe
	if err := r.Get(ctx, req.NamespacedName, &probe); err != nil {
		if apierrors.IsNotFound(err) {
			r.Scheduler.Unregister(results.KindTCPProbe, req.NamespacedName)
			return ctrl.Result{}, r.Publisher.PublishSpec(ctx, tombstone(results.KindTCPProbe, req.Namespace, req.Name))
		}
		return ctrl.Result{}, err
	}

	if !probe.DeletionTimestamp.IsZero() {
		r.Scheduler.Unregister(results.KindTCPProbe, req.NamespacedName)
		return ctrl.Result{}, r.Publisher.PublishSpec(ctx, tombstone(results.KindTCPProbe, probe.Namespace, probe.Name))
	}

	spec := tcpProbeSpecUpdate(&probe)
	if err := r.Publisher.PublishSpec(ctx, spec); err != nil {
		return ctrl.Result{}, err
	}

	original := probe.DeepCopy()
	now := metav1.NewTime(r.Clock())
	probe.Status.ObservedGeneration = probe.Generation
	setSuspendedCondition(&probe.Status.Conditions, probe.Generation, probe.Spec.Suspend, now)

	if probe.Spec.Suspend {
		r.Scheduler.Unregister(results.KindTCPProbe, req.NamespacedName)
	} else {
		r.Scheduler.Register(spec)
	}

	if probeStatusChanged(original.Status.ObservedGeneration, probe.Status.ObservedGeneration, original.Status.Conditions, probe.Status.Conditions) {
		if err := r.Status().Patch(ctx, &probe, client.MergeFrom(original)); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *TCPProbeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&syntheticsv1alpha1.TCPProbe{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}
