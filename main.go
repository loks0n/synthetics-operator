package main

import (
	"context"
	"crypto/tls"
	"flag"
	"log/slog"
	"os"
	"strings"
	"time"

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
	"github.com/loks0n/synthetics-operator/controllers"
	internalmetrics "github.com/loks0n/synthetics-operator/internal/metrics"
	internalprobes "github.com/loks0n/synthetics-operator/internal/probes"
	internalwebhookcerts "github.com/loks0n/synthetics-operator/internal/webhookcerts"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(syntheticsv1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeConcurrency int
	var webhookPort int
	var enableLeaderElection bool
	var webhookNamespace string
	var webhookServiceName string
	var webhookSecretName string
	var validatingWebhookConfiguration string
	var mutatingWebhookConfiguration string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metrics endpoint binds to.")
	flag.IntVar(&probeConcurrency, "probe-concurrency", 4, "Number of concurrent HTTP probes.")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "Webhook server port.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	flag.StringVar(&webhookNamespace, "webhook-namespace", namespaceFromEnv(), "Namespace containing the webhook service and certificate secret.")
	flag.StringVar(&webhookServiceName, "webhook-service-name", "synthetics-operator-webhook-service", "Webhook service name used in serving certificates.")
	flag.StringVar(&webhookSecretName, "webhook-secret-name", "synthetics-webhook-certs", "Secret name for self-managed webhook certificates.")
	flag.StringVar(&validatingWebhookConfiguration, "validating-webhook-configuration", "synthetics-operator-validating-webhook-configuration", "ValidatingWebhookConfiguration name to inject with the CA bundle.")
	flag.StringVar(&mutatingWebhookConfiguration, "mutating-webhook-configuration", "synthetics-operator-mutating-webhook-configuration", "MutatingWebhookConfiguration name to inject with the CA bundle.")
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
	if err := certManager.Initialize(context.Background()); err != nil {
		ctrl.Log.WithName("setup").Error(err, "unable to initialize webhook certificates")
		os.Exit(1)
	}

	store, err := internalmetrics.NewStore()
	if err != nil {
		ctrl.Log.WithName("setup").Error(err, "unable to create metrics store")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "synthetics-operator.synthetics.dev",
		HealthProbeBindAddress: ":8081",
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
		WebhookServer: webhook.NewServer(webhook.Options{
			Port: webhookPort,
			TLSOpts: []func(*tls.Config){
				func(cfg *tls.Config) {
					cfg.GetCertificate = certManager.GetCertificate
				},
			},
		}),
	})
	if err != nil {
		ctrl.Log.WithName("setup").Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := syntheticsv1alpha1.SetupWebhookWithManager(mgr); err != nil {
		ctrl.Log.WithName("setup").Error(err, "unable to create webhook", "webhook", "HTTPProbe")
		os.Exit(1)
	}

	if err := mgr.Add(store.Server(metricsAddr)); err != nil {
		ctrl.Log.WithName("setup").Error(err, "unable to add metrics server")
		os.Exit(1)
	}

	if err := mgr.Add(certManager); err != nil {
		ctrl.Log.WithName("setup").Error(err, "unable to add webhook certificate manager")
		os.Exit(1)
	}

	scheduler := internalprobes.NewScheduler(
		ctrl.Log.WithName("scheduler"),
		internalprobes.HTTPExecutor{},
		internalprobes.NewWorkerPool(
			ctrl.Log.WithName("workers"),
			probeConcurrency,
			store,
		),
	)
	if err := mgr.Add(scheduler); err != nil {
		ctrl.Log.WithName("setup").Error(err, "unable to add scheduler")
		os.Exit(1)
	}

	reconciler := &controllers.HTTPProbeReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Scheduler: scheduler,
		Metrics:   store,
		Clock:     time.Now,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		ctrl.Log.WithName("setup").Error(err, "unable to create controller", "controller", "HTTPProbe")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		ctrl.Log.WithName("setup").Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		ctrl.Log.WithName("setup").Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	ctrl.Log.WithName("setup").Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.WithName("setup").Error(err, "problem running manager")
		os.Exit(1)
	}
}

func namespaceFromEnv() string {
	for _, key := range []string{"POD_NAMESPACE", "NAMESPACE"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return "default"
}
