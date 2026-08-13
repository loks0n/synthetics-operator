// Package heartbeat implements the inbound side of the Heartbeat CRD: token
// generation, and the HTTP receiver that turns a ping into a NATS message.
//
// The receiver is the only component in the operator that accepts traffic
// from outside the cluster, so it is deliberately small. It resolves a token
// to a {namespace, name}, publishes a HeartbeatPing, and returns. It holds no
// timers, evaluates no freshness, and touches no Kubernetes API — the metrics
// consumer decides whether a heartbeat is late, and the controller persists
// the last ping to CR status.
package heartbeat

import (
	"context"
	"io"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

	"github.com/loks0n/synthetics-operator/internal/natsbus"
	"github.com/loks0n/synthetics-operator/internal/results"
)

// Bus is the slice of the NATS client the receiver needs. Declared here
// rather than widening natsbus.Publisher so the controller and prober aren't
// forced to implement heartbeat methods they never call.
type Bus interface {
	SubscribeSpecs(ctx context.Context, handler func(context.Context, results.SpecUpdate), opts ...natsbus.SubscribeOption) error
	PublishHeartbeatPing(ctx context.Context, msg results.HeartbeatPing) error
	RequestSpecResync(ctx context.Context) error
}

// maxOutputBytes caps how much of a request body is carried on the ping.
// Better Stack accepts job output on the same request, and it is genuinely
// useful for diagnosis, but this ends up in logs on every run — a job that
// pipes a megabyte of stdout should not be able to flood them.
const maxOutputBytes = 4 << 10

// failSuffix is the URL segment that reports an explicit failure, matching
// Better Stack's `<url>/fail` so migrated scripts need no edit.
const failSuffix = "fail"

// Receiver serves heartbeat pings. Its token index is built entirely from the
// NATS spec stream, which keeps the receiver stateless with respect to the
// Kubernetes API in the same way the prober and metrics deployments are.
type Receiver struct {
	Log logr.Logger
	Bus Bus
	// Now is injectable for tests.
	Now func() time.Time
	// Warmup bounds how long the receiver reports unready after start while
	// it waits for the spec resync it requested. Pings that arrive in this
	// window get a 503, not a 404, so a retrying client succeeds instead of
	// recording a spurious failure against a token that is perfectly valid.
	Warmup time.Duration

	mu sync.RWMutex
	// identities maps token to the Heartbeat that owns it.
	identities map[string]types.NamespacedName
	// tokens is the reverse index, needed to evict the old token when a
	// Heartbeat's token rotates. Without it a rotated-away token would keep
	// working for the lifetime of the process.
	tokens map[types.NamespacedName]string
	// warm flips once the first spec batch lands or Warmup elapses.
	warm bool
}

func (r *Receiver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// NeedLeaderElection is false: every replica must serve, since the Service
// fronts all of them and a ping is not something to queue behind an election.
func (*Receiver) NeedLeaderElection() bool { return false }

// Start subscribes to the spec stream and asks the controller for an
// immediate resync, then blocks until ctx is cancelled.
func (r *Receiver) Start(ctx context.Context) error {
	r.mu.Lock()
	if r.identities == nil {
		r.identities = map[string]types.NamespacedName{}
		r.tokens = map[types.NamespacedName]string{}
	}
	r.mu.Unlock()

	warmup := r.Warmup
	if warmup <= 0 {
		warmup = 10 * time.Second
	}
	// Mark warm on a timer as well as on first spec, so a cluster with zero
	// Heartbeats — where no spec will ever arrive — still becomes ready.
	timer := time.AfterFunc(warmup, r.markWarm)
	defer timer.Stop()

	// Subscribe first, then ask. The controller answers in milliseconds and
	// core NATS drops a message nobody is subscribed to, so requesting before
	// the subscription exists loses the very reply being asked for — and the
	// receiver would 503 until the next periodic tick instead of a few
	// milliseconds.
	ready := make(chan struct{})
	subscribed := make(chan error, 1)
	go func() { subscribed <- r.Bus.SubscribeSpecs(ctx, r.onSpec, natsbus.WithReady(ready)) }()

	select {
	case <-ready:
		if err := r.Bus.RequestSpecResync(ctx); err != nil {
			// Non-fatal: the controller resyncs periodically anyway, so this
			// only costs a longer cold window.
			r.Log.Error(err, "requesting spec resync")
		}
	case err := <-subscribed:
		return err
	case <-ctx.Done():
		return nil
	}

	select {
	case err := <-subscribed:
		return err
	case <-ctx.Done():
		return nil
	}
}

func (r *Receiver) markWarm() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.warm = true
}

// Ready reports whether the token index can be trusted to be complete.
func (r *Receiver) Ready() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.warm
}

