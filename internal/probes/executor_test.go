package probes

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/loks0n/synthetics-operator/internal/results"
)

func TestExecutorsRejectMissingKindPayload(t *testing.T) {
	timestamp := time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		kind     results.Kind
		executor Executor
	}{
		{name: "HTTPProbe", kind: results.KindHTTPProbe, executor: HTTPExecutor{}},
		{name: "DNSProbe", kind: results.KindDNSProbe, executor: DNSExecutor{}},
		{name: "TCPProbe", kind: results.KindTCPProbe, executor: TCPExecutor{}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			job := results.ProbeJob{
				Spec: results.SpecUpdate{
					Kind: test.kind, Name: "probe", Namespace: "default", Generation: 7,
				},
				ScheduledAt: timestamp,
			}
			got := test.executor.Execute(t.Context(), job)
			if got.Result != "config_error" {
				t.Fatalf("result=%q, want config_error", got.Result)
			}
			if got.Kind != test.kind || got.Name != "probe" || got.Namespace != "default" || got.Generation != 7 || !got.Timestamp.Equal(timestamp) {
				t.Fatalf("identity was not preserved: %+v", got)
			}
		})
	}
}

func TestHTTPExecutorOwnsJobToResultPolicy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	job := results.ProbeJob{Spec: results.SpecUpdate{
		Kind: results.KindHTTPProbe, Name: "api", Namespace: "default", Generation: 3,
		HTTPProbe: &results.HTTPProbeSpecPayload{
			URL: server.URL, Method: http.MethodGet, TimeoutMs: 1000,
			Assertions: []results.Assertion{{Name: "created", Expr: "status_code = 201"}},
		},
	}}

	got := (HTTPExecutor{}).Execute(t.Context(), job)
	if got.Result != "ok" || got.HTTPStatusCode != http.StatusCreated || got.Method != http.MethodGet {
		t.Fatalf("unexpected HTTPProbe result: %+v", got)
	}
	if len(got.AssertionResults) != 1 || got.AssertionResults[0].Result != 1 {
		t.Fatalf("assertions were not projected: %+v", got.AssertionResults)
	}
}

type deadlineDialer struct {
	deadline time.Time
}

func (d *deadlineDialer) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	deadline, ok := ctx.Deadline()
	if ok {
		d.deadline = deadline
	}
	return nil, syscall.ECONNREFUSED
}

func TestTCPExecutorOwnsTimeoutClassificationAndTelemetry(t *testing.T) {
	dialer := &deadlineDialer{}
	start := time.Now()
	job := results.ProbeJob{Spec: results.SpecUpdate{
		Kind: results.KindTCPProbe, Name: "database", Namespace: "default", Generation: 4,
		TCPProbe: &results.TCPProbeSpecPayload{
			Host: "mysql.default.svc", Port: 3306, TimeoutMs: 250,
		},
	}}

	got := (TCPExecutor{Dialer: dialer}).Execute(t.Context(), job)
	if got.Result != "connect_refused" || got.TCPHost != "mysql.default.svc" || got.TCPPort != 3306 {
		t.Fatalf("unexpected TCPProbe result: %+v", got)
	}
	if dialer.deadline.IsZero() {
		t.Fatal("TCPProbe timeout was not applied to the transport context")
	}
	remaining := dialer.deadline.Sub(start)
	if remaining <= 0 || remaining > 500*time.Millisecond {
		t.Fatalf("unexpected transport deadline: %s", remaining)
	}
}

func TestDNSExecutorOwnsPayloadValidation(t *testing.T) {
	job := results.ProbeJob{Spec: results.SpecUpdate{
		Kind: results.KindDNSProbe, Name: "dns", Namespace: "default",
		DNSProbe: &results.DNSProbeSpecPayload{
			Name: "", Type: "A", Resolver: "1.1.1.1:53", TimeoutMs: 1000,
		},
	}}

	got := (DNSExecutor{}).Execute(t.Context(), job)
	if got.Result != "config_error" {
		t.Fatalf("result=%q, want config_error", got.Result)
	}
}

// With the private networks denied, nothing a probe spec names may reach the
// prober's own network: not directly, not through a redirect, not as a TCP
// target and not as a DNS resolver. Each is refused before anything is sent.
func TestExecutorsRefuseDeniedNetworks(t *testing.T) {
	var internalHits int
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		internalHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer internal.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/admin", http.StatusFound)
	}))
	defer redirect.Close()
	_, port, _ := net.SplitHostPort(internal.Listener.Addr().String())
	portNumber, _ := strconv.Atoi(port)

	denied, err := ParseDeniedNetworks("private")
	if err != nil {
		t.Fatal(err)
	}
	egress := Egress{Denied: denied}
	httpJob := func(url string) results.ProbeJob {
		return results.ProbeJob{Spec: results.SpecUpdate{Kind: results.KindHTTPProbe,
			HTTPProbe: &results.HTTPProbeSpecPayload{TimeoutMs: 2000, URL: url, Method: http.MethodGet}}}
	}
	tests := []struct {
		name     string
		executor Executor
		job      results.ProbeJob
	}{
		{"loopback URL", HTTPExecutor{Egress: egress}, httpJob(internal.URL)},
		{"redirect to loopback", HTTPExecutor{Egress: egress}, httpJob(redirect.URL)},
		{"cloud metadata", HTTPExecutor{Egress: egress}, httpJob("http://169.254.169.254/computeMetadata/v1/")},
		{"loopback TCP port", TCPExecutor{Egress: egress}, results.ProbeJob{Spec: results.SpecUpdate{Kind: results.KindTCPProbe,
			TCPProbe: &results.TCPProbeSpecPayload{TimeoutMs: 2000, Host: "127.0.0.1", Port: int32(portNumber)}}}},
		{"loopback DNS resolver", DNSExecutor{Egress: egress}, results.ProbeJob{Spec: results.SpecUpdate{Kind: results.KindDNSProbe,
			DNSProbe: &results.DNSProbeSpecPayload{TimeoutMs: 2000, Name: "example.com", Type: "A", Resolver: "127.0.0.1:" + port}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			start := time.Now()
			result := test.executor.Execute(context.Background(), test.job)
			if result.Result != "target_denied" {
				t.Fatalf("expected target_denied, got %s", result.Result)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("refusal took %s; the address was dialled", elapsed)
			}
		})
	}

	// The zero value denies nothing, so probes of in-cluster services keep working.
	if result := (HTTPExecutor{}).Execute(context.Background(), httpJob(internal.URL)); result.Result != "ok" {
		t.Fatalf("unguarded probe of a loopback service: expected ok, got %s", result.Result)
	}
	if internalHits != 1 {
		t.Fatalf("internal service saw %d requests; only the unguarded check below may reach it", internalHits)
	}
}
