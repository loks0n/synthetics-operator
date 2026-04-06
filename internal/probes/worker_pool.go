package probes

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
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

type AssertionResult struct {
	Type   string
	Name   string
	Passed bool
}

type Result struct {
	Success          bool
	ConfigError      bool
	StatusCode       int
	Duration         time.Duration
	Completed        time.Time
	Message          string
	AssertionResults []AssertionResult
	CertExpiryTime   *time.Time
}

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
		httpClient = http.DefaultClient
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return Result{
			Completed: time.Now(),
			Duration:  time.Since(start),
			Message:   err.Error(),
		}
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	bodyBytes, _ := io.ReadAll(resp.Body)
	duration := time.Since(start)

	var certExpiry *time.Time
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		t := resp.TLS.PeerCertificates[0].NotAfter
		certExpiry = &t
	}

	var assertions []AssertionResult
	var failMessages []string

	statusPassed := resp.StatusCode == probe.Spec.Assertions.Status
	assertions = append(assertions, AssertionResult{Type: "status", Name: "status", Passed: statusPassed})
	if !statusPassed {
		failMessages = append(failMessages, fmt.Sprintf("status %d != %d", resp.StatusCode, probe.Spec.Assertions.Status))
	}

	if probe.Spec.Assertions.Latency != nil {
		latencyPassed := duration.Milliseconds() <= int64(probe.Spec.Assertions.Latency.MaxMs)
		assertions = append(assertions, AssertionResult{Type: "latency", Name: "latency", Passed: latencyPassed})
		if !latencyPassed {
			failMessages = append(failMessages, fmt.Sprintf("latency %dms > maxMs %d", duration.Milliseconds(), probe.Spec.Assertions.Latency.MaxMs))
		}
	}

	if probe.Spec.Assertions.Body != nil {
		if probe.Spec.Assertions.Body.Contains != "" {
			containsPassed := strings.Contains(string(bodyBytes), probe.Spec.Assertions.Body.Contains)
			assertions = append(assertions, AssertionResult{Type: "body_contains", Name: "body_contains", Passed: containsPassed})
			if !containsPassed {
				failMessages = append(failMessages, "body does not contain expected string")
			}
		}
		for _, ja := range probe.Spec.Assertions.Body.JSON {
			actual, evalErr := evalJSONPath(bodyBytes, ja.Path)
			jsonPassed := evalErr == nil && actual == ja.Value
			assertions = append(assertions, AssertionResult{Type: "body_json", Name: ja.Path, Passed: jsonPassed})
			if !jsonPassed {
				if evalErr != nil {
					failMessages = append(failMessages, fmt.Sprintf("json path %s: %v", ja.Path, evalErr))
				} else {
					failMessages = append(failMessages, fmt.Sprintf("json path %s: %q != %q", ja.Path, actual, ja.Value))
				}
			}
		}
	}

	message := fmt.Sprintf("received status %d", resp.StatusCode)
	if len(failMessages) > 0 {
		message = strings.Join(failMessages, "; ")
	}

	return Result{
		Success:          len(failMessages) == 0,
		StatusCode:       resp.StatusCode,
		Completed:        time.Now(),
		Duration:         duration,
		Message:          message,
		AssertionResults: assertions,
		CertExpiryTime:   certExpiry,
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

	base := http.DefaultTransport.(*http.Transport).Clone()
	base.TLSClientConfig = tlsCfg
	return &http.Client{Transport: base}, nil
}

// evalJSONPath evaluates a simple JSONPath expression against JSON bytes.
// Supports dot-notation paths like $.field, $.field.subfield.
func evalJSONPath(body []byte, path string) (string, error) {
	if path != "$" && !strings.HasPrefix(path, "$.") {
		return "", errors.New("path must start with $")
	}

	var data any
	if err := json.Unmarshal(body, &data); err != nil {
		return "", fmt.Errorf("invalid JSON body: %w", err)
	}

	if path == "$" {
		b, _ := json.Marshal(data)
		return string(b), nil
	}

	current := data
	for part := range strings.SplitSeq(path[2:], ".") {
		m, ok := current.(map[string]any)
		if !ok {
			return "", fmt.Errorf("cannot navigate into non-object at %q", part)
		}
		val, ok := m[part]
		if !ok {
			return "", fmt.Errorf("key %q not found", part)
		}
		current = val
	}

	switch v := current.(type) {
	case string:
		return v, nil
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10), nil
		}
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	case bool:
		return strconv.FormatBool(v), nil
	case nil:
		return "null", nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
}

