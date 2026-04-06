package controllers

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
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
	Scheme    *runtime.Scheme
	Scheduler *internalprobes.Scheduler
	Metrics   *internalmetrics.Store
	Clock     func() time.Time
}

func (r *HTTPProbeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var probe syntheticsv1alpha1.HTTPProbe
	if err := r.Get(ctx, req.NamespacedName, &probe); err != nil {
		if apierrors.IsNotFound(err) {
			r.Scheduler.Unregister(req.NamespacedName)
			r.Metrics.Delete(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !probe.DeletionTimestamp.IsZero() {
		r.Scheduler.Unregister(req.NamespacedName)
		r.Metrics.Delete(req.NamespacedName)
		return ctrl.Result{}, nil
	}

	original := probe.DeepCopy()
	now := metav1.NewTime(r.Clock())

	probe.Status.ObservedGeneration = probe.Generation
	setSuspendedCondition(&probe, probe.Spec.Suspend, now)
	if probe.Status.LastRunTime == nil {
		probe.Status.Summary = &syntheticsv1alpha1.ProbeSummary{
			Message: "probe registered and waiting for first execution",
		}
		apimeta.SetStatusCondition(&probe.Status.Conditions, metav1.Condition{
			Type:               syntheticsv1alpha1.ConditionReady,
			Status:             metav1.ConditionUnknown,
			Reason:             syntheticsv1alpha1.ReasonInitializing,
			Message:            "probe registered and waiting for first execution",
			LastTransitionTime: now,
			ObservedGeneration: probe.Generation,
		})
	}

	if probe.Spec.Suspend {
		r.Scheduler.Unregister(req.NamespacedName)
		r.Metrics.Delete(req.NamespacedName)
	} else {
		r.Scheduler.Register(&probe)
	}

	if statusChanged(original, &probe) {
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

func statusChanged(before, after *syntheticsv1alpha1.HTTPProbe) bool {
	if before.Status.ObservedGeneration != after.Status.ObservedGeneration {
		return true
	}
	if before.Status.ConsecutiveFailures != after.Status.ConsecutiveFailures {
		return true
	}
	if len(before.Status.Conditions) != len(after.Status.Conditions) {
		return true
	}
	for i := range before.Status.Conditions {
		if before.Status.Conditions[i] != after.Status.Conditions[i] {
			return true
		}
	}
	return (before.Status.LastRunTime == nil) != (after.Status.LastRunTime == nil)
}

func setSuspendedCondition(probe *syntheticsv1alpha1.HTTPProbe, suspended bool, now metav1.Time) {
	condition := metav1.Condition{
		Type:               syntheticsv1alpha1.ConditionSuspended,
		ObservedGeneration: probe.Generation,
		LastTransitionTime: now,
	}
	if suspended {
		condition.Status = metav1.ConditionTrue
		condition.Reason = syntheticsv1alpha1.ReasonSuspended
		condition.Message = "probe execution is suspended"
	} else {
		condition.Status = metav1.ConditionFalse
		condition.Reason = syntheticsv1alpha1.ReasonResumed
		condition.Message = "probe execution is active"
	}
	apimeta.SetStatusCondition(&probe.Status.Conditions, condition)
}
