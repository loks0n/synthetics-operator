package metrics

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
)

func TestMetricLabelsAppearedOnEveryMetric(t *testing.T) {
	store, err := NewStore()
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	name := types.NamespacedName{Namespace: "default", Name: "probe"}
	store.Upsert(name, ProbeState{
		Kind:                 "HTTPProbe",
		Result:               ResultOK,
		DurationMilliseconds: 42,
		LastRunTimestamp:     9999,
		URL:                  "http://example.com",
		Method:               "GET",
		HTTPStatusCode:       200,
		HTTPVersion:          1.1,
	})
	store.SetMetricLabels("HTTPProbe", name, map[string]string{
		"team": "payments",
		"env":  "production",
	})

	body := scrape(t, store)
	want := []string{
		`synthetics_probe{`,
		`synthetics_probe_duration_ms{`,
		`synthetics_probe_last_run_timestamp{`,
		`synthetics_probe_http_status_code{`,
		`synthetics_probe_http_version{`,
		`synthetics_probe_http_info{`,
	}
	for _, metric := range want {
		for _, label := range []string{`team="payments"`, `env="production"`} {
			for line := range strings.SplitSeq(body, "\n") {
				if strings.HasPrefix(line, metric) && !strings.Contains(line, label) {
					t.Errorf("%s line missing %s:\n%s", metric, label, line)
				}
			}
		}
	}
}

func TestMetricLabelsClearRemovesLabels(t *testing.T) {
	store, err := NewStore()
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	name := types.NamespacedName{Namespace: "default", Name: "probe"}
	store.Upsert(name, ProbeState{Kind: "HTTPProbe", Result: ResultOK})
	store.SetMetricLabels("HTTPProbe", name, map[string]string{"team": "payments"})

	body := scrape(t, store)
	if !strings.Contains(body, `team="payments"`) {
		t.Fatal("expected team label before clear")
	}

	store.ClearMetricLabels("HTTPProbe", name)
	body = scrape(t, store)
	if strings.Contains(body, `team="payments"`) {
		t.Fatal("expected team label to disappear after ClearMetricLabels")
	}
}

func TestMetricLabelsOnTestMetrics(t *testing.T) {
	store, err := NewStore()
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	name := types.NamespacedName{Namespace: "default", Name: "test"}
	store.UpsertTest(name, TestState{Kind: "PlaywrightTest", Result: ResultOK})
	store.SetMetricLabels("PlaywrightTest", name, map[string]string{"env": "staging"})

	body := scrape(t, store)
	found := false
	for line := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(line, "synthetics_test{") && strings.Contains(line, `env="staging"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected env=staging on synthetics_test, got:\n%s", body)
	}
}

func TestMetricLabelsPropagatesToSuppressed(t *testing.T) {
	store, err := NewStore()
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	owner := types.NamespacedName{Namespace: "default", Name: "owner"}
	dep := types.NamespacedName{Namespace: "default", Name: "dep"}

	store.Upsert(dep, ProbeState{Kind: "HTTPProbe", Result: ResultConnectTimeout})
	store.Upsert(owner, ProbeState{Kind: "HTTPProbe", Result: ResultConnectTimeout})
	store.SetMetricLabels("HTTPProbe", owner, map[string]string{"team": "platform"})
	store.SetDepends("HTTPProbe", owner, []syntheticsv1alpha1.DependencyRef{
		{Kind: syntheticsv1alpha1.DependencyKindHTTPProbe, Name: "dep"},
	})

	body := scrape(t, store)
	found := false
	for line := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(line, "synthetics_probe_suppressed{") && strings.Contains(line, `team="platform"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected team=platform on synthetics_probe_suppressed, got:\n%s", body)
	}
}
