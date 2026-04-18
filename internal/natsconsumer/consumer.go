package natsconsumer

import (
	"context"
	"encoding/json"
	"time"

	"github.com/go-logr/logr"
	natsgo "github.com/nats-io/nats.go"
	"k8s.io/apimachinery/pkg/types"

	internalmetrics "github.com/loks0n/synthetics-operator/internal/metrics"
	"github.com/loks0n/synthetics-operator/internal/results"
)

const subject = "synthetics.tests"

// Consumer subscribes to the NATS subject and forwards test results to the
// metrics store. It implements controller-runtime's Runnable interface.
type Consumer struct {
	log     logr.Logger
	natsURL string
	store   *internalmetrics.Store
}

func New(log logr.Logger, natsURL string, store *internalmetrics.Store) *Consumer {
	return &Consumer{log: log, natsURL: natsURL, store: store}
}

func (c *Consumer) Start(ctx context.Context) error {
	nc, err := natsgo.Connect(c.natsURL,
		natsgo.MaxReconnects(-1),
		natsgo.ReconnectWait(250*time.Millisecond),
		natsgo.ReconnectJitter(500*time.Millisecond, 500*time.Millisecond),
		natsgo.DisconnectErrHandler(func(_ *natsgo.Conn, err error) {
			if err != nil {
				c.log.Error(err, "NATS disconnected")
			}
		}),
		natsgo.ReconnectHandler(func(_ *natsgo.Conn) {
			c.log.Info("NATS reconnected")
		}),
	)
	if err != nil {
		return err
	}
	defer nc.Close()

	sub, err := nc.Subscribe(subject, func(msg *natsgo.Msg) {
		c.recordResult(ctx, msg.Data)
	})
	if err != nil {
		return err
	}
	defer sub.Unsubscribe() //nolint:errcheck

	c.log.Info("subscribed to NATS", "subject", subject, "url", c.natsURL)
	<-ctx.Done()
	return nil
}

func (c *Consumer) recordResult(ctx context.Context, data []byte) {
	var r results.TestResult
	if err := json.Unmarshal(data, &r); err != nil {
		c.log.Error(err, "failed to parse test result")
		c.store.RecordParseFailure(ctx)
		return
	}
	name := types.NamespacedName{Name: r.Name, Namespace: r.Namespace}
	c.store.RecordTestResult(ctx, name, string(r.Kind), r.Success, r.DurationMs, float64(r.Timestamp.Unix()))
}
