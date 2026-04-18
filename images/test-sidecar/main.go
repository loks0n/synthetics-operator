// test-sidecar runs alongside a CronJob test runner as a native sidecar.
// It waits for the runner to write its result JSON to /results/output.json,
// then publishes the payload to the NATS subject synthetics.tests.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/loks0n/synthetics-operator/internal/results"
)

const (
	subject      = "synthetics.tests"
	outputFile   = "/results/output.json"
	pollInterval = 500 * time.Millisecond
	maxWait      = 10 * time.Minute
)

func main() {
	var natsURL string
	flag.StringVar(&natsURL, "nats-url", nats.DefaultURL, "NATS server URL")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := run(ctx, log, natsURL); err != nil {
		log.Error("test-sidecar failed", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger, natsURL string) error {
	nc, err := nats.Connect(natsURL)
	if err != nil {
		return err
	}
	defer nc.Close()

	log.Info("waiting for result file", "path", outputFile)
	data, err := waitForFile(ctx, outputFile)
	if err != nil {
		return err
	}

	var r results.TestResult
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}

	if err := nc.Publish(subject, data); err != nil {
		return err
	}

	log.Info("published test result", "kind", r.Kind, "name", r.Name, "namespace", r.Namespace, "success", r.Success)
	return nc.Flush()
}

func waitForFile(ctx context.Context, path string) ([]byte, error) {
	deadline := time.Now().Add(maxWait)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			return data, nil
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, os.ErrDeadlineExceeded
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}
