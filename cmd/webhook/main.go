// Command webhook serves admission webhooks for the four CRDs. Stateless;
// reads its serving certificate from the Secret written by the controller.
// No reconcilers, probe execution, or metrics here.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
	internalwebhookcerts "github.com/loks0n/synthetics-operator/internal/webhookcerts"
)

func main() {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(syntheticsv1alpha1.AddToScheme(scheme))

	var (
		webhookPort                    int
		webhookNamespace               string
		webhookServiceName             string
		webhookSecretName              string
		validatingWebhookConfiguration string
		mutatingWebhookConfiguration   string
	)

	flag.IntVar(&webhookPort, "webhook-port", 9443, "Webhook server port.")
	flag.StringVar(&webhookNamespace, "webhook-namespace", namespaceFromEnv(), "Namespace containing the webhook certificate secret.")
	flag.StringVar(&webhookServiceName, "webhook-service-name", "synthetics-operator-webhook-service", "Webhook service name used in serving certificates.")
	flag.StringVar(&webhookSecretName, "webhook-secret-name", "synthetics-webhook-certs", "Secret name for self-managed webhook certificates.")
	flag.StringVar(&validatingWebhookConfiguration, "validating-webhook-configuration", "synthetics-operator-validating-webhook-configuration", "ValidatingWebhookConfiguration name.")
	flag.StringVar(&mutatingWebhookConfiguration, "mutating-webhook-configuration", "synthetics-operator-mutating-webhook-configuration", "MutatingWebhookConfiguration name.")
	flag.Parse()

	ctrl.SetLogger(logr.FromSlogHandler(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	log := ctrl.Log.WithName("webhook")

	if err := run(scheme, log, webhookPort, webhookNamespace, webhookSecretName, webhookServiceName, validatingWebhookConfiguration, mutatingWebhookConfiguration); err != nil {
		log.Error(err, "exiting")
		os.Exit(1)
	}
}

func run(
	scheme *runtime.Scheme,
	log logr.Logger,
	webhookPort int,
	webhookNamespace, webhookSecretName, webhookServiceName,
	validatingCfg, mutatingCfg string,
) error {
	cfg := ctrl.GetConfigOrDie()
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("kubernetes clientset: %w", err)
	}
	certManager := internalwebhookcerts.NewManager(
		log.WithName("webhook-certs"),
		clientset,
		webhookNamespace,
		webhookSecretName,
		webhookServiceName,
		"",
		validatingCfg,
		mutatingCfg,
	)
	if err := certManager.InitializeReadOnly(context.Background()); err != nil {
		return fmt.Errorf("loading webhook certificates: %w", err)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: ":8081",
		Metrics:                metricsserver.Options{BindAddress: "0"},
		WebhookServer: webhook.NewServer(webhook.Options{
			Port: webhookPort,
			TLSOpts: []func(*tls.Config){
				func(c *tls.Config) { c.GetCertificate = certManager.GetCertificate },
			},
		}),
	})
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}

	for _, setup := range []func(ctrl.Manager) error{
		syntheticsv1alpha1.SetupWebhookWithManager,
		syntheticsv1alpha1.SetupDNSWebhookWithManager,
		syntheticsv1alpha1.SetupK6TestWebhookWithManager,
		syntheticsv1alpha1.SetupPlaywrightTestWebhookWithManager,
	} {
		if err := setup(mgr); err != nil {
			return fmt.Errorf("register webhook: %w", err)
		}
	}

	if err := mgr.Add(certManager); err != nil {
		return fmt.Errorf("adding cert watcher: %w", err)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("healthz: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("readyz: %w", err)
	}

	log.Info("starting webhook")
	return mgr.Start(ctrl.SetupSignalHandler())
}

func namespaceFromEnv() string {
	for _, key := range []string{"POD_NAMESPACE", "NAMESPACE"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return "default"
}
