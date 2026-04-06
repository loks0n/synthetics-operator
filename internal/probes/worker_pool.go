package probes

import (
	"context"
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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

	req, err := http.NewRequestWithContext(ctx, strings.ToUpper(probe.Spec.Request.Method), probe.Spec.Request.URL, nil)
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
	}
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
// needed to execute a probe and record results without the WorkerPool knowing
// about any specific CRD type. Future probe types (DnsProbe, K6Test) each
// produce their own probeJob via a type-specific constructor.
type probeJob struct {
	key     types.NamespacedName
	timeout time.Duration
	// run executes the probe and returns a jobResult ready for the retry loop.
	run func(ctx context.Context) jobResult
}

// jobResult captures the execution output and knows how to apply it to the
// live k8s object. Produced by probeJob.run; consumed inside runProbe's
// retry.RetryOnConflict closure.
type jobResult struct {
	// newObject returns a fresh zero-value instance of the probe's CRD type
	// for use as the target of client.Get inside the retry loop.
	newObject func() client.Object
	// applyStatus mutates the live object in-place (setting status fields,
	// conditions, consecutive-failure counter) and returns the metrics state
	// to record. Called inside retry.RetryOnConflict; must be idempotent when
	// given the same live object.
	applyStatus func(client.Object) internalmetrics.ProbeState
}

// httpProbeApplier applies an HTTP probe Result to a live HTTPProbe k8s object.
// It is the HTTPProbe-specific half of the probe/metrics translation that was
// previously embedded in runProbe.
type httpProbeApplier struct {
	result Result
}

func (a *httpProbeApplier) apply(obj client.Object) internalmetrics.ProbeState {
	current := obj.(*syntheticsv1alpha1.HTTPProbe)
	now := metav1.NewTime(a.result.Completed)

	current.Status.ObservedGeneration = current.Generation
	current.Status.LastRunTime = &now
	current.Status.Summary = &syntheticsv1alpha1.ProbeSummary{
		Success:     a.result.Success && !a.result.ConfigError,
		ConfigError: a.result.ConfigError,
		StatusCode:  a.result.StatusCode,
		Message:     a.result.Message,
	}

	switch {
	case a.result.Success && !a.result.ConfigError:
		current.Status.ConsecutiveFailures = 0
		current.Status.LastSuccessTime = &now
		apimeta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type:               syntheticsv1alpha1.ConditionReady,
			Status:             metav1.ConditionTrue,
			Reason:             syntheticsv1alpha1.ReasonProbeSucceeded,
			Message:            a.result.Message,
			LastTransitionTime: now,
			ObservedGeneration: current.Generation,
		})
	case a.result.ConfigError:
		apimeta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type:               syntheticsv1alpha1.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             syntheticsv1alpha1.ReasonConfigError,
			Message:            a.result.Message,
			LastTransitionTime: now,
			ObservedGeneration: current.Generation,
		})
	default:
		current.Status.ConsecutiveFailures++
		current.Status.LastFailureTime = &now
		apimeta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type:               syntheticsv1alpha1.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             syntheticsv1alpha1.ReasonProbeFailed,
			Message:            a.result.Message,
			LastTransitionTime: now,
			ObservedGeneration: current.Generation,
		})
	}

	assertionResults := make([]internalmetrics.AssertionResult, len(a.result.AssertionResults))
	for i, ar := range a.result.AssertionResults {
		assertionResults[i] = internalmetrics.AssertionResult{
			Type:   ar.Type,
			Name:   ar.Name,
			Passed: boolToFloat(ar.Passed),
		}
	}

	return internalmetrics.ProbeState{
		Success:              boolToFloat(a.result.Success && !a.result.ConfigError),
		DurationMilliseconds: float64(a.result.Duration.Milliseconds()),
		LastRunTimestamp:     float64(a.result.Completed.Unix()),
		ConfigError:          boolToFloat(a.result.ConfigError),
		ConsecutiveFailures:  float64(current.Status.ConsecutiveFailures),
		AssertionResults:     assertionResults,
	}
}

// newHTTPProbeJob constructs a probeJob for an HTTPProbe. This is the only
// place in the codebase that couples the WorkerPool to the HTTPProbe CRD type.
func newHTTPProbeJob(probe *syntheticsv1alpha1.HTTPProbe, exec Executor) probeJob {
	return probeJob{
		key:     types.NamespacedName{Namespace: probe.Namespace, Name: probe.Name},
		timeout: probe.Spec.Timeout.Duration,
		run: func(ctx context.Context) jobResult {
			result := exec.Execute(ctx, probe)
			applier := &httpProbeApplier{result: result}
			return jobResult{
				newObject:   func() client.Object { return &syntheticsv1alpha1.HTTPProbe{} },
				applyStatus: applier.apply,
			}
		},
	}
}

// WorkerPool executes probeJobs concurrently. It has no knowledge of any
// specific CRD type; all probe-type-specific logic lives in the probeJob.
type WorkerPool struct {
	logger  logr.Logger
	queue   chan probeJob
	metrics *internalmetrics.Store
	client  client.Client
	once    sync.Once
}

func NewWorkerPool(logger logr.Logger, concurrency int, metrics *internalmetrics.Store, kubeClient client.Client) *WorkerPool {
	if concurrency < 1 {
		concurrency = 1
	}
	return &WorkerPool{
		logger:  logger,
		queue:   make(chan probeJob, concurrency*16),
		metrics: metrics,
		client:  kubeClient,
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

	res := job.run(runCtx)

	_ = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := res.newObject()
		if err := p.client.Get(ctx, job.key, current); err != nil {
			return client.IgnoreNotFound(err)
		}
		original := current.DeepCopyObject().(client.Object)
		state := res.applyStatus(current)
		p.metrics.Upsert(job.key, state)
		return p.client.Status().Patch(ctx, current, client.MergeFrom(original))
	})
}

func boolToFloat(v bool) float64 {
	if v {
		return 1
	}
	return 0
}
