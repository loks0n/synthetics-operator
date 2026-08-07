// Package probes implements HTTP, DNS, and TCP probe execution plus the
// in-process scheduler. The scheduler publishes probe jobs to NATS; the
// per-kind executors form the complete job-to-result interface used by the
// prober deployment.
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

	"github.com/loks0n/synthetics-operator/internal/results"
)

// HTTPExecutor satisfies Executor at compile time.
var _ Executor = HTTPExecutor{}

// HTTPExecutor is the production Executor that makes real HTTP requests.
type HTTPExecutor struct {
	Client *http.Client
}

// httpResult holds transport details while HTTPExecutor builds the public
// ProbeResult returned through the Executor interface.
type httpResult struct {
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
func (r httpResult) success() bool { return !r.ConfigError && r.TransportErr == nil }

type httpRequest struct {
	URL     string
	Method  string
	Headers map[string]string
	Body    string
	TLS     *results.TLSConfig
}

// Execute owns the full HTTPProbe job-to-result policy.
func (e HTTPExecutor) Execute(ctx context.Context, job results.ProbeJob) results.ProbeResult {
	payload := job.Spec.HTTPProbe
	if payload == nil {
		return configErrorResult(job)
	}

	runCtx, cancel := runContext(ctx, payload.TimeoutMs)
	defer cancel()

	raw := e.executeRequest(runCtx, httpRequest{
		URL: payload.URL, Method: payload.Method, Headers: payload.Headers,
		Body: payload.Body, TLS: payload.TLS,
	})
	out := probeResult(job)
	out.DurationMs = raw.Duration.Milliseconds()
	out.URL = payload.URL
	out.Method = strings.ToUpper(payload.Method)
	out.HTTPStatusCode = raw.StatusCode
	out.HTTPVersion = raw.HTTPVersion
	out.HTTPPhaseDNSMs = raw.PhaseDNSMs
	out.HTTPPhaseConnectMs = raw.PhaseConnectMs
	out.HTTPPhaseTLSMs = raw.PhaseTLSMs
	out.HTTPPhaseProcessingMs = raw.PhaseProcessingMs
	out.HTTPPhaseTransferMs = raw.PhaseTransferMs
	if raw.CertExpiryTime != nil {
		out.TLSCertExpiryUnix = raw.CertExpiryTime.Unix()
	}

	switch {
	case raw.ConfigError:
		out.Result = "config_error"
	case raw.TransportErr != nil:
		out.Result = classifyHTTPTransport(raw.TransportErr)
	case len(payload.Assertions) > 0:
		out.Result, out.FailedAssertion, out.AssertionResults = evalHTTPAssertions(raw, payload.Assertions)
	default:
		out.Result = "ok"
	}
	return out
}

func (e HTTPExecutor) executeRequest(ctx context.Context, request httpRequest) httpResult {
	start := time.Now()
	parsedURL, err := url.Parse(request.URL)
	if err != nil || parsedURL == nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
		return httpResult{
			ConfigError: true,
			Completed:   time.Now(),
			Duration:    time.Since(start),
			Message:     "invalid request URL",
		}
	}

	var bodyReader io.Reader
	if request.Body != "" {
		bodyReader = strings.NewReader(request.Body)
	}

	req, err := http.NewRequestWithContext(ctx, strings.ToUpper(request.Method), request.URL, bodyReader)
	if err != nil {
		return httpResult{
			ConfigError: true,
			Completed:   time.Now(),
			Duration:    time.Since(start),
			Message:     fmt.Sprintf("build request: %v", err),
		}
	}
	for key, val := range request.Headers {
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
	if request.TLS != nil {
		tlsClient, tlsErr := e.buildTLSClient(request.TLS)
		if tlsErr != nil {
			return httpResult{
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
		return httpResult{
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

	return httpResult{
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

func (e HTTPExecutor) buildTLSClient(config *results.TLSConfig) (*http.Client, error) {
	tlsCfg := &tls.Config{
		InsecureSkipVerify: config.InsecureSkipVerify,
	}
	if config.CACert != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(config.CACert)) {
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

// classifyHTTPTransport maps an http.Client.Do error to the public result
// vocabulary.
func classifyHTTPTransport(err error) string {
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
