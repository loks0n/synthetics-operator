# Examples

Drop-in manifests for the CRDs the operator manages. Each one is meant to be a minimal starting point — copy, tweak, `kubectl apply -f`.

## Probes

| File | Demonstrates |
|---|---|
| [`httpprobe-basic.yaml`](httpprobe-basic.yaml) | Minimal HTTP health check with one assertion |
| [`httpprobe-advanced.yaml`](httpprobe-advanced.yaml) | POST + headers + body, TLS with custom CA, latency/SSL assertions, `metricLabels` |
| [`dnsprobe-basic.yaml`](dnsprobe-basic.yaml) | A-record probe against a specific resolver |
| [`dnsprobe-mx.yaml`](dnsprobe-mx.yaml) | MX-record probe for mail routing |

## Tests

| File | Demonstrates |
|---|---|
| [`k6test-basic.yaml`](k6test-basic.yaml) | k6 load test with thresholds inside the script, `runner.resources` tuning |
| [`playwrighttest-basic.yaml`](playwrighttest-basic.yaml) | Simple Playwright test against an in-cluster service |
| [`playwrighttest-advanced.yaml`](playwrighttest-advanced.yaml) | Multi-step browser flow with credentials from a `Secret` via `runner.env` |

## Cross-cutting

| File | Demonstrates |
|---|---|
| [`depends-chain.yaml`](depends-chain.yaml) | Three probes in a dependency chain — alert suppression filters cascaded failures to the root cause |

## After applying

Port-forward the operator's metrics service to inspect what each CR is emitting:

```sh
kubectl -n synthetics-system port-forward svc/synthetics-operator-metrics 8080:8080
curl -s http://localhost:8080/metrics | grep synthetics_
```

See the top-level README §3 for the full metric schema.
