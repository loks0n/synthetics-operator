// Command prober executes HTTPProbe, DNSProbe, TCPProbe, and SMTPProbe runs. Stateless: pulls
// jobs from synthetics.probes.jobs via a NATS queue group, or from a Redis list
// of that name, and publishes results to synthetics.probes.results on the same bus. Each job carries the spec it needs,
// so there is nothing to hydrate at startup. No Kubernetes API access.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-logr/logr"

	"github.com/loks0n/synthetics-operator/internal/natsbus"
	"github.com/loks0n/synthetics-operator/internal/prober"
	"github.com/loks0n/synthetics-operator/internal/probes"
	"github.com/loks0n/synthetics-operator/internal/redisbus"
)

// transport is what the prober needs from NATS or Redis.
type transport interface {
	prober.JobSubscriber
	prober.ResultPublisher
	Close()
}

func main() {
	var natsURL, redisURL, deniedNetworks string
	var concurrency int
	flag.StringVar(&natsURL, "nats-url", "", "NATS server URL. Exactly one of --nats-url and --redis-url is required.")
	flag.StringVar(&redisURL, "redis-url", "", "Redis URL (redis://[user:password@]host:port[/db]) to pop jobs from and push results to, for a stack with no NATS.")
	flag.IntVar(&concurrency, "concurrency", prober.DefaultConcurrency, "Probes this replica runs at once.")
	flag.StringVar(&deniedNetworks, "denied-networks", "", "Comma-separated CIDRs probes may not connect to; `private` adds the private, loopback, link-local and CGNAT ranges. Empty allows everything.")
	flag.Parse()

	ctrl := logr.FromSlogHandler(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	log := ctrl.WithName("prober")

	if err := run(log, natsURL, redisURL, deniedNetworks, concurrency); err != nil {
		log.Error(err, "exiting")
		os.Exit(1)
	}
}

func run(log logr.Logger, natsURL, redisURL, deniedNetworks string, concurrency int) error {
	if (natsURL == "") == (redisURL == "") {
		return errors.New("exactly one of --nats-url and --redis-url is required")
	}
	denied, err := probes.ParseDeniedNetworks(deniedNetworks)
	if err != nil {
		return fmt.Errorf("--denied-networks: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	var bus transport
	if redisURL != "" {
		redisbus.SetClientLog(log.WithName("redis"))
		client, err := redisbus.Connect(log.WithName("redis"), redisURL)
		if err != nil {
			return fmt.Errorf("connecting Redis: %w", err)
		}
		bus = client
	} else {
		client, err := natsbus.Connect(log.WithName("nats"), natsURL)
		if err != nil {
			return fmt.Errorf("connecting NATS: %w", err)
		}
		bus = client
	}
	defer bus.Close()

	healthErr := serveHealth(ctx, log.WithName("health"))
	workerErr := make(chan error, 1)

	worker := prober.NewWorker(log, bus, bus, concurrency, probes.Egress{Denied: denied})
	go func() { workerErr <- worker.Start(ctx) }()

	// Wait for the worker even after a signal: it returns once the jobs it was
	// already handed are probed and published, which closing the bus would cut short.
	select {
	case err := <-workerErr:
		return err
	case err := <-healthErr:
		return err
	}
}

// serveHealth starts a minimal HTTP server on :8081 answering /healthz and
// /readyz. Returns a channel that fires only on unexpected ListenAndServe
// error — ctx cancellation shuts down cleanly and sends nothing.
func serveHealth(ctx context.Context, log logr.Logger) <-chan error {
	errCh := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := &http.Server{Addr: ":8081", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Error(err, "health server shutdown")
		}
	}()
	return errCh
}
