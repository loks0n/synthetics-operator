// Package natsbus centralises NATS publish + subscribe wiring for the
// three Phase-14 deployments. A single connection, helpers for the JSON
// wire types, and controller-runtime-Runnable-compatible subscribers.
package natsbus

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	natsgo "github.com/nats-io/nats.go"

	"github.com/loks0n/synthetics-operator/internal/results"
)

// Publisher is the thin publish interface used by the controller and probe
// worker. Tests swap in an in-memory fake.
type Publisher interface {
	PublishSpec(ctx context.Context, msg results.SpecUpdate) error
	PublishProbeJob(ctx context.Context, msg results.ProbeJob) error
	PublishProbeResult(ctx context.Context, msg results.ProbeResult) error
}

// Client satisfies Publisher at compile time.
var _ Publisher = (*Client)(nil)

// Client wraps a single NATS connection for the lifetime of a binary.
type Client struct {
	log logr.Logger
	nc  *natsgo.Conn
}

// Connect opens a NATS connection with the standard reconnect/retry options
// used across the Phase-14 deployments. Returns a Client that can be used as
// a Publisher and/or Subscriber.
func Connect(log logr.Logger, natsURL string) (*Client, error) {
	nc, err := natsgo.Connect(natsURL,
		natsgo.RetryOnFailedConnect(true),
		natsgo.MaxReconnects(-1),
		natsgo.ReconnectWait(250*time.Millisecond),
		natsgo.ReconnectJitter(500*time.Millisecond, 500*time.Millisecond),
		natsgo.DisconnectErrHandler(func(_ *natsgo.Conn, err error) {
			if err != nil {
				log.Error(err, "NATS disconnected")
			}
		}),
		natsgo.ReconnectHandler(func(_ *natsgo.Conn) {
			log.Info("NATS reconnected")
		}),
		natsgo.ConnectHandler(func(_ *natsgo.Conn) {
			log.Info("NATS connected", "url", natsURL)
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("connecting to NATS: %w", err)
	}
	return &Client{log: log, nc: nc}, nil
}

// Close releases the underlying connection. Safe to call from a defer.
func (c *Client) Close() {
	if c.nc != nil {
		c.nc.Close()
	}
}

// PublishSpec publishes a SpecUpdate to synthetics.specs.
func (c *Client) PublishSpec(_ context.Context, msg results.SpecUpdate) error {
	return c.publishJSON(results.SubjectSpecs, msg)
}

// PublishProbeJob publishes a ProbeJob to synthetics.probes.jobs.
func (c *Client) PublishProbeJob(_ context.Context, msg results.ProbeJob) error {
	return c.publishJSON(results.SubjectProbeJobs, msg)
}

// PublishProbeResult publishes a ProbeResult to synthetics.probes.results.
func (c *Client) PublishProbeResult(_ context.Context, msg results.ProbeResult) error {
	return c.publishJSON(results.SubjectProbeResults, msg)
}

func (c *Client) publishJSON(subject string, msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", subject, err)
	}
	if err := c.nc.Publish(subject, data); err != nil {
		return fmt.Errorf("publish %s: %w", subject, err)
	}
	return nil
}

// SubscribeSpecs delivers every SpecUpdate to handler. Pub-sub subscription
// (all subscribers get every message). Runs until ctx is cancelled.
func (c *Client) SubscribeSpecs(ctx context.Context, handler func(context.Context, results.SpecUpdate)) error {
	return subscribeJSON(ctx, c, results.SubjectSpecs, "", handler)
}

// SubscribeProbeJobs is a queue-group subscription (each job delivered to
// exactly one worker in the group). Runs until ctx is cancelled.
func (c *Client) SubscribeProbeJobs(ctx context.Context, handler func(context.Context, results.ProbeJob)) error {
	return subscribeJSON(ctx, c, results.SubjectProbeJobs, results.ProberQueue, handler)
}

// SubscribeProbeResults delivers every ProbeResult to handler.
func (c *Client) SubscribeProbeResults(ctx context.Context, handler func(context.Context, results.ProbeResult)) error {
	return subscribeJSON(ctx, c, results.SubjectProbeResults, "", handler)
}

// SubscribeTestResults delivers every TestResult to handler.
func (c *Client) SubscribeTestResults(ctx context.Context, handler func(context.Context, results.TestResult)) error {
	return subscribeJSON(ctx, c, results.SubjectTestResults, "", handler)
}

func subscribeJSON[T any](ctx context.Context, c *Client, subject, queueGroup string, handler func(context.Context, T)) error {
	decode := func(msg *natsgo.Msg) {
		var payload T
		if err := json.Unmarshal(msg.Data, &payload); err != nil {
			c.log.Error(err, "failed to decode NATS message", "subject", subject)
			return
		}
		handler(ctx, payload)
	}
	var sub *natsgo.Subscription
	var err error
	if queueGroup != "" {
		sub, err = c.nc.QueueSubscribe(subject, queueGroup, decode)
	} else {
		sub, err = c.nc.Subscribe(subject, decode)
	}
	if err != nil {
		return fmt.Errorf("subscribe %s: %w", subject, err)
	}
	c.log.Info("subscribed to NATS", "subject", subject, "queueGroup", queueGroup)
	<-ctx.Done()
	if err := sub.Unsubscribe(); err != nil {
		c.log.Error(err, "unsubscribe", "subject", subject)
	}
	return nil
}
