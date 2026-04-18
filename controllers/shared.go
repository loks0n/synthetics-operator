package controllers

import (
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
	internalprobes "github.com/loks0n/synthetics-operator/internal/probes"
)

// ProbeScheduler is the scheduling interface controllers depend on.
type ProbeScheduler interface {
	Register(job internalprobes.Job)
	Unregister(name types.NamespacedName)
}

func probeStatusChanged(beforeGen, afterGen int64, beforeConds, afterConds []metav1.Condition) bool {
	if beforeGen != afterGen {
		return true
	}
	if len(beforeConds) != len(afterConds) {
		return true
	}
	for i := range beforeConds {
		if beforeConds[i] != afterConds[i] {
			return true
		}
	}
	return false
}

func setSuspendedCondition(conditions *[]metav1.Condition, generation int64, suspended bool, now metav1.Time) {
	condition := metav1.Condition{
		Type:               syntheticsv1alpha1.ConditionSuspended,
		ObservedGeneration: generation,
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
	apimeta.SetStatusCondition(conditions, condition)
}
