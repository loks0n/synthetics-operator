package controllers

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
	"github.com/loks0n/synthetics-operator/internal/heartbeat"
	"github.com/loks0n/synthetics-operator/internal/results"
)

const testBaseURL = "https://heartbeats.example.com"

func newHeartbeat(name string, mutate func(*syntheticsv1alpha1.Heartbeat)) *syntheticsv1alpha1.Heartbeat {
	beat := &syntheticsv1alpha1.Heartbeat{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "prod"},
		Spec: syntheticsv1alpha1.HeartbeatSpec{
			Period: metav1.Duration{Duration: time.Minute},
			Grace:  metav1.Duration{Duration: 3 * time.Minute},
		},
	}
	if mutate != nil {
		mutate(beat)
	}
	return beat
}

// newHeartbeatReconciler wires a reconciler over a fake client seeded with
// objects, and returns it alongside the client and publisher.
func newHeartbeatReconciler(t *testing.T, objects ...client.Object) (*HeartbeatReconciler, client.Client, *fakePublisher) {
	t.Helper()
	builder := fakeclient.NewClientBuilder().WithScheme(unitScheme())
	for _, object := range objects {
		if beat, ok := object.(*syntheticsv1alpha1.Heartbeat); ok {
			builder = builder.WithStatusSubresource(beat)
		}
	}
	k8sClient := builder.WithObjects(objects...).Build()
	publisher := &fakePublisher{}
	return &HeartbeatReconciler{
		Client:    k8sClient,
		Scheme:    unitScheme(),
		Publisher: publisher,
		Clock:     time.Now,
		BaseURL:   testBaseURL,
	}, k8sClient, publisher
}

func reconcileHeartbeat(t *testing.T, r *HeartbeatReconciler, name string) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "prod", Name: name},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return result
}

func loadHeartbeat(t *testing.T, k8sClient client.Client) *syntheticsv1alpha1.Heartbeat {
	t.Helper()
	var beat syntheticsv1alpha1.Heartbeat
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Namespace: "prod", Name: "db-backup"}, &beat); err != nil {
		t.Fatalf("loading Heartbeat: %v", err)
	}
	return &beat
}

func loadSecret(t *testing.T, k8sClient client.Client, name string) *corev1.Secret {
	t.Helper()
	var secret corev1.Secret
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Namespace: "prod", Name: name}, &secret); err != nil {
		t.Fatalf("loading Secret %q: %v", name, err)
	}
	return &secret
}

func TestHeartbeatReconcileMintsTokenAndPublishes(t *testing.T) {
	r, k8sClient, publisher := newHeartbeatReconciler(t, newHeartbeat("db-backup", nil))
	reconcileHeartbeat(t, r, "db-backup")

	secret := loadSecret(t, k8sClient, "db-backup-heartbeat")
	token := string(secret.Data[tokenSecretKey])
	if !heartbeat.ValidToken(token) {
		t.Fatalf("Secret token %q is not a well-formed generated token", token)
	}

	wantURL := testBaseURL + "/" + token
	if got := string(secret.Data[urlSecretKey]); got != wantURL {
		t.Errorf("Secret url = %q, want %q", got, wantURL)
	}

	beat := loadHeartbeat(t, k8sClient)
	if beat.Status.URL != wantURL {
		t.Errorf("status.url = %q, want %q", beat.Status.URL, wantURL)
	}
	if beat.Status.TokenSecretName != "db-backup-heartbeat" {
		t.Errorf("status.tokenSecretName = %q", beat.Status.TokenSecretName)
	}
	if !apimeta.IsStatusConditionTrue(beat.Status.Conditions, ConditionTokenReady) {
		t.Error("expected TokenReady to be true")
	}

	spec := publisher.latestSpec()
	if spec == nil || spec.Kind != results.KindHeartbeat || spec.Heartbeat == nil {
		t.Fatalf("unexpected published spec: %+v", spec)
	}
	if spec.Heartbeat.Token != token {
		t.Errorf("published token = %q, want %q", spec.Heartbeat.Token, token)
	}
	if spec.Heartbeat.PeriodMs != 60000 || spec.Heartbeat.GraceMs != 180000 {
		t.Errorf("published period/grace = %d/%d, want 60000/180000", spec.Heartbeat.PeriodMs, spec.Heartbeat.GraceMs)
	}
	// A Heartbeat is never dispatched, so it must not look schedulable.
	if spec.IntervalMs != 0 {
		t.Errorf("IntervalMs = %d, want 0 — a Heartbeat must not register with the scheduler", spec.IntervalMs)
	}
}

