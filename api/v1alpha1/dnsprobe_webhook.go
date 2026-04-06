package v1alpha1

import (
	"context"
	"fmt"
	"net"
	"slices"
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

var _ webhook.CustomDefaulter = &DNSProbe{}
var _ webhook.CustomValidator = &DNSProbe{}

var validDNSTypes = []string{"A", "AAAA", "CNAME", "MX", "TXT", "NS", "PTR"}

func SetupDNSWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&DNSProbe{}).
		WithDefaulter(&DNSProbe{}).
		WithValidator(&DNSProbe{}).
		Complete()
}

func (d *DNSProbe) Default(ctx context.Context, obj runtime.Object) error {
	probe := obj.(*DNSProbe)
	logger := log.FromContext(ctx)
	if probe.Spec.Interval.Duration == 0 {
		probe.Spec.Interval.Duration = 30 * time.Second
	}
	if probe.Spec.Timeout.Duration == 0 {
		probe.Spec.Timeout.Duration = 10 * time.Second
	}
	if probe.Spec.Query.Type == "" {
		probe.Spec.Query.Type = "A"
	}
	logger.V(1).Info("defaulted DNSProbe", "name", probe.Name)
	return nil
}

func (d *DNSProbe) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, obj.(*DNSProbe).validate()
}

func (d *DNSProbe) ValidateUpdate(_ context.Context, _, obj runtime.Object) (admission.Warnings, error) {
	return nil, obj.(*DNSProbe).validate()
}

func (d *DNSProbe) ValidateDelete(context.Context, runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (d *DNSProbe) validate() error {
	var allErrs field.ErrorList

	if d.Spec.Interval.Duration <= 0 {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "interval"), d.Spec.Interval.Duration.String(), "must be greater than zero"))
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

	queryType := strings.ToUpper(d.Spec.Query.Type)
	if !slices.Contains(validDNSTypes, queryType) {
		allErrs = append(allErrs, field.NotSupported(field.NewPath("spec", "query", "type"), d.Spec.Query.Type, validDNSTypes))
	}

	if d.Spec.Query.Resolver != "" {
		host, port, err := net.SplitHostPort(d.Spec.Query.Resolver)
		if err != nil || host == "" || port == "" {
			allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "query", "resolver"), d.Spec.Query.Resolver, "must be in host:port format"))
		}
	}

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
