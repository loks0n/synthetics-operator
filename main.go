package main

import (
	"context"
	"crypto/tls"
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
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
	"github.com/loks0n/synthetics-operator/controllers"
	internalevents "github.com/loks0n/synthetics-operator/internal/events"
	internalmetrics "github.com/loks0n/synthetics-operator/internal/metrics"
	"github.com/loks0n/synthetics-operator/internal/metricsconsumer"
	"github.com/loks0n/synthetics-operator/internal/natsbus"
	internalprobes "github.com/loks0n/synthetics-operator/internal/probes"
	"github.com/loks0n/synthetics-operator/internal/probeworker"
	internalwebhookcerts "github.com/loks0n/synthetics-operator/internal/webhookcerts"
)

// Role selects the binary's behaviour. Phase 14 splits the single operator
// into three stateless deployments plus the existing webhook split.
type Role string

const (
	RoleController     Role = "controller"      // reconciler + scheduler + event notifier
	RoleWebhook        Role = "webhook"         // admission webhook server only
	RoleProbeWorker    Role = "probe-worker"    // NATS-driven HTTP/DNS probe execution
	RoleMetricsConsumer Role = "metrics"        // NATS-subscribed Prometheus endpoint
)

func main() {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(syntheticsv1alpha1.AddToScheme(scheme))

	var role string
	var metricsAddr string
	var webhookPort int
	var enableLeaderElection bool
	var webhookNamespace string
	var webhookServiceName string
	var webhookSecretName string
	var validatingWebhookConfiguration string
	var mutatingWebhookConfiguration string
	var natsURL string
	var testSidecarImage string
	var k6RunnerImage string
	var playwrightRunnerImage string

	flag.StringVar(&role, "role", "controller", "Binary role: controller, webhook, probe-worker, metrics.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the /metrics endpoint binds to (metrics role).")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "Webhook server port (webhook role).")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for the controller manager (controller role).")
	flag.StringVar(&webhookNamespace, "webhook-namespace", namespaceFromEnv(), "Namespace containing the webhook service and certificate secret.")
	flag.StringVar(&webhookServiceName, "webhook-service-name", "synthetics-operator-webhook-service", "Webhook service name used in serving certificates.")
	flag.StringVar(&webhookSecretName, "webhook-secret-name", "synthetics-webhook-certs", "Secret name for self-managed webhook certificates.")
	flag.StringVar(&validatingWebhookConfiguration, "validating-webhook-configuration", "synthetics-operator-validating-webhook-configuration", "ValidatingWebhookConfiguration name to inject with the CA bundle.")
	flag.StringVar(&mutatingWebhookConfiguration, "mutating-webhook-configuration", "synthetics-operator-mutating-webhook-configuration", "MutatingWebhookConfiguration name to inject with the CA bundle.")
	flag.StringVar(&natsURL, "nats-url", "", "NATS server URL (required for controller, probe-worker, metrics roles).")
	flag.StringVar(&testSidecarImage, "test-sidecar-image", "", "Image for the test-sidecar container in K6Test/PlaywrightTest jobs (controller role).")
	flag.StringVar(&k6RunnerImage, "k6-runner-image", "", "Image for the k6-runner init container (controller role).")
	flag.StringVar(&playwrightRunnerImage, "playwright-runner-image", "", "Image for the playwright-runner container (controller role).")
	flag.Parse()

	ctrl.SetLogger(logr.FromSlogHandler(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))
	log := ctrl.Log.WithName("setup").WithValues("role", role)

	var runErr error
	switch Role(role) {
	case RoleController:
		runErr = runController(scheme, natsURL, testSidecarImage, k6RunnerImage, playwrightRunnerImage, enableLeaderElection)
	case RoleWebhook:
		runErr = runWebhook(scheme, webhookNamespace, webhookSecretName, webhookServiceName, validatingWebhookConfiguration, mutatingWebhookConfiguration, webhookPort)
	case RoleProbeWorker:
		runErr = runProbeWorker(natsURL)
	case RoleMetricsConsumer:
		runErr = runMetricsConsumer(metricsAddr, natsURL)
	default:
		log.Error(fmt.Errorf("unknown role %q", role), "valid roles: controller, webhook, probe-worker, metrics")
		os.Exit(1)
	}

	if runErr != nil {
		log.Error(runErr, "binary exited with error")
		os.Exit(1)
	}
}

