package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
	"github.com/loks0n/synthetics-operator/controllers"
	internalmetrics "github.com/loks0n/synthetics-operator/internal/metrics"
	"github.com/loks0n/synthetics-operator/internal/natsconsumer"
	internalprobes "github.com/loks0n/synthetics-operator/internal/probes"
	internalwebhookcerts "github.com/loks0n/synthetics-operator/internal/webhookcerts"
)

func main() {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(syntheticsv1alpha1.AddToScheme(scheme))

	var metricsAddr string
	var probeConcurrency int
	var webhookPort int
	var webhookOnly bool
	var enableLeaderElection bool
	var webhookNamespace string
	var webhookServiceName string
	var webhookSecretName string
	var validatingWebhookConfiguration string
	var mutatingWebhookConfiguration string
	var natsURL string
	var testSidecarImage string
	var k6RunnerImage string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metrics endpoint binds to.")
	flag.IntVar(&probeConcurrency, "probe-concurrency", 4, "Number of concurrent HTTP probes.")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "Webhook server port.")
	flag.BoolVar(&webhookOnly, "webhook-only", false, "Run only the webhook server (no reconciler, no probe workers, no cert rotation).")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	flag.StringVar(&webhookNamespace, "webhook-namespace", namespaceFromEnv(), "Namespace containing the webhook service and certificate secret.")
	flag.StringVar(&webhookServiceName, "webhook-service-name", "synthetics-operator-webhook-service", "Webhook service name used in serving certificates.")
	flag.StringVar(&webhookSecretName, "webhook-secret-name", "synthetics-webhook-certs", "Secret name for self-managed webhook certificates.")
	flag.StringVar(&validatingWebhookConfiguration, "validating-webhook-configuration", "synthetics-operator-validating-webhook-configuration", "ValidatingWebhookConfiguration name to inject with the CA bundle.")
	flag.StringVar(&mutatingWebhookConfiguration, "mutating-webhook-configuration", "synthetics-operator-mutating-webhook-configuration", "MutatingWebhookConfiguration name to inject with the CA bundle.")
	flag.StringVar(&natsURL, "nats-url", "", "NATS server URL for consuming CronJob probe results (e.g. nats://nats:4222). Empty disables NATS.")
	flag.StringVar(&testSidecarImage, "test-sidecar-image", "", "Image for the test-sidecar container in K6Test jobs.")
	flag.StringVar(&k6RunnerImage, "k6-runner-image", "", "Image for the k6-runner init container in K6Test jobs.")
	flag.Parse()

	ctrl.SetLogger(logr.FromSlogHandler(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	cfg := ctrl.GetConfigOrDie()
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		ctrl.Log.WithName("setup").Error(err, "unable to create kubernetes clientset")
		os.Exit(1)
	}

	certManager := internalwebhookcerts.NewManager(
		ctrl.Log.WithName("webhook-certs"),
		clientset,
		webhookNamespace,
		webhookSecretName,
		webhookServiceName,
		"",
		validatingWebhookConfiguration,
		mutatingWebhookConfiguration,
	)

	var runErr error
	if webhookOnly {
		runErr = runWebhookOnly(cfg, scheme, certManager, webhookPort)
	} else {
		runErr = runController(cfg, scheme, certManager, metricsAddr, natsURL, testSidecarImage, k6RunnerImage, probeConcurrency, enableLeaderElection)
	}
	if runErr != nil {
		ctrl.Log.WithName("setup").Error(runErr, "manager exited with error")
		os.Exit(1)
	}
}

