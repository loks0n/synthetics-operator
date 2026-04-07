package v1alpha1

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

var webhookClient client.Client

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		m.Run() // unit tests still run; envtest tests self-skip
		return
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(AddToScheme(scheme))

	testEnv := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),
		},
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			Paths: []string{
				filepath.Join("..", "..", "config", "webhook", "manifests.yaml"),
			},
		},
	}

	cfg, err := testEnv.Start()
	if err != nil {
		panic("start envtest: " + err.Error())
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: "0",
		Metrics:                metricsserver.Options{BindAddress: "0"},
		WebhookServer: webhook.NewServer(webhook.Options{
			Host:    testEnv.WebhookInstallOptions.LocalServingHost,
			Port:    testEnv.WebhookInstallOptions.LocalServingPort,
			CertDir: testEnv.WebhookInstallOptions.LocalServingCertDir,
		}),
	})
	if err != nil {
		panic("create manager: " + err.Error())
	}

	if err := SetupWebhookWithManager(mgr); err != nil {
		panic("setup webhook: " + err.Error())
	}

	mgrCtx, cancel := context.WithCancel(ctrl.SetupSignalHandler())
	go func() { _ = mgr.Start(mgrCtx) }()

	webhookClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		panic("create client: " + err.Error())
	}
	if err := webhookClient.Create(mgrCtx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}); err != nil && !apierrors.IsAlreadyExists(err) {
		panic("create namespace: " + err.Error())
	}

	code := m.Run()
	cancel()
	_ = testEnv.Stop()
	os.Exit(code)
}

// waitForWebhook retries until the webhook server accepts connections.
func waitForWebhook(t *testing.T, create func() error) error {
	t.Helper()
	var err error
	for range 20 {
		err = create()
		if err == nil || (!strings.Contains(err.Error(), "connect") && !strings.Contains(err.Error(), "EOF")) {
			return err
		}
		time.Sleep(250 * time.Millisecond)
	}
	return err
}

func TestWebhookAcceptsValidProbe(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set")
	}

	probe := &HTTPProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "valid-probe", Namespace: "default"},
		Spec: HTTPProbeSpec{
			Interval: metav1.Duration{Duration: 30 * time.Second},
			Timeout:  metav1.Duration{Duration: 10 * time.Second},
			Request:  HTTPRequestSpec{URL: "https://example.com/health", Method: "GET"},
		},
	}

	if err := waitForWebhook(t, func() error {
		return webhookClient.Create(t.Context(), probe.DeepCopy())
	}); err != nil {
		t.Fatalf("expected valid probe to be accepted, got: %v", err)
	}
}

func TestWebhookAppliesDefaults(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set")
	}

	minimal := &HTTPProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "defaulted-probe", Namespace: "default"},
		Spec: HTTPProbeSpec{
			Request: HTTPRequestSpec{URL: "https://example.com/health"},
		},
	}

	if err := waitForWebhook(t, func() error {
		return webhookClient.Create(t.Context(), minimal.DeepCopy())
	}); err != nil {
		t.Fatalf("expected minimal probe to be accepted, got: %v", err)
	}

	persisted := &HTTPProbe{}
	if err := webhookClient.Get(t.Context(), types.NamespacedName{Name: "defaulted-probe", Namespace: "default"}, persisted); err != nil {
		t.Fatalf("get probe: %v", err)
	}

	if persisted.Spec.Interval.Duration != 30*time.Second {
		t.Errorf("expected default interval 30s, got %v", persisted.Spec.Interval.Duration)
	}
	if persisted.Spec.Timeout.Duration != 10*time.Second {
		t.Errorf("expected default timeout 10s, got %v", persisted.Spec.Timeout.Duration)
	}
	if persisted.Spec.Request.Method != http.MethodGet {
		t.Errorf("expected default method GET, got %q", persisted.Spec.Request.Method)
	}
}

func TestWebhookRejectsInvalidMethod(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set")
	}

	probe := &HTTPProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-method", Namespace: "default"},
		Spec: HTTPProbeSpec{
			Interval: metav1.Duration{Duration: 30 * time.Second},
			Timeout:  metav1.Duration{Duration: 10 * time.Second},
			Request:  HTTPRequestSpec{URL: "http://127.0.0.1/health", Method: "DELETE"},
		},
	}

	err := waitForWebhook(t, func() error {
		return webhookClient.Create(t.Context(), probe.DeepCopy())
	})
	if err == nil {
		t.Fatal("expected webhook to reject POST method, got nil error")
	}
	if !strings.Contains(err.Error(), "Unsupported value") && !strings.Contains(err.Error(), "supported values") {
		t.Fatalf("expected unsupported value error, got: %v", err)
	}
}
