package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/types"
)

func TestNewStore(t *testing.T) {
	if _, err := NewStore(); err != nil {
		t.Fatalf("NewStore() error: %v", err)
	}
}

func TestStoreUpsertAndSnapshot(t *testing.T) {
	store, err := NewStore()
	if err != nil {
		t.Fatalf("NewStore() error: %v", err)
	}

	key := types.NamespacedName{Namespace: "default", Name: "my-probe"}
	state := ProbeState{
		Kind:                 "HTTPProbe",
		Result:               ResultOK,
		DurationMilliseconds: 42,
		LastRunTimestamp:     1000,
	}
	store.Upsert(key, state)

	got, ok := store.Snapshot("HTTPProbe", key)
	if !ok {
		t.Fatal("expected Snapshot to find key after Upsert")
	}
	if diff := cmp.Diff(state, got); diff != "" {
		t.Fatalf("Snapshot mismatch (-want +got):\n%s", diff)
	}
}

func TestStoreSnapshotMissing(t *testing.T) {
	store, err := NewStore()
	if err != nil {
		t.Fatalf("NewStore() error: %v", err)
	}

	_, ok := store.Snapshot("HTTPProbe", types.NamespacedName{Namespace: "x", Name: "y"})
	if ok {
		t.Fatal("expected Snapshot to return false for unknown key")
	}
}

func TestStoreDelete(t *testing.T) {
	store, err := NewStore()
	if err != nil {
		t.Fatalf("NewStore() error: %v", err)
	}

	key := types.NamespacedName{Namespace: "default", Name: "probe"}
	store.Upsert(key, ProbeState{Kind: "HTTPProbe", Result: ResultOK})
	store.Delete("HTTPProbe", key)

	_, ok := store.Snapshot("HTTPProbe", key)
	if ok {
		t.Fatal("expected Snapshot to return false after Delete")
	}
}

func TestStoreKeepsSameNamedProbeKindsIndependent(t *testing.T) {
	store, err := NewStore()
	if err != nil {
		t.Fatal(err)
	}
	name := types.NamespacedName{Namespace: "default", Name: "shared"}
	store.Upsert(name, ProbeState{Kind: "HTTPProbe", Result: ResultOK})
	store.Upsert(name, ProbeState{Kind: "TCPProbe", Result: ResultConnectRefused})

	if _, ok := store.Snapshot("HTTPProbe", name); !ok {
		t.Fatal("HTTPProbe missing")
	}
	if tcp, ok := store.Snapshot("TCPProbe", name); !ok || tcp.Result != ResultConnectRefused {
		t.Fatalf("TCPProbe missing or overwritten: %+v %v", tcp, ok)
	}

	store.Delete("TCPProbe", name)
	if _, ok := store.Snapshot("HTTPProbe", name); !ok {
		t.Fatal("TCPProbe deletion removed same-named HTTPProbe")
	}
	if _, ok := store.Snapshot("TCPProbe", name); ok {
		t.Fatal("TCPProbe remained after deletion")
	}
}

func TestStoreMetricsScrape(t *testing.T) {
	store, err := NewStore()
	if err != nil {
		t.Fatalf("NewStore() error: %v", err)
	}

	key := types.NamespacedName{Namespace: "default", Name: "probe"}
	store.Upsert(key, ProbeState{
		Kind:                 "HTTPProbe",
		Result:               ResultOK,
		DurationMilliseconds: 55,
		LastRunTimestamp:     9999,
	})

	srv := httptest.NewServer(store.Server("").handler)
	defer srv.Close()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("scrape metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	for _, want := range []string{
		"synthetics_probe{",
		"synthetics_probe_duration_ms",
		"synthetics_probe_last_run_timestamp",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}

	if strings.Contains(text, "synthetics_probe_tls_cert_expiry") {
		t.Error("tls_cert_expiry metric should not be present when TLSCertExpiry is 0")
	}
}