// probeJob is the unit the WorkerPool queue carries. It contains everything
// needed to execute a probe and record metrics without the WorkerPool knowing
// about any specific CRD type. Future probe types (DnsProbe, K6Test) each
// produce their own probeJob via a type-specific constructor.
type probeJob struct {
	key     types.NamespacedName
	timeout time.Duration
	// run executes the probe and returns the resulting metrics state.
	// ConsecutiveFailures is intentionally left at zero; runProbe fills it in
	// from the previous store snapshot so the worker pool needs no k8s client.
	run func(ctx context.Context) internalmetrics.ProbeState
}

// resultToProbeState converts an HTTP probe Result into a ProbeState suitable
// for the metrics store. ConsecutiveFailures is not computed here — it depends
// on previous state and is filled in by runProbe.
func resultToProbeState(r Result) internalmetrics.ProbeState {
	assertionResults := make([]internalmetrics.AssertionResult, len(r.AssertionResults))
	for i, ar := range r.AssertionResults {
		assertionResults[i] = internalmetrics.AssertionResult{
			Type:   ar.Type,
			Name:   ar.Name,
			Passed: boolToFloat(ar.Passed),
		}
	}
	state := internalmetrics.ProbeState{
		Success:              boolToFloat(r.Success && !r.ConfigError),
		DurationMilliseconds: float64(r.Duration.Milliseconds()),
		LastRunTimestamp:     float64(r.Completed.Unix()),
		ConfigError:          boolToFloat(r.ConfigError),
		AssertionResults:     assertionResults,
	}
	if r.CertExpiryTime != nil {
		state.TLSCertExpiry = float64(r.CertExpiryTime.Unix())
	}
	return state
}

// newHTTPProbeJob constructs a probeJob for an HTTPProbe. This is the only
// place in the codebase that couples the WorkerPool to the HTTPProbe CRD type.
func newHTTPProbeJob(probe *syntheticsv1alpha1.HTTPProbe, exec Executor) probeJob {
	return probeJob{
		key:     types.NamespacedName{Namespace: probe.Namespace, Name: probe.Name},
		timeout: probe.Spec.Timeout.Duration,
		run: func(ctx context.Context) internalmetrics.ProbeState {
			return resultToProbeState(exec.Execute(ctx, probe))
		},
	}
}

// WorkerPool executes probeJobs concurrently. It has no knowledge of any
// specific CRD type; all probe-type-specific logic lives in the probeJob.
// The pool never writes to the Kubernetes API — all results flow into the
// in-memory metrics store only.
type WorkerPool struct {
	logger  logr.Logger
	queue   chan probeJob
	metrics *internalmetrics.Store
	once    sync.Once
}

func NewWorkerPool(logger logr.Logger, concurrency int, metrics *internalmetrics.Store) *WorkerPool {
	if concurrency < 1 {
		concurrency = 1
	}
	return &WorkerPool{
		logger:  logger,
		queue:   make(chan probeJob, concurrency*16),
		metrics: metrics,
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

func (p *WorkerPool) Enqueue(ctx context.Context, job probeJob) {
	select {
	case <-ctx.Done():
		return
	case p.queue <- job:
	default:
		p.logger.Error(errors.New("queue full"), "dropping probe execution", "namespace", job.key.Namespace, "name", job.key.Name)
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

func (p *WorkerPool) runProbe(ctx context.Context, job probeJob) {
	runCtx, cancel := context.WithTimeout(ctx, job.timeout)
	defer cancel()

	state := job.run(runCtx)

	prev, _ := p.metrics.Snapshot(job.key)
	switch {
	case state.Success == 1:
		state.ConsecutiveFailures = 0
	case state.ConfigError == 1:
		state.ConsecutiveFailures = prev.ConsecutiveFailures
	default:
		state.ConsecutiveFailures = prev.ConsecutiveFailures + 1
	}

	p.metrics.Upsert(job.key, state)
}

func boolToFloat(v bool) float64 {
	if v {
		return 1
	}
	return 0
}