// runWebhookOnly starts only the webhook server. It reads the cert Secret
// written by the controller deployment and watches it for hot-reload. No
// reconcilers, probe workers, metrics server, or cert rotation run here.
func runWebhookOnly(cfg *rest.Config, scheme *runtime.Scheme, certManager *internalwebhookcerts.Manager, webhookPort int) error {
	if err := certManager.InitializeReadOnly(context.Background()); err != nil {
		return fmt.Errorf("loading webhook certificates: %w", err)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: ":8081",
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
		WebhookServer: webhook.NewServer(webhook.Options{
			Port: webhookPort,
			TLSOpts: []func(*tls.Config){
				func(c *tls.Config) {
					c.GetCertificate = certManager.GetCertificate
				},
			},
		}),
	})
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}

	if err := syntheticsv1alpha1.SetupWebhookWithManager(mgr); err != nil {
		return fmt.Errorf("registering HTTPProbe webhook: %w", err)
	}

	if err := syntheticsv1alpha1.SetupDNSWebhookWithManager(mgr); err != nil {
		return fmt.Errorf("registering DNSProbe webhook: %w", err)
	}

	if err := syntheticsv1alpha1.SetupK6TestWebhookWithManager(mgr); err != nil {
		return fmt.Errorf("registering K6Test webhook: %w", err)
	}

	if err := mgr.Add(certManager); err != nil {
		return fmt.Errorf("adding cert watcher: %w", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("setting up health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("setting up ready check: %w", err)
	}

	ctrl.Log.WithName("setup").Info("starting webhook-only manager")
	return mgr.Start(ctrl.SetupSignalHandler())
}

// runController starts the full operator: cert rotation, metrics server,
// probe scheduler, and reconcilers. It does not serve webhooks — those run
// in a separate synthetics-operator-webhook deployment.
func runController(
	cfg *rest.Config,
	scheme *runtime.Scheme,
	certManager *internalwebhookcerts.Manager,
	metricsAddr, natsURL, testSidecarImage, k6RunnerImage string,
	probeConcurrency int,
	enableLeaderElection bool,
) error {
	if err := certManager.Initialize(context.Background()); err != nil {
		return fmt.Errorf("initializing webhook certificates: %w", err)
	}

	store, err := internalmetrics.NewStore()
	if err != nil {
		return fmt.Errorf("creating metrics store: %w", err)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "synthetics-operator.synthetics.dev",
		HealthProbeBindAddress: ":8081",
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
	})
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}

	if err := mgr.Add(store.Server(metricsAddr)); err != nil {
		return fmt.Errorf("adding metrics server: %w", err)
	}

	if err := mgr.Add(certManager); err != nil {
		return fmt.Errorf("adding cert manager: %w", err)
	}

	if natsURL != "" {
		consumer := natsconsumer.New(ctrl.Log.WithName("nats-consumer"), natsURL, store)
		if err := mgr.Add(consumer); err != nil {
			return fmt.Errorf("adding nats consumer: %w", err)
		}
	}

	scheduler := internalprobes.NewScheduler(
		ctrl.Log.WithName("scheduler"),
		internalprobes.NewWorkerPool(
			ctrl.Log.WithName("workers"),
			probeConcurrency,
		),
	)
	if err := mgr.Add(scheduler); err != nil {
		return fmt.Errorf("adding scheduler: %w", err)
	}

	reconciler := &controllers.HTTPProbeReconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		Scheduler:    scheduler,
		HTTPExecutor: internalprobes.HTTPExecutor{},
		Metrics:      store,
		Clock:        time.Now,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("creating HTTPProbe controller: %w", err)
	}

	dnsReconciler := &controllers.DNSProbeReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		Scheduler:   scheduler,
		DNSExecutor: internalprobes.DNSExecutor{},
		Metrics:     store,
		Clock:       time.Now,
	}
	if err := dnsReconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("creating DNSProbe controller: %w", err)
	}

	k6Reconciler := &controllers.K6TestReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		Clock:            time.Now,
		NATSUrl:          natsURL,
		TestSidecarImage: testSidecarImage,
		K6RunnerImage:    k6RunnerImage,
	}
	if err := k6Reconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("creating K6Test controller: %w", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("setting up health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("setting up ready check: %w", err)
	}

	ctrl.Log.WithName("setup").Info("starting manager")
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
