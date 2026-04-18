// Command controller runs the reconciler, scheduler, and transition-event
// notifier. Watches the four CRDs, publishes spec snapshots + probe jobs to
// NATS. Does not execute probes or serve /metrics — those live in the
// probe-worker and metrics binaries.
package main

import (
	"context"
	"errors"
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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
	"github.com/loks0n/synthetics-operator/controllers"
	internalevents "github.com/loks0n/synthetics-operator/internal/events"
	internalmetrics "github.com/loks0n/synthetics-operator/internal/metrics"
	"github.com/loks0n/synthetics-operator/internal/metricsconsumer"
	"github.com/loks0n/synthetics-operator/internal/natsbus"
	internalprobes "github.com/loks0n/synthetics-operator/internal/probes"
	internalwebhookcerts "github.com/loks0n/synthetics-operator/internal/webhookcerts"
)

func main() {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(syntheticsv1alpha1.AddToScheme(scheme))

	var (
		enableLeaderElection           bool
		webhookNamespace               string
		webhookServiceName             string
		webhookSecretName              string
		validatingWebhookConfiguration string
		mutatingWebhookConfiguration   string
		natsURL                        string
		testSidecarImage               string
		k6RunnerImage                  string
		playwrightRunnerImage          string
	)

	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for the controller manager.")
	flag.StringVar(&webhookNamespace, "webhook-namespace", namespaceFromEnv(), "Namespace containing the webhook service and certificate secret.")
	flag.StringVar(&webhookServiceName, "webhook-service-name", "synthetics-operator-webhook-service", "Webhook service name used in serving certificates.")
	flag.StringVar(&webhookSecretName, "webhook-secret-name", "synthetics-webhook-certs", "Secret name for self-managed webhook certificates.")
	flag.StringVar(&validatingWebhookConfiguration, "validating-webhook-configuration", "synthetics-operator-validating-webhook-configuration", "ValidatingWebhookConfiguration name to inject with the CA bundle.")
	flag.StringVar(&mutatingWebhookConfiguration, "mutating-webhook-configuration", "synthetics-operator-mutating-webhook-configuration", "MutatingWebhookConfiguration name to inject with the CA bundle.")
	flag.StringVar(&natsURL, "nats-url", "", "NATS server URL (required).")
	flag.StringVar(&testSidecarImage, "test-sidecar-image", "", "Image for the test-sidecar container in K6Test/PlaywrightTest jobs.")
	flag.StringVar(&k6RunnerImage, "k6-runner-image", "", "Image for the k6-runner init container.")
	flag.StringVar(&playwrightRunnerImage, "playwright-runner-image", "", "Image for the playwright-runner container.")
	flag.Parse()

	ctrl.SetLogger(logr.FromSlogHandler(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	log := ctrl.Log.WithName("controller")

	if err := run(scheme, log, enableLeaderElection, webhookNamespace, webhookSecretName, webhookServiceName, validatingWebhookConfiguration, mutatingWebhookConfiguration, natsURL, testSidecarImage, k6RunnerImage, playwrightRunnerImage); err != nil {
		log.Error(err, "exiting")
		os.Exit(1)
	}
}

func run(
	scheme *runtime.Scheme,
	log logr.Logger,
	enableLeaderElection bool,
	webhookNamespace, webhookSecretName, webhookServiceName,
	validatingWebhookConfiguration, mutatingWebhookConfiguration,
	natsURL, testSidecarImage, k6RunnerImage, playwrightRunnerImage string,
) error {
	if natsURL == "" {
		return errors.New("--nats-url is required")
	}

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
		validatingWebhookConfiguration,
		mutatingWebhookConfiguration,
	)
	if err := certManager.Initialize(context.Background()); err != nil {
		return fmt.Errorf("initializing webhook certificates: %w", err)
	}

	bus, err := natsbus.Connect(log.WithName("nats"), natsURL)
	if err != nil {
		return fmt.Errorf("connecting NATS: %w", err)
	}
	defer bus.Close()

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "synthetics-operator.synthetics.dev",
		HealthProbeBindAddress: ":8081",
		Metrics:                metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}

	if err := mgr.Add(certManager); err != nil {
		return fmt.Errorf("adding cert manager: %w", err)
	}

	// Event notifier needs a metrics store internally to detect transitions,
	// but this binary does not serve /metrics — the metrics binary does.
	store, err := internalmetrics.NewStore()
	if err != nil {
		return fmt.Errorf("creating event-notifier store: %w", err)
	}
	notifier := internalevents.New(mgr.GetClient(), mgr.GetEventRecorderFor("synthetics-operator"))
	store.OnProbeTransition = notifier.OnProbeTransition
	store.OnTestTransition = notifier.OnTestTransition
	eventConsumer := &metricsconsumer.Consumer{
		Log:   log.WithName("event-consumer"),
		Bus:   bus,
		Store: store,
	}
	if err := mgr.Add(eventConsumer); err != nil {
		return fmt.Errorf("adding event consumer: %w", err)
	}

	scheduler := internalprobes.NewScheduler(log.WithName("scheduler"), bus)
	if err := mgr.Add(scheduler); err != nil {
		return fmt.Errorf("adding scheduler: %w", err)
	}

	if err := (&controllers.HTTPProbeReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Scheduler: scheduler,
		Publisher: bus,
		Clock:     time.Now,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("creating HTTPProbe controller: %w", err)
	}

	if err := (&controllers.DNSProbeReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Scheduler: scheduler,
		Publisher: bus,
		Clock:     time.Now,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("creating DNSProbe controller: %w", err)
	}

	if err := (&controllers.K6TestReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		Publisher:        bus,
		Clock:            time.Now,
		NATSUrl:          natsURL,
		TestSidecarImage: testSidecarImage,
		K6RunnerImage:    k6RunnerImage,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("creating K6Test controller: %w", err)
	}

	if err := (&controllers.PlaywrightTestReconciler{
		Client:                mgr.GetClient(),
		Scheme:                mgr.GetScheme(),
		Publisher:             bus,
		Clock:                 time.Now,
		NATSUrl:               natsURL,
		TestSidecarImage:      testSidecarImage,
		PlaywrightRunnerImage: playwrightRunnerImage,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("creating PlaywrightTest controller: %w", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("healthz: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("readyz: %w", err)
	}

	log.Info("starting controller")
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
