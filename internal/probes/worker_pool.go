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
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
	internalmetrics "github.com/loks0n/synthetics-operator/internal/metrics"
)

// Executor runs a single HTTP probe and returns the result.
type Executor interface {
	Execute(context.Context, *syntheticsv1alpha1.HTTPProbe) Result
}

// HTTPExecutor is the production Executor that makes real HTTP requests.
type HTTPExecutor struct {
	Client *http.Client
}

type Result struct {
	ConfigError       bool
	TransportErr      error // non-nil when the HTTP client returned an error before a response
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
// outcomes are evaluated separately and don't enter here.
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
	defer func() {
		_ = resp.Body.Close()
	}()

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

// buildTLSClient constructs an *http.Client configured from the probe's TLS spec.
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

// newTransport returns a fresh transport that never reuses connections.
// This ensures DNS, TCP connect, and TLS handshake phases are measured on
// every probe run rather than being skipped when a keep-alive connection
// is available from a previous run.
func newTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DisableKeepAlives = true
	return t
}

// Job is the unit the WorkerPool queue carries. It contains everything needed
// to execute a probe and record metrics. Callers construct Jobs via NewHTTPJob
// or NewDNSJob; the WorkerPool and Scheduler are ignorant of probe types.
type Job struct {
	Key      types.NamespacedName
	Interval time.Duration
	Timeout  time.Duration
	Run      func(ctx context.Context)
}

// resultToProbeState converts an HTTP probe Result into a ProbeState suitable
// for the metrics store. Callers then set Result/FailedAssertion via the
// assertion helpers or classifyHTTP.
func resultToProbeState(r Result) internalmetrics.ProbeState {
	state := internalmetrics.ProbeState{
		Kind:                  "HTTPProbe",
		DurationMilliseconds:  float64(r.Duration.Milliseconds()),
		LastRunTimestamp:      float64(r.Completed.Unix()),
		HTTPStatusCode:        float64(r.StatusCode),
		HTTPVersion:           r.HTTPVersion,
		HTTPPhaseDNSMs:        r.PhaseDNSMs,
		HTTPPhaseConnectMs:    r.PhaseConnectMs,
		HTTPPhaseTLSMs:        r.PhaseTLSMs,
		HTTPPhaseProcessingMs: r.PhaseProcessingMs,
		HTTPPhaseTransferMs:   r.PhaseTransferMs,
	}
	if r.CertExpiryTime != nil {
		state.TLSCertExpiry = float64(r.CertExpiryTime.Unix())
	}
	return state
}

// classifyHTTPTransport maps a *http.Client.Do error to a Result enum value.
// Called only when there's no response body at all — assertion_failed lives in
// applyHTTPAssertions.
func classifyHTTPTransport(err error) internalmetrics.Result {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return internalmetrics.ResultDNSFailed
	}
	if isTLSError(err) {
		return internalmetrics.ResultTLSFailed
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		switch opErr.Op {
		case "dial":
			if opErr.Timeout() {
				return internalmetrics.ResultConnectTimeout
			}
			return internalmetrics.ResultConnectRefused
		case "read":
			return internalmetrics.ResultRecvTimeout
		}
	}
	// Bare context.DeadlineExceeded (e.g. body read hit the probe timeout) and
	// any other generic transport error fall through to recv_timeout. Connect-
	// time timeouts are caught by the Op=="dial" branch above.
	if errors.Is(err, context.DeadlineExceeded) {
		return internalmetrics.ResultRecvTimeout
	}
	return internalmetrics.ResultConnectRefused
}

// isTLSError returns true for certificate verification and TLS handshake
// failures. Covers both the typed tls.CertificateVerificationError and x509
// verification errors that wrap with it.
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

func applyHTTPAssertions(state *internalmetrics.ProbeState, r Result, assertions []syntheticsv1alpha1.Assertion) {
	sslExpiryDays := float64(-1)
	if r.CertExpiryTime != nil {
		sslExpiryDays = time.Until(*r.CertExpiryTime).Hours() / 24
	}
	ac := assertionContext{
		"status_code":     float64(r.StatusCode),
		"duration_ms":     float64(r.Duration.Milliseconds()),
		"ssl_expiry_days": sslExpiryDays,
	}
	ok, failedName, results := evalAssertions(assertions, ac)
	state.AssertionResults = results
	if ok {
		state.Result = internalmetrics.ResultOK
		state.FailedAssertion = ""
		return
	}
	state.Result = internalmetrics.ResultAssertionFailed
	state.FailedAssertion = failedName
}

// NewHTTPJob constructs a Job for an HTTPProbe. This is the only place in the
// codebase that couples the Job abstraction to the HTTPProbe CRD type.
func NewHTTPJob(probe *syntheticsv1alpha1.HTTPProbe, exec Executor, store *internalmetrics.Store) Job {
	snapshot := probe.DeepCopy()
	key := types.NamespacedName{Namespace: probe.Namespace, Name: probe.Name}
	return Job{
		Key:      key,
		Interval: snapshot.Spec.Interval.Duration,
		Timeout:  snapshot.Spec.Timeout.Duration,
		Run: func(ctx context.Context) {
			r := exec.Execute(ctx, snapshot)
			state := resultToProbeState(r)
			state.URL = snapshot.Spec.Request.URL
			state.Method = strings.ToUpper(snapshot.Spec.Request.Method)
			switch {
			case r.ConfigError:
				state.Result = internalmetrics.ResultConfigError
			case r.TransportErr != nil:
				state.Result = classifyHTTPTransport(r.TransportErr)
			case len(snapshot.Spec.Assertions) > 0:
				applyHTTPAssertions(&state, r, snapshot.Spec.Assertions)
			default:
				state.Result = internalmetrics.ResultOK
			}
			store.Upsert(key, state)
		},
	}
}

// WorkerPool executes Jobs concurrently. It has no knowledge of any specific
// CRD type; all probe-type-specific logic lives in Job.Run.
// The pool never writes to the Kubernetes API — all results flow into the
// in-memory metrics store only (via Job.Run closures).
type WorkerPool struct {
	logger logr.Logger
	queue  chan Job
	once   sync.Once
}

func NewWorkerPool(logger logr.Logger, concurrency int) *WorkerPool {
	if concurrency < 1 {
		concurrency = 1
	}
	return &WorkerPool{
		logger: logger,
		queue:  make(chan Job, concurrency*16),
	}
}

func (p *WorkerPool) Start(ctx context.Context) error {
	p.once.Do(func() {
		workers := max(1, cap(p.queue)/16)
		for range workers {
			go p.worker(ctx)
		}
	})
	<-ctx.Done()
	return nil
}

func (p *WorkerPool) Enqueue(ctx context.Context, job Job) {
	select {
	case <-ctx.Done():
		return
	case p.queue <- job:
	default:
		p.logger.Error(errors.New("queue full"), "dropping probe execution", "namespace", job.Key.Namespace, "name", job.Key.Name)
	}
}

func (p *WorkerPool) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-p.queue:
			p.runProbe(ctx, job)
		}
	}
}

func (p *WorkerPool) runProbe(ctx context.Context, job Job) {
	runCtx, cancel := context.WithTimeout(ctx, job.Timeout)
	defer cancel()
	job.Run(runCtx)
}

// parseHTTPVersion converts a proto string like "HTTP/1.1" or "HTTP/2.0" to a
// float64 (1.0, 1.1, 2.0, 3.0). Returns 0 for unknown or empty strings.
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
