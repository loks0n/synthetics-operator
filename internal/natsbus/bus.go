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

// PublishHeartbeatPing publishes a HeartbeatPing to synthetics.heartbeats.pings.
func (c *Client) PublishHeartbeatPing(_ context.Context, msg results.HeartbeatPing) error {
	return c.publishJSON(results.SubjectHeartbeatPings, msg)
}

// SubscribeOption tunes a subscription.
type SubscribeOption func(*subscribeConfig)

type subscribeConfig struct {
	ready chan<- struct{}
}

// WithReady closes ch once the subscription is established on the server.
//
// Subscribe* blocks for the lifetime of the subscription, so callers run it in
// a goroutine — which means "the goroutine has started" says nothing about
// whether the subscription exists yet. Anything that publishes a message it
// then expects a reply to must wait for this signal first, or the reply can
// land in the gap and be dropped: core NATS has no retention, so a message
// with no current subscriber is gone, not queued.
func WithReady(ch chan<- struct{}) SubscribeOption {
	return func(cfg *subscribeConfig) { cfg.ready = ch }
}

func (cfg subscribeConfig) signalReady() {
	if cfg.ready != nil {
		close(cfg.ready)
	}
}

// SignalReady fires the readiness signal carried in opts, if any.
//
// Exported for test fakes standing in for *Client: a fake that ignores
// WithReady would deadlock the caller waiting on it, and — worse — would let
// the subscribe-then-request ordering regress without any test noticing.
func SignalReady(opts []SubscribeOption) {
	newSubscribeConfig(opts).signalReady()
}

func newSubscribeConfig(opts []SubscribeOption) subscribeConfig {
	var cfg subscribeConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

// RequestSpecResync asks the controller to re-broadcast every live spec now,
// rather than waiting out the resync interval.
//
// A subscriber that restarts on its own — one metrics replica, one receiver
// pod — knows nothing until the next periodic resync, because core NATS
// replays nothing. For the receiver that gap is user-visible: it 404s pings
// for tokens it hasn't learned yet, so a cron job that happens to fire in the
// window records a spurious failure.
func (c *Client) RequestSpecResync(_ context.Context) error {
	if err := c.nc.Publish(results.SubjectSpecsResync, nil); err != nil {
		return fmt.Errorf("publish %s: %w", results.SubjectSpecsResync, err)
	}
	return nil
}

// SubscribeHeartbeatPings delivers every HeartbeatPing to handler.
func (c *Client) SubscribeHeartbeatPings(ctx context.Context, handler func(context.Context, results.HeartbeatPing), opts ...SubscribeOption) error {
	return subscribeJSON(ctx, c, results.SubjectHeartbeatPings, "", handler, opts...)
}

// SubscribeSpecResyncRequests invokes handler on every resync request. The
// payload is empty — the message itself is the whole signal.
func (c *Client) SubscribeSpecResyncRequests(ctx context.Context, handler func(context.Context), opts ...SubscribeOption) error {
	sub, err := c.nc.Subscribe(results.SubjectSpecsResync, func(*natsgo.Msg) { handler(ctx) })
	if err != nil {
		return fmt.Errorf("subscribe %s: %w", results.SubjectSpecsResync, err)
	}
	if err := c.nc.Flush(); err != nil {
		return fmt.Errorf("flush %s subscription: %w", results.SubjectSpecsResync, err)
	}
	c.log.Info("subscribed to NATS", "subject", results.SubjectSpecsResync)
	newSubscribeConfig(opts).signalReady()
	<-ctx.Done()
	if err := sub.Unsubscribe(); err != nil {
		c.log.Error(err, "unsubscribe", "subject", results.SubjectSpecsResync)
	}
	return nil
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
func (c *Client) SubscribeSpecs(ctx context.Context, handler func(context.Context, results.SpecUpdate), opts ...SubscribeOption) error {
	return subscribeJSON(ctx, c, results.SubjectSpecs, "", handler, opts...)
}

// SubscribeProbeJobs is a queue-group subscription (each job delivered to
// exactly one worker in the group). Runs until ctx is cancelled.
func (c *Client) SubscribeProbeJobs(ctx context.Context, handler func(context.Context, results.ProbeJob), opts ...SubscribeOption) error {
	return subscribeJSON(ctx, c, results.SubjectProbeJobs, results.ProberQueue, handler, opts...)
}

// SubscribeProbeResults delivers every ProbeResult to handler.
func (c *Client) SubscribeProbeResults(ctx context.Context, handler func(context.Context, results.ProbeResult), opts ...SubscribeOption) error {
	return subscribeJSON(ctx, c, results.SubjectProbeResults, "", handler, opts...)
}

// SubscribeTestResults delivers every TestResult to handler.
func (c *Client) SubscribeTestResults(ctx context.Context, handler func(context.Context, results.TestResult), opts ...SubscribeOption) error {
	return subscribeJSON(ctx, c, results.SubjectTestResults, "", handler, opts...)
}

func subscribeJSON[T any](ctx context.Context, c *Client, subject, queueGroup string, handler func(context.Context, T), opts ...SubscribeOption) error {
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
	// Round-trip to the server so the subscription is known to exist before
	// readiness is signalled — nc.Subscribe only queues the protocol message.
	if err := c.nc.Flush(); err != nil {
		return fmt.Errorf("flush %s subscription: %w", subject, err)
	}
	c.log.Info("subscribed to NATS", "subject", subject, "queueGroup", queueGroup)
	newSubscribeConfig(opts).signalReady()
	<-ctx.Done()
	if err := sub.Unsubscribe(); err != nil {
		c.log.Error(err, "unsubscribe", "subject", subject)
	}
	return nil
}
