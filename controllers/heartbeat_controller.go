package controllers

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
	"github.com/loks0n/synthetics-operator/internal/heartbeat"
	"github.com/loks0n/synthetics-operator/internal/natsbus"
	"github.com/loks0n/synthetics-operator/internal/results"
)

// Keys written into the token Secret. `url` is a convenience so a pinging job
// can mount one key and curl it, without needing to know the base URL or how
// to assemble a path.
const (
	tokenSecretKey = "token"
	urlSecretKey   = "url"
)

// Condition types specific to Heartbeat. Token provisioning can fail
// independently of everything else — a bring-your-own Secret that doesn't
// exist yet is the common case — so it gets its own condition rather than
// being folded into a generic Ready.
const (
	ConditionTokenReady = "TokenReady"

	reasonTokenProvisioned = "TokenProvisioned"
	reasonTokenMissing     = "TokenMissing"
	reasonTokenInvalid     = "TokenInvalid"
)

// HeartbeatReconciler provisions each Heartbeat's token and publishes its
// spec. It registers nothing with the scheduler: a Heartbeat is never
// executed, only awaited.
type HeartbeatReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Publisher natsbus.Publisher
	Clock     func() time.Time
	// BaseURL is the externally reachable origin the receiver is published
	// on, e.g. https://heartbeats.appwrite.systems. Empty means URLs can't be
	// rendered; the heartbeat still works if a caller knows the path, but
	// status.url is left blank and a condition says why.
	BaseURL string
}

func (r *HeartbeatReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var beat syntheticsv1alpha1.Heartbeat
	if err := r.Get(ctx, req.NamespacedName, &beat); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.Publisher.PublishSpec(ctx, tombstone(results.KindHeartbeat, req.Namespace, req.Name))
		}
		return ctrl.Result{}, err
	}

	if !beat.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.Publisher.PublishSpec(ctx, tombstone(results.KindHeartbeat, beat.Namespace, beat.Name))
	}

	original := beat.DeepCopy()
	now := metav1.NewTime(r.Clock())

	token, secretName, tokenErr := r.resolveToken(ctx, &beat)
	if tokenErr != nil {
		// Publish a tombstone so a Heartbeat whose token just became
		// unreadable stops accepting pings on a token nobody can verify,
		// rather than continuing on a stale in-memory index entry.
		if err := r.Publisher.PublishSpec(ctx, tombstone(results.KindHeartbeat, beat.Namespace, beat.Name)); err != nil {
			return ctrl.Result{}, err
		}
		setTokenCondition(&beat.Status.Conditions, beat.Generation, now, false, tokenErr.reason, tokenErr.message)
		beat.Status.ObservedGeneration = beat.Generation
		beat.Status.URL = ""
		beat.Status.TokenSecretName = ""
		if err := r.patchStatus(ctx, original, &beat); err != nil {
			return ctrl.Result{}, err
		}
		// A missing bring-your-own Secret is a race with the user's own
		// apply, not a permanent error; retry without spamming the log.
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	pingURL := r.pingURL(token)
	if err := r.syncSecretURL(ctx, &beat, secretName, pingURL); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.Publisher.PublishSpec(ctx, heartbeatSpecUpdate(&beat, token)); err != nil {
		return ctrl.Result{}, err
	}

	beat.Status.ObservedGeneration = beat.Generation
	beat.Status.URL = pingURL
	beat.Status.TokenSecretName = secretName
	setSuspendedCondition(&beat.Status.Conditions, beat.Generation, beat.Spec.Suspend, now)
	setTokenCondition(&beat.Status.Conditions, beat.Generation, now, true, reasonTokenProvisioned,
		fmt.Sprintf("token available in Secret %q", secretName))

	return ctrl.Result{}, r.patchStatus(ctx, original, &beat)
}

func (r *HeartbeatReconciler) patchStatus(ctx context.Context, original, beat *syntheticsv1alpha1.Heartbeat) error {
	if !heartbeatStatusChanged(&original.Status, &beat.Status) {
		return nil
	}
	return r.Status().Patch(ctx, beat, client.MergeFrom(original))
}

// Token implements SpecResyncer's HeartbeatTokenReader. ok=false means the
// token can't be resolved right now; the caller should skip rather than
// publish a spec that would evict a working token from the receiver's index.
func (r *HeartbeatReconciler) Token(ctx context.Context, beat *syntheticsv1alpha1.Heartbeat) (string, bool) {
	token, _, err := r.resolveToken(ctx, beat)
	if err != nil {
		return "", false
	}
	return token, true
}

// tokenError carries the condition wording for a token that couldn't be
// resolved, so Reconcile can report it without a second switch on the cause.
type tokenError struct {
	reason  string
	message string
}

func (e *tokenError) Error() string { return e.message }

