package v1alpha1

import (
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// ValidateRunnerNodeSelector checks that a runner's nodeSelector holds valid
// Kubernetes label keys and values.
//
// Without this the CRD accepts anything a map[string]string can hold, and the
// break surfaces one layer down: pod-spec validation rejects the CronJob the
// controller builds, so the test never runs and the only trace is a reconcile
// error in the operator log. Rejecting at admission puts the error on the
// kubectl apply that caused it.
func ValidateRunnerNodeSelector(selector map[string]string, path *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	for key, value := range selector {
		if errs := validation.IsQualifiedName(key); len(errs) > 0 {
			allErrs = append(allErrs, field.Invalid(path.Key(key), key, strings.Join(errs, "; ")))
		}
		if errs := validation.IsValidLabelValue(value); len(errs) > 0 {
			allErrs = append(allErrs, field.Invalid(path.Key(key), value, strings.Join(errs, "; ")))
		}
	}
	return allErrs
}
