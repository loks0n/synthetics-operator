package v1alpha1

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DependencyKind is the CRD kind for a DependencyRef target.
type DependencyKind string

const (
	DependencyKindHTTPProbe      DependencyKind = "HTTPProbe"
	DependencyKindDNSProbe       DependencyKind = "DNSProbe"
	DependencyKindK6Test         DependencyKind = "K6Test"
	DependencyKindPlaywrightTest DependencyKind = "PlaywrightTest"
)

// DependencyRef points to another synthetics.dev CR in the same namespace.
// Failures on the owning probe or test are suppressed from alerting metrics
// when any transitive dep in this list is currently unhealthy — see the
// `synthetics_probe_suppressed` / `synthetics_test_suppressed` gauges.
type DependencyRef struct {
	// +kubebuilder:validation:Enum=HTTPProbe;DNSProbe;K6Test;PlaywrightTest
	Kind DependencyKind `json:"kind"`
	Name string         `json:"name"`
}

// maxDependsDepth bounds the DFS used for cycle detection and transitive
// existence validation at admission time. 16 levels is well past anything
// plausible; a chain longer than that is almost certainly a configuration
// error the user will want to hear about.
const maxDependsDepth = 16

// ValidateDepends performs admission-time checks on a spec.depends list:
// each entry has a known kind, a valid DNS-1123 subdomain name, isn't a
// self-reference, isn't duplicated, the referenced CR exists in the same
// namespace, and the transitive dep graph contains no cycle leading back to
// the owner.
//
// Returns a non-empty field.ErrorList on any violation; the caller wraps it
// into a k8s Invalid error.
func ValidateDepends(ctx context.Context, reader client.Reader, ownerKind DependencyKind, ownerNamespace, ownerName string, deps []DependencyRef, path *field.Path) field.ErrorList {
	if len(deps) == 0 {
		return nil
	}

	var allErrs field.ErrorList
	seen := map[DependencyRef]bool{}
	for i, dep := range deps {
		fp := path.Index(i)

		switch dep.Kind {
		case DependencyKindHTTPProbe, DependencyKindDNSProbe, DependencyKindK6Test, DependencyKindPlaywrightTest:
		default:
			allErrs = append(allErrs, field.NotSupported(fp.Child("kind"), string(dep.Kind),
				[]string{string(DependencyKindHTTPProbe), string(DependencyKindDNSProbe), string(DependencyKindK6Test), string(DependencyKindPlaywrightTest)}))
			continue
		}

		if errs := validation.IsDNS1123Subdomain(dep.Name); len(errs) > 0 {
			allErrs = append(allErrs, field.Invalid(fp.Child("name"), dep.Name, errs[0]))
			continue
		}

		if dep.Kind == ownerKind && dep.Name == ownerName {
			allErrs = append(allErrs, field.Invalid(fp, dep, "probe or test cannot depend on itself"))
			continue
		}

		if seen[dep] {
			allErrs = append(allErrs, field.Duplicate(fp, dep))
			continue
		}
		seen[dep] = true

		if err := checkDepExists(ctx, reader, ownerNamespace, dep); err != nil {
			allErrs = append(allErrs, field.Invalid(fp, dep, err.Error()))
			continue
		}
	}

	if len(allErrs) == 0 {
		if err := checkNoCycle(ctx, reader, ownerKind, ownerNamespace, ownerName, deps); err != nil {
			allErrs = append(allErrs, field.Invalid(path, deps, err.Error()))
		}
	}

	return allErrs
}

func checkDepExists(ctx context.Context, reader client.Reader, namespace string, dep DependencyRef) error {
	key := client.ObjectKey{Namespace: namespace, Name: dep.Name}
	var err error
	switch dep.Kind {
	case DependencyKindHTTPProbe:
		err = reader.Get(ctx, key, &HTTPProbe{})
	case DependencyKindDNSProbe:
		err = reader.Get(ctx, key, &DNSProbe{})
	case DependencyKindK6Test:
		err = reader.Get(ctx, key, &K6Test{})
	case DependencyKindPlaywrightTest:
		err = reader.Get(ctx, key, &PlaywrightTest{})
	default:
		return fmt.Errorf("unknown dependency kind %q", dep.Kind)
	}
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%s %q not found in namespace %q", dep.Kind, dep.Name, namespace)
		}
		return fmt.Errorf("looking up %s %q: %w", dep.Kind, dep.Name, err)
	}
	return nil
}

// checkNoCycle walks the transitive dep graph from the owner's direct deps.
// A cycle is detected when we revisit the owner. Safe against other cycles
// via the visited set.
func checkNoCycle(ctx context.Context, reader client.Reader, ownerKind DependencyKind, ownerNamespace, ownerName string, directDeps []DependencyRef) error {
	ownerRef := DependencyRef{Kind: ownerKind, Name: ownerName}
	visited := map[DependencyRef]bool{}
	return walkForCycle(ctx, reader, ownerRef, ownerNamespace, directDeps, visited, 0)
}

func walkForCycle(ctx context.Context, reader client.Reader, ownerRef DependencyRef, namespace string, deps []DependencyRef, visited map[DependencyRef]bool, depth int) error {
	if depth >= maxDependsDepth {
		return fmt.Errorf("dependency chain exceeds max depth %d", maxDependsDepth)
	}
	for _, dep := range deps {
		if dep == ownerRef {
			return fmt.Errorf("dependency cycle: %s/%s eventually depends on itself", ownerRef.Kind, ownerRef.Name)
		}
		if visited[dep] {
			continue
		}
		visited[dep] = true

		subDeps, err := fetchDepends(ctx, reader, namespace, dep)
		if err != nil {
			// Missing or unreadable: don't block on cycle detection for reasons
			// already reported by checkDepExists.
			continue
		}
		if err := walkForCycle(ctx, reader, ownerRef, namespace, subDeps, visited, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func fetchDepends(ctx context.Context, reader client.Reader, namespace string, dep DependencyRef) ([]DependencyRef, error) {
	switch dep.Kind {
	case DependencyKindHTTPProbe:
		var p HTTPProbe
		if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: dep.Name}, &p); err != nil {
			return nil, err
		}
		return p.Spec.Depends, nil
	case DependencyKindDNSProbe:
		var p DNSProbe
		if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: dep.Name}, &p); err != nil {
			return nil, err
		}
		return p.Spec.Depends, nil
	case DependencyKindK6Test:
		var t K6Test
		if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: dep.Name}, &t); err != nil {
			return nil, err
		}
		return t.Spec.Depends, nil
	case DependencyKindPlaywrightTest:
		var t PlaywrightTest
		if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: dep.Name}, &t); err != nil {
			return nil, err
		}
		return t.Spec.Depends, nil
	}
	return nil, fmt.Errorf("unknown dependency kind %q", dep.Kind)
}

