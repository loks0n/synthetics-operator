// Package redisbus carries probe jobs and results over Redis lists, for a
// prober that runs beside Redis but no NATS, such as a Docker Compose stack.
//
// The scheduler LPUSHes each ProbeJob onto synthetics.probes.jobs and probers
// BRPOP it, so every job goes to one prober. Probers LPUSH each ProbeResult
// onto synthetics.probes.results for the consumer to BRPOP. Both lists keep
// what nobody has popped yet: jobs wait for a prober that is restarting, and
// results wait for their consumer.
package redisbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/redis/go-redis/v9"

	"github.com/loks0n/synthetics-operator/internal/natsbus"
	"github.com/loks0n/synthetics-operator/internal/results"
)

// pollTimeout bounds each BRPOP, so a stopping prober notices within it.
const pollTimeout = time.Second

// retryWait spaces out pops while Redis is unreachable.
const retryWait = 250 * time.Millisecond

// clientLog takes the messages go-redis would print to stderr itself, one per
// failed dial, into the prober's log at debug verbosity: the bus reports an
// outage once, at error level, on its own.
type clientLog struct{ log logr.Logger }

func (l clientLog) Printf(_ context.Context, format string, v ...any) {
	l.log.V(1).Info(fmt.Sprintf(format, v...))
}

// SetClientLog sends go-redis's own messages to log. go-redis keeps one
// logger for every client in the process, so the binary sets it once.
func SetClientLog(log logr.Logger) {
	redis.SetLogger(clientLog{log: log})
}

// Client wraps a Redis connection pool for the lifetime of the prober.
type Client struct {
	log     logr.Logger
	rdb     *redis.Client
	address string
}

// Connect parses a redis:// or rediss:// URL. No connection is made until the
// first command, and a Redis that is down is retried until it is back.
func Connect(log logr.Logger, redisURL string) (*Client, error) {
	options, err := redis.ParseURL(redisURL)
	if err != nil {
		// The parse error can quote the URL, password included.
		return nil, errors.New("parsing Redis URL: expected redis://[user:password@]host:port[/db]")
	}
	return &Client{log: log, rdb: redis.NewClient(options), address: options.Addr}, nil
}

// Close releases the connection pool. Safe to call from a defer.
func (c *Client) Close() {
	if err := c.rdb.Close(); err != nil {
		c.log.Error(err, "closing Redis")
	}
}

// PublishProbeResult pushes a ProbeResult onto synthetics.probes.results.
func (c *Client) PublishProbeResult(ctx context.Context, msg results.ProbeResult) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", results.SubjectProbeResults, err)
	}
	if err := c.rdb.LPush(ctx, results.SubjectProbeResults, data).Err(); err != nil {
		return fmt.Errorf("push %s: %w", results.SubjectProbeResults, err)
	}
	return nil
}

// SubscribeProbeJobs pops jobs one at a time and hands each to handler, until
// ctx is cancelled. handler blocking holds the next pop back, so jobs this
// replica has no room for stay on the list for other replicas.
//
// Readiness is signalled straight away: the list keeps jobs pushed before the
// first pop, so there is no gap for one to be lost in.
func (c *Client) SubscribeProbeJobs(ctx context.Context, handler func(context.Context, results.ProbeJob), opts ...natsbus.SubscribeOption) error {
	natsbus.SignalReady(opts)
	c.log.Info("popping probe jobs from Redis", "address", c.address, "key", results.SubjectProbeJobs)

	down := false
	for ctx.Err() == nil {
		// Not ctx: cancelling a BRPOP the server already answered drops the job it popped.
		reply, err := c.rdb.BRPop(context.WithoutCancel(ctx), pollTimeout, results.SubjectProbeJobs).Result()
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			if !down {
				c.log.Error(err, "Redis unreachable, retrying", "address", c.address)
				down = true
			}
			select {
			case <-ctx.Done():
			case <-time.After(retryWait):
			}
			continue
		}
		if down {
			c.log.Info("Redis reachable again", "address", c.address)
			down = false
		}

		var job results.ProbeJob
		if err := json.Unmarshal([]byte(reply[1]), &job); err != nil {
			c.log.Error(err, "failed to decode probe job", "key", results.SubjectProbeJobs)
			continue
		}
		// A job popped after shutdown began must still run to completion:
		// executing it against the cancelled ctx reports a healthy target as down.
		handler(context.WithoutCancel(ctx), job)
	}
	return nil
}
