// Command heartbeat serves the inbound heartbeat endpoint. It learns which
// tokens are live from the NATS spec stream and republishes each accepted
// ping onto synthetics.heartbeats.pings. No Kubernetes API access.
//
// This is the only operator component exposed to the public internet, so it
// runs as its own Deployment behind its own Service: a request storm against
// the ping endpoint degrades heartbeat ingestion and nothing else.
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

	"github.com/loks0n/synthetics-operator/internal/heartbeat"
	"github.com/loks0n/synthetics-operator/internal/natsbus"
)

func main() {
	var natsURL string
	var bindAddress string
	var warmup time.Duration
	flag.StringVar(&natsURL, "nats-url", "", "NATS server URL (required).")
	flag.StringVar(&bindAddress, "bind-address", ":8080", "Address the ping endpoint binds to.")
	flag.DurationVar(&warmup, "warmup", 10*time.Second, "How long to report unready while waiting for the initial spec resync.")
	flag.Parse()

	root := logr.FromSlogHandler(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	log := root.WithName("heartbeat")

	if err := run(log, natsURL, bindAddress, warmup); err != nil {
		log.Error(err, "exiting")
		os.Exit(1)
	}
}

func run(log logr.Logger, natsURL, bindAddress string, warmup time.Duration) error {
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

	receiver := &heartbeat.Receiver{Log: log, Bus: bus, Warmup: warmup}

	receiverErr := make(chan error, 1)
	go func() { receiverErr <- receiver.Start(ctx) }()

	server := &http.Server{
		Addr:    bindAddress,
		Handler: receiver.Handler(),
		// A ping carries at most a few KiB of job output; anything slower
		// than this is a stuck client holding a connection open.
		ReadTimeout:       15 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      15 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Info("serving heartbeat endpoint", "address", bindAddress)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-receiverErr:
		if err != nil {
			return err
		}
	case err := <-serverErr:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return server.Shutdown(shutdownCtx)
}
