package v1alpha1

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// minHeartbeatPeriod floors the expected ping interval. Anything under a
// second is a metric-cardinality and status-write problem, not a monitoring
// requirement — a job that runs more often than once a second wants a
// counter, not a heartbeat.
const minHeartbeatPeriod = time.Second

var _ webhook.CustomDefaulter = &Heartbeat{}

// +kubebuilder:object:generate=false
type HeartbeatValidator struct {
	reader client.Reader
}

var _ webhook.CustomValidator = &HeartbeatValidator{}

func SetupHeartbeatWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&Heartbeat{}).
		WithDefaulter(&Heartbeat{}).
		WithValidator(&HeartbeatValidator{reader: mgr.GetAPIReader()}).
		Complete()
}

// Default fills Grace from Period. Better Stack makes grace mandatory and
// people routinely set it equal to the period; mirroring that keeps migrated
// specs terse without inventing a slack window the user didn't ask for.
func (h *Heartbeat) Default(ctx context.Context, obj runtime.Object) error {
	heartbeat := obj.(*Heartbeat)
	if heartbeat.Spec.Grace.Duration == 0 {
		heartbeat.Spec.Grace.Duration = heartbeat.Spec.Period.Duration
	}
	if ref := heartbeat.Spec.TokenSecretRef; ref != nil && ref.Key == "" {
		ref.Key = "token"
	}
	log.FromContext(ctx).V(1).Info("defaulted Heartbeat", "name", heartbeat.Name)
	return nil
}

func (v *HeartbeatValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, obj.(*Heartbeat).validate(ctx, v.reader)
}

func (v *HeartbeatValidator) ValidateUpdate(ctx context.Context, oldObj, obj runtime.Object) (admission.Warnings, error) {
	heartbeat := obj.(*Heartbeat)
	if err := heartbeat.validate(ctx, v.reader); err != nil {
		return nil, err
	}
	// Swapping token sources silently invalidates the URL every existing
	// caller is pinging, and the failure mode is a heartbeat that goes down
	// with no sign of why. Warn rather than reject: rotation is legitimate.
	if tokenSourceChanged(oldObj.(*Heartbeat), heartbeat) {
		return admission.Warnings{
			"spec.tokenSecretRef changed: the heartbeat URL will change and existing callers will start receiving 404s until they are updated",
		}, nil
	}
	return nil, nil
}

func (v *HeartbeatValidator) ValidateDelete(context.Context, runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func tokenSourceChanged(old, updated *Heartbeat) bool {
	switch {
	case old.Spec.TokenSecretRef == nil && updated.Spec.TokenSecretRef == nil:
		return false
	case old.Spec.TokenSecretRef == nil || updated.Spec.TokenSecretRef == nil:
		return true
	default:
		return *old.Spec.TokenSecretRef != *updated.Spec.TokenSecretRef
	}
}

func (h *Heartbeat) validate(ctx context.Context, reader client.Reader) error {
	var allErrs field.ErrorList

	periodPath := field.NewPath("spec", "period")
	switch {
	case h.Spec.Period.Duration <= 0:
		allErrs = append(allErrs, field.Required(periodPath, "period is required and must be greater than zero"))
	case h.Spec.Period.Duration < minHeartbeatPeriod:
		allErrs = append(allErrs, field.Invalid(periodPath, h.Spec.Period.Duration.String(),
			fmt.Sprintf("must be at least %s", minHeartbeatPeriod)))
	}

	if h.Spec.Grace.Duration < 0 {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "grace"), h.Spec.Grace.Duration.String(), "must not be negative"))
	}

	allErrs = append(allErrs, validateTokenSecretRef(h.Spec.TokenSecretRef, field.NewPath("spec", "tokenSecretRef"))...)

	allErrs = append(allErrs, ValidateDepends(ctx, reader, DependencyKindHeartbeat, h.Namespace, h.Name, h.Spec.Depends, field.NewPath("spec", "depends"))...)
	allErrs = append(allErrs, ValidateMetricLabels(h.Spec.MetricLabels, field.NewPath("spec", "metricLabels"))...)

	if len(allErrs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(schema.GroupKind{Group: GroupVersion.Group, Kind: "Heartbeat"}, h.Name, allErrs)
}

func validateTokenSecretRef(ref *TokenSecretRef, path *field.Path) field.ErrorList {
	if ref == nil {
		return nil
	}
	var allErrs field.ErrorList
	switch {
	case ref.Name == "":
		allErrs = append(allErrs, field.Required(path.Child("name"), "name is required"))
	case len(validation.IsDNS1123Subdomain(ref.Name)) > 0:
		allErrs = append(allErrs, field.Invalid(path.Child("name"), ref.Name, "must be a valid Secret name"))
	}
	if ref.Key != "" && len(validation.IsConfigMapKey(ref.Key)) > 0 {
		allErrs = append(allErrs, field.Invalid(path.Child("key"), ref.Key, "must be a valid Secret key"))
	}
	return allErrs
}

func (h *Heartbeat) String() string {
	return fmt.Sprintf("%s/%s", h.Namespace, h.Name)
}
