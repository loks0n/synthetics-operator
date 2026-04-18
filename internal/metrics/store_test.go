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
		Result:               ResultOK,
		DurationMilliseconds: 42,
		LastRunTimestamp:     1000,
	}
	store.Upsert(key, state)

	got, ok := store.Snapshot(key)
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

	_, ok := store.Snapshot(types.NamespacedName{Namespace: "x", Name: "y"})
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
	store.Upsert(key, ProbeState{Result: ResultOK})
	store.Delete(key)

	_, ok := store.Snapshot(key)
	if ok {
		t.Fatal("expected Snapshot to return false after Delete")
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
