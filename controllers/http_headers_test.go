package controllers

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
)

func TestResolveRequestHeaders_MergesSecretValues(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "api-creds", Namespace: "default"},
		Data:       map[string][]byte{"key": []byte("s3cret")},
	}
	k8sClient := fakeclient.NewClientBuilder().WithScheme(unitScheme()).WithObjects(secret).Build()

	req := syntheticsv1alpha1.HTTPRequestSpec{
		Headers: map[string]string{"Accept": "application/json", "X-Appwrite-Key": "placeholder"},
		HeadersFrom: []syntheticsv1alpha1.HeaderFromSource{
			{
				Name:         "X-Appwrite-Key",
				SecretKeyRef: syntheticsv1alpha1.SecretKeySelector{Name: "api-creds", Key: "key"},
			},
		},
	}

	headers, err := resolveRequestHeaders(context.Background(), k8sClient, "default", req)
	if err != nil {
		t.Fatalf("resolveRequestHeaders: %v", err)
	}
	if headers["X-Appwrite-Key"] != "s3cret" {
		t.Fatalf("expected secret value to win, got %q", headers["X-Appwrite-Key"])
	}
	if headers["Accept"] != "application/json" {
		t.Fatalf("expected literal header preserved, got %q", headers["Accept"])
	}
	// The input spec must not be mutated: it is the live CR object.
	if req.Headers["X-Appwrite-Key"] != "placeholder" {
		t.Fatal("resolveRequestHeaders mutated the input header map")
	}
}

func TestResolveRequestHeaders_NoHeadersFromIsPassthrough(t *testing.T) {
	k8sClient := fakeclient.NewClientBuilder().WithScheme(unitScheme()).Build()

	req := syntheticsv1alpha1.HTTPRequestSpec{Headers: map[string]string{"Accept": "text/html"}}
	headers, err := resolveRequestHeaders(context.Background(), k8sClient, "default", req)
	if err != nil {
		t.Fatalf("resolveRequestHeaders: %v", err)
	}
	if headers["Accept"] != "text/html" {
		t.Fatalf("expected passthrough headers, got %+v", headers)
	}
}

func TestResolveRequestHeaders_MissingSecretAndKey(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "api-creds", Namespace: "default"},
		Data:       map[string][]byte{"key": []byte("s3cret")},
	}
	k8sClient := fakeclient.NewClientBuilder().WithScheme(unitScheme()).WithObjects(secret).Build()

	missingSecret := syntheticsv1alpha1.HTTPRequestSpec{
		HeadersFrom: []syntheticsv1alpha1.HeaderFromSource{
			{Name: "X-Key", SecretKeyRef: syntheticsv1alpha1.SecretKeySelector{Name: "nope", Key: "key"}},
		},
	}
	if _, err := resolveRequestHeaders(context.Background(), k8sClient, "default", missingSecret); err == nil {
		t.Fatal("expected error for missing secret")
	}

	missingKey := syntheticsv1alpha1.HTTPRequestSpec{
		HeadersFrom: []syntheticsv1alpha1.HeaderFromSource{
			{Name: "X-Key", SecretKeyRef: syntheticsv1alpha1.SecretKeySelector{Name: "api-creds", Key: "nope"}},
		},
	}
	if _, err := resolveRequestHeaders(context.Background(), k8sClient, "default", missingKey); err == nil {
		t.Fatal("expected error for missing key")
	}
}

func TestHTTPProbeReconcile_PublishesResolvedHeaders(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "api-creds", Namespace: "default"},
		Data:       map[string][]byte{"key": []byte("s3cret")},
	}
	probe := &syntheticsv1alpha1.HTTPProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "secret-probe", Namespace: "default"},
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Interval: metav1.Duration{Duration: 30 * time.Second},
			Timeout:  metav1.Duration{Duration: 5 * time.Second},
			Request: syntheticsv1alpha1.HTTPRequestSpec{
				URL:    "http://example.com",
				Method: "GET",
				HeadersFrom: []syntheticsv1alpha1.HeaderFromSource{
					{
						Name:         "X-Appwrite-Key",
						SecretKeyRef: syntheticsv1alpha1.SecretKeySelector{Name: "api-creds", Key: "key"},
					},
				},
			},
		},
	}

	k8sClient := fakeclient.NewClientBuilder().
		WithScheme(unitScheme()).
		WithStatusSubresource(probe).
		WithObjects(probe, secret).
		Build()

	sched := newFakeScheduler()
	pub := &fakePublisher{}
	r := newUnitReconciler(k8sClient, sched, pub)

	key := types.NamespacedName{Namespace: "default", Name: "secret-probe"}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	spec := pub.latestSpec()
	if spec == nil || spec.HTTPProbe == nil {
		t.Fatalf("expected an HTTPProbe spec publish, got %+v", spec)
	}
	if spec.HTTPProbe.Headers["X-Appwrite-Key"] != "s3cret" {
		t.Fatalf("expected resolved header in published spec, got %+v", spec.HTTPProbe.Headers)
	}
}

func TestHTTPProbeReconcile_MissingSecretReturnsError(t *testing.T) {
	probe := &syntheticsv1alpha1.HTTPProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "broken-probe", Namespace: "default"},
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Interval: metav1.Duration{Duration: 30 * time.Second},
			Timeout:  metav1.Duration{Duration: 5 * time.Second},
			Request: syntheticsv1alpha1.HTTPRequestSpec{
				URL:    "http://example.com",
				Method: "GET",
				HeadersFrom: []syntheticsv1alpha1.HeaderFromSource{
					{Name: "X-Key", SecretKeyRef: syntheticsv1alpha1.SecretKeySelector{Name: "missing", Key: "key"}},
				},
			},
		},
	}

	k8sClient := fakeclient.NewClientBuilder().
		WithScheme(unitScheme()).
		WithStatusSubresource(probe).
		WithObjects(probe).
		Build()

	sched := newFakeScheduler()
	pub := &fakePublisher{}
	r := newUnitReconciler(k8sClient, sched, pub)

	key := types.NamespacedName{Namespace: "default", Name: "broken-probe"}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err == nil {
		t.Fatal("expected reconcile error when the referenced secret is missing")
	}
	if pub.latestSpec() != nil {
		t.Fatalf("no spec should be published on resolution failure, got %+v", pub.latestSpec())
	}
}
