package heartbeat

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/loks0n/synthetics-operator/internal/natsbus"
	"github.com/loks0n/synthetics-operator/internal/results"
)

// fakeBus records published pings and lets a test drive the spec stream by
// hand, so token-index behaviour can be exercised without a NATS server.
type fakeBus struct {
	mu           sync.Mutex
	pings        []results.HeartbeatPing
	publishErr   error
	resyncCalled bool
	// specsSubscribed and resyncBeforeSubscribe pin the ordering contract:
	// core NATS drops a message with no current subscriber, so requesting a
	// resync before the spec subscription exists silently loses the reply.
	specsSubscribed       bool
	resyncBeforeSubscribe bool
	handler               func(context.Context, results.SpecUpdate)
	subscribed            chan struct{}
}

func newFakeBus() *fakeBus {
	return &fakeBus{subscribed: make(chan struct{})}
}

func (b *fakeBus) SubscribeSpecs(ctx context.Context, handler func(context.Context, results.SpecUpdate), opts ...natsbus.SubscribeOption) error {
	b.mu.Lock()
	b.handler = handler
	b.specsSubscribed = true
	b.mu.Unlock()
	// Honour WithReady exactly as the real client does, so the receiver's
	// subscribe-before-request ordering is exercised rather than bypassed.
	natsbus.SignalReady(opts)
	close(b.subscribed)
	<-ctx.Done()
	return nil
}

func (b *fakeBus) PublishHeartbeatPing(_ context.Context, msg results.HeartbeatPing) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.publishErr != nil {
		return b.publishErr
	}
	b.pings = append(b.pings, msg)
	return nil
}

func (b *fakeBus) RequestSpecResync(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resyncCalled = true
	b.resyncBeforeSubscribe = !b.specsSubscribed
	return nil
}

func (b *fakeBus) send(spec results.SpecUpdate) {
	b.mu.Lock()
	handler := b.handler
	b.mu.Unlock()
	handler(context.Background(), spec)
}

func (b *fakeBus) recorded() []results.HeartbeatPing {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]results.HeartbeatPing(nil), b.pings...)
}

const testToken = "hb_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func heartbeatSpec(namespace, name, token string) results.SpecUpdate {
	return results.SpecUpdate{
		Kind:      results.KindHeartbeat,
		Name:      name,
		Namespace: namespace,
		Heartbeat: &results.HeartbeatSpecPayload{Token: token, PeriodMs: 60000, GraceMs: 60000},
	}
}

// startReceiver spins up a Receiver against a fake bus and returns both plus
// a test server fronting the handler. Cleanup is registered on t.
func startReceiver(t *testing.T) (*Receiver, *fakeBus, *httptest.Server) {
	t.Helper()
	bus := newFakeBus()
	receiver := &Receiver{
		Log:    logr.Discard(),
		Bus:    bus,
		Now:    func() time.Time { return time.Unix(1700000000, 0).UTC() },
		Warmup: time.Hour, // never auto-warm; tests drive readiness via specs
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = receiver.Start(ctx)
	}()
	<-bus.subscribed

	server := httptest.NewServer(receiver.Handler())
	t.Cleanup(func() {
		server.Close()
		cancel()
		<-done
	})
	return receiver, bus, server
}

// reply is a fully-read response. Returning a value rather than an
// *http.Response keeps every call site free of body-closing ceremony.
type reply struct {
	StatusCode int
	Header     http.Header
	Body       string
}

func do(t *testing.T, server *httptest.Server, method, path, body string) reply {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, reader)
	if err != nil {
		t.Fatalf("building %s %s: %v", method, path, err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading %s %s: %v", method, path, err)
	}
	return reply{StatusCode: response.StatusCode, Header: response.Header, Body: string(raw)}
}

func get(t *testing.T, server *httptest.Server, path string) reply {
	t.Helper()
	return do(t, server, http.MethodGet, path, "")
}

func TestReceiverRequestsResyncOnStart(t *testing.T) {
	_, bus, _ := startReceiver(t)
	bus.mu.Lock()
	defer bus.mu.Unlock()
	if !bus.resyncCalled {
		t.Fatal("expected the receiver to request a spec resync on start")
	}
}

// Regression: the resync request must not be published until the spec
// subscription is established. Core NATS has no retention, so a reply that
// arrives before the subscriber exists is dropped, and the receiver falls
// back to the controller's periodic tick — a measured ~39s of 503s on a cold
// pod instead of a few milliseconds.
func TestReceiverSubscribesBeforeRequestingResync(t *testing.T) {
	_, bus, _ := startReceiver(t)
	bus.mu.Lock()
	defer bus.mu.Unlock()
	if !bus.resyncCalled {
		t.Fatal("expected the receiver to request a spec resync on start")
	}
	if bus.resyncBeforeSubscribe {
		t.Fatal("resync was requested before the spec subscription existed; the reply would be dropped")
	}
}

