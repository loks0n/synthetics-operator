package v1alpha1

import (
	"context"
	"fmt"
	"net"
	"slices"
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

var _ webhook.CustomDefaulter = &DNSProbe{}

// DNSProbeValidator holds the client.Reader needed for dep validation.
//
// +kubebuilder:object:generate=false
type DNSProbeValidator struct {
	reader client.Reader
}

var _ webhook.CustomValidator = &DNSProbeValidator{}

func SetupDNSWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&DNSProbe{}).
		WithDefaulter(&DNSProbe{}).
		WithValidator(&DNSProbeValidator{reader: mgr.GetAPIReader()}).
		Complete()
}

func (d *DNSProbe) Default(ctx context.Context, obj runtime.Object) error {
	probe := obj.(*DNSProbe)
	defaultIntervalTimeout(&probe.Spec.Interval, &probe.Spec.Timeout)
	if probe.Spec.Query.Type == "" {
		probe.Spec.Query.Type = "A"
	}
	log.FromContext(ctx).V(1).Info("defaulted DNSProbe", "name", probe.Name)
	return nil
}

func (v *DNSProbeValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, obj.(*DNSProbe).validate(ctx, v.reader)
}

func (v *DNSProbeValidator) ValidateUpdate(ctx context.Context, _, obj runtime.Object) (admission.Warnings, error) {
	return nil, obj.(*DNSProbe).validate(ctx, v.reader)
}

func (v *DNSProbeValidator) ValidateDelete(context.Context, runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (d *DNSProbe) validate(ctx context.Context, reader client.Reader) error {
	var allErrs field.ErrorList

	if err := validateProbeInterval(d.Spec.Interval.Duration); err != nil {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "interval"), d.Spec.Interval.Duration.String(), err.Error()))
	}
	if d.Spec.Timeout.Duration <= 0 {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "timeout"), d.Spec.Timeout.Duration.String(), "must be greater than zero"))
	}
	if d.Spec.Interval.Duration > 0 && d.Spec.Timeout.Duration > d.Spec.Interval.Duration {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "timeout"), d.Spec.Timeout.Duration.String(), "must be less than or equal to interval"))
	}

	if strings.TrimSpace(d.Spec.Query.Name) == "" {
		allErrs = append(allErrs, field.Required(field.NewPath("spec", "query", "name"), "must be non-empty"))
	}

	dnsTypes := []string{"A", "AAAA", "CNAME", "MX", "TXT", "NS", "PTR"}
	queryType := strings.ToUpper(d.Spec.Query.Type)
	if !slices.Contains(dnsTypes, queryType) {
		allErrs = append(allErrs, field.NotSupported(field.NewPath("spec", "query", "type"), d.Spec.Query.Type, dnsTypes))
	}

	if d.Spec.Query.Resolver != "" {
		host, port, err := net.SplitHostPort(d.Spec.Query.Resolver)
		if err != nil || host == "" || port == "" {
			allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "query", "resolver"), d.Spec.Query.Resolver, "must be in host:port format"))
		}
	}

	for i, a := range d.Spec.Assertions {
		fp := field.NewPath("spec", "assertions").Index(i)
		if a.Name == "" {
			allErrs = append(allErrs, field.Required(fp.Child("name"), "assertion name is required"))
		}
		if err := ValidateAssertionExpr(a.Expr, dnsAssertionVars()); err != nil {
			allErrs = append(allErrs, field.Invalid(fp.Child("expr"), a.Expr, err.Error()))
		}
	}

	allErrs = append(allErrs, ValidateDepends(ctx, reader, DependencyKindDNSProbe, d.Namespace, d.Name, d.Spec.Depends, field.NewPath("spec", "depends"))...)

	if len(allErrs) == 0 {
		return nil
	}

	return apierrors.NewInvalid(
		schema.GroupKind{Group: GroupVersion.Group, Kind: "DNSProbe"},
		d.Name,
		allErrs,
	)
}

func (d *DNSProbe) String() string {
	return fmt.Sprintf("%s/%s", d.Namespace, d.Name)
}