func TestStoreTCPMetrics(t *testing.T) {
	store, err := NewStore()
	if err != nil {
		t.Fatal(err)
	}
	store.Upsert(types.NamespacedName{Namespace: "default", Name: "mysql"}, ProbeState{
		Kind: "TCPProbe", Result: ResultOK, DurationMilliseconds: 12,
		TCPHost: "mysql.default.svc", TCPPort: 3306,
		AssertionResults: []AssertionResult{{Name: "fast", Expr: "duration_ms < 1000", Result: 1}},
	})

	srv := httptest.NewServer(store.Server("").handler)
	defer srv.Close()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	for _, want := range []string{
		`synthetics_probe_tcp_info{host="mysql.default.svc",kind="TCPProbe",name="mysql",namespace="default",port="3306"} 1`,
		`synthetics_probe{host="mysql.default.svc",kind="TCPProbe",name="mysql",namespace="default",port="3306"} 1`,
		`synthetics_probe_assertion_result{assertion="fast",expr="duration_ms < 1000",host="mysql.default.svc",kind="TCPProbe",name="mysql",namespace="default",port="3306"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics output missing %q\n%s", want, text)
		}
	}
}

func TestServerDoesNotRequireLeaderElection(t *testing.T) {
	// The metrics server must run on every replica so Prometheus scrapes
	// succeed against any pod fronted by the Service. See NeedLeaderElection.
	store, err := NewStore()
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if store.Server("").NeedLeaderElection() {
		t.Fatal("Server.NeedLeaderElection must return false")
	}
}

func TestStoreTLSCertExpiryMetric(t *testing.T) {
	store, err := NewStore()
	if err != nil {
		t.Fatalf("NewStore() error: %v", err)
	}

	key := types.NamespacedName{Namespace: "default", Name: "tls-probe"}
	store.Upsert(key, ProbeState{
		Kind:          "HTTPProbe",
		Result:        ResultOK,
		TLSCertExpiry: 1800000000,
	})

	srv := httptest.NewServer(store.Server("").handler)
	defer srv.Close()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("scrape metrics: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	if !strings.Contains(text, "synthetics_probe_tls_cert_expiry_timestamp_seconds") {
		t.Error("expected synthetics_probe_tls_cert_expiry_timestamp_seconds in metrics output")
	}
	if !strings.Contains(text, "1.8e+09") && !strings.Contains(text, "1800000000") {
		t.Errorf("expected cert expiry value 1800000000 in metrics output, got:\n%s", text)
	}
}

func TestStoreDNSMetricsNameTheQuestion(t *testing.T) {
	// A DNS failure is about a name and the nameserver that was asked for it, so
	// every metric for the probe says both without a join.
	store, err := NewStore()
	if err != nil {
		t.Fatal(err)
	}
	store.Upsert(types.NamespacedName{Namespace: "default", Name: "zone-ns1"}, ProbeState{
		Kind: "DNSProbe", Result: ResultOK, DurationMilliseconds: 8,
		DNSQueryName: "cloud.example.com", DNSResolver: "ns1.example.net:53",
		DNSAnswerCount: 2,
	})

	text := scrape(t, store)
	for _, want := range []string{
		`synthetics_probe{kind="DNSProbe",name="zone-ns1",namespace="default",query="cloud.example.com",resolver="ns1.example.net:53"} 1`,
		`synthetics_probe_dns_response_answer_count{kind="DNSProbe",name="zone-ns1",namespace="default",query="cloud.example.com",resolver="ns1.example.net:53"} 2`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics output missing %q\n%s", want, text)
		}
	}
}

func TestStoreDNSMetricsWithoutAResolver(t *testing.T) {
	// A probe that names no resolver asks whichever one the runner is configured
	// with, so there is no resolver to name and the label is left off.
	store, err := NewStore()
	if err != nil {
		t.Fatal(err)
	}
	store.Upsert(types.NamespacedName{Namespace: "default", Name: "default-resolver"}, ProbeState{
		Kind: "DNSProbe", Result: ResultOK, DNSQueryName: "cloud.example.com",
	})

	text := scrape(t, store)
	want := `synthetics_probe{kind="DNSProbe",name="default-resolver",namespace="default",query="cloud.example.com"} 1`
	if !strings.Contains(text, want) {
		t.Errorf("metrics output missing %q\n%s", want, text)
	}
	if strings.Contains(text, `resolver=""`) {
		t.Errorf("empty resolver label emitted\n%s", text)
	}
}