// resolveToken returns the Heartbeat's token and the Secret holding it,
// generating both when the user hasn't supplied their own.
//
// The generated token is read back from the Secret rather than reminted every
// reconcile — a token that rotated on every reconcile would break every
// caller continuously.
func (r *HeartbeatReconciler) resolveToken(ctx context.Context, beat *syntheticsv1alpha1.Heartbeat) (string, string, *tokenError) {
	if ref := beat.Spec.TokenSecretRef; ref != nil {
		token, err := r.adoptToken(ctx, beat.Namespace, ref)
		if err != nil {
			return "", "", err
		}
		return token, ref.Name, nil
	}

	secretName := generatedSecretName(beat.Name)
	var secret corev1.Secret
	err := r.Get(ctx, client.ObjectKey{Namespace: beat.Namespace, Name: secretName}, &secret)
	switch {
	case err == nil:
		if token := strings.TrimSpace(string(secret.Data[tokenSecretKey])); token != "" {
			return token, secretName, nil
		}
		// Secret exists but the token key is gone — someone edited it. Mint a
		// replacement rather than leaving the Heartbeat permanently broken.
	case apierrors.IsNotFound(err):
	default:
		return "", "", &tokenError{reason: reasonTokenMissing, message: fmt.Sprintf("reading Secret %q: %v", secretName, err)}
	}

	token, genErr := heartbeat.NewToken()
	if genErr != nil {
		return "", "", &tokenError{reason: reasonTokenMissing, message: genErr.Error()}
	}

	secret = corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: beat.Namespace, Name: secretName},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, &secret, func() error {
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		// Re-check inside the mutate function: CreateOrUpdate re-reads, so a
		// Secret created by a concurrent reconcile is visible here and its
		// token must win over the one we just minted.
		if existing := strings.TrimSpace(string(secret.Data[tokenSecretKey])); existing != "" {
			token = existing
		} else {
			secret.Data[tokenSecretKey] = []byte(token)
		}
		secret.Data[urlSecretKey] = []byte(r.pingURL(token))
		return controllerutil.SetControllerReference(beat, &secret, r.Scheme)
	}); err != nil {
		return "", "", &tokenError{reason: reasonTokenMissing, message: fmt.Sprintf("writing Secret %q: %v", secretName, err)}
	}
	return token, secretName, nil
}

// adoptToken reads a token out of a Secret the user brought. The Secret is
// never written to — its lifecycle and rotation belong to whoever created it.
func (r *HeartbeatReconciler) adoptToken(ctx context.Context, namespace string, ref *syntheticsv1alpha1.TokenSecretRef) (string, *tokenError) {
	key := ref.Key
	if key == "" {
		key = tokenSecretKey
	}

	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", &tokenError{reason: reasonTokenMissing, message: fmt.Sprintf("Secret %q not found", ref.Name)}
		}
		return "", &tokenError{reason: reasonTokenMissing, message: fmt.Sprintf("reading Secret %q: %v", ref.Name, err)}
	}

	token := strings.TrimSpace(string(secret.Data[key]))
	if token == "" {
		return "", &tokenError{reason: reasonTokenMissing, message: fmt.Sprintf("Secret %q has no non-empty key %q", ref.Name, key)}
	}
	if err := heartbeat.AcceptableToken(token); err != nil {
		return "", &tokenError{reason: reasonTokenInvalid, message: fmt.Sprintf("token in Secret %q is unusable: %v", ref.Name, err)}
	}
	return token, nil
}

// syncSecretURL keeps the `url` key current after a BaseURL change. Only
// operator-owned Secrets are touched: a Secret the user brought is theirs.
func (r *HeartbeatReconciler) syncSecretURL(ctx context.Context, beat *syntheticsv1alpha1.Heartbeat, secretName, pingURL string) error {
	if beat.Spec.TokenSecretRef != nil || pingURL == "" {
		return nil
	}
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: beat.Namespace, Name: secretName}, &secret); err != nil {
		return client.IgnoreNotFound(err)
	}
	if string(secret.Data[urlSecretKey]) == pingURL {
		return nil
	}
	patched := secret.DeepCopy()
	if patched.Data == nil {
		patched.Data = map[string][]byte{}
	}
	patched.Data[urlSecretKey] = []byte(pingURL)
	return r.Patch(ctx, patched, client.MergeFrom(&secret))
}

// generatedSecretName is deterministic so the token survives a controller
// restart and so two reconciles can't produce two Secrets for one Heartbeat.
func generatedSecretName(name string) string {
	return name + "-heartbeat"
}

// pingURL renders the endpoint the monitored job calls. Returns empty when no
// base URL is configured, which callers treat as "not published yet".
func (r *HeartbeatReconciler) pingURL(token string) string {
	base := strings.TrimSuffix(strings.TrimSpace(r.BaseURL), "/")
	if base == "" {
		return ""
	}
	return base + "/" + url.PathEscape(token)
}

func setTokenCondition(conditions *[]metav1.Condition, generation int64, now metav1.Time, ready bool, reason, message string) {
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	apimeta.SetStatusCondition(conditions, metav1.Condition{
		Type:               ConditionTokenReady,
		Status:             status,
		ObservedGeneration: generation,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
	})
}

// heartbeatStatusChanged ignores LastPingTime and LastResult: those are owned
// by the ping writer, and comparing them here would make every reconcile race
// with an in-flight ping patch.
func heartbeatStatusChanged(before, after *syntheticsv1alpha1.HeartbeatStatus) bool {
	if before.URL != after.URL || before.TokenSecretName != after.TokenSecretName {
		return true
	}
	return probeStatusChanged(before.ObservedGeneration, after.ObservedGeneration, before.Conditions, after.Conditions)
}

func (r *HeartbeatReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&syntheticsv1alpha1.Heartbeat{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// Watch the token Secrets too: if one is deleted, the next reconcile
		// mints a replacement instead of leaving a Heartbeat whose URL 404s
		// until something unrelated triggers a reconcile.
		WatchesRawSource(source.Kind(
			mgr.GetCache(),
			client.Object(&corev1.Secret{}),
			handler.TypedEnqueueRequestForOwner[client.Object](mgr.GetScheme(), mgr.GetRESTMapper(), &syntheticsv1alpha1.Heartbeat{}),
		)).
		Complete(r)
}