func TestPingPublishesForKnownToken(t *testing.T) {
	_, bus, server := startReceiver(t)
	bus.send(heartbeatSpec("prod", "db-backup", testToken))

	if got := get(t, server, "/"+testToken).StatusCode; got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}

	pings := bus.recorded()
	if len(pings) != 1 {
		t.Fatalf("published %d pings, want 1", len(pings))
	}
	if pings[0].Name != "db-backup" || pings[0].Namespace != "prod" {
		t.Fatalf("ping identity = %s/%s, want prod/db-backup", pings[0].Namespace, pings[0].Name)
	}
	if pings[0].Failed {
		t.Fatal("a plain ping must not be marked failed")
	}
	if !pings[0].ReceivedAt.Equal(time.Unix(1700000000, 0)) {
		t.Fatalf("ReceivedAt = %v, want the injected clock", pings[0].ReceivedAt)
	}
}

func TestPingOutcomeFromURLSuffix(t *testing.T) {
	tests := []struct {
		name         string
		path         string
		wantStatus   int
		wantFailed   bool
		wantExitCode int
	}{
		{name: "plain", path: "/" + testToken, wantStatus: http.StatusOK},
		{name: "explicit fail", path: "/" + testToken + "/fail", wantStatus: http.StatusOK, wantFailed: true},
		{name: "exit code zero", path: "/" + testToken + "/0", wantStatus: http.StatusOK},
		{name: "exit code nonzero", path: "/" + testToken + "/2", wantStatus: http.StatusOK, wantFailed: true, wantExitCode: 2},
		{name: "trailing slash", path: "/" + testToken + "/", wantStatus: http.StatusOK},
		{name: "unknown suffix", path: "/" + testToken + "/boom", wantStatus: http.StatusNotFound},
		{name: "negative exit code", path: "/" + testToken + "/-1", wantStatus: http.StatusNotFound},
		{name: "extra segment", path: "/" + testToken + "/0/0", wantStatus: http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, bus, server := startReceiver(t)
			bus.send(heartbeatSpec("prod", "db-backup", testToken))

			if got := get(t, server, tc.path).StatusCode; got != tc.wantStatus {
				t.Fatalf("status = %d, want %d", got, tc.wantStatus)
			}
			pings := bus.recorded()
			if tc.wantStatus != http.StatusOK {
				if len(pings) != 0 {
					t.Fatalf("published %d pings for a rejected path, want 0", len(pings))
				}
				return
			}
			if len(pings) != 1 {
				t.Fatalf("published %d pings, want 1", len(pings))
			}
			if pings[0].Failed != tc.wantFailed {
				t.Errorf("Failed = %v, want %v", pings[0].Failed, tc.wantFailed)
			}
			if pings[0].ExitCode != tc.wantExitCode {
				t.Errorf("ExitCode = %d, want %d", pings[0].ExitCode, tc.wantExitCode)
			}
		})
	}
}

func TestUnknownTokenIs404OnceWarm(t *testing.T) {
	_, bus, server := startReceiver(t)
	// Any spec marks the receiver warm, including one for a different token.
	bus.send(heartbeatSpec("prod", "other", testToken))

	unknown := "hb_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if got := get(t, server, "/"+unknown).StatusCode; got != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", got)
	}
	if len(bus.recorded()) != 0 {
		t.Fatal("an unknown token must not publish a ping")
	}
}

// A cold receiver has not learned any tokens yet. Answering 404 there would
// make a perfectly valid cron job record a failure, so it must answer 503.
func TestUnknownTokenIs503WhileWarmingUp(t *testing.T) {
	_, _, server := startReceiver(t)

	response := get(t, server, "/"+testToken)
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.StatusCode)
	}
	if response.Header.Get("Retry-After") == "" {
		t.Error("expected a Retry-After header so clients know to come back")
	}
}

func TestReadyzReflectsWarmup(t *testing.T) {
	_, bus, server := startReceiver(t)

	if got := get(t, server, "/readyz").StatusCode; got != http.StatusServiceUnavailable {
		t.Fatalf("cold /readyz = %d, want 503", got)
	}
	bus.send(heartbeatSpec("prod", "db-backup", testToken))
	if got := get(t, server, "/readyz").StatusCode; got != http.StatusOK {
		t.Fatalf("warm /readyz = %d, want 200", got)
	}
}

// An empty cluster publishes no Heartbeat specs at all, so readiness cannot
// depend on one arriving.
func TestWarmupTimerMarksReadyWithoutSpecs(t *testing.T) {
	bus := newFakeBus()
	receiver := &Receiver{Log: logr.Discard(), Bus: bus, Warmup: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = receiver.Start(ctx) }()
	<-bus.subscribed

	deadline := time.Now().Add(2 * time.Second)
	for !receiver.Ready() {
		if time.Now().After(deadline) {
			t.Fatal("receiver never became ready after the warmup window")
		}
		time.Sleep(time.Millisecond)
	}
}

