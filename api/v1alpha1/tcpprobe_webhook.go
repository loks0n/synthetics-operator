package v1alpha1

import (
	"context"
	"fmt"
	"net"
	"strings"

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

var _ webhook.CustomDefaulter = &TCPProbe{}

// +kubebuilder:object:generate=false
type TCPProbeValidator struct {
	reader client.Reader
}

var _ webhook.CustomValidator = &TCPProbeValidator{}

func SetupTCPProbeWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&TCPProbe{}).
		WithDefaulter(&TCPProbe{}).
		WithValidator(&TCPProbeValidator{reader: mgr.GetAPIReader()}).
		Complete()
}

func (p *TCPProbe) Default(ctx context.Context, obj runtime.Object) error {
	probe := obj.(*TCPProbe)
	defaultIntervalTimeout(&probe.Spec.Interval, &probe.Spec.Timeout)
	probe.Spec.Target.Host = strings.TrimSpace(probe.Spec.Target.Host)
	log.FromContext(ctx).V(1).Info("defaulted TCPProbe", "name", probe.Name)
	return nil
}

func (v *TCPProbeValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, obj.(*TCPProbe).validate(ctx, v.reader)
}

func (v *TCPProbeValidator) ValidateUpdate(ctx context.Context, _, obj runtime.Object) (admission.Warnings, error) {
	return nil, obj.(*TCPProbe).validate(ctx, v.reader)
}

func (v *TCPProbeValidator) ValidateDelete(context.Context, runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (p *TCPProbe) validate(ctx context.Context, reader client.Reader) error {
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

	host := strings.TrimSpace(p.Spec.Target.Host)
	hostPath := field.NewPath("spec", "target", "host")
	if host == "" {
		allErrs = append(allErrs, field.Required(hostPath, "host is required"))
	} else if net.ParseIP(host) == nil {
		dnsHost := strings.TrimSuffix(host, ".")
		if errs := validation.IsDNS1123Subdomain(dnsHost); len(errs) > 0 {
			allErrs = append(allErrs, field.Invalid(hostPath, p.Spec.Target.Host, "must be a DNS hostname, IPv4 address, or IPv6 address without a scheme or port"))
		}
	}
	if p.Spec.Target.Port < 1 || p.Spec.Target.Port > 65535 {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "target", "port"), p.Spec.Target.Port, "must be between 1 and 65535"))
	}

	for i, a := range p.Spec.Assertions {
		fp := field.NewPath("spec", "assertions").Index(i)
		if a.Name == "" {
			allErrs = append(allErrs, field.Required(fp.Child("name"), "assertion name is required"))
		}
		if err := ValidateAssertionExpr(a.Expr, tcpAssertionVars()); err != nil {
			allErrs = append(allErrs, field.Invalid(fp.Child("expr"), a.Expr, err.Error()))
		}
	}

	allErrs = append(allErrs, ValidateDepends(ctx, reader, DependencyKindTCPProbe, p.Namespace, p.Name, p.Spec.Depends, field.NewPath("spec", "depends"))...)
	allErrs = append(allErrs, ValidateMetricLabels(p.Spec.MetricLabels, field.NewPath("spec", "metricLabels"))...)

	if len(allErrs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(schema.GroupKind{Group: GroupVersion.Group, Kind: "TCPProbe"}, p.Name, allErrs)
}

func (p *TCPProbe) String() string {
	return fmt.Sprintf("%s/%s", p.Namespace, p.Name)
}
