package prober

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/redis/go-redis/v9"

	"github.com/loks0n/synthetics-operator/internal/natsbus"
	internalprobes "github.com/loks0n/synthetics-operator/internal/probes"
	"github.com/loks0n/synthetics-operator/internal/redisbus"
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
	w := NewWorker(logr.Discard(), nil, pub, 1, internalprobes.Egress{})
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
	w := NewWorker(logr.Discard(), nil, pub, 1, internalprobes.Egress{})

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
	w := NewWorker(logr.Discard(), nil, pub, 1, internalprobes.Egress{})

	job := httpJob("http://127.0.0.1:1")
	job.Spec.Suspend = true
	w.onJob(context.Background(), job)

	if len(pub.published) != 0 {
		t.Fatalf("suspended probe should publish nothing, got %d results", len(pub.published))
	}
}

func TestWorkerIgnoresTestKinds(t *testing.T) {
	pub := &fakeResultPublisher{}
	w := NewWorker(logr.Discard(), nil, pub, 1, internalprobes.Egress{})

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
	executors := defaultExecutors(internalprobes.Egress{})
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
	w := NewWorker(logr.Discard(), nil, pub, concurrency, internalprobes.Egress{})
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

func TestBusDoesNotLogTheNATSPassword(t *testing.T) {
	const password = "s3cret-nats-password"
	server, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true, Username: "appwrite", Password: password})
	if err != nil {
		t.Fatal(err)
	}
	server.Start()
	defer server.Shutdown()
	if !server.ReadyForConnections(5 * time.Second) {
		t.Fatal("NATS server not ready")
	}

	var mu sync.Mutex
	var lines []string
	log := funcr.New(func(prefix, args string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, args)
	}, funcr.Options{})

	addr := server.Addr().(*net.TCPAddr)
	bus, err := natsbus.Connect(log, fmt.Sprintf("nats://appwrite:%s@127.0.0.1:%d", password, addr.Port))
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Close()

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		logged := strings.Join(lines, "\n")
		mu.Unlock()
		if strings.Contains(logged, "NATS connected") {
			if strings.Contains(logged, password) {
				t.Fatalf("log contains the NATS password: %s", logged)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no connect log line; got %q", logged)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// logLines collects what a logger writes, for tests that read it back.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logger() logr.Logger {
	return funcr.New(func(prefix, args string) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.lines = append(l.lines, prefix+" "+args)
	}, funcr.Options{})
}