// A rotated token must stop working. Leaving the old one live would mean a
// leaked token could keep a dead job looking healthy indefinitely.
func TestTokenRotationEvictsThePreviousToken(t *testing.T) {
	_, bus, server := startReceiver(t)
	bus.send(heartbeatSpec("prod", "db-backup", testToken))

	rotated := "hb_cccccccccccccccccccccccccccccccc"
	bus.send(heartbeatSpec("prod", "db-backup", rotated))

	if got := get(t, server, "/"+testToken).StatusCode; got != http.StatusNotFound {
		t.Fatalf("old token status = %d, want 404", got)
	}
	if got := get(t, server, "/"+rotated).StatusCode; got != http.StatusOK {
		t.Fatalf("rotated token status = %d, want 200", got)
	}
}

func TestTombstoneEvictsToken(t *testing.T) {
	_, bus, server := startReceiver(t)
	bus.send(heartbeatSpec("prod", "db-backup", testToken))
	bus.send(results.SpecUpdate{
		Kind:      results.KindHeartbeat,
		Name:      "db-backup",
		Namespace: "prod",
		Deleted:   true,
	})

	if got := get(t, server, "/"+testToken).StatusCode; got != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 after deletion", got)
	}
}

// Two Heartbeats in different namespaces may share a name; the index is keyed
// by token, so both must resolve independently.
func TestTokensAreNamespaceScoped(t *testing.T) {
	_, bus, server := startReceiver(t)
	other := "hb_dddddddddddddddddddddddddddddddd"
	bus.send(heartbeatSpec("prod", "db-backup", testToken))
	bus.send(heartbeatSpec("staging", "db-backup", other))

	get(t, server, "/"+testToken)
	get(t, server, "/"+other)

	pings := bus.recorded()
	if len(pings) != 2 {
		t.Fatalf("published %d pings, want 2", len(pings))
	}
	if pings[0].Namespace != "prod" || pings[1].Namespace != "staging" {
		t.Fatalf("namespaces = %q, %q; want prod, staging", pings[0].Namespace, pings[1].Namespace)
	}
}

func TestSpecsForOtherKindsAreIgnored(t *testing.T) {
	receiver, bus, _ := startReceiver(t)
	bus.send(results.SpecUpdate{
		Kind:      results.KindHTTPProbe,
		Name:      "api",
		Namespace: "prod",
		HTTPProbe: &results.HTTPProbeSpecPayload{URL: "https://example.test"},
	})
	if len(receiver.Snapshot()) != 0 {
		t.Fatal("a non-Heartbeat spec must not enter the token index")
	}
}

func TestPostBodyIsCapturedAndTruncated(t *testing.T) {
	_, bus, server := startReceiver(t)
	bus.send(heartbeatSpec("prod", "db-backup", testToken))

	do(t, server, http.MethodPost, "/"+testToken, strings.Repeat("x", maxOutputBytes*2))

	pings := bus.recorded()
	if len(pings) != 1 {
		t.Fatalf("published %d pings, want 1", len(pings))
	}
	if len(pings[0].Output) != maxOutputBytes {
		t.Fatalf("output length = %d, want it truncated to %d", len(pings[0].Output), maxOutputBytes)
	}
}

func TestGetBodyIsNotCaptured(t *testing.T) {
	_, bus, server := startReceiver(t)
	bus.send(heartbeatSpec("prod", "db-backup", testToken))
	get(t, server, "/"+testToken)

	if output := bus.recorded()[0].Output; output != "" {
		t.Fatalf("Output = %q, want empty for a GET", output)
	}
}

func TestUnsupportedMethodIsRejected(t *testing.T) {
	_, bus, server := startReceiver(t)
	bus.send(heartbeatSpec("prod", "db-backup", testToken))

	response := do(t, server, http.MethodDelete, "/"+testToken, "")

	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", response.StatusCode)
	}
	if allow := response.Header.Get("Allow"); allow != "GET, HEAD, POST" {
		t.Errorf("Allow = %q, want %q", allow, "GET, HEAD, POST")
	}
}

// If the ping can't reach NATS, telling the caller "ok" would let a job
// believe it checked in when nothing recorded it.
func TestPublishFailureReturns500(t *testing.T) {
	_, bus, server := startReceiver(t)
	bus.send(heartbeatSpec("prod", "db-backup", testToken))

	bus.mu.Lock()
	bus.publishErr = errors.New("nats is down")
	bus.mu.Unlock()

	if got := get(t, server, "/"+testToken).StatusCode; got != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", got)
	}
}

// The token is a credential; an error page must not echo it back.
func TestErrorResponsesDoNotEchoTheToken(t *testing.T) {
	_, bus, server := startReceiver(t)
	bus.send(heartbeatSpec("prod", "other", testToken))

	unknown := "hb_eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	if body := get(t, server, "/"+unknown).Body; strings.Contains(body, unknown) {
		t.Fatalf("404 body echoed the token: %q", body)
	}
}