func (r *Receiver) onSpec(_ context.Context, msg results.SpecUpdate) {
	if msg.Kind != results.KindHeartbeat {
		return
	}
	name := types.NamespacedName{Namespace: msg.Namespace, Name: msg.Name}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.warm = true

	// Drop whatever token this Heartbeat used to have before installing the
	// new one; on a tombstone that eviction is the whole operation.
	if previous, ok := r.tokens[name]; ok {
		delete(r.identities, previous)
		delete(r.tokens, name)
	}
	if msg.Deleted || msg.Heartbeat == nil || msg.Heartbeat.Token == "" {
		return
	}
	r.identities[msg.Heartbeat.Token] = name
	r.tokens[name] = msg.Heartbeat.Token
}

// Snapshot returns the current token index. Test-facing.
func (r *Receiver) Snapshot() map[string]types.NamespacedName {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]types.NamespacedName, len(r.identities))
	maps.Copy(out, r.identities)
	return out
}

func (r *Receiver) lookup(token string) (types.NamespacedName, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	name, ok := r.identities[token]
	return name, ok
}

// Handler returns the ping mux. Two endpoints only:
//
//	/<token>            a successful run
//	/<token>/fail       an explicit failure
//	/<token>/<exitcode> a run that finished with that exit code; 0 is success
//
// The shapes mirror Better Stack's heartbeat URLs exactly, so migrating a
// cron job means swapping the hostname and nothing else.
func (r *Receiver) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", r.servePing)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !r.Ready() {
			http.Error(w, "warming up", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func (r *Receiver) servePing(w http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodPost:
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	token, outcome, ok := parsePath(req.URL.Path)
	if !ok {
		http.NotFound(w, req)
		return
	}

	name, known := r.lookup(token)
	if !known {
		// Distinguish "not yet loaded" from "no such heartbeat". A 503 tells a
		// retrying client to come back; a 404 tells it the URL is wrong. Both
		// omit the token from the body so an error page can't leak it onward.
		if !r.Ready() {
			w.Header().Set("Retry-After", "5")
			http.Error(w, "heartbeat receiver is still loading; retry shortly", http.StatusServiceUnavailable)
			return
		}
		http.NotFound(w, req)
		return
	}

	ping := results.HeartbeatPing{
		Name:       name.Name,
		Namespace:  name.Namespace,
		ReceivedAt: r.now(),
		Failed:     outcome.failed,
		ExitCode:   outcome.exitCode,
		Output:     readOutput(req),
	}

	if err := r.Bus.PublishHeartbeatPing(req.Context(), ping); err != nil {
		// Publishing failed, so nothing downstream will record this ping.
		// Saying "ok" would let the job believe it checked in when it didn't,
		// and the heartbeat would go down with the job none the wiser.
		r.Log.Error(err, "publishing heartbeat ping", "namespace", name.Namespace, "heartbeat", name.Name)
		http.Error(w, "failed to record heartbeat", http.StatusInternalServerError)
		return
	}

	log := r.Log.WithValues("namespace", name.Namespace, "heartbeat", name.Name, "failed", ping.Failed, "exitCode", ping.ExitCode)
	if ping.Output != "" {
		log = log.WithValues("output", ping.Output)
	}
	log.Info("heartbeat received")

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n")
}

// outcome is what the URL suffix said about the run.
type outcome struct {
	failed   bool
	exitCode int
}

// parsePath splits a ping URL into its token and outcome. Returns ok=false
// for anything that isn't a plausible ping so callers can 404 without a map
// lookup, which also keeps unparseable garbage out of the token index's lock.
func parsePath(path string) (string, outcome, bool) {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	if len(segments) == 0 || segments[0] == "" {
		return "", outcome{}, false
	}
	token := segments[0]
	if AcceptableToken(token) != nil {
		return "", outcome{}, false
	}

	switch len(segments) {
	case 1:
		return token, outcome{}, true
	case 2:
		suffix := segments[1]
		if suffix == failSuffix {
			return token, outcome{failed: true}, true
		}
		code, err := strconv.Atoi(suffix)
		if err != nil || code < 0 {
			return "", outcome{}, false
		}
		return token, outcome{failed: code != 0, exitCode: code}, true
	default:
		return "", outcome{}, false
	}
}

// readOutput reads at most maxOutputBytes of the request body. Errors are
// swallowed: a truncated or unreadable body is diagnostic detail, and losing
// it must never cost the job its check-in.
func readOutput(req *http.Request) string {
	if req.Body == nil || req.Method != http.MethodPost {
		return ""
	}
	buf, err := io.ReadAll(io.LimitReader(req.Body, maxOutputBytes))
	if err != nil && len(buf) == 0 {
		return ""
	}
	return strings.TrimSpace(string(buf))
}
