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

var _ webhook.CustomDefaulter = &HTTPProbe{}
var _ webhook.CustomValidator = &HTTPProbe{}

func SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&HTTPProbe{}).
		WithDefaulter(&HTTPProbe{}).
		WithValidator(&HTTPProbe{}).
		Complete()
}

func (h *HTTPProbe) Default(ctx context.Context, obj runtime.Object) error {
	probe := obj.(*HTTPProbe)
	logger := log.FromContext(ctx)
	if probe.Spec.Interval.Duration == 0 {
		probe.Spec.Interval.Duration = 30 * time.Second
	}
	if probe.Spec.Timeout.Duration == 0 {
		probe.Spec.Timeout.Duration = 10 * time.Second
	}
	if probe.Spec.Request.Method == "" {
		probe.Spec.Request.Method = "GET"
	}
	if probe.Spec.Assertions.Status == 0 {
		probe.Spec.Assertions.Status = 200
	}
	logger.V(1).Info("defaulted HTTPProbe", "name", probe.Name)
	return nil
}

func (h *HTTPProbe) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, obj.(*HTTPProbe).validate()
}

func (h *HTTPProbe) ValidateUpdate(_ context.Context, _, obj runtime.Object) (admission.Warnings, error) {
	return nil, obj.(*HTTPProbe).validate()
}

func (h *HTTPProbe) ValidateDelete(context.Context, runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (h *HTTPProbe) validate() error {
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

	method := strings.ToUpper(h.Spec.Request.Method)
	if method != "GET" && method != "HEAD" {
		allErrs = append(allErrs, field.NotSupported(field.NewPath("spec", "request", "method"), h.Spec.Request.Method, []string{"GET", "HEAD"}))
	}
	if method == "HEAD" && h.Spec.Assertions.Body != nil {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "assertions", "body"), nil, "body assertions cannot be used with HEAD method"))
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

	if h.Spec.Assertions.Latency != nil && h.Spec.Assertions.Latency.MaxMs <= 0 {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "assertions", "latency", "maxMs"), h.Spec.Assertions.Latency.MaxMs, "must be greater than zero"))
	}

	if h.Spec.Assertions.Body != nil {
		for i, ja := range h.Spec.Assertions.Body.JSON {
			if !strings.HasPrefix(ja.Path, "$.") && ja.Path != "$" {
				allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "assertions", "body", "json").Index(i).Child("path"), ja.Path, "must be a valid JSONPath expression starting with $"))
			}
		}
	}

	if len(allErrs) == 0 {
		return nil
	}

	return apierrors.NewInvalid(
		schema.GroupKind{Group: GroupVersion.Group, Kind: "HTTPProbe"},
		h.Name,
		allErrs,
	)
}

func (h *HTTPProbe) String() string {
	return fmt.Sprintf("%s/%s", h.Namespace, h.Name)
}
