package prober

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-logr/logr"

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
	w := &Worker{Log: logr.Discard(), Publisher: pub}
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
	w := &Worker{Log: logr.Discard(), Publisher: pub}

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
	w := &Worker{Log: logr.Discard(), Publisher: pub}

	job := httpJob("http://127.0.0.1:1")
	job.Spec.Suspend = true
	w.onJob(context.Background(), job)

	if len(pub.published) != 0 {
		t.Fatalf("suspended probe should publish nothing, got %d results", len(pub.published))
	}
}

func TestWorkerIgnoresTestKinds(t *testing.T) {
	pub := &fakeResultPublisher{}
	w := &Worker{Log: logr.Discard(), Publisher: pub}

	job := httpJob("http://127.0.0.1:1")
	job.Spec.Kind = results.KindK6Test
	w.onJob(context.Background(), job)

	if len(pub.published) != 0 {
		t.Fatalf("test kinds are executed by CronJobs, got %d results", len(pub.published))
	}
}
