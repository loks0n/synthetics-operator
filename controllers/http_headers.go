package controllers

import (
	"context"
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
)

// resolveRequestHeaders merges spec.request.headers with headers resolved
// from spec.request.headersFrom Secret references. Secrets are read from the
// probe's own namespace; a HeadersFrom entry overrides a literal header of
// the same name. Returns the literal map untouched when headersFrom is empty.
func resolveRequestHeaders(ctx context.Context, r client.Reader, namespace string, req syntheticsv1alpha1.HTTPRequestSpec) (map[string]string, error) {
	if len(req.HeadersFrom) == 0 {
		return req.Headers, nil
	}
	merged := make(map[string]string, len(req.Headers)+len(req.HeadersFrom))
	maps.Copy(merged, req.Headers)
	for _, h := range req.HeadersFrom {
		var secret corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: h.SecretKeyRef.Name}, &secret); err != nil {
			return nil, fmt.Errorf("resolving header %q from secret %q: %w", h.Name, h.SecretKeyRef.Name, err)
		}
		value, ok := secret.Data[h.SecretKeyRef.Key]
		if !ok {
			return nil, fmt.Errorf("resolving header %q: secret %q has no key %q", h.Name, h.SecretKeyRef.Name, h.SecretKeyRef.Key)
		}
		merged[h.Name] = string(value)
	}
	return merged, nil
}
