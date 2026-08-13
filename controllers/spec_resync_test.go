package controllers

import (
	"context"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
	"github.com/loks0n/synthetics-operator/internal/natsbus"
	"github.com/loks0n/synthetics-operator/internal/results"
)

type notifyingPublisher struct {
	fakePublisher
	published chan struct{}
}

func (p *notifyingPublisher) PublishSpec(ctx context.Context, msg results.SpecUpdate) error {
	if err := p.fakePublisher.PublishSpec(ctx, msg); err != nil {
		return err
	}
	select {
	case p.published <- struct{}{}:
	default:
	}
	return nil
}

type delayedResyncRequests struct {
	subscribed chan struct{}

	mu      sync.Mutex
	opts    []natsbus.SubscribeOption
	handler func(context.Context)
}

func (r *delayedResyncRequests) SubscribeSpecResyncRequests(ctx context.Context, handler func(context.Context), opts ...natsbus.SubscribeOption) error {
	r.mu.Lock()
	r.opts = opts
	r.handler = handler
	r.mu.Unlock()
	close(r.subscribed)
	<-ctx.Done()
	return nil
}

func (r *delayedResyncRequests) signalReady() {
	<-r.subscribed
	r.mu.Lock()
	opts := append([]natsbus.SubscribeOption(nil), r.opts...)
	r.mu.Unlock()
	natsbus.SignalReady(opts)
}

func TestSpecResyncRepublishesAllLiveSpecs(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := syntheticsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	probe := &syntheticsv1alpha1.HTTPProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Request: syntheticsv1alpha1.HTTPRequestSpec{URL: "https://example.com"},
		},
	}
	test := &syntheticsv1alpha1.PlaywrightTest{
		ObjectMeta: metav1.ObjectMeta{Name: "flow", Namespace: "default"},
	}
	tcp := &syntheticsv1alpha1.TCPProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "database", Namespace: "default"},
		Spec:       syntheticsv1alpha1.TCPProbeSpec{Target: syntheticsv1alpha1.TCPTarget{Host: "database", Port: 5432}},
	}

	cl := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(probe, test, tcp).Build()
	pub := &fakePublisher{}

	r := &SpecResyncer{Client: cl, Publisher: pub, Log: log.Log}
	r.resync(context.Background())

	pub.mu.Lock()
	defer pub.mu.Unlock()
	if len(pub.specs) != 3 {
		t.Fatalf("expected 3 spec updates, got %d: %+v", len(pub.specs), pub.specs)
	}
	seen := map[results.Kind]string{}
	for _, s := range pub.specs {
		if s.Deleted {
			t.Fatalf("resync must not publish tombstones, got %+v", s)
		}
		seen[s.Kind] = s.Name
	}
	if seen[results.KindHTTPProbe] != "web" || seen[results.KindPlaywrightTest] != "flow" || seen[results.KindTCPProbe] != "database" {
		t.Fatalf("unexpected specs republished: %v", seen)
	}
}

func TestSpecResyncWaitsForRequestSubscriptionBeforeInitialBroadcast(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := syntheticsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	probe := &syntheticsv1alpha1.HTTPProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: syntheticsv1alpha1.HTTPProbeSpec{
			Request: syntheticsv1alpha1.HTTPRequestSpec{URL: "https://example.com"},
		},
	}
	cl := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(probe).Build()
	pub := &notifyingPublisher{published: make(chan struct{}, 1)}
	reqs := &delayedResyncRequests{subscribed: make(chan struct{})}
	r := &SpecResyncer{Client: cl, Publisher: pub, Interval: time.Hour, Log: log.Log, Requests: reqs}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	<-reqs.subscribed
	select {
	case <-pub.published:
		t.Fatal("initial resync published before resync-request subscription was ready")
	case <-time.After(100 * time.Millisecond):
	}

	reqs.signalReady()
	select {
	case <-pub.published:
	case <-time.After(time.Second):
		t.Fatal("initial resync did not publish after resync-request subscription became ready")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("SpecResyncer did not stop after context cancellation")
	}
}
