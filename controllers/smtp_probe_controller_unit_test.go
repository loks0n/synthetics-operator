package controllers

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
	"github.com/loks0n/synthetics-operator/internal/results"
)

func TestSMTPProbeReconcilePublishesResolvedTLSMode(t *testing.T) {
	submission := &syntheticsv1alpha1.SMTPProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "submission", Namespace: "mail"},
		Spec: syntheticsv1alpha1.SMTPProbeSpec{
			Interval: metav1.Duration{Duration: time.Minute},
			Timeout:  metav1.Duration{Duration: 5 * time.Second},
			// Mode left empty, as on an object admitted before defaulting existed.
			Target: syntheticsv1alpha1.TCPTarget{Host: "smtp.example.com", Port: 587},
			TLS:    syntheticsv1alpha1.SMTPTLSConfig{TLSConfig: syntheticsv1alpha1.TLSConfig{InsecureSkipVerify: true}},
		},
	}
	cl := fakeclient.NewClientBuilder().WithScheme(unitScheme()).WithStatusSubresource(submission).WithObjects(submission).Build()
	scheduler := newFakeScheduler()
	publisher := &fakePublisher{}
	reconciler := &SMTPProbeReconciler{Client: cl, Scheme: unitScheme(), Scheduler: scheduler, Publisher: publisher, Clock: time.Now}
	key := types.NamespacedName{Namespace: "mail", Name: "submission"}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if !scheduler.isActive(results.KindSMTPProbe, key) {
		t.Fatal("SMTPProbe was not scheduled")
	}
	published := publisher.latestSpec()
	if published == nil || published.SMTPProbe == nil {
		t.Fatalf("no SMTPProbe payload published: %+v", published)
	}
	payload := published.SMTPProbe
	if payload.TLSMode != "starttls" || payload.TLS == nil || !payload.TLS.InsecureSkipVerify || payload.TimeoutMs != 5000 {
		t.Fatalf("payload does not carry the resolved TLS policy: %+v", payload)
	}

	if err := cl.Delete(context.Background(), submission); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if scheduler.isActive(results.KindSMTPProbe, key) {
		t.Fatal("deleted SMTPProbe stayed scheduled")
	}
	if tombstone := publisher.latestSpec(); tombstone == nil || !tombstone.Deleted || tombstone.Kind != results.KindSMTPProbe {
		t.Fatalf("unexpected tombstone: %+v", tombstone)
	}
}

// TestSMTPProbeEnvtestAdmitsAndReconciles runs the SMTPProbe webhook and
// reconciler against a real API server: the object the reconciler sees is the
// one admission defaulted, and status lands through the status subresource.
func TestSMTPProbeEnvtestAdmitsAndReconciles(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run make test-envtest")
	}
	testEnv := &envtest.Environment{
		CRDDirectoryPaths: []string{filepath.Join("..", "config", "crd", "bases")},
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			Paths: []string{filepath.Join("..", "config", "webhook", "manifests.yaml")},
		},
	}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = testEnv.Stop() })

	skipNameValidation := true
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 unitScheme(),
		HealthProbeBindAddress: "0",
		Metrics:                metricsserver.Options{BindAddress: "0"},
		Controller:             config.Controller{SkipNameValidation: &skipNameValidation},
		WebhookServer: webhook.NewServer(webhook.Options{
			Host:    testEnv.WebhookInstallOptions.LocalServingHost,
			Port:    testEnv.WebhookInstallOptions.LocalServingPort,
			CertDir: testEnv.WebhookInstallOptions.LocalServingCertDir,
		}),
	})
	if err != nil {
		t.Fatalf("create manager: %v", err)
	}
	if err := syntheticsv1alpha1.SetupSMTPProbeWebhookWithManager(mgr); err != nil {
		t.Fatalf("setup SMTP webhook: %v", err)
	}
	scheduler := newFakeScheduler()
	publisher := &fakePublisher{}
	reconciler := &SMTPProbeReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Scheduler: scheduler, Publisher: publisher, Clock: time.Now}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		t.Fatalf("setup SMTP reconciler: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()

	k8sClient, err := client.New(cfg, client.Options{Scheme: unitScheme()})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "mail"}}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	key := types.NamespacedName{Namespace: "mail", Name: "submission"}
	submission := &syntheticsv1alpha1.SMTPProbe{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		// No mode, interval or timeout: admission fills them in.
		Spec: syntheticsv1alpha1.SMTPProbeSpec{Target: syntheticsv1alpha1.TCPTarget{Host: " smtp.example.com ", Port: 587}},
	}
	// The webhook server comes up with the manager, so the first creates can
	// fail to connect.
	eventually(t, "SMTPProbe admitted", func() bool { return k8sClient.Create(ctx, submission.DeepCopy()) == nil })

	eventually(t, "SMTPProbe scheduled", func() bool { return scheduler.isActive(results.KindSMTPProbe, key) })
	published := publisher.latestSpec()
	if published == nil || published.SMTPProbe == nil {
		t.Fatalf("no SMTPProbe payload published: %+v", published)
	}
	if payload := published.SMTPProbe; payload.TLSMode != "starttls" || payload.Host != "smtp.example.com" || payload.TimeoutMs == 0 {
		t.Fatalf("published payload does not carry the admitted defaults: %+v", payload)
	}

	var persisted syntheticsv1alpha1.SMTPProbe
	eventually(t, "status written", func() bool {
		if k8sClient.Get(ctx, key, &persisted) != nil {
			return false
		}
		condition := apimeta.FindStatusCondition(persisted.Status.Conditions, syntheticsv1alpha1.ConditionSuspended)
		return persisted.Status.ObservedGeneration == persisted.Generation && condition != nil && condition.Status == metav1.ConditionFalse
	})

	if persisted.Spec.TLS.Mode != syntheticsv1alpha1.SMTPTLSModeStartTLS || persisted.Spec.Target.Host != "smtp.example.com" {
		t.Fatalf("admission did not default the stored object: %+v", persisted.Spec)
	}

	suspended := persisted.DeepCopy()
	suspended.Spec.Suspend = true
	if err := k8sClient.Patch(ctx, suspended, client.MergeFrom(&persisted)); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	eventually(t, "suspended SMTPProbe unscheduled", func() bool {
		if scheduler.isActive(results.KindSMTPProbe, key) || k8sClient.Get(ctx, key, &persisted) != nil {
			return false
		}
		return apimeta.IsStatusConditionTrue(persisted.Status.Conditions, syntheticsv1alpha1.ConditionSuspended)
	})

	if err := k8sClient.Delete(ctx, &persisted); err != nil {
		t.Fatalf("delete: %v", err)
	}
	eventually(t, "tombstone published", func() bool {
		tombstone := publisher.latestSpec()
		return tombstone != nil && tombstone.Deleted && tombstone.Kind == results.KindSMTPProbe && tombstone.Name == key.Name
	})
	if scheduler.isActive(results.KindSMTPProbe, key) {
		t.Fatal("deleted SMTPProbe stayed scheduled")
	}
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
