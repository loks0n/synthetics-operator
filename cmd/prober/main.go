// Command prober executes HTTPProbe and DNSProbe runs. Stateless:
// subscribes to synthetics.specs for the spec cache, pulls jobs from
// synthetics.probes.jobs via a NATS queue group, publishes results to
// synthetics.probes.results. No Kubernetes API access.
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
)

func main() {
	var natsURL string
	flag.StringVar(&natsURL, "nats-url", "", "NATS server URL (required).")
	flag.Parse()

	ctrl := logr.FromSlogHandler(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	log := ctrl.WithName("prober")

	if err := run(log, natsURL); err != nil {
		log.Error(err, "exiting")
		os.Exit(1)
	}
}

func run(log logr.Logger, natsURL string) error {
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

	healthErr := serveHealth(ctx, log.WithName("health"))
	workerErr := make(chan error, 1)

	worker := &prober.Worker{Log: log, Bus: bus, Publisher: bus}
	go func() { workerErr <- worker.Start(ctx) }()

	select {
	case <-ctx.Done():
		return nil
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
