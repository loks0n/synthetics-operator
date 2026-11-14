package prober

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	natsserver "github.com/nats-io/nats-server/v2/server"

	"github.com/loks0n/synthetics-operator/internal/natsbus"
	"github.com/loks0n/synthetics-operator/internal/results"
)

type fakeResultPublisher struct {
	mu        sync.Mutex
	published []results.ProbeResult
}

func TestWorkerExecutesTCPJob(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			_ = conn.Close()
		}
		close(accepted)
	}()

	addr := listener.Addr().(*net.TCPAddr)
	pub := &fakeResultPublisher{}
	w := NewWorker(logr.Discard(), nil, pub, 1)
	w.onJob(context.Background(), results.ProbeJob{Spec: results.SpecUpdate{
		Kind: results.KindTCPProbe, Name: "mysql", Namespace: "default",
		TCPProbe: &results.TCPProbeSpecPayload{
			Host: addr.IP.String(), Port: int32(addr.Port), TimeoutMs: 1000,
			Assertions: []results.Assertion{{Name: "fast", Expr: "duration_ms < 5000"}},
		},
	}})
	<-accepted

	if len(pub.published) != 1 {
		t.Fatalf("expected one result, got %d", len(pub.published))
	}
	got := pub.published[0]
	if got.Result != "ok" || got.TCPHost != addr.IP.String() || got.TCPPort != int32(addr.Port) {
		t.Fatalf("unexpected TCP result: %+v", got)
	}
	if len(got.AssertionResults) != 1 || got.AssertionResults[0].Result != 1 {
		t.Fatalf("expected passing duration assertion: %+v", got.AssertionResults)
	}
}

func (f *fakeResultPublisher) PublishProbeResult(_ context.Context, msg results.ProbeResult) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published = append(f.published, msg)
	return nil
}

func httpJob(url string) results.ProbeJob {
	return results.ProbeJob{
		Spec: results.SpecUpdate{
			Kind:      results.KindHTTPProbe,
			Name:      "probe",
			Namespace: "default",
			HTTPProbe: &results.HTTPProbeSpecPayload{
				URL:        url,
				Method:     http.MethodGet,
				TimeoutMs:  5000,
				Assertions: []results.Assertion{{Name: "ok", Expr: "status_code = 200"}},
			},
		},
	}
}

// The regression this guards: a worker that has just started has seen no
// spec traffic at all. Because the job carries its spec, it must still
// execute — previously it silently dropped the job, which made every
// scaled-up replica discard its share of probe runs.
func TestWorkerExecutesJobWithNoPriorState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	pub := &fakeResultPublisher{}
	w := NewWorker(logr.Discard(), nil, pub, 1)

	w.onJob(context.Background(), httpJob(srv.URL))

	if len(pub.published) != 1 {
		t.Fatalf("expected exactly one result published, got %d", len(pub.published))
	}
	if got := pub.published[0].Result; got != "ok" {
		t.Fatalf("expected result %q, got %q", "ok", got)
	}
}

func TestWorkerSkipsSuspendedJob(t *testing.T) {
	pub := &fakeResultPublisher{}
	w := NewWorker(logr.Discard(), nil, pub, 1)

	job := httpJob("http://127.0.0.1:1")
	job.Spec.Suspend = true
	w.onJob(context.Background(), job)

	if len(pub.published) != 0 {
		t.Fatalf("suspended probe should publish nothing, got %d results", len(pub.published))
	}
}

func TestWorkerIgnoresTestKinds(t *testing.T) {
	pub := &fakeResultPublisher{}
	w := NewWorker(logr.Discard(), nil, pub, 1)

	job := httpJob("http://127.0.0.1:1")
	job.Spec.Kind = results.KindK6Test
	w.onJob(context.Background(), job)

	if len(pub.published) != 0 {
		t.Fatalf("test kinds are executed by CronJobs, got %d results", len(pub.published))
	}
}

// blockingExecutor stands in for a probe against a slow, healthy target: it
// reports "ok" after delay, or the cancellation it saw if ctx ended first.
type blockingExecutor struct{ delay time.Duration }

