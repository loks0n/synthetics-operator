package v1alpha1

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var _ webhook.CustomDefaulter = &K6Test{}

// K6TestValidator holds the client.Reader needed for dep validation.
//
// +kubebuilder:object:generate=false
type K6TestValidator struct {
	reader client.Reader
}

var _ webhook.CustomValidator = &K6TestValidator{}

func SetupK6TestWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&K6Test{}).
		WithDefaulter(&K6Test{}).
		WithValidator(&K6TestValidator{reader: mgr.GetAPIReader()}).
		Complete()
}

func (k *K6Test) Default(ctx context.Context, obj runtime.Object) error {
	t := obj.(*K6Test)
	if t.Spec.Interval.Duration == 0 {
		t.Spec.Interval.Duration = time.Hour
	}
	if t.Spec.TTLAfterFinished.Duration == 0 {
		t.Spec.TTLAfterFinished.Duration = time.Hour
	}
	if t.Spec.K6Version == "" {
		t.Spec.K6Version = "0.50.0"
	}
	log.FromContext(ctx).V(1).Info("defaulted K6Test", "name", t.Name)
	return nil
}

func (v *K6TestValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, obj.(*K6Test).validate(ctx, v.reader)
}

func (v *K6TestValidator) ValidateUpdate(ctx context.Context, _, obj runtime.Object) (admission.Warnings, error) {
	return nil, obj.(*K6Test).validate(ctx, v.reader)
}

func (v *K6TestValidator) ValidateDelete(context.Context, runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (k *K6Test) validate(ctx context.Context, reader client.Reader) error {
	var allErrs field.ErrorList

	if err := validateCronInterval(k.Spec.Interval.Duration); err != nil {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "interval"), k.Spec.Interval.Duration.String(), err.Error()))
	}

	if k.Spec.K6Version == "" {
		allErrs = append(allErrs, field.Required(field.NewPath("spec", "k6Version"), "k6Version is required"))
	}

	if k.Spec.Script.ConfigMap.Name == "" {
		allErrs = append(allErrs, field.Required(field.NewPath("spec", "script", "configMap", "name"), "configMap name is required"))
	}
	if k.Spec.Script.ConfigMap.Key == "" {
		allErrs = append(allErrs, field.Required(field.NewPath("spec", "script", "configMap", "key"), "configMap key is required"))
	}

	allErrs = append(allErrs, ValidateDepends(ctx, reader, DependencyKindK6Test, k.Namespace, k.Name, k.Spec.Depends, field.NewPath("spec", "depends"))...)
	allErrs = append(allErrs, ValidateMetricLabels(k.Spec.MetricLabels, field.NewPath("spec", "metricLabels"))...)

	if len(allErrs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(
		schema.GroupKind{Group: GroupVersion.Group, Kind: "K6Test"},
		k.Name,
		allErrs,
	)
}

// validateCronInterval checks that a duration can be expressed as a standard
// cron schedule. Valid values: 1m–30m evenly dividing 60; 1h–12h evenly
// dividing 24; 24h. Used by CronJob-backed kinds (K6Test, PlaywrightTest)
// where cron is the underlying transport.
func validateCronInterval(d time.Duration) error {
	totalMinutes := int(d.Minutes())
	if totalMinutes < 1 {
		return errors.New("must be at least 1m (cron resolution)")
	}
	return validateMinuteInterval(totalMinutes)
}

// validateProbeInterval permits any positive sub-minute duration (in-process
// scheduler supports arbitrary ticks) and otherwise falls back to the cron
// divisibility rule so HTTPProbe/DNSProbe stay consistent with the CronJob-
// backed kinds for intervals >= 1m.
func validateProbeInterval(d time.Duration) error {
	if d <= 0 {
		return errors.New("must be greater than zero")
	}
	if d < time.Minute {
		return nil
	}
	return validateMinuteInterval(int(d.Minutes()))
}

func validateMinuteInterval(totalMinutes int) error {
	if totalMinutes < 60 {
		if 60%totalMinutes != 0 {
			return errors.New("sub-hour intervals must evenly divide 60 (valid: 2m, 3m, 4m, 5m, 6m, 10m, 12m, 15m, 20m, 30m)")
		}
		return nil
	}
	if totalMinutes%60 != 0 {
		return errors.New("intervals >= 1h must be whole hours")
	}
	hours := totalMinutes / 60
	if hours > 24 || 24%hours != 0 {
		return errors.New("hour intervals must evenly divide 24 (valid: 1h, 2h, 3h, 4h, 6h, 8h, 12h, 24h)")
	}
	return nil
}

func (k *K6Test) String() string {
	return fmt.Sprintf("%s/%s", k.Namespace, k.Name)
}
