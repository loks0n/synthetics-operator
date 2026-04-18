package v1alpha1

import (
	"context"
	"crypto/x509"
	"fmt"
	"net/url"

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

var _ webhook.CustomDefaulter = &HTTPProbe{}

// HTTPProbeValidator holds the dependencies needed to validate an HTTPProbe
// spec, including the client.Reader used for admission-time dependency
// existence + cycle checks. Constructed by SetupWebhookWithManager.
//
// +kubebuilder:object:generate=false
type HTTPProbeValidator struct {
	reader client.Reader
}

var _ webhook.CustomValidator = &HTTPProbeValidator{}

func SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&HTTPProbe{}).
		WithDefaulter(&HTTPProbe{}).
		WithValidator(&HTTPProbeValidator{reader: mgr.GetAPIReader()}).
		Complete()
}

func (h *HTTPProbe) Default(ctx context.Context, obj runtime.Object) error {
	probe := obj.(*HTTPProbe)
	defaultIntervalTimeout(&probe.Spec.Interval, &probe.Spec.Timeout)
	if probe.Spec.Request.Method == "" {
		probe.Spec.Request.Method = "GET"
	}
	log.FromContext(ctx).V(1).Info("defaulted HTTPProbe", "name", probe.Name)
	return nil
}

func (v *HTTPProbeValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, obj.(*HTTPProbe).validate(ctx, v.reader)
}

func (v *HTTPProbeValidator) ValidateUpdate(ctx context.Context, _, obj runtime.Object) (admission.Warnings, error) {
	return nil, obj.(*HTTPProbe).validate(ctx, v.reader)
}

func (v *HTTPProbeValidator) ValidateDelete(context.Context, runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (h *HTTPProbe) validate(ctx context.Context, reader client.Reader) error {
	var allErrs field.ErrorList

	if err := validateProbeInterval(h.Spec.Interval.Duration); err != nil {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "interval"), h.Spec.Interval.Duration.String(), err.Error()))
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
		if err := ValidateAssertionExpr(a.Expr, httpAssertionVars()); err != nil {
			allErrs = append(allErrs, field.Invalid(fp.Child("expr"), a.Expr, err.Error()))
		}
	}

	if h.Spec.TLS != nil && h.Spec.TLS.CACert != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(h.Spec.TLS.CACert)) {
			allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "tls", "caCert"), "<pem>", "must be a valid PEM-encoded certificate"))
		}
	}

	allErrs = append(allErrs, ValidateDepends(ctx, reader, DependencyKindHTTPProbe, h.Namespace, h.Name, h.Spec.Depends, field.NewPath("spec", "depends"))...)

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