func (e blockingExecutor) Execute(ctx context.Context, job results.ProbeJob) results.ProbeResult {
	result := results.ProbeResult{Kind: job.Spec.Kind, Name: job.Spec.Name, Result: "ok"}
	select {
	case <-time.After(e.delay):
	case <-ctx.Done():
		result.Result = "cancelled"
	}
	return result
}

// A rollout stops a prober while jobs are queued in its buffer and one is
// mid-probe. Every one of them must still be probed against a live context
// and published before Start returns, or the probe reads as failed or missing.
func TestWorkerFinishesDeliveredJobsOnShutdown(t *testing.T) {
	server, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	server.Start()
	defer server.Shutdown()
	if !server.ReadyForConnections(5 * time.Second) {
		t.Fatal("NATS server not ready")
	}

	bus, err := natsbus.Connect(logr.Discard(), server.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Close()

	const jobs = 5
	pub := &fakeResultPublisher{}
	executors := defaultExecutors()
	executors[results.KindHTTPProbe] = blockingExecutor{delay: 200 * time.Millisecond}
	w := &Worker{Log: logr.Discard(), Bus: bus, Publisher: pub, Concurrency: 2, executors: executors}

	// The server counts its own internal subscriptions too, so wait for the
	// worker's to be added on top of them before publishing.
	baseline := server.NumSubscriptions()
	ctx, stop := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- w.Start(ctx) }()
	for deadline := time.Now().Add(5 * time.Second); server.NumSubscriptions() == baseline; {
		if time.Now().After(deadline) {
			t.Fatal("worker never subscribed")
		}
		time.Sleep(10 * time.Millisecond)
	}

	for i := range jobs {
		if err := bus.PublishProbeJob(ctx, results.ProbeJob{Spec: results.SpecUpdate{
			Kind: results.KindHTTPProbe, Name: fmt.Sprintf("probe-%d", i),
		}}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(100 * time.Millisecond) // two jobs are mid-probe, the rest are buffered
	stop()

	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not return after shutdown")
	}

	pub.mu.Lock()
	defer pub.mu.Unlock()
	if len(pub.published) != jobs {
		t.Fatalf("published %d of %d delivered jobs", len(pub.published), jobs)
	}
	for _, got := range pub.published {
		if got.Result != "ok" {
			t.Fatalf("%s probed against a cancelled context: %s", got.Name, got.Result)
		}
	}
}

// concurrencyExecutor records how many probes it is running at once.
type concurrencyExecutor struct {
	running, peak *atomic.Int64
	delay         time.Duration
}

func (e concurrencyExecutor) Execute(_ context.Context, job results.ProbeJob) results.ProbeResult {
	now := e.running.Add(1)
	for {
		peak := e.peak.Load()
		if now <= peak || e.peak.CompareAndSwap(peak, now) {
			break
		}
	}
	time.Sleep(e.delay)
	e.running.Add(-1)
	return results.ProbeResult{Kind: job.Spec.Kind, Name: job.Spec.Name, Result: "ok"}
}

// One slow target must not hold up the probes queued behind it, and a replica
// must not run more probes at once than it was given slots for.
func TestWorkerRunsJobsConcurrentlyUpToItsLimit(t *testing.T) {
	const jobs, concurrency = 12, 4
	var running, peak atomic.Int64
	pub := &fakeResultPublisher{}
	w := NewWorker(logr.Discard(), nil, pub, concurrency)
	w.executors[results.KindHTTPProbe] = concurrencyExecutor{running: &running, peak: &peak, delay: 100 * time.Millisecond}
	w.slots = make(chan struct{}, concurrency)

	start := time.Now()
	for i := range jobs {
		w.dispatch(context.Background(), results.ProbeJob{Spec: results.SpecUpdate{
			Kind: results.KindHTTPProbe, Name: fmt.Sprintf("probe-%d", i),
		}})
	}
	w.running.Wait()
	elapsed := time.Since(start)

	if got := peak.Load(); got != concurrency {
		t.Fatalf("expected %d probes at once, peaked at %d", concurrency, got)
	}
	if len(pub.published) != jobs {
		t.Fatalf("published %d of %d jobs", len(pub.published), jobs)
	}
	// 12 jobs of 100ms in batches of 4 take ~300ms; one at a time takes 1.2s.
	if elapsed > 700*time.Millisecond {
		t.Fatalf("%d jobs took %s; they ran one at a time", jobs, elapsed)
	}
}
