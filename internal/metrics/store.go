package metrics

import (
	"context"
	"net/http"
	"sync"
	"time"

	promclient "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	apimetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/metric"
	"k8s.io/apimachinery/pkg/types"
)

type ProbeState struct {
	Success              float64
	DurationMilliseconds float64
	ConsecutiveFailures  float64
	LastRunTimestamp     float64
	ConfigError          float64
}

type Store struct {
	mu       sync.RWMutex
	probes   map[types.NamespacedName]ProbeState
	registry *promclient.Registry
	exporter *otelprom.Exporter
	provider *metric.MeterProvider
}

func NewStore() (*Store, error) {
	registry := promclient.NewRegistry()
	exporter, err := otelprom.New(otelprom.WithRegisterer(registry))
	if err != nil {
		return nil, err
	}
	provider := metric.NewMeterProvider(metric.WithReader(exporter))
	meter := provider.Meter("synthetics-operator")

	store := &Store{
		probes:   map[types.NamespacedName]ProbeState{},
		registry: registry,
		exporter: exporter,
		provider: provider,
	}

	successGauge, err := meter.Float64ObservableGauge("synthetics_probe_success")
	if err != nil {
		return nil, err
	}
	durationGauge, err := meter.Float64ObservableGauge("synthetics_probe_duration_ms")
	if err != nil {
		return nil, err
	}
	failuresGauge, err := meter.Float64ObservableGauge("synthetics_consecutive_failures")
	if err != nil {
		return nil, err
	}
	lastRunGauge, err := meter.Float64ObservableGauge("synthetics_last_run_timestamp")
	if err != nil {
		return nil, err
	}
	configErrorGauge, err := meter.Float64ObservableGauge("synthetics_probe_config_error")
	if err != nil {
		return nil, err
	}

	_, err = meter.RegisterCallback(func(_ context.Context, observer apimetric.Observer) error {
		store.mu.RLock()
		defer store.mu.RUnlock()
		for name, state := range store.probes {
			attrs := []attribute.KeyValue{
				attribute.String("name", name.Name),
				attribute.String("namespace", name.Namespace),
				attribute.String("kind", "HttpProbe"),
			}
			observer.ObserveFloat64(successGauge, state.Success, apimetric.WithAttributes(attrs...))
			observer.ObserveFloat64(durationGauge, state.DurationMilliseconds, apimetric.WithAttributes(attrs...))
			observer.ObserveFloat64(failuresGauge, state.ConsecutiveFailures, apimetric.WithAttributes(attrs...))
			observer.ObserveFloat64(lastRunGauge, state.LastRunTimestamp, apimetric.WithAttributes(attrs...))
			observer.ObserveFloat64(configErrorGauge, state.ConfigError, apimetric.WithAttributes(attrs...))
		}
		return nil
	}, successGauge, durationGauge, failuresGauge, lastRunGauge, configErrorGauge)
	if err != nil {
		return nil, err
	}

	return store, nil
}

func (s *Store) Upsert(name types.NamespacedName, state ProbeState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.probes[name] = state
}

func (s *Store) Delete(name types.NamespacedName) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.probes, name)
}

func (s *Store) Snapshot(name types.NamespacedName) (ProbeState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.probes[name]
	return state, ok
}

func (s *Store) Server(addr string) *Server {
	return &Server{
		addr: addr,
		handler: promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{
			EnableOpenMetrics: true,
		}),
	}
}

type Server struct {
	addr    string
	handler http.Handler
}

func (s *Server) Start(ctx context.Context) error {
	server := &http.Server{
		Addr:              s.addr,
		Handler:           s.handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
