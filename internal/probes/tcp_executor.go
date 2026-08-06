package probes

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"syscall"
	"time"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
)

// TCPDialer is the connection surface TCPExecutor needs. net.Dialer satisfies
// it; tests can inject deterministic network failures without sleeping.
type TCPDialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// TCPResult holds the outcome of one TCP connection attempt.
type TCPResult struct {
	ConfigError bool
	ConnectErr  error
	Duration    time.Duration
	Completed   time.Time
	Message     string
}

func (r TCPResult) Success() bool { return !r.ConfigError && r.ConnectErr == nil }

// TCPExecutor establishes and immediately closes a TCP connection. It does
// not send bytes or perform a protocol/TLS handshake.
type TCPExecutor struct {
	Dialer TCPDialer
}

func (e TCPExecutor) Execute(ctx context.Context, probe *syntheticsv1alpha1.TCPProbe) TCPResult {
	start := time.Now()
	host := strings.TrimSpace(probe.Spec.Target.Host)
	port := probe.Spec.Target.Port
	if host == "" || port < 1 || port > 65535 {
		return TCPResult{
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
		return TCPResult{
			ConnectErr: err,
			Duration:   time.Since(start),
			Completed:  time.Now(),
			Message:    err.Error(),
		}
	}
	_ = conn.Close()
	return TCPResult{
		Duration:  time.Since(start),
		Completed: time.Now(),
		Message:   "connected to " + address,
	}
}

// ClassifyTCPConnect maps dial failures to the public result vocabulary.
func ClassifyTCPConnect(err error) string {
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
