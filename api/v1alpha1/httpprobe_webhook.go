package v1alpha1

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var _ webhook.CustomDefaulter = &HttpProbe{}
var _ webhook.CustomValidator = &HttpProbe{}

func SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&HttpProbe{}).
		WithDefaulter(&HttpProbe{}).
		WithValidator(&HttpProbe{}).
		Complete()
}

func (h *HttpProbe) Default(ctx context.Context, _ runtime.Object) error {
	logger := log.FromContext(ctx)
	if h.Spec.Interval.Duration == 0 {
		h.Spec.Interval.Duration = 30 * time.Second
	}
	if h.Spec.Timeout.Duration == 0 {
		h.Spec.Timeout.Duration = 10 * time.Second
	}
	if h.Spec.Request.Method == "" {
		h.Spec.Request.Method = "GET"
	}
	if h.Spec.Assertions.Status == 0 {
		h.Spec.Assertions.Status = 200
	}
	logger.V(1).Info("defaulted HttpProbe", "name", h.Name)
	return nil
}

func (h *HttpProbe) ValidateCreate(ctx context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, h.validate()
}

func (h *HttpProbe) ValidateUpdate(ctx context.Context, _, _ runtime.Object) (admission.Warnings, error) {
	return nil, h.validate()
}

func (h *HttpProbe) ValidateDelete(context.Context, runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (h *HttpProbe) validate() error {
	var allErrs field.ErrorList

	if h.Spec.Interval.Duration <= 0 {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "interval"), h.Spec.Interval.Duration.String(), "must be greater than zero"))
	}
	if h.Spec.Timeout.Duration <= 0 {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "timeout"), h.Spec.Timeout.Duration.String(), "must be greater than zero"))
	}
	if h.Spec.Interval.Duration > 0 && h.Spec.Timeout.Duration > h.Spec.Interval.Duration {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "timeout"), h.Spec.Timeout.Duration.String(), "must be less than or equal to interval"))
	}

	if strings.ToUpper(h.Spec.Request.Method) != "GET" {
		allErrs = append(allErrs, field.NotSupported(field.NewPath("spec", "request", "method"), h.Spec.Request.Method, []string{"GET"}))
	}

	parsedURL, err := url.Parse(h.Spec.Request.URL)
	if err != nil || parsedURL == nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "request", "url"), h.Spec.Request.URL, "must be a valid absolute URL"))
	} else if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		allErrs = append(allErrs, field.NotSupported(field.NewPath("spec", "request", "url"), parsedURL.Scheme, []string{"http", "https"}))
	}

	if h.Spec.Assertions.Status < 100 || h.Spec.Assertions.Status > 599 {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "assertions", "status"), h.Spec.Assertions.Status, "must be a valid HTTP status code"))
	}

	if len(allErrs) == 0 {
		return nil
	}

	return apierrors.NewInvalid(
		schema.GroupKind{Group: GroupVersion.Group, Kind: "HttpProbe"},
		h.Name,
		allErrs,
	)
}

func (h *HttpProbe) String() string {
	return fmt.Sprintf("%s/%s", h.Namespace, h.Name)
}
