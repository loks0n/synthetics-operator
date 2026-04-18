// Package probes implements HTTP and DNS probe execution plus the
// in-process scheduler. The scheduler publishes probe jobs to NATS; the
// executors themselves are pure logic shared with the prober
// deployment.
package probes

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"time"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
)

// Executor runs a single HTTP probe and returns the result. Declared as an
// interface so the prober package can call against the abstraction
// without importing the concrete transport-specific details.
type Executor interface {
	Execute(context.Context, *syntheticsv1alpha1.HTTPProbe) Result
}

// HTTPExecutor satisfies Executor at compile time.
var _ Executor = HTTPExecutor{}

// HTTPExecutor is the production Executor that makes real HTTP requests.
type HTTPExecutor struct {
	Client *http.Client
}

// Result holds the outcome of a single HTTP probe execution. Callers
// inspect the ConfigError / TransportErr / StatusCode fields to decide
// what Result enum value to emit.
type Result struct {
	ConfigError       bool
	TransportErr      error
	StatusCode        int
	HTTPVersion       float64
	Duration          time.Duration
	Completed         time.Time
	Message           string
	CertExpiryTime    *time.Time
	PhaseDNSMs        float64
	PhaseConnectMs    float64
	PhaseTLSMs        float64
	PhaseProcessingMs float64
	PhaseTransferMs   float64
}

// Success reports whether the HTTP request completed end-to-end. Assertion
// outcomes are evaluated separately.
func (r Result) Success() bool { return !r.ConfigError && r.TransportErr == nil }

func (e HTTPExecutor) Execute(ctx context.Context, probe *syntheticsv1alpha1.HTTPProbe) Result {
	start := time.Now()
	parsedURL, err := url.Parse(probe.Spec.Request.URL)
	if err != nil || parsedURL == nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
		return Result{
			ConfigError: true,
			Completed:   time.Now(),
			Duration:    time.Since(start),
			Message:     "invalid request URL",
		}
	}

	var bodyReader io.Reader
	if probe.Spec.Request.Body != "" {
		bodyReader = strings.NewReader(probe.Spec.Request.Body)
	}

	req, err := http.NewRequestWithContext(ctx, strings.ToUpper(probe.Spec.Request.Method), probe.Spec.Request.URL, bodyReader)
	if err != nil {
		return Result{
			ConfigError: true,
			Completed:   time.Now(),
			Duration:    time.Since(start),
			Message:     fmt.Sprintf("build request: %v", err),
		}
	}
	for key, val := range probe.Spec.Request.Headers {
		req.Header.Set(key, val)
	}

	var (
		dnsStart, dnsDone         time.Time
		connectStart, connectDone time.Time
		tlsStart, tlsDone         time.Time
		wroteRequest              time.Time
		firstByte                 time.Time
	)
	trace := &httptrace.ClientTrace{
		DNSStart:             func(_ httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone:              func(_ httptrace.DNSDoneInfo) { dnsDone = time.Now() },
		ConnectStart:         func(_, _ string) { connectStart = time.Now() },
		ConnectDone:          func(_, _ string, _ error) { connectDone = time.Now() },
		TLSHandshakeStart:    func() { tlsStart = time.Now() },
		TLSHandshakeDone:     func(_ tls.ConnectionState, _ error) { tlsDone = time.Now() },
		WroteRequest:         func(_ httptrace.WroteRequestInfo) { wroteRequest = time.Now() },
		GotFirstResponseByte: func() { firstByte = time.Now() },
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))

	httpClient := e.Client
	if probe.Spec.TLS != nil {
		tlsClient, tlsErr := e.buildTLSClient(probe)
		if tlsErr != nil {
			return Result{
				ConfigError: true,
				Completed:   time.Now(),
				Duration:    time.Since(start),
				Message:     fmt.Sprintf("build TLS client: %v", tlsErr),
			}
		}
		httpClient = tlsClient
	}
	if httpClient == nil {
		httpClient = &http.Client{Transport: newTransport()}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return Result{
			TransportErr: err,
			Completed:    time.Now(),
			Duration:     time.Since(start),
			Message:      err.Error(),
		}
	}
	defer func() { _ = resp.Body.Close() }()

	transferStart := time.Now()
	_, _ = io.ReadAll(resp.Body)
	transferEnd := time.Now()
	duration := time.Since(start)

	msDiff := func(a, b time.Time) float64 {
		if a.IsZero() || b.IsZero() {
			return 0
		}
		return float64(b.Sub(a).Milliseconds())
	}

	var certExpiry *time.Time
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		t := resp.TLS.PeerCertificates[0].NotAfter
		certExpiry = &t
	}

	return Result{
		StatusCode:        resp.StatusCode,
		HTTPVersion:       parseHTTPVersion(resp.Proto),
		Completed:         time.Now(),
		Duration:          duration,
		Message:           fmt.Sprintf("received status %d", resp.StatusCode),
		CertExpiryTime:    certExpiry,
		PhaseDNSMs:        msDiff(dnsStart, dnsDone),
		PhaseConnectMs:    msDiff(connectStart, connectDone),
		PhaseTLSMs:        msDiff(tlsStart, tlsDone),
		PhaseProcessingMs: msDiff(wroteRequest, firstByte),
		PhaseTransferMs:   float64(transferEnd.Sub(transferStart).Milliseconds()),
	}
}

func (e HTTPExecutor) buildTLSClient(probe *syntheticsv1alpha1.HTTPProbe) (*http.Client, error) {
	tlsCfg := &tls.Config{
		InsecureSkipVerify: probe.Spec.TLS.InsecureSkipVerify,
	}
	if probe.Spec.TLS.CACert != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(probe.Spec.TLS.CACert)) {
			return nil, errors.New("tls.caCert contains no valid PEM certificates")
		}
		tlsCfg.RootCAs = pool
	}
	base := newTransport()
	base.TLSClientConfig = tlsCfg
	return &http.Client{Transport: base}, nil
}

func newTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DisableKeepAlives = true
	return t
}

// ClassifyHTTPTransport maps an http.Client.Do error to a result-class
// string (matching metrics.Result values). Called by probers after
// Execute when TransportErr is non-nil.
func ClassifyHTTPTransport(err error) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns_failed"
	}
	if isTLSError(err) {
		return "tls_failed"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		switch opErr.Op {
		case "dial":
			if opErr.Timeout() {
				return "connect_timeout"
			}
			return "connect_refused"
		case "read":
			return "recv_timeout"
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "recv_timeout"
	}
	return "connect_refused"
}

func isTLSError(err error) bool {
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return true
	}
	var unknownAuthErr x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthErr) {
		return true
	}
	var invalidErr x509.CertificateInvalidError
	if errors.As(err, &invalidErr) {
		return true
	}
	var hostnameErr x509.HostnameError
	return errors.As(err, &hostnameErr)
}

func parseHTTPVersion(proto string) float64 {
	switch strings.TrimPrefix(proto, "HTTP/") {
	case "1.0":
		return 1.0
	case "1.1":
		return 1.1
	case "2", "2.0":
		return 2.0
	case "3", "3.0":
		return 3.0
	}
	return 0
}