func (l *logLines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// startRedisWorker runs w until the test ends, and returns a client on the
// same Redis for the test to play scheduler and result consumer with.
//
// The returned channel closes once Start returns, with its error in *err.
func startRedisWorker(t *testing.T, server *miniredis.Miniredis, password string, w *Worker) (client *redis.Client, stop context.CancelFunc, stopped <-chan struct{}, err *error) {
	t.Helper()
	client = redis.NewClient(&redis.Options{Addr: server.Addr(), Password: password})
	t.Cleanup(func() { _ = client.Close() })
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	var startErr error
	go func() {
		defer close(done)
		startErr = w.Start(ctx)
	}()
	t.Cleanup(func() {
		stop()
		<-done
	})
	return client, stop, done, &startErr
}

// popResult waits for the next result the prober pushed, as its consumer would.
func popResult(t *testing.T, client *redis.Client) map[string]any {
	t.Helper()
	reply, err := client.BRPop(context.Background(), 5*time.Second, results.SubjectProbeResults).Result()
	if err != nil {
		t.Fatalf("no probe result: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(reply[1]), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

// A stack with Redis and no NATS pushes jobs as JSON onto a list, written by
// a producer in another language rather than marshalled from these structs.
// The prober must probe the target and push the result back, skipping a job
// it cannot decode instead of stopping.
func TestWorkerProbesJobsFromRedis(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer target.Close()

	server := miniredis.RunT(t)
	bus, err := redisbus.Connect(logr.Discard(), "redis://"+server.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Close()
	client, _, _, _ := startRedisWorker(t, server, "", NewWorker(logr.Discard(), bus, bus, 1, internalprobes.Egress{}))

	job := `{"spec":{"kind":"HTTPProbe","namespace":"project","name":"monitor","generation":42,"intervalMs":60000,` +
		`"httpProbe":{"timeoutMs":5000,"url":"` + target.URL + `","method":"GET"}},"scheduledAt":"2026-11-17T10:00:00.000000Z"}`
	if err := client.LPush(context.Background(), results.SubjectProbeJobs, "not json", job).Err(); err != nil {
		t.Fatal(err)
	}

	got := popResult(t, client)
	if got["kind"] != "HTTPProbe" || got["namespace"] != "project" || got["name"] != "monitor" || got["generation"] != float64(42) {
		t.Fatalf("result names another job: %v", got)
	}
	if got["result"] != "ok" || got["httpStatusCode"] != float64(http.StatusTeapot) {
		t.Fatalf("unexpected result: %v", got)
	}
	if n := client.LLen(context.Background(), results.SubjectProbeJobs).Val(); n != 0 {
		t.Fatalf("%d jobs left on the list", n)
	}
}

// Over Redis a replica pops a job only when it has room for one, so stopping
// it mid-batch finishes what it popped and leaves the rest on the list for
// the next replica, rather than holding them in a buffer that dies with it.
func TestRedisWorkerLeavesJobsItHasNoRoomForOnShutdown(t *testing.T) {
	server := miniredis.RunT(t)
	bus, err := redisbus.Connect(logr.Discard(), "redis://"+server.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Close()

	const jobs = 6
	pub := &fakeResultPublisher{}
	executors := defaultExecutors(internalprobes.Egress{})
	executors[results.KindHTTPProbe] = blockingExecutor{delay: 300 * time.Millisecond}
	w := &Worker{Log: logr.Discard(), Bus: bus, Publisher: pub, Concurrency: 2, executors: executors}
	client, stop, stopped, startErr := startRedisWorker(t, server, "", w)

	for i := range jobs {
		data, _ := json.Marshal(results.ProbeJob{Spec: results.SpecUpdate{Kind: results.KindHTTPProbe, Name: fmt.Sprintf("probe-%d", i)}})
		if err := client.LPush(context.Background(), results.SubjectProbeJobs, data).Err(); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(100 * time.Millisecond) // two jobs are mid-probe, a third waits for a slot
	stop()
	select {
	case <-stopped:
		if *startErr != nil {
			t.Fatal(*startErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not return after shutdown")
	}

	pub.mu.Lock()
	defer pub.mu.Unlock()
	left := client.LLen(context.Background(), results.SubjectProbeJobs).Val()
	if len(pub.published) != 3 || left != jobs-3 {
		t.Fatalf("probed %d and left %d of %d jobs; want 3 probed, the rest left", len(pub.published), left, jobs)
	}
	for _, got := range pub.published {
		if got.Result != "ok" {
			t.Fatalf("%s probed against a cancelled context: %s", got.Name, got.Result)
		}
	}
}

// Redis restarting must not stop the prober: it says so once, keeps retrying,
// and probes the jobs pushed after Redis is back.
func TestRedisWorkerRecoversFromARedisRestart(t *testing.T) {
	server := miniredis.RunT(t)
	var logs logLines
	// No retries inside go-redis, so every pop during the outage fails.
	bus, err := redisbus.Connect(logs.logger(), "redis://"+server.Addr()+"?max_retries=-1")
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Close()

	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	startRedisWorker(t, server, "", NewWorker(logr.Discard(), bus, bus, 1, internalprobes.Egress{}))
	time.Sleep(100 * time.Millisecond)
	address := server.Addr()
	server.Close()
	time.Sleep(time.Second) // several pops fail while Redis is down
	// A fresh server on the same address: miniredis's own Restart hangs on the
	// BRPOP it was blocked in when it closed.
	server = miniredis.NewMiniRedis()
	if err := server.StartAddr(address); err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client := redis.NewClient(&redis.Options{Addr: address})
	defer client.Close()

	addr := listener.Addr().(*net.TCPAddr)
	data, _ := json.Marshal(results.ProbeJob{Spec: results.SpecUpdate{
		Kind: results.KindTCPProbe, Namespace: "project", Name: "database",
		TCPProbe: &results.TCPProbeSpecPayload{Host: addr.IP.String(), Port: int32(addr.Port), TimeoutMs: 1000},
	}})
	if err := client.LPush(context.Background(), results.SubjectProbeJobs, data).Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	})
	if got := popResult(t, client); got["name"] != "database" || got["result"] != "ok" {
		t.Fatalf("unexpected result after the restart: %v\n%s", got, logs.String())
	}

	logged := logs.String()
	if n := strings.Count(logged, "Redis unreachable"); n != 1 {
		t.Fatalf("logged the outage %d times, want once:\n%s", n, logged)
	}
	if !strings.Contains(logged, "Redis reachable again") {
		t.Fatalf("never logged the recovery:\n%s", logged)
	}
}

// The Redis URL carries the password, and neither the prober's logs nor the
// error for a URL it cannot parse may repeat it.
func TestRedisBusDoesNotLogThePassword(t *testing.T) {
	const password = "s3cret-redis-password"
	if _, err := redisbus.Connect(logr.Discard(), "redis://:"+password+"@127.0.0.1:port"); err == nil || strings.Contains(err.Error(), password) {
		t.Fatalf("parse error leaks the password or is missing: %v", err)
	}

	server := miniredis.RunT(t)
	server.RequireAuth(password)
	var logs logLines
	bus, err := redisbus.Connect(logs.logger(), "redis://:"+password+"@"+server.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Close()
	client, _, _, _ := startRedisWorker(t, server, password, NewWorker(logs.logger(), bus, bus, 1, internalprobes.Egress{}))

	data, _ := json.Marshal(httpJob("http://127.0.0.1:1"))
	if err := client.LPush(context.Background(), results.SubjectProbeJobs, data).Err(); err != nil {
		t.Fatal(err)
	}
	if got := popResult(t, client); got["result"] != "connect_refused" {
		t.Fatalf("unexpected result: %v", got)
	}
	if logged := logs.String(); strings.Contains(logged, password) {
		t.Fatalf("log contains the Redis password: %s", logged)
	}
}
