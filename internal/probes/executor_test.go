package probes

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
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