// runWebhook starts only the admission webhook server. Reads the cert
// Secret written by the controller; no reconcilers, probe execution, or
// metrics happen here.
func runWebhook(
	scheme *runtime.Scheme,
	webhookNamespace, webhookSecretName, webhookServiceName,
	validatingCfg, mutatingCfg string,
	webhookPort int,
) error {
	cfg := ctrl.GetConfigOrDie()
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("kubernetes clientset: %w", err)
	}
	certManager := internalwebhookcerts.NewManager(
		ctrl.Log.WithName("webhook-certs"),
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
				func(c *tls.Config) {
					c.GetCertificate = certManager.GetCertificate
				},
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
	if err := addHealthChecks(mgr); err != nil {
		return err
	}
	return mgr.Start(ctrl.SetupSignalHandler())
}

// runController starts reconcilers + scheduler + event notifier. Publishes
// SpecUpdates and ProbeJobs to NATS. Under Phase 14 it does not execute
// probes or serve /metrics.
func runController(
	scheme *runtime.Scheme,
	natsURL, testSidecarImage, k6RunnerImage, playwrightRunnerImage string,
	enableLeaderElection bool,
) error {
	if natsURL == "" {
		return errors.New("--nats-url is required for the controller role")
	}
	cfg := ctrl.GetConfigOrDie()
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("kubernetes clientset: %w", err)
	}
	certManager := internalwebhookcerts.NewManager(
		ctrl.Log.WithName("webhook-certs"),
		clientset,
		namespaceFromEnv(),
		"synthetics-webhook-certs",
		"synthetics-operator-webhook-service",
		"",
		"synthetics-operator-validating-webhook-configuration",
		"synthetics-operator-mutating-webhook-configuration",
	)
	if err := certManager.Initialize(context.Background()); err != nil {
		return fmt.Errorf("initializing webhook certificates: %w", err)
	}

	bus, err := natsbus.Connect(ctrl.Log.WithName("nats"), natsURL)
	if err != nil {
		return fmt.Errorf("connecting NATS: %w", err)
	}

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

	// Event notifier listens on the result streams to emit k8s events on the
	// CR when a probe/test transitions between ok and non-ok. Metrics store
	// is internal-only — used for transition detection — the controller
	// does not serve /metrics.
	store, err := internalmetrics.NewStore()
	if err != nil {
		return fmt.Errorf("creating event-notifier store: %w", err)
	}
	notifier := internalevents.New(mgr.GetClient(), mgr.GetEventRecorderFor("synthetics-operator"))
	store.OnProbeTransition = notifier.OnProbeTransition
	store.OnTestTransition = notifier.OnTestTransition
	eventConsumer := &metricsconsumer.Consumer{
		Log:   ctrl.Log.WithName("event-consumer"),
		Bus:   bus,
		Store: store,
	}
	if err := mgr.Add(eventConsumer); err != nil {
		return fmt.Errorf("adding event consumer: %w", err)
	}

	scheduler := internalprobes.NewScheduler(ctrl.Log.WithName("scheduler"), bus)
	if err := mgr.Add(scheduler); err != nil {
		return fmt.Errorf("adding scheduler: %w", err)
	}

	httpReconciler := &controllers.HTTPProbeReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Scheduler: scheduler,
		Publisher: bus,
		Clock:     time.Now,
	}
	if err := httpReconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("creating HTTPProbe controller: %w", err)
	}

	dnsReconciler := &controllers.DNSProbeReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Scheduler: scheduler,
		Publisher: bus,
		Clock:     time.Now,
	}
	if err := dnsReconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("creating DNSProbe controller: %w", err)
	}

	k6Reconciler := &controllers.K6TestReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		Publisher:        bus,
		Clock:            time.Now,
		NATSUrl:          natsURL,
		TestSidecarImage: testSidecarImage,
		K6RunnerImage:    k6RunnerImage,
	}
	if err := k6Reconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("creating K6Test controller: %w", err)
	}

	playwrightReconciler := &controllers.PlaywrightTestReconciler{
		Client:                mgr.GetClient(),
		Scheme:                mgr.GetScheme(),
		Publisher:             bus,
		Clock:                 time.Now,
		NATSUrl:               natsURL,
		TestSidecarImage:      testSidecarImage,
		PlaywrightRunnerImage: playwrightRunnerImage,
	}
	if err := playwrightReconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("creating PlaywrightTest controller: %w", err)
	}

	if err := addHealthChecks(mgr); err != nil {
		return err
	}
	defer bus.Close()
	return mgr.Start(ctrl.SetupSignalHandler())
}

