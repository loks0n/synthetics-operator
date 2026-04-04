package probes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

type Executor interface {
	Execute(context.Context, *syntheticsv1alpha1.HttpProbe) Result
}

type HTTPExecutor struct {
	Client *http.Client
}

type Result struct {
	Success     bool
	ConfigError bool
	StatusCode  int
	Duration    time.Duration
	Completed   time.Time
	Message     string
}

func (e HTTPExecutor) Execute(ctx context.Context, probe *syntheticsv1alpha1.HttpProbe) Result {
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
	_, _ = io.Copy(io.Discard, resp.Body)

	return Result{
		Success:    resp.StatusCode == probe.Spec.Assertions.Status,
		StatusCode: resp.StatusCode,
		Completed:  time.Now(),
		Duration:   time.Since(start),
		Message:    fmt.Sprintf("received status %d", resp.StatusCode),
	}
}

type WorkerPool struct {
	logger   logr.Logger
	queue    chan *syntheticsv1alpha1.HttpProbe
	executor Executor
	metrics  *internalmetrics.Store
	client   client.Client
	once     sync.Once
}

func NewWorkerPool(logger logr.Logger, concurrency int, executor Executor, metrics *internalmetrics.Store, kubeClient client.Client) *WorkerPool {
	if concurrency < 1 {
		concurrency = 1
	}
	return &WorkerPool{
		logger:   logger,
		queue:    make(chan *syntheticsv1alpha1.HttpProbe, concurrency*16),
		executor: executor,
		metrics:  metrics,
		client:   kubeClient,
	}
}

func (p *WorkerPool) Start(ctx context.Context) error {
	p.once.Do(func() {
		workers := cap(p.queue) / 16
		if workers < 1 {
			workers = 1
		}
		for i := 0; i < workers; i++ {
			go p.worker(ctx)
		}
	})
	<-ctx.Done()
	return nil
}

func (p *WorkerPool) Enqueue(ctx context.Context, probe *syntheticsv1alpha1.HttpProbe) {
	select {
	case <-ctx.Done():
		return
	case p.queue <- probe:
	default:
		p.logger.Error(errors.New("queue full"), "dropping probe execution", "namespace", probe.Namespace, "name", probe.Name)
	}
}

func (p *WorkerPool) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case probe := <-p.queue:
			p.runProbe(ctx, probe)
		}
	}
}

func (p *WorkerPool) runProbe(ctx context.Context, probe *syntheticsv1alpha1.HttpProbe) {
	runCtx, cancel := context.WithTimeout(ctx, probe.Spec.Timeout.Duration)
	defer cancel()

	result := p.executor.Execute(runCtx, probe)
	key := types.NamespacedName{Namespace: probe.Namespace, Name: probe.Name}

	state := internalmetrics.ProbeState{
		Success:              boolToFloat(result.Success && !result.ConfigError),
		DurationMilliseconds: float64(result.Duration.Milliseconds()),
		LastRunTimestamp:     float64(result.Completed.Unix()),
		ConfigError:          boolToFloat(result.ConfigError),
		ConsecutiveFailures:  0,
	}

	_ = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &syntheticsv1alpha1.HttpProbe{}
		if err := p.client.Get(ctx, key, current); err != nil {
			return client.IgnoreNotFound(err)
		}

		original := current.DeepCopy()
		now := metav1.NewTime(result.Completed)
		current.Status.ObservedGeneration = current.Generation
		current.Status.LastRunTime = &now
		current.Status.Summary = &syntheticsv1alpha1.ProbeSummary{
			Success:     result.Success && !result.ConfigError,
			ConfigError: result.ConfigError,
			StatusCode:  result.StatusCode,
			Message:     result.Message,
		}

		if result.Success && !result.ConfigError {
			current.Status.ConsecutiveFailures = 0
			current.Status.LastSuccessTime = &now
			apimeta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
				Type:               syntheticsv1alpha1.ConditionReady,
				Status:             metav1.ConditionTrue,
				Reason:             syntheticsv1alpha1.ReasonProbeSucceeded,
				Message:            result.Message,
				LastTransitionTime: now,
				ObservedGeneration: current.Generation,
			})
		} else {
			if result.ConfigError {
				apimeta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
					Type:               syntheticsv1alpha1.ConditionReady,
					Status:             metav1.ConditionFalse,
					Reason:             syntheticsv1alpha1.ReasonConfigError,
					Message:            result.Message,
					LastTransitionTime: now,
					ObservedGeneration: current.Generation,
				})
			} else {
				current.Status.ConsecutiveFailures++
				current.Status.LastFailureTime = &now
				apimeta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
					Type:               syntheticsv1alpha1.ConditionReady,
					Status:             metav1.ConditionFalse,
					Reason:             syntheticsv1alpha1.ReasonProbeFailed,
					Message:            result.Message,
					LastTransitionTime: now,
					ObservedGeneration: current.Generation,
				})
			}
		}

		state.ConsecutiveFailures = float64(current.Status.ConsecutiveFailures)
		p.metrics.Upsert(key, state)

		return p.client.Status().Patch(ctx, current, client.MergeFrom(original))
	})
}

func boolToFloat(v bool) float64 {
	if v {
		return 1
	}
	return 0
}
