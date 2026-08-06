package probes

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/loks0n/synthetics-operator/internal/results"
)

// TCPDialer is the connection surface TCPExecutor needs. net.Dialer satisfies
// it; tests can inject deterministic network failures without sleeping.
type TCPDialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// tcpResult holds connection details while TCPExecutor builds the public
// ProbeResult returned through the Executor interface.
type tcpResult struct {
	ConfigError bool
	ConnectErr  error
	Duration    time.Duration
	Completed   time.Time
	Message     string
}

func (r tcpResult) success() bool { return !r.ConfigError && r.ConnectErr == nil }

type tcpTarget struct {
	Host string
	Port int32
}

// TCPExecutor establishes and immediately closes a TCP connection. It does
// not send bytes or perform a protocol/TLS handshake.
type TCPExecutor struct {
	Dialer TCPDialer
}

var _ Executor = TCPExecutor{}

// Execute owns the full TCPProbe job-to-result policy.
func (e TCPExecutor) Execute(ctx context.Context, job results.ProbeJob) results.ProbeResult {
	payload := job.Spec.TCPProbe
	if payload == nil {
		return configErrorResult(job)
	}

	runCtx, cancel := runContext(ctx, payload.TimeoutMs)
	defer cancel()

	raw := e.connect(runCtx, tcpTarget{Host: payload.Host, Port: payload.Port})
	out := probeResult(job)
	out.DurationMs = raw.Duration.Milliseconds()
	out.TCPHost = payload.Host
	out.TCPPort = payload.Port

	switch {
	case raw.ConfigError:
		out.Result = "config_error"
	case raw.ConnectErr != nil:
		out.Result = classifyTCPConnect(raw.ConnectErr)
	case len(payload.Assertions) > 0:
		out.Result, out.FailedAssertion, out.AssertionResults = evalTCPAssertions(raw, payload.Assertions)
	default:
		out.Result = "ok"
	}
	return out
}

func (e TCPExecutor) connect(ctx context.Context, target tcpTarget) tcpResult {
	start := time.Now()
	host := strings.TrimSpace(target.Host)
	port := target.Port
	if host == "" || port < 1 || port > 65535 {
		return tcpResult{
			ConfigError: true,
			Duration:    time.Since(start),
			Completed:   time.Now(),
			Message:     "target host and a port between 1 and 65535 are required",
		}
	}

	dialer := e.Dialer
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	address := net.JoinHostPort(host, strconv.Itoa(int(port)))
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return tcpResult{
			ConnectErr: err,
			Duration:   time.Since(start),
			Completed:  time.Now(),
			Message:    err.Error(),
		}
	}
	_ = conn.Close()
	return tcpResult{
		Duration:  time.Since(start),
		Completed: time.Now(),
		Message:   "connected to " + address,
	}
}

// classifyTCPConnect maps dial failures to the public result vocabulary.
func classifyTCPConnect(err error) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns_failed"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "connect_timeout"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "connect_timeout"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "connect_refused"
	}
	return "connect_failed"
}