// A token that changed on every reconcile would break every caller
// continuously, which is the single worst failure this controller can have.
func TestHeartbeatTokenIsStableAcrossReconciles(t *testing.T) {
	r, k8sClient, _ := newHeartbeatReconciler(t, newHeartbeat("db-backup", nil))

	reconcileHeartbeat(t, r, "db-backup")
	first := string(loadSecret(t, k8sClient, "db-backup-heartbeat").Data[tokenSecretKey])
	reconcileHeartbeat(t, r, "db-backup")
	second := string(loadSecret(t, k8sClient, "db-backup-heartbeat").Data[tokenSecretKey])

	if first != second {
		t.Fatalf("token changed between reconciles: %q then %q", first, second)
	}
}

func TestHeartbeatTokensAreUniquePerHeartbeat(t *testing.T) {
	r, k8sClient, _ := newHeartbeatReconciler(t,
		newHeartbeat("db-backup", nil),
		newHeartbeat("db-replica", nil),
	)
	reconcileHeartbeat(t, r, "db-backup")
	reconcileHeartbeat(t, r, "db-replica")

	first := string(loadSecret(t, k8sClient, "db-backup-heartbeat").Data[tokenSecretKey])
	second := string(loadSecret(t, k8sClient, "db-replica-heartbeat").Data[tokenSecretKey])
	if first == second {
		t.Fatal("two Heartbeats were given the same token")
	}
}

// Someone editing the token out of the Secret should get a working heartbeat
// back, not one that is permanently broken.
func TestHeartbeatRemintsWhenTheTokenKeyIsEmptied(t *testing.T) {
	r, k8sClient, _ := newHeartbeatReconciler(t, newHeartbeat("db-backup", nil))
	reconcileHeartbeat(t, r, "db-backup")

	secret := loadSecret(t, k8sClient, "db-backup-heartbeat")
	secret.Data[tokenSecretKey] = nil
	if err := k8sClient.Update(context.Background(), secret); err != nil {
		t.Fatal(err)
	}

	reconcileHeartbeat(t, r, "db-backup")
	if token := string(loadSecret(t, k8sClient, "db-backup-heartbeat").Data[tokenSecretKey]); !heartbeat.ValidToken(token) {
		t.Fatalf("token was not reminted; got %q", token)
	}
}

func TestHeartbeatAdoptsASuppliedToken(t *testing.T) {
	supplied := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-token", Namespace: "prod"},
		Data:       map[string][]byte{"token": []byte("a-perfectly-fine-token")},
	}
	beat := newHeartbeat("db-backup", func(h *syntheticsv1alpha1.Heartbeat) {
		h.Spec.TokenSecretRef = &syntheticsv1alpha1.TokenSecretRef{Name: "my-token", Key: "token"}
	})
	r, k8sClient, publisher := newHeartbeatReconciler(t, beat, supplied)
	reconcileHeartbeat(t, r, "db-backup")

	if got := publisher.latestSpec().Heartbeat.Token; got != "a-perfectly-fine-token" {
		t.Errorf("published token = %q, want the supplied one", got)
	}
	if got := loadHeartbeat(t, k8sClient).Status.URL; got != testBaseURL+"/a-perfectly-fine-token" {
		t.Errorf("status.url = %q", got)
	}
	// A Secret the user brought is theirs; the operator must not write to it.
	if _, ok := loadSecret(t, k8sClient, "my-token").Data[urlSecretKey]; ok {
		t.Error("operator wrote a url key into a user-owned Secret")
	}
}

// A referenced Secret that doesn't exist yet is a race with the user's own
// apply, so it must requeue rather than fail permanently — and it must not
// leave a stale token live in the receiver.
func TestHeartbeatWithMissingSecretRequeuesAndTombstones(t *testing.T) {
	beat := newHeartbeat("db-backup", func(h *syntheticsv1alpha1.Heartbeat) {
		h.Spec.TokenSecretRef = &syntheticsv1alpha1.TokenSecretRef{Name: "absent", Key: "token"}
	})
	r, k8sClient, publisher := newHeartbeatReconciler(t, beat)

	result := reconcileHeartbeat(t, r, "db-backup")
	if result.RequeueAfter == 0 {
		t.Error("expected a requeue while waiting for the Secret to appear")
	}

	spec := publisher.latestSpec()
	if spec == nil || !spec.Deleted {
		t.Fatalf("expected a tombstone so the receiver stops honouring a token it can't verify; got %+v", spec)
	}

	loaded := loadHeartbeat(t, k8sClient)
	if apimeta.IsStatusConditionTrue(loaded.Status.Conditions, ConditionTokenReady) {
		t.Error("TokenReady should be false when the Secret is missing")
	}
	if loaded.Status.URL != "" {
		t.Errorf("status.url = %q, want empty when no token is available", loaded.Status.URL)
	}
}

