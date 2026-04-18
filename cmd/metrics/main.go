// Command metrics serves Prometheus metrics derived from NATS streams.
// Subscribes to synthetics.specs + synthetics.probes.results +
// synthetics.tests.results. No Kubernetes API access.
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

	internalmetrics "github.com/loks0n/synthetics-operator/internal/metrics"
	"github.com/loks0n/synthetics-operator/internal/metricsconsumer"
	"github.com/loks0n/synthetics-operator/internal/natsbus"
)

func main() {
	var natsURL string
	var metricsAddr string
	flag.StringVar(&natsURL, "nats-url", "", "NATS server URL (required).")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the /metrics endpoint binds to.")
	flag.Parse()

	ctrl := logr.FromSlogHandler(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	log := ctrl.WithName("metrics")

	if err := run(log, natsURL, metricsAddr); err != nil {
		log.Error(err, "exiting")
		os.Exit(1)
	}
}

func run(log logr.Logger, natsURL, metricsAddr string) error {
	if natsURL == "" {
		return errors.New("--nats-url is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	bus, err := natsbus.Connect(log.WithName("nats"), natsURL)
	if err != nil {
		return fmt.Errorf("connecting NATS: %w", err)
	}
	defer bus.Close()

	store, err := internalmetrics.NewStore()
	if err != nil {
		return fmt.Errorf("creating metrics store: %w", err)
	}

	consumer := &metricsconsumer.Consumer{Log: log, Bus: bus, Store: store}
	consumerErr := make(chan error, 1)
	go func() { consumerErr <- consumer.Start(ctx) }()

	metricsErr := make(chan error, 1)
	go func() { metricsErr <- store.Server(metricsAddr).Start(ctx) }()

	healthErr := serveHealth(ctx, log.WithName("health"))

	select {
	case <-ctx.Done():
		return nil
	case err := <-consumerErr:
		return err
	case err := <-metricsErr:
		return err
	case err := <-healthErr:
		return err
	}
}

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
