package v1alpha1

import (
	"context"
	"crypto/x509"
	"fmt"
	"strings"

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

var _ webhook.CustomDefaulter = &SMTPProbe{}

// +kubebuilder:object:generate=false
type SMTPProbeValidator struct {
	reader client.Reader
}

var _ webhook.CustomValidator = &SMTPProbeValidator{}

func SetupSMTPProbeWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&SMTPProbe{}).
		WithDefaulter(&SMTPProbe{}).
		WithValidator(&SMTPProbeValidator{reader: mgr.GetAPIReader()}).
		Complete()
}

func (p *SMTPProbe) Default(ctx context.Context, obj runtime.Object) error {
	probe := obj.(*SMTPProbe)
	defaultIntervalTimeout(&probe.Spec.Interval, &probe.Spec.Timeout)
	probe.Spec.Target.Host = strings.TrimSpace(probe.Spec.Target.Host)
	if probe.Spec.TLS.Mode == "" {
		probe.Spec.TLS.Mode = DefaultSMTPTLSMode(probe.Spec.Target.Port)
	}
	log.FromContext(ctx).V(1).Info("defaulted SMTPProbe", "name", probe.Name)
	return nil
}

func (v *SMTPProbeValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, obj.(*SMTPProbe).validate(ctx, v.reader)
}

func (v *SMTPProbeValidator) ValidateUpdate(ctx context.Context, _, obj runtime.Object) (admission.Warnings, error) {
	return nil, obj.(*SMTPProbe).validate(ctx, v.reader)
}

func (v *SMTPProbeValidator) ValidateDelete(context.Context, runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (p *SMTPProbe) validate(ctx context.Context, reader client.Reader) error {
	var allErrs field.ErrorList

	if err := validateProbeInterval(p.Spec.Interval.Duration); err != nil {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "interval"), p.Spec.Interval.Duration.String(), err.Error()))
	}
	if p.Spec.Timeout.Duration <= 0 {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "timeout"), p.Spec.Timeout.Duration.String(), "must be greater than zero"))
	}
	if p.Spec.Interval.Duration > 0 && p.Spec.Timeout.Duration > p.Spec.Interval.Duration {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "timeout"), p.Spec.Timeout.Duration.String(), "must be less than or equal to interval"))
	}

	allErrs = append(allErrs, validateTCPTarget(p.Spec.Target, field.NewPath("spec", "target"))...)
	allErrs = append(allErrs, p.Spec.TLS.validate(field.NewPath("spec", "tls"))...)

	for i, a := range p.Spec.Assertions {
		fp := field.NewPath("spec", "assertions").Index(i)
		if a.Name == "" {
			allErrs = append(allErrs, field.Required(fp.Child("name"), "assertion name is required"))
		}
		if err := ValidateAssertionExpr(a.Expr, smtpAssertionVars()); err != nil {
			allErrs = append(allErrs, field.Invalid(fp.Child("expr"), a.Expr, err.Error()))
		}
	}

	allErrs = append(allErrs, ValidateDepends(ctx, reader, DependencyKindSMTPProbe, p.Namespace, p.Name, p.Spec.Depends, field.NewPath("spec", "depends"))...)
	allErrs = append(allErrs, ValidateMetricLabels(p.Spec.MetricLabels, field.NewPath("spec", "metricLabels"))...)

	if len(allErrs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(schema.GroupKind{Group: GroupVersion.Group, Kind: "SMTPProbe"}, p.Name, allErrs)
}

func (c *SMTPTLSConfig) validate(path *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	switch c.Mode {
	case SMTPTLSModeStartTLS, SMTPTLSModeImplicit:
	case SMTPTLSModeNone:
		// Verification settings on a plaintext probe would read as checked
		// while nothing is, so they are refused rather than ignored.
		if c.InsecureSkipVerify || c.CACert != "" {
			allErrs = append(allErrs, field.Forbidden(path, "insecureSkipVerify and caCert require mode starttls or implicit"))
		}
	default:
		allErrs = append(allErrs, field.NotSupported(path.Child("mode"), string(c.Mode),
			[]string{string(SMTPTLSModeNone), string(SMTPTLSModeStartTLS), string(SMTPTLSModeImplicit)}))
	}
	if c.CACert != "" && !x509.NewCertPool().AppendCertsFromPEM([]byte(c.CACert)) {
		allErrs = append(allErrs, field.Invalid(path.Child("caCert"), "<pem>", "must be a valid PEM-encoded certificate"))
	}
	return allErrs
}

func (p *SMTPProbe) String() string {
	return fmt.Sprintf("%s/%s", p.Namespace, p.Name)
}