func TestHeartbeatRejectsAnUnusableSuppliedToken(t *testing.T) {
	supplied := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-token", Namespace: "prod"},
		Data:       map[string][]byte{"token": []byte("short")},
	}
	beat := newHeartbeat("db-backup", func(h *syntheticsv1alpha1.Heartbeat) {
		h.Spec.TokenSecretRef = &syntheticsv1alpha1.TokenSecretRef{Name: "my-token", Key: "token"}
	})
	r, k8sClient, _ := newHeartbeatReconciler(t, beat, supplied)
	reconcileHeartbeat(t, r, "db-backup")

	condition := apimeta.FindStatusCondition(loadHeartbeat(t, k8sClient).Status.Conditions, ConditionTokenReady)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != reasonTokenInvalid {
		t.Fatalf("expected a TokenInvalid condition, got %+v", condition)
	}
}

func TestHeartbeatDeletionPublishesTombstone(t *testing.T) {
	r, _, publisher := newHeartbeatReconciler(t)
	reconcileHeartbeat(t, r, "gone")

	spec := publisher.latestSpec()
	if spec == nil || !spec.Deleted || spec.Kind != results.KindHeartbeat {
		t.Fatalf("expected a Heartbeat tombstone, got %+v", spec)
	}
}

func TestHeartbeatSuspendIsPublishedAndConditioned(t *testing.T) {
	beat := newHeartbeat("db-backup", func(h *syntheticsv1alpha1.Heartbeat) { h.Spec.Suspend = true })
	r, k8sClient, publisher := newHeartbeatReconciler(t, beat)
	reconcileHeartbeat(t, r, "db-backup")

	if !publisher.latestSpec().Suspend {
		t.Error("expected the published spec to carry suspend")
	}
	if !apimeta.IsStatusConditionTrue(loadHeartbeat(t, k8sClient).Status.Conditions, syntheticsv1alpha1.ConditionSuspended) {
		t.Error("expected the Suspended condition to be true")
	}
}

// Without a base URL the heartbeat still functions, but there is no URL to
// render — status must be honest about that rather than emitting a bare path.
func TestHeartbeatWithoutBaseURLLeavesURLEmpty(t *testing.T) {
	r, k8sClient, _ := newHeartbeatReconciler(t, newHeartbeat("db-backup", nil))
	r.BaseURL = ""
	reconcileHeartbeat(t, r, "db-backup")

	if got := loadHeartbeat(t, k8sClient).Status.URL; got != "" {
		t.Fatalf("status.url = %q, want empty when no base URL is configured", got)
	}
}

func TestHeartbeatSecretURLFollowsBaseURLChange(t *testing.T) {
	r, k8sClient, _ := newHeartbeatReconciler(t, newHeartbeat("db-backup", nil))
	reconcileHeartbeat(t, r, "db-backup")

	r.BaseURL = "https://hb.example.org"
	reconcileHeartbeat(t, r, "db-backup")

	secret := loadSecret(t, k8sClient, "db-backup-heartbeat")
	want := "https://hb.example.org/" + string(secret.Data[tokenSecretKey])
	if got := string(secret.Data[urlSecretKey]); got != want {
		t.Errorf("Secret url = %q, want %q", got, want)
	}
}

// The Secret must be garbage-collected with its Heartbeat; an orphaned token
// Secret per deleted heartbeat accumulates silently.
func TestHeartbeatSecretIsOwnedByTheHeartbeat(t *testing.T) {
	r, k8sClient, _ := newHeartbeatReconciler(t, newHeartbeat("db-backup", nil))
	reconcileHeartbeat(t, r, "db-backup")

	owners := loadSecret(t, k8sClient, "db-backup-heartbeat").OwnerReferences
	if len(owners) != 1 || owners[0].Kind != "Heartbeat" || owners[0].Name != "db-backup" {
		t.Fatalf("owner references = %+v, want a single Heartbeat/db-backup owner", owners)
	}
}

func TestHeartbeatSpecUpdateReplaysStatusForReseeding(t *testing.T) {
	pinged := metav1.NewTime(time.Unix(1700000000, 0))
	beat := newHeartbeat("db-backup", func(h *syntheticsv1alpha1.Heartbeat) {
		h.Status.LastPingTime = &pinged
		h.Status.LastResult = syntheticsv1alpha1.HeartbeatResultFailed
	})

	spec := heartbeatSpecUpdate(beat, "hb_token")
	if spec.Heartbeat.LastPingUnix != pinged.Unix() {
		t.Errorf("LastPingUnix = %d, want %d", spec.Heartbeat.LastPingUnix, pinged.Unix())
	}
	if spec.Heartbeat.LastResult != syntheticsv1alpha1.HeartbeatResultFailed {
		t.Errorf("LastResult = %q, want failed", spec.Heartbeat.LastResult)
	}
}
