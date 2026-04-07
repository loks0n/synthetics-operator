package v1alpha1

import (
	"context"
	"crypto/x509"
	"fmt"
	"net/url"
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

	if h.Spec.Request.Method == "" {
		allErrs = append(allErrs, field.Required(field.NewPath("spec", "request", "method"), "method is required"))
	}

	parsedURL, err := url.Parse(h.Spec.Request.URL)
	if err != nil || parsedURL == nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "request", "url"), h.Spec.Request.URL, "must be a valid absolute URL"))
	} else if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		allErrs = append(allErrs, field.NotSupported(field.NewPath("spec", "request", "url"), parsedURL.Scheme, []string{"http", "https"}))
	}

	for i, a := range h.Spec.Assertions {
		fp := field.NewPath("spec", "assertions").Index(i)
		if a.Name == "" {
			allErrs = append(allErrs, field.Required(fp.Child("name"), "assertion name is required"))
		}
		if err := ValidateAssertionExpr(a.Expr, ValidHTTPAssertionVars); err != nil {
			allErrs = append(allErrs, field.Invalid(fp.Child("expr"), a.Expr, err.Error()))
		}
	}

	if h.Spec.TLS != nil && h.Spec.TLS.CACert != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(h.Spec.TLS.CACert)) {
			allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "tls", "caCert"), "<pem>", "must be a valid PEM-encoded certificate"))
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
