package v1alpha1

import (
	"context"
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

var _ webhook.CustomDefaulter = &PlaywrightTest{}

// PlaywrightTestValidator holds the client.Reader needed for dep validation.
//
// +kubebuilder:object:generate=false
type PlaywrightTestValidator struct {
	reader client.Reader
}

var _ webhook.CustomValidator = &PlaywrightTestValidator{}

func SetupPlaywrightTestWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&PlaywrightTest{}).
		WithDefaulter(&PlaywrightTest{}).
		WithValidator(&PlaywrightTestValidator{reader: mgr.GetAPIReader()}).
		Complete()
}

func (p *PlaywrightTest) Default(ctx context.Context, obj runtime.Object) error {
	t := obj.(*PlaywrightTest)
	if t.Spec.Interval.Duration == 0 {
		t.Spec.Interval.Duration = time.Hour
	}
	if t.Spec.TTLAfterFinished.Duration == 0 {
		t.Spec.TTLAfterFinished.Duration = time.Hour
	}
	log.FromContext(ctx).V(1).Info("defaulted PlaywrightTest", "name", t.Name)
	return nil
}

func (v *PlaywrightTestValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, obj.(*PlaywrightTest).validate(ctx, v.reader)
}

func (v *PlaywrightTestValidator) ValidateUpdate(ctx context.Context, _, obj runtime.Object) (admission.Warnings, error) {
	return nil, obj.(*PlaywrightTest).validate(ctx, v.reader)
}

func (v *PlaywrightTestValidator) ValidateDelete(context.Context, runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (p *PlaywrightTest) validate(ctx context.Context, reader client.Reader) error {
	var allErrs field.ErrorList

	if err := validateCronInterval(p.Spec.Interval.Duration); err != nil {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "interval"), p.Spec.Interval.Duration.String(), err.Error()))
	}

	if p.Spec.Script.ConfigMap.Name == "" {
		allErrs = append(allErrs, field.Required(field.NewPath("spec", "script", "configMap", "name"), "configMap name is required"))
	}
	if p.Spec.Script.ConfigMap.Key == "" {
		allErrs = append(allErrs, field.Required(field.NewPath("spec", "script", "configMap", "key"), "configMap key is required"))
	}

	allErrs = append(allErrs, ValidateDepends(ctx, reader, DependencyKindPlaywrightTest, p.Namespace, p.Name, p.Spec.Depends, field.NewPath("spec", "depends"))...)

	if len(allErrs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(
		schema.GroupKind{Group: GroupVersion.Group, Kind: "PlaywrightTest"},
		p.Name,
		allErrs,
	)
}

func (p *PlaywrightTest) String() string {
	return fmt.Sprintf("%s/%s", p.Namespace, p.Name)
}