// runProbeWorker runs a stateless probe-worker. Subscribes to specs + jobs,
// executes probes, publishes ProbeResults. No Kubernetes API access needed.
func runProbeWorker(natsURL string) error {
	if natsURL == "" {
		return errors.New("--nats-url is required for the probe-worker role")
	}
	log := ctrl.Log.WithName("probe-worker")
	bus, err := natsbus.Connect(log.WithName("nats"), natsURL)
	if err != nil {
		return fmt.Errorf("connecting NATS: %w", err)
	}
	defer bus.Close()

	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("kube config: %w", err)
	}
	// We still create a manager to get a controller-runtime Runnable host +
	// health endpoints, but no reconcilers are registered.
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 runtime.NewScheme(),
		HealthProbeBindAddress: ":8081",
		Metrics:                metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}

	worker := &probeworker.Worker{Log: log, Bus: bus, Publisher: bus}
	if err := mgr.Add(worker); err != nil {
		return fmt.Errorf("adding probe worker: %w", err)
	}
	if err := addHealthChecks(mgr); err != nil {
		return err
	}
	return mgr.Start(ctrl.SetupSignalHandler())
}

// runMetricsConsumer subscribes to NATS and serves Prometheus metrics.
func runMetricsConsumer(metricsAddr, natsURL string) error {
	if natsURL == "" {
		return errors.New("--nats-url is required for the metrics role")
	}
	log := ctrl.Log.WithName("metrics-consumer")
	bus, err := natsbus.Connect(log.WithName("nats"), natsURL)
	if err != nil {
		return fmt.Errorf("connecting NATS: %w", err)
	}
	defer bus.Close()

	store, err := internalmetrics.NewStore()
	if err != nil {
		return fmt.Errorf("creating metrics store: %w", err)
	}

	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("kube config: %w", err)
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 runtime.NewScheme(),
		HealthProbeBindAddress: ":8081",
		Metrics:                metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}

	consumer := &metricsconsumer.Consumer{Log: log, Bus: bus, Store: store}
	if err := mgr.Add(consumer); err != nil {
		return fmt.Errorf("adding consumer: %w", err)
	}
	if err := mgr.Add(store.Server(metricsAddr)); err != nil {
		return fmt.Errorf("adding metrics server: %w", err)
	}
	if err := addHealthChecks(mgr); err != nil {
		return err
	}
	return mgr.Start(ctrl.SetupSignalHandler())
}

func addHealthChecks(mgr ctrl.Manager) error {
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("setting up health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("setting up ready check: %w", err)
	}
	return nil
}

func namespaceFromEnv() string {
	for _, key := range []string{"POD_NAMESPACE", "NAMESPACE"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return "default"
}

// silence unused — rest is imported purely for side-effects in other roles.
var _ *rest.Config