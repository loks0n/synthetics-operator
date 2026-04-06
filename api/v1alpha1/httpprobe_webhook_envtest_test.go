package v1alpha1

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

func TestHTTPProbeWebhookRejectsInvalidMethod(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set")
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
		t.Fatalf("start envtest: %v", err)
	}
	defer func() {
		_ = testEnv.Stop()
	}()

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: "0",
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
		WebhookServer: webhook.NewServer(webhook.Options{
			Host:    testEnv.WebhookInstallOptions.LocalServingHost,
			Port:    testEnv.WebhookInstallOptions.LocalServingPort,
			CertDir: testEnv.WebhookInstallOptions.LocalServingCertDir,
		}),
	})
	if err != nil {
		t.Fatalf("create manager: %v", err)
	}

	if err := SetupWebhookWithManager(mgr); err != nil {
		t.Fatalf("setup webhook: %v", err)
	}

	ctx := t.Context()
	go func() {
		_ = mgr.Start(ctx)
	}()

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace: %v", err)
	}

	probe := &HTTPProbe{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "invalid-method",
			Namespace: "default",
		},
		Spec: HTTPProbeSpec{
			Interval: metav1.Duration{Duration: 30 * time.Second},
			Timeout:  metav1.Duration{Duration: 10 * time.Second},
			Request: HTTPRequestSpec{
				URL:    "http://127.0.0.1/health",
				Method: "POST",
			},
			Assertions: HTTPAssertions{Status: 200},
		},
	}

	var createErr error
	for range 20 {
		createErr = k8sClient.Create(ctx, probe.DeepCopy())
		if createErr == nil {
			t.Fatal("expected webhook rejection, create succeeded")
		}
		if !strings.Contains(createErr.Error(), "connect") && !strings.Contains(createErr.Error(), "EOF") {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}

	if createErr == nil {
		t.Fatal("expected webhook rejection, got nil error")
	}
	if !strings.Contains(createErr.Error(), "supported values") && !strings.Contains(createErr.Error(), "Unsupported value") {
		t.Fatalf("expected webhook validation error, got %v", createErr)
	}
}
