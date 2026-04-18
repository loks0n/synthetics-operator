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
	internalmetrics "github.com/loks0n/synthetics-operator/internal/metrics"
	internalprobes "github.com/loks0n/synthetics-operator/internal/probes"
)

type HTTPProbeReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	Scheduler    ProbeScheduler
	HTTPExecutor internalprobes.Executor
	Metrics      *internalmetrics.Store
	Clock        func() time.Time
}

func (r *HTTPProbeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var probe syntheticsv1alpha1.HTTPProbe
	kind := string(syntheticsv1alpha1.DependencyKindHTTPProbe)
	if err := r.Get(ctx, req.NamespacedName, &probe); err != nil {
		if apierrors.IsNotFound(err) {
			r.Scheduler.Unregister(req.NamespacedName)
			r.Metrics.Delete(req.NamespacedName)
			r.Metrics.ClearDepends(kind, req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !probe.DeletionTimestamp.IsZero() {
		r.Scheduler.Unregister(req.NamespacedName)
		r.Metrics.Delete(req.NamespacedName)
		r.Metrics.ClearDepends(kind, req.NamespacedName)
		return ctrl.Result{}, nil
	}

	r.Metrics.SetDepends(kind, req.NamespacedName, probe.Spec.Depends)

	original := probe.DeepCopy()
	now := metav1.NewTime(r.Clock())

	probe.Status.ObservedGeneration = probe.Generation
	setSuspendedCondition(&probe.Status.Conditions, probe.Generation, probe.Spec.Suspend, now)

	if probe.Spec.Suspend {
		r.Scheduler.Unregister(req.NamespacedName)
		r.Metrics.Delete(req.NamespacedName)
	} else {
		r.Scheduler.Register(internalprobes.NewHTTPJob(&probe, r.HTTPExecutor, r.Metrics))
	}

	if probeStatusChanged(original.Status.ObservedGeneration, probe.Status.ObservedGeneration, original.Status.Conditions, probe.Status.Conditions) {
		if err := r.Status().Patch(ctx, &probe, client.MergeFrom(original)); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *HTTPProbeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&syntheticsv1alpha1.HTTPProbe{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}
